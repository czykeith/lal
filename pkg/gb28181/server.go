// Copyright 2024, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package gb28181

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/q191201771/naza/pkg/nazanet"
)

// Gb28181Server GB28181信令服务器
type Gb28181Server struct {
	addr          string
	conn          *nazanet.UdpConnection
	udpConn       *net.UDPConn // 底层UDP连接，用于发送数据
	config        *ServerConfig
	devices       map[string]*Device            // 设备ID -> 设备信息
	streams       map[string]*Stream            // stream_name -> 流信息
	deviceStreams map[string]map[string]*Stream // 设备ID -> stream_name -> 流信息（索引优化）
	nonces        map[string]string             // 设备ID -> nonce（用于认证）
	mutex         sync.RWMutex
	stopChan      chan struct{} // 停止信号
	onInvite      func(deviceId, channelId, streamName string, port int, isTcp bool) error
	onBye         func(deviceId, channelId, streamName string) error
	onReconnect   func(deviceId string) error
}

// ServerConfig GB28181服务器配置
type ServerConfig struct {
	LocalSipId           string // 本地SIP ID
	LocalSipIp           string // 本地SIP IP
	LocalSipPort         int    // 本地SIP端口
	LocalSipDomain       string // 本地SIP域
	Username             string // 认证用户名（可选）
	Password             string // 认证密码（可选）
	Expires              int    // 注册过期时间（秒）
	CatalogQueryInterval int    // Catalog查询间隔（秒），0表示不启用定时查询
	// 视频参数配置
	VideoCodec     string // 视频编码格式：H264/H265（默认H264）
	VideoWidth     int    // 视频宽度（分辨率，默认0表示不指定）
	VideoHeight    int    // 视频高度（分辨率，默认0表示不指定）
	VideoBitrate   int    // 视频码率（kbps，默认0表示不指定）
	VideoFramerate int    // 视频帧率（fps，默认0表示不指定）
	VideoProfile   string // H264 Profile：baseline/main/high（默认不指定）
	VideoLevel     string // H264 Level：如3.1、4.0等（默认不指定）
}

// Device 设备信息
type Device struct {
	DeviceId          string
	DeviceName        string
	Ip                string
	Port              int
	Status            string // online/offline
	RegisterTime      time.Time
	KeepaliveTime     time.Time     // 最后心跳时间
	KeepaliveInterval time.Duration // 心跳间隔（根据客户端实际心跳计算）
	Channels          map[string]*Channel
	mutex             sync.RWMutex
}

// GetChannels 获取通道列表的副本（线程安全）
func (d *Device) GetChannels() []*Channel {
	d.mutex.RLock()
	defer d.mutex.RUnlock()

	channels := make([]*Channel, 0, len(d.Channels))
	for _, ch := range d.Channels {
		channels = append(channels, ch)
	}
	return channels
}

// Channel 通道信息
type Channel struct {
	ChannelId   string
	ChannelName string
	Status      string // idle/streaming
	StreamName  string
}

// Stream 流信息
type Stream struct {
	DeviceId   string
	ChannelId  string
	StreamName string
	CallId     string
	SessionId  string
	Port       int
	IsTcp      bool
	StartTime  time.Time
}

// NewGb28181Server 创建GB28181信令服务器
func NewGb28181Server(config *ServerConfig) *Gb28181Server {
	if config.Expires == 0 {
		config.Expires = 3600
	}
	if config.LocalSipDomain == "" {
		config.LocalSipDomain = config.LocalSipId
	}

	server := &Gb28181Server{
		config:        config,
		devices:       make(map[string]*Device),
		streams:       make(map[string]*Stream),
		deviceStreams: make(map[string]map[string]*Stream),
		nonces:        make(map[string]string),
		stopChan:      make(chan struct{}),
	}

	// 启动设备状态监控
	go server.monitorDevices()

	// 启动定时Catalog查询（如果配置了查询间隔）
	if config.CatalogQueryInterval > 0 {
		go server.scheduleCatalogQuery()
	}

	return server
}

// getDeviceDomain 获取设备域（GB28181标准：使用设备ID的前10位作为区域编码/域）
func getDeviceDomain(deviceId string) string {
	if len(deviceId) >= 10 {
		return deviceId[:10] // 使用前10位作为域（区域编码）
	}
	return deviceId // 如果设备ID长度不足10位，使用完整ID
}

// SetOnInvite 设置INVITE回调
func (s *Gb28181Server) SetOnInvite(fn func(deviceId, channelId, streamName string, port int, isTcp bool) error) {
	s.onInvite = fn
}

// SetOnBye 设置BYE回调
func (s *Gb28181Server) SetOnBye(fn func(deviceId, channelId, streamName string) error) {
	s.onBye = fn
}

// SetOnReconnect 设置设备重连回调
func (s *Gb28181Server) SetOnReconnect(fn func(deviceId string) error) {
	s.onReconnect = fn
}

// Listen 监听UDP端口
func (s *Gb28181Server) Listen() error {
	addr := fmt.Sprintf(":%d", s.config.LocalSipPort)

	// 创建底层UDP连接用于发送
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	s.udpConn = udpConn

	// 创建UdpConnection用于接收（通过RunLoop）
	conn, err := nazanet.NewUdpConnection(func(option *nazanet.UdpConnectionOption) {
		option.LAddr = addr
		option.Conn = udpConn
	})
	if err != nil {
		udpConn.Close()
		return err
	}
	s.conn = conn
	s.addr = addr
	Log.Infof("GB28181 server listen on %s", addr)
	return nil
}

// RunLoop 运行循环
func (s *Gb28181Server) RunLoop() error {
	return s.conn.RunLoop(func(b []byte, raddr *net.UDPAddr, err error) bool {
		if len(b) == 0 && err != nil {
			return false
		}

		s.handleMessage(b, raddr)
		return true
	})
}

// handleMessage 处理接收到的SIP消息
func (s *Gb28181Server) handleMessage(data []byte, raddr *net.UDPAddr) {
	// 打印原始消息内容（用于调试）
	Log.Infof("========== GB28181 收到消息 ==========")
	Log.Infof("来源地址: %s:%d", raddr.IP.String(), raddr.Port)
	Log.Infof("消息长度: %d 字节", len(data))
	Log.Infof("原始消息内容:\n%s", string(data))
	Log.Infof("----------------------------------------")

	msg, err := ParseSipMessage(data)
	if err != nil {
		Log.Warnf("parse sip message failed. err=%+v", err)
		return
	}

	// 打印解析后的消息详细信息
	Log.Infof("解析后的消息信息:")
	Log.Infof("  方法: %s", msg.Method)
	Log.Infof("  请求URI: %s", msg.RequestUri)
	Log.Infof("  状态码: %d", msg.StatusCode)
	Log.Infof("  状态文本: %s", msg.StatusText)
	Log.Infof("  消息头:")
	for key, value := range msg.Headers {
		Log.Infof("    %s: %s", key, value)
	}
	if msg.Body != "" {
		bodyPreviewLen := 500
		if len(msg.Body) < bodyPreviewLen {
			bodyPreviewLen = len(msg.Body)
		}
		Log.Infof("  消息体长度: %d 字节", len(msg.Body))
		Log.Infof("  消息体内容(前%d字节):\n%s", bodyPreviewLen, msg.Body[:bodyPreviewLen])
		if len(msg.Body) > bodyPreviewLen {
			Log.Infof("  ... (消息体还有 %d 字节未显示)", len(msg.Body)-bodyPreviewLen)
		}
	}
	Log.Infof("========================================")

	switch msg.Method {
	case SipMethodRegister:
		s.handleRegister(msg, raddr)
	case SipMethodMessage:
		s.handleKeepalive(msg, raddr)
	case SipMethodInvite:
		s.handleInvite(msg, raddr)
	case SipMethodAck:
		s.handleAck(msg, raddr)
	case SipMethodBye:
		s.handleBye(msg, raddr)
	default:
		if msg.StatusCode > 0 {
			s.handleResponse(msg, raddr)
		}
	}
}

// handleRegister 处理REGISTER请求
func (s *Gb28181Server) handleRegister(msg *SipMessage, raddr *net.UDPAddr) {
	deviceId := ExtractDeviceId(msg.Headers["from"])
	if deviceId == "" {
		Log.Warnf("extract device id failed. from=%s", msg.Headers["from"])
		s.sendResponse(msg, 400, "Bad Request", raddr)
		return
	}

	// 检查是否需要认证
	authHeader := msg.Headers["authorization"]
	if s.config.Password != "" {
		// 如果配置了密码，需要认证
		if authHeader == "" {
			// 没有 Authorization 头，发送 401 挑战
			nonce := GenerateNonce()
			s.mutex.Lock()
			s.nonces[deviceId] = nonce
			s.mutex.Unlock()

			realm := s.config.LocalSipDomain
			if realm == "" {
				realm = s.config.LocalSipId
			}

			callId := msg.Headers["call-id"]
			from := msg.Headers["from"]
			to := msg.Headers["to"]
			cseq := msg.Headers["cseq"]
			via := msg.Headers["via"]

			headers := map[string]string{
				"Via":              via,
				"From":             from,
				"To":               to + ";tag=" + GenerateTag(),
				"Call-ID":          callId,
				"CSeq":             cseq,
				"WWW-Authenticate": BuildWWWAuthenticate(realm, nonce),
				"Content-Length":   "0",
			}

			response := BuildSipResponse(401, "Unauthorized", headers, "")
			s.sendRaw(response, raddr)
			Log.Infof("send 401 challenge to device. device_id=%s", deviceId)
			return
		}

		// 验证认证信息
		auth := ParseAuthorization(authHeader)
		if auth == nil {
			Log.Warnf("parse authorization header failed. device_id=%s", deviceId)
			s.sendResponse(msg, 401, "Unauthorized", raddr)
			return
		}

		// 获取存储的 nonce
		s.mutex.RLock()
		storedNonce, hasNonce := s.nonces[deviceId]
		s.mutex.RUnlock()

		if !hasNonce || storedNonce != auth.Nonce {
			Log.Warnf("invalid nonce. device_id=%s, stored=%s, received=%s", deviceId, storedNonce, auth.Nonce)
			s.sendResponse(msg, 401, "Unauthorized", raddr)
			return
		}

		// 验证用户名和密码
		username := s.config.Username
		if username == "" {
			username = deviceId
		}

		uri := msg.RequestUri
		if !auth.Verify(username, s.config.Password, "REGISTER", uri) {
			Log.Warnf("authentication failed. device_id=%s, username=%s", deviceId, username)
			s.sendResponse(msg, 403, "Forbidden", raddr)
			return
		}

		// 认证成功，清除 nonce
		s.mutex.Lock()
		delete(s.nonces, deviceId)
		s.mutex.Unlock()

		Log.Infof("device authentication success. device_id=%s", deviceId)
	}

	// 提取Contact信息
	contact := msg.Headers["contact"]
	ip, port := ExtractContactAddr(contact)
	if ip == "" {
		ip = raddr.IP.String()
	}
	if port == 0 {
		port = raddr.Port
	}

	// 获取过期时间
	expires := s.config.Expires
	if expHeader := msg.Headers["expires"]; expHeader != "" {
		if exp, err := strconv.Atoi(expHeader); err == nil && exp > 0 {
			expires = exp
		}
	}

	// 检查是否是注销请求（Expires=0）
	if expires == 0 {
		// 设备注销：标记为离线，但不删除设备，以便API接口能查询到离线设备信息
		s.mutex.Lock()
		device, exists := s.devices[deviceId]
		if exists {
			// 更新设备状态为离线
			device.Status = "offline"
			Log.Infof("device unregister, marked as offline. device_id=%s", deviceId)
		} else {
			Log.Warnf("device unregister but device not found. device_id=%s", deviceId)
		}
		// 清除认证nonce
		delete(s.nonces, deviceId)
		s.mutex.Unlock()

		// 发送200 OK响应
		callId := msg.Headers["call-id"]
		from := msg.Headers["from"]
		to := msg.Headers["to"]
		cseq := msg.Headers["cseq"]
		via := msg.Headers["via"]

		headers := map[string]string{
			"Via":            via,
			"From":           from,
			"To":             to + ";tag=" + GenerateTag(),
			"Call-ID":        callId,
			"CSeq":           cseq,
			"Contact":        fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain),
			"Expires":        "0",
			"Content-Length": "0",
		}

		response := BuildSipResponse(200, "OK", headers, "")
		s.sendRaw(response, raddr)
		return
	}

	// 更新或创建设备
	s.mutex.Lock()
	device, exists := s.devices[deviceId]
	isReconnect := exists // 设备已存在，说明是重连
	if !exists {
		device = &Device{
			DeviceId:     deviceId,
			DeviceName:   deviceId,
			Channels:     make(map[string]*Channel),
			RegisterTime: time.Now(),
		}
		s.devices[deviceId] = device
	}
	device.Ip = ip
	device.Port = port
	device.Status = "online"
	device.KeepaliveTime = time.Now()
	s.mutex.Unlock()

	Log.Infof("device register. device_id=%s, ip=%s, port=%d, expires=%d, is_reconnect=%v", deviceId, ip, port, expires, isReconnect)

	// 设备注册成功后，主动查询通道列表
	go s.queryCatalog(deviceId)

	// 如果是设备重连，尝试恢复拉流任务
	if isReconnect && s.onReconnect != nil {
		go func() {
			// 延迟一点时间，等待设备完全上线
			time.Sleep(500 * time.Millisecond)
			if err := s.onReconnect(deviceId); err != nil {
				Log.Warnf("device reconnect callback failed. device_id=%s, err=%+v", deviceId, err)
			}
		}()
	}

	// 发送200 OK响应
	callId := msg.Headers["call-id"]
	from := msg.Headers["from"]
	to := msg.Headers["to"]
	cseq := msg.Headers["cseq"]
	via := msg.Headers["via"]

	headers := map[string]string{
		"Via":            via,
		"From":           from,
		"To":             to + ";tag=" + GenerateTag(),
		"Call-ID":        callId,
		"CSeq":           cseq,
		"Contact":        fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain),
		"Expires":        fmt.Sprintf("%d", expires),
		"Content-Length": "0",
	}

	response := BuildSipResponse(200, "OK", headers, "")
	s.sendRaw(response, raddr)
}

// handleKeepalive 处理MESSAGE请求（心跳或响应）
func (s *Gb28181Server) handleKeepalive(msg *SipMessage, raddr *net.UDPAddr) {
	deviceId := ExtractDeviceId(msg.Headers["from"])
	if deviceId == "" {
		s.sendResponse(msg, 400, "Bad Request", raddr)
		return
	}

	s.mutex.Lock()
	device, exists := s.devices[deviceId]
	if !exists {
		// 设备未注册，返回401要求重新注册（服务重启后常见情况）
		s.mutex.Unlock()
		Log.Warnf("keepalive from unregistered device, require re-register. device_id=%s, from=%s:%d",
			deviceId, raddr.IP.String(), raddr.Port)

		// 发送401 Unauthorized，要求设备重新注册
		callId := msg.Headers["call-id"]
		from := msg.Headers["from"]
		to := msg.Headers["to"]
		cseq := msg.Headers["cseq"]
		via := msg.Headers["via"]

		realm := s.config.LocalSipDomain
		if realm == "" {
			realm = s.config.LocalSipId
		}

		nonce := GenerateNonce()
		s.mutex.Lock()
		s.nonces[deviceId] = nonce
		s.mutex.Unlock()

		headers := map[string]string{
			"Via":              via,
			"From":             from,
			"To":               to + ";tag=" + GenerateTag(),
			"Call-ID":          callId,
			"CSeq":             cseq,
			"WWW-Authenticate": BuildWWWAuthenticate(realm, nonce),
			"Content-Length":   "0",
		}

		response := BuildSipResponse(401, "Unauthorized", headers, "")
		s.sendRaw(response, raddr)
		return
	}

	// 设备已注册，更新心跳时间
	now := time.Now()
	// 计算心跳间隔（基于客户端实际心跳时间）
	if !device.KeepaliveTime.IsZero() {
		interval := now.Sub(device.KeepaliveTime)
		// 如果间隔合理（10秒到300秒之间），更新心跳间隔
		// 使用加权平均，平滑处理心跳间隔变化
		if interval >= 10*time.Second && interval <= 300*time.Second {
			if device.KeepaliveInterval == 0 {
				device.KeepaliveInterval = interval
			} else {
				// 加权平均：新值占30%，旧值占70%，平滑处理
				device.KeepaliveInterval = time.Duration(float64(device.KeepaliveInterval)*0.7 + float64(interval)*0.3)
			}
		}
	}
	device.KeepaliveTime = now
	device.Status = "online"
	s.mutex.Unlock()

	// 检查是否是通道列表响应（Body中包含XML）
	body := msg.Body
	if len(body) > 0 {
		// 记录收到的完整消息内容用于调试
		Log.Infof("========== 收到 MESSAGE 响应 ==========")
		Log.Infof("设备ID: %s", deviceId)
		Log.Infof("来源地址: %s:%d", raddr.IP.String(), raddr.Port)
		Log.Infof("消息体长度: %d 字节", len(body))
		Log.Infof("完整消息体内容:\n%s", body)
		Log.Infof("========================================")

		// 检查是否是Catalog响应（大小写不敏感）
		// GB28181标准：设备返回的Catalog响应使用<Response>标签，包含<CmdType>Catalog</CmdType>
		// 某些设备可能返回 <CmdType>Catalog</CmdType> 或 <CmdType>catalog</CmdType>
		bodyLower := strings.ToLower(body)
		// 检查是否是Response格式的Catalog响应（检查Response标签和CmdType）
		isCatalogResponse := strings.Contains(body, "<?xml") &&
			(strings.Contains(bodyLower, "<response>") || strings.Contains(body, "<Response>")) &&
			strings.Contains(bodyLower, "<cmdtype>catalog</cmdtype>")

		// 如果上面没匹配到，再检查是否直接包含Catalog（某些设备可能格式不同）
		if !isCatalogResponse {
			isCatalogResponse = strings.Contains(body, "<?xml") &&
				strings.Contains(bodyLower, "<cmdtype>catalog</cmdtype>")
		}

		Log.Infof("检查 Catalog 响应: is_xml=%v, contains_response=%v, contains_catalog=%v, body_preview=%s",
			strings.Contains(body, "<?xml"),
			strings.Contains(bodyLower, "<response>") || strings.Contains(body, "<Response>"),
			strings.Contains(bodyLower, "<cmdtype>catalog</cmdtype>"),
			func() string {
				previewLen := 300
				if len(body) < previewLen {
					previewLen = len(body)
				}
				return body[:previewLen]
			}())

		if isCatalogResponse {
			// 这是通道列表响应，解析XML
			Log.Infof("检测到 Catalog 响应，开始解析. device_id=%s", deviceId)
			s.parseCatalogResponse(deviceId, body)
		} else if strings.Contains(body, "<?xml") {
			// 其他XML消息（如Notify心跳），不需要解析通道列表
			Log.Infof("收到其他 XML 消息（非 Catalog）. device_id=%s, body_preview=%s", deviceId, func() string {
				previewLen := 200
				if len(body) < previewLen {
					previewLen = len(body)
				}
				return body[:previewLen]
			}())
		} else {
			Log.Debugf("收到非 XML 消息体. device_id=%s, body_len=%d", deviceId, len(body))
		}
	} else {
		Log.Debugf("收到空消息体. device_id=%s", deviceId)
	}

	// 发送200 OK响应
	callId := msg.Headers["call-id"]
	from := msg.Headers["from"]
	to := msg.Headers["to"]
	cseq := msg.Headers["cseq"]
	via := msg.Headers["via"]

	headers := map[string]string{
		"Via":            via,
		"From":           from,
		"To":             to + ";tag=" + GenerateTag(),
		"Call-ID":        callId,
		"CSeq":           cseq,
		"Content-Length": "0",
	}

	response := BuildSipResponse(200, "OK", headers, "")
	s.sendRaw(response, raddr)
}

// handleInvite 处理INVITE请求（设备主动推流）
func (s *Gb28181Server) handleInvite(msg *SipMessage, raddr *net.UDPAddr) {
	deviceId := ExtractDeviceId(msg.Headers["from"])
	if deviceId == "" {
		s.sendResponse(msg, 400, "Bad Request", raddr)
		return
	}

	// 从SDP中提取信息（目前暂时不需要解析SDP）
	callId := msg.Headers["call-id"]

	// 检查设备是否存在
	s.mutex.RLock()
	_, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		Log.Warnf("receive INVITE from unregistered device. device_id=%s", deviceId)
		s.sendResponse(msg, 404, "Not Found", raddr)
		return
	}

	Log.Infof("receive INVITE from device. device_id=%s, call_id=%s", deviceId, callId)

	// 检查是否已经存在相同的Call-ID的流
	s.mutex.RLock()
	var existingStream *Stream
	for _, stream := range s.streams {
		if stream.CallId == callId {
			existingStream = stream
			break
		}
	}
	s.mutex.RUnlock()

	var targetStream *Stream
	if existingStream != nil {
		Log.Warnf("receive duplicate INVITE with same Call-ID. device_id=%s, call_id=%s, existing_stream=%s", deviceId, callId, existingStream.StreamName)
		targetStream = existingStream
		// 发送200 OK响应（即使流已存在）
	} else {
		// 解析SDP获取RTP端口等信息（简化处理）
		// 实际应该解析SDP获取RTP信息，这里使用默认端口段分配端口
		// 生成流名称（使用device_id+channel_id，设备主动推流时channel_id使用device_id）
		channelId := deviceId // 简化处理，使用设备ID作为通道ID
		streamName := deviceId + channelId

		// 从端口池分配一个端口用于接收RTP数据（端口为0表示自动分配）
		port := 0

		// 记录流信息（设备主动推流）
		s.mutex.Lock()
		stream := &Stream{
			DeviceId:   deviceId,
			ChannelId:  channelId,
			StreamName: streamName,
			CallId:     callId,
			Port:       port,
			IsTcp:      false,
			StartTime:  time.Now(),
		}
		s.streams[streamName] = stream
		// 更新设备流索引
		if s.deviceStreams[deviceId] == nil {
			s.deviceStreams[deviceId] = make(map[string]*Stream)
		}
		s.deviceStreams[deviceId][streamName] = stream
		targetStream = stream
		s.mutex.Unlock()

		Log.Infof("accept INVITE from device. device_id=%s, stream_name=%s", deviceId, streamName)
	}

	// 发送200 OK响应，构建包含服务器RTP端口的SDP
	// 注意：对于设备主动推流，服务器需要在SDP中告诉设备使用哪个端口接收RTP
	// 这里先使用端口0（自动分配），实际端口会在创建RTP Pub Session时确定
	from := msg.Headers["from"]
	to := msg.Headers["to"]
	cseq := msg.Headers["cseq"]
	via := msg.Headers["via"]
	tag := GenerateTag()

	headers := map[string]string{
		"Via":            via,
		"From":           from,
		"To":             to + ";tag=" + tag,
		"Call-ID":        callId,
		"CSeq":           cseq,
		"Contact":        fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain),
		"Content-Type":   "application/sdp",
		"Content-Length": "0", // 先设置为0，后面会更新
	}

	// 构建SDP响应（使用服务器要接收RTP的端口）
	// 注意：实际端口会在收到ACK后创建RTP Pub Session时确定，这里先使用占位符
	// 但为了兼容，我们先构建一个基本的SDP响应
	responseSdp := s.buildSdp(targetStream.Port, targetStream.IsTcp)
	headers["Content-Length"] = fmt.Sprintf("%d", len(responseSdp))
	response := BuildSipResponse(200, "OK", headers, responseSdp)
	s.sendRaw(response, raddr)
}

// handleAck 处理ACK请求
func (s *Gb28181Server) handleAck(msg *SipMessage, raddr *net.UDPAddr) {
	// ACK通常不需要响应
	callId := msg.Headers["call-id"]
	Log.Debugf("receive ACK from %s, call_id=%s", raddr.String(), callId)

	// 对于设备主动推流，收到ACK后应该调用onInvite回调创建RTP Pub Session
	// 查找对应的流
	s.mutex.RLock()
	var targetStream *Stream
	for _, stream := range s.streams {
		if stream.CallId == callId {
			targetStream = stream
			break
		}
	}
	s.mutex.RUnlock()

	if targetStream != nil && s.onInvite != nil {
		// 设备主动推流：收到ACK后调用回调创建RTP Pub Session
		Log.Infof("receive ACK for device push stream. call_id=%s, stream_name=%s", callId, targetStream.StreamName)
		if err := s.onInvite(targetStream.DeviceId, targetStream.ChannelId, targetStream.StreamName, targetStream.Port, targetStream.IsTcp); err != nil {
			Log.Warnf("onInvite callback failed after ACK. err=%+v", err)
		}
	}
}

// handleBye 处理BYE请求
func (s *Gb28181Server) handleBye(msg *SipMessage, raddr *net.UDPAddr) {
	deviceId := ExtractDeviceId(msg.Headers["from"])
	if deviceId == "" {
		s.sendResponse(msg, 400, "Bad Request", raddr)
		return
	}

	Log.Infof("receive BYE from device. device_id=%s", deviceId)

	// 查找并停止对应的流（使用索引优化）
	s.mutex.Lock()
	if deviceStreams, ok := s.deviceStreams[deviceId]; ok {
		streamNames := make([]string, 0, len(deviceStreams))
		streams := make([]*Stream, 0, len(deviceStreams))
		for streamName, stream := range deviceStreams {
			streamNames = append(streamNames, streamName)
			streams = append(streams, stream)
		}
		// 先释放锁，调用回调
		s.mutex.Unlock()

		if s.onBye != nil {
			for i, streamName := range streamNames {
				s.onBye(streams[i].DeviceId, streams[i].ChannelId, streamName)
			}
		}

		// 重新加锁删除
		s.mutex.Lock()
		for _, streamName := range streamNames {
			delete(s.streams, streamName)
		}
		delete(s.deviceStreams, deviceId)
		s.mutex.Unlock()
	} else {
		s.mutex.Unlock()
	}

	// 发送200 OK响应
	callId := msg.Headers["call-id"]
	from := msg.Headers["from"]
	to := msg.Headers["to"]
	cseq := msg.Headers["cseq"]
	via := msg.Headers["via"]

	headers := map[string]string{
		"Via":            via,
		"From":           from,
		"To":             to + ";tag=" + GenerateTag(),
		"Call-ID":        callId,
		"CSeq":           cseq,
		"Content-Length": "0",
	}

	response := BuildSipResponse(200, "OK", headers, "")
	s.sendRaw(response, raddr)
}

// handleResponse 处理响应消息
func (s *Gb28181Server) handleResponse(msg *SipMessage, raddr *net.UDPAddr) {
	Log.Infof("========== 收到 SIP 响应 ==========")
	Log.Infof("状态码: %d %s", msg.StatusCode, msg.StatusText)
	Log.Infof("来源地址: %s:%d", raddr.IP.String(), raddr.Port)
	callId := msg.Headers["call-id"]
	Log.Infof("Call-ID: %s", callId)
	if msg.Body != "" {
		Log.Infof("响应体长度: %d 字节", len(msg.Body))
		Log.Infof("响应体内容:\n%s", msg.Body)
	}
	Log.Infof("========================================")

	// 处理200 OK响应
	if msg.StatusCode == 200 {
		// 检查响应体中是否包含 Catalog XML（某些设备可能在 200 OK 响应中返回 Catalog）
		body := msg.Body
		if len(body) > 0 && strings.Contains(body, "<?xml") {
			bodyLower := strings.ToLower(body)
			if strings.Contains(bodyLower, "<cmdtype>catalog</cmdtype>") {
				// 从 From 头提取设备ID（响应中的 From 头是设备）
				deviceId := ExtractDeviceId(msg.Headers["from"])
				if deviceId != "" {
					Log.Infof("在 200 OK 响应中检测到 Catalog XML，开始解析. device_id=%s", deviceId)
					s.parseCatalogResponse(deviceId, body)
				} else {
					Log.Warnf("在 200 OK 响应中检测到 Catalog XML，但无法提取设备ID. from=%s", msg.Headers["from"])
				}
			}
		}

		// 从To头中提取Call-ID对应的流信息
		callId := msg.Headers["call-id"]

		// 查找对应的流
		s.mutex.RLock()
		var targetStream *Stream
		for _, stream := range s.streams {
			if stream.CallId == callId {
				targetStream = stream
				break
			}
		}
		s.mutex.RUnlock()

		if targetStream != nil {
			// 解析SDP获取RTP信息
			sdp := msg.Body
			Log.Infof("receive INVITE 200 OK response. call_id=%s, stream_name=%s, sdp_len=%d", callId, targetStream.StreamName, len(sdp))

			// 检查设备是否存在
			s.mutex.RLock()
			_, deviceExists := s.devices[targetStream.DeviceId]
			s.mutex.RUnlock()

			if deviceExists {
				// 发送ACK请求，完成INVITE三次握手
				// ACK的Request-URI使用200 OK响应中的Contact头，如果没有则使用INVITE请求中的To头
				contact := msg.Headers["contact"]
				var requestUri string
				if contact != "" {
					// 从Contact头提取URI
					// Contact: <sip:34020000001310000001@192.168.1.100:5060>
					re := regexp.MustCompile(`<([^>]+)>`)
					matches := re.FindStringSubmatch(contact)
					if len(matches) > 1 {
						requestUri = matches[1]
					} else {
						requestUri = contact
					}
				} else {
					// 使用To头构建Request-URI
					to := msg.Headers["to"]
					// To: <sip:34020000001310000001@192.168.1.100:5060>;tag=xxx
					re := regexp.MustCompile(`<([^>]+)>`)
					matches := re.FindStringSubmatch(to)
					if len(matches) > 1 {
						requestUri = matches[1]
					} else {
						// 使用域格式构建Request-URI（符合GB28181标准）
						deviceDomain := getDeviceDomain(targetStream.DeviceId)
						requestUri = fmt.Sprintf("sip:%s@%s", targetStream.ChannelId, deviceDomain)
					}
				}

				// 构建ACK请求（From头使用域格式，符合GB28181标准）
				from := fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain)
				to := msg.Headers["to"] // 使用200 OK响应中的To头（包含tag）
				cseq := msg.Headers["cseq"]
				// CSeq可能包含方法名，ACK只需要数字
				cseqNum := cseq
				if strings.Contains(cseq, " ") {
					cseqNum = strings.Split(cseq, " ")[0]
				}
				via := msg.Headers["via"]

				ackHeaders := map[string]string{
					"Via":            via,
					"From":           from,
					"To":             to, // 使用200 OK响应中的To头（包含tag）
					"Call-ID":        callId,
					"CSeq":           cseqNum + " ACK",
					"Content-Length": "0",
				}

				ackRequest := BuildSipRequest(SipMethodAck, requestUri, ackHeaders, "")
				s.sendRaw(ackRequest, raddr)
				Log.Infof("send ACK to device. device_id=%s, channel_id=%s, stream_name=%s", targetStream.DeviceId, targetStream.ChannelId, targetStream.StreamName)
			}

			// 如果设置了回调，调用回调处理
			if s.onInvite != nil {
				// 从SDP中提取RTP端口（简化处理，实际应该解析SDP）
				// 这里使用之前设置的端口
				if err := s.onInvite(targetStream.DeviceId, targetStream.ChannelId, targetStream.StreamName, targetStream.Port, targetStream.IsTcp); err != nil {
					Log.Warnf("onInvite callback failed. err=%+v", err)
				}
			}
		}
	}
}

// sendResponse 发送响应
func (s *Gb28181Server) sendResponse(req *SipMessage, statusCode int, statusText string, raddr *net.UDPAddr) {
	callId := req.Headers["call-id"]
	from := req.Headers["from"]
	to := req.Headers["to"]
	cseq := req.Headers["cseq"]
	via := req.Headers["via"]

	headers := map[string]string{
		"Via":            via,
		"From":           from,
		"To":             to + ";tag=" + GenerateTag(),
		"Call-ID":        callId,
		"CSeq":           cseq,
		"Content-Length": "0",
	}

	response := BuildSipResponse(statusCode, statusText, headers, "")
	s.sendRaw(response, raddr)
}

// sendRaw 发送原始消息
func (s *Gb28181Server) sendRaw(data string, raddr *net.UDPAddr) {
	if s.udpConn == nil {
		Log.Warnf("udp connection not initialized")
		return
	}
	_, err := s.udpConn.WriteToUDP([]byte(data), raddr)
	if err != nil {
		Log.Warnf("send sip message failed. err=%+v", err)
	}
}

// Invite 主动邀请设备推流
// streamType: 0=主码流，1=辅码流
func (s *Gb28181Server) Invite(deviceId, channelId, streamName string, port int, isTcp bool, streamType int) error {
	s.mutex.RLock()
	device, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("device not found: %s", deviceId)
	}

	// 生成Call-ID和Tag
	callId := GenerateCallId()
	branch := GenerateBranch()

	// 构建INVITE请求（使用域格式，符合GB28181标准）
	deviceDomain := getDeviceDomain(deviceId)
	requestUri := fmt.Sprintf("sip:%s@%s", channelId, deviceDomain)
	from := fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain)
	to := fmt.Sprintf("<sip:%s@%s>", channelId, deviceDomain)

	// 构建SDP
	sdp := s.buildSdp(port, isTcp)

	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + GenerateTag(),
		"To":             to,
		"Call-ID":        callId,
		"CSeq":           "1 INVITE",
		"Contact":        fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain),
		"Content-Type":   "application/sdp",
		"Content-Length": fmt.Sprintf("%d", len(sdp)),
		"Subject":        fmt.Sprintf("%s:0,%s:0:%d", channelId, s.config.LocalSipId, streamType),
		"Max-Forwards":   "70",
	}

	request := BuildSipRequest(SipMethodInvite, requestUri, headers, sdp)

	// 发送请求
	addr := &net.UDPAddr{
		IP:   net.ParseIP(device.Ip),
		Port: device.Port,
	}
	s.sendRaw(request, addr)

	// 记录流信息
	s.mutex.Lock()
	stream := &Stream{
		DeviceId:   deviceId,
		ChannelId:  channelId,
		StreamName: streamName,
		CallId:     callId,
		Port:       port,
		IsTcp:      isTcp,
		StartTime:  time.Now(),
	}
	s.streams[streamName] = stream
	// 更新设备流索引
	if s.deviceStreams[deviceId] == nil {
		s.deviceStreams[deviceId] = make(map[string]*Stream)
	}
	s.deviceStreams[deviceId][streamName] = stream
	s.mutex.Unlock()

	Log.Infof("send INVITE to device. device_id=%s, channel_id=%s, stream_name=%s, port=%d", deviceId, channelId, streamName, port)

	return nil
}

// Bye 停止设备推流
func (s *Gb28181Server) Bye(deviceId, channelId, streamName string) error {
	s.mutex.Lock()
	stream, exists := s.streams[streamName]
	if !exists {
		s.mutex.Unlock()
		return fmt.Errorf("stream not found: %s", streamName)
	}
	delete(s.streams, streamName)
	// 从设备流索引中删除
	if deviceStreams, ok := s.deviceStreams[deviceId]; ok {
		delete(deviceStreams, streamName)
		if len(deviceStreams) == 0 {
			delete(s.deviceStreams, deviceId)
		}
	}
	s.mutex.Unlock()

	s.mutex.RLock()
	device, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("device not found: %s", deviceId)
	}

	// 构建BYE请求（使用域格式，符合GB28181标准）
	deviceDomain := getDeviceDomain(deviceId)
	requestUri := fmt.Sprintf("sip:%s@%s", channelId, deviceDomain)
	from := fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain)
	to := fmt.Sprintf("<sip:%s@%s>", channelId, deviceDomain)

	branch := GenerateBranch()
	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + GenerateTag(),
		"To":             to,
		"Call-ID":        stream.CallId,
		"CSeq":           "1 BYE",
		"Content-Length": "0",
		"Max-Forwards":   "70",
	}

	request := BuildSipRequest(SipMethodBye, requestUri, headers, "")

	// 发送请求
	addr := &net.UDPAddr{
		IP:   net.ParseIP(device.Ip),
		Port: device.Port,
	}
	s.sendRaw(request, addr)

	Log.Infof("send BYE to device. device_id=%s, channel_id=%s, stream_name=%s", deviceId, channelId, streamName)

	return nil
}

// buildSdp 构建SDP消息（包含视频参数）
func (s *Gb28181Server) buildSdp(port int, isTcp bool) string {
	protocol := "RTP/AVP"
	if isTcp {
		protocol = "TCP/RTP/AVP"
	}

	// 确定视频编码格式和RTP payload type
	videoCodec := s.config.VideoCodec
	if videoCodec == "" {
		videoCodec = "H264" // 默认H264
	}

	// GB28181标准RTP payload type映射
	// 96: PS (Program Stream)
	// 98: H264
	// 97: MPEG4
	// 99: H265 (如果支持)
	var payloadTypes []string
	var rtpmapLines []string

	// 根据配置的编码格式添加对应的payload type
	switch strings.ToUpper(videoCodec) {
	case "H265", "HEVC":
		payloadTypes = []string{"96", "99"} // PS和H265
		rtpmapLines = []string{
			"a=rtpmap:96 PS/90000",
			"a=rtpmap:99 H265/90000",
		}
		// H265的fmtp参数
		if s.config.VideoWidth > 0 && s.config.VideoHeight > 0 {
			rtpmapLines = append(rtpmapLines, fmt.Sprintf("a=fmtp:99 profile-id=1;level-id=93;sprop-sps=;sprop-pps="))
		}
	case "H264":
		fallthrough
	default:
		payloadTypes = []string{"96", "98"} // PS和H264
		rtpmapLines = []string{
			"a=rtpmap:96 PS/90000",
			"a=rtpmap:98 H264/90000",
		}
		// H264的fmtp参数
		// 构建H264的fmtp参数（即使没有指定分辨率也添加基本参数）
		fmtpParams := "a=fmtp:98 "
		params := []string{}

		// profile-level-id参数
		profileLevelId := "42E028" // 默认baseline profile level 3.1
		if s.config.VideoProfile != "" {
			// 根据profile设置profile-level-id的前4位
			// baseline: 42E0, main: 4D00, high: 6400
			profileMap := map[string]string{
				"baseline": "42E0",
				"main":     "4D00",
				"high":     "6400",
			}
			if profilePrefix, ok := profileMap[strings.ToLower(s.config.VideoProfile)]; ok {
				// 根据level设置后2位
				levelMap := map[string]string{
					"3.0": "1F", "3.1": "28", "3.2": "2A",
					"4.0": "33", "4.1": "29", "4.2": "2A",
				}
				if levelId, ok := levelMap[s.config.VideoLevel]; ok {
					profileLevelId = profilePrefix + levelId
				} else {
					profileLevelId = profilePrefix + "28" // 默认level 3.1
				}
			}
		} else if s.config.VideoLevel != "" {
			// 只指定了level，使用默认baseline profile
			levelMap := map[string]string{
				"3.0": "1F", "3.1": "28", "3.2": "2A",
				"4.0": "33", "4.1": "29", "4.2": "2A",
			}
			if levelId, ok := levelMap[s.config.VideoLevel]; ok {
				profileLevelId = "42E0" + levelId
			}
		}
		params = append(params, "profile-level-id="+profileLevelId)

		// packetization-mode参数（1表示非交错模式，0表示交错模式）
		params = append(params, "packetization-mode=1")

		// 如果指定了分辨率，可以添加sprop-parameter-sets（可选）
		// 注意：实际使用时，sprop-parameter-sets应该从SPS/PPS中提取

		fmtpParams += strings.Join(params, ";")
		rtpmapLines = append(rtpmapLines, fmtpParams)
	}

	// 构建SDP主体
	sdp := fmt.Sprintf(`v=0
o=%s 0 0 IN IP4 %s
s=Play
c=IN IP4 %s
t=0 0
m=video %d %s %s
a=recvonly
`, s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipIp, port, protocol, strings.Join(payloadTypes, " "))

	// 添加rtpmap行
	for _, line := range rtpmapLines {
		sdp += line + "\n"
	}

	// 添加视频参数（如果指定了）
	if s.config.VideoWidth > 0 && s.config.VideoHeight > 0 {
		sdp += fmt.Sprintf("a=resolution:%dx%d\n", s.config.VideoWidth, s.config.VideoHeight)
	}

	if s.config.VideoFramerate > 0 {
		sdp += fmt.Sprintf("a=framerate:%.2f\n", float64(s.config.VideoFramerate))
	}

	if s.config.VideoBitrate > 0 {
		// 码率通常通过fmtp参数传递，但也可以单独指定
		sdp += fmt.Sprintf("a=bitrate:%d\n", s.config.VideoBitrate)
	}

	return sdp
}

// GetDevices 获取所有设备
func (s *Gb28181Server) GetDevices() []*Device {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	devices := make([]*Device, 0, len(s.devices))
	for _, device := range s.devices {
		devices = append(devices, device)
	}
	return devices
}

// GetDevice 获取设备
func (s *Gb28181Server) GetDevice(deviceId string) *Device {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.devices[deviceId]
}

// GetStream 获取流信息
func (s *Gb28181Server) GetStream(streamName string) *Stream {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.streams[streamName]
}

// HasStream 检查流是否存在
func (s *Gb28181Server) HasStream(streamName string) bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	_, exists := s.streams[streamName]
	return exists
}

// GetStreamsByDeviceId 获取设备的所有流（使用索引优化）
func (s *Gb28181Server) GetStreamsByDeviceId(deviceId string) []*Stream {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	deviceStreams, exists := s.deviceStreams[deviceId]
	if !exists || len(deviceStreams) == 0 {
		return nil
	}

	streams := make([]*Stream, 0, len(deviceStreams))
	for _, stream := range deviceStreams {
		streams = append(streams, stream)
	}
	return streams
}

// GetAllStreams 获取所有流信息
func (s *Gb28181Server) GetAllStreams() []*Stream {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	streams := make([]*Stream, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	return streams
}

// QueryDeviceInfo 查询设备信息（发送查询请求）
func (s *Gb28181Server) QueryDeviceInfo(deviceId string) error {
	s.mutex.RLock()
	device, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("device not found: %s", deviceId)
	}

	// 构建设备信息查询XML
	deviceInfoXml := BuildDeviceInfoQueryXML(deviceId, GenerateSN())

	// 构建MESSAGE请求（使用域格式，符合GB28181标准）
	deviceDomain := getDeviceDomain(deviceId)
	requestUri := fmt.Sprintf("sip:%s@%s", deviceId, deviceDomain)
	from := fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain)
	to := fmt.Sprintf("<sip:%s@%s>", deviceId, deviceDomain)

	branch := GenerateBranch()
	callId := GenerateCallId()
	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + GenerateTag(),
		"To":             to,
		"Call-ID":        callId,
		"CSeq":           "1 MESSAGE",
		"Max-Forwards":   "70",
		"Content-Type":   "Application/MANSCDP+xml",
		"Content-Length": fmt.Sprintf("%d", len(deviceInfoXml)),
	}

	request := BuildSipRequest(SipMethodMessage, requestUri, headers, deviceInfoXml)

	// 发送请求
	addr := &net.UDPAddr{
		IP:   net.ParseIP(device.Ip),
		Port: device.Port,
	}
	s.sendRaw(request, addr)

	Log.Infof("send device info query to device. device_id=%s", deviceId)
	return nil
}

// QueryDeviceStatus 查询设备状态（发送查询请求）
func (s *Gb28181Server) QueryDeviceStatus(deviceId string) error {
	s.mutex.RLock()
	device, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("device not found: %s", deviceId)
	}

	// 构建设备状态查询XML
	deviceStatusXml := BuildDeviceStatusQueryXML(deviceId, GenerateSN())

	// 构建MESSAGE请求（使用域格式，符合GB28181标准）
	deviceDomain := getDeviceDomain(deviceId)
	requestUri := fmt.Sprintf("sip:%s@%s", deviceId, deviceDomain)
	from := fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain)
	to := fmt.Sprintf("<sip:%s@%s>", deviceId, deviceDomain)

	branch := GenerateBranch()
	callId := GenerateCallId()
	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + GenerateTag(),
		"To":             to,
		"Call-ID":        callId,
		"CSeq":           "1 MESSAGE",
		"Max-Forwards":   "70",
		"Content-Type":   "Application/MANSCDP+xml",
		"Content-Length": fmt.Sprintf("%d", len(deviceStatusXml)),
	}

	request := BuildSipRequest(SipMethodMessage, requestUri, headers, deviceStatusXml)

	// 发送请求
	addr := &net.UDPAddr{
		IP:   net.ParseIP(device.Ip),
		Port: device.Port,
	}
	s.sendRaw(request, addr)

	Log.Infof("send device status query to device. device_id=%s", deviceId)
	return nil
}

// QueryCatalog 查询通道列表（发送查询请求）
func (s *Gb28181Server) QueryCatalog(deviceId string) error {
	s.mutex.RLock()
	_, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("device not found: %s", deviceId)
	}

	// 调用内部查询方法
	go s.queryCatalog(deviceId)
	return nil
}

// queryCatalog 查询设备通道列表
func (s *Gb28181Server) queryCatalog(deviceId string) {
	time.Sleep(100 * time.Millisecond) // 等待注册完成

	s.mutex.RLock()
	device, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		Log.Warnf("query catalog failed: device not found. device_id=%s", deviceId)
		return
	}

	Log.Infof("========== 开始查询设备通道列表 ==========")
	Log.Infof("设备ID: %s, IP: %s, Port: %d", deviceId, device.Ip, device.Port)

	// 构建CATALOG查询XML（GB28181标准格式）
	catalogXml := BuildCatalogQueryXML(deviceId, GenerateSN())

	// 构建MESSAGE请求（参考 lalmax 实现）
	// Request-URI 和 To 使用域格式，From 和 Contact 使用 IP:Port 格式
	deviceDomain := getDeviceDomain(deviceId)
	requestUri := fmt.Sprintf("sip:%s@%s", deviceId, deviceDomain)
	// From 使用 IP:Port 格式（物理地址）
	from := fmt.Sprintf("<sip:%s@%s:%d>", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort)
	// To 使用域格式（逻辑地址）
	to := fmt.Sprintf("<sip:%s@%s>", deviceId, deviceDomain)

	branch := GenerateBranch()
	callId := GenerateCallId()
	tag := GenerateTag()
	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + tag,
		"To":             to,
		"Call-ID":        callId,
		"User-Agent":     "LALServer",
		"CSeq":           "1 MESSAGE",
		"Max-Forwards":   "70",
		"Contact":        fmt.Sprintf("<sip:%s@%s:%d>;tag=%s", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort, tag),
		"Expires":        fmt.Sprintf("%d", s.config.Expires),
		"Content-Type":   "Application/MANSCDP+xml",
		"Content-Length": fmt.Sprintf("%d", len(catalogXml)),
	}

	request := BuildSipRequest(SipMethodMessage, requestUri, headers, catalogXml)

	// 发送请求
	addr := &net.UDPAddr{
		IP:   net.ParseIP(device.Ip),
		Port: device.Port,
	}
	s.sendRaw(request, addr)

	Log.Infof("========== 发送 Catalog 查询请求 ==========")
	Log.Infof("设备ID: %s", deviceId)
	Log.Infof("目标地址: %s:%d", device.Ip, device.Port)
	Log.Infof("请求URI: %s", requestUri)
	Log.Infof("From: %s", from)
	Log.Infof("To: %s", to)
	Log.Infof("Call-ID: %s", callId)
	Log.Infof("CSeq: 1 MESSAGE")
	Log.Infof("XML内容:\n%s", catalogXml)
	Log.Infof("==========================================")
}

// parseCatalogResponse 解析通道列表响应
func (s *Gb28181Server) parseCatalogResponse(deviceId string, xmlBody string) {
	Log.Infof("========== 开始解析 Catalog 响应 ==========")
	Log.Infof("设备ID: %s", deviceId)
	Log.Infof("XML 内容长度: %d 字节", len(xmlBody))
	Log.Infof("完整 XML 内容:\n%s", xmlBody)
	Log.Infof("==========================================")

	// GB28181 XML格式解析
	type Item struct {
		DeviceID     string `xml:"DeviceID"`
		Name         string `xml:"Name"`
		Status       string `xml:"Status"`
		ParentID     string `xml:"ParentID"`
		Manufacturer string `xml:"Manufacturer"`
		Model        string `xml:"Model"`
		Owner        string `xml:"Owner"`
		CivilCode    string `xml:"CivilCode"`
		Address      string `xml:"Address"`
		Parental     string `xml:"Parental"`
		SafetyWay    string `xml:"SafetyWay"`
		RegisterWay  string `xml:"RegisterWay"`
		Secrecy      string `xml:"Secrecy"`
		IPAddress    string `xml:"IPAddress"`
		Port         string `xml:"Port"`
		Password     string `xml:"Password"`
	}

	type CatalogResponse struct {
		XMLName    xml.Name `xml:"Response"`
		CmdType    string   `xml:"CmdType"`
		SN         string   `xml:"SN"`
		DeviceID   string   `xml:"DeviceID"`
		SumNum     string   `xml:"SumNum"` // 总数量（可能用于分页）
		Result     string   `xml:"Result"` // 结果：OK 表示成功
		DeviceList struct {
			Items []Item `xml:"Item"`
		} `xml:"DeviceList"`
		ItemList struct {
			Items []Item `xml:"Item"`
		} `xml:"ItemList"`
	}

	// 使用工具函数处理XML编码转换
	xmlBytes, err := DecodeGbkToUtf8([]byte(xmlBody))
	if err != nil {
		Log.Warnf("convert GBK to UTF-8 failed. device_id=%s, err=%+v", deviceId, err)
		xmlBytes = []byte(xmlBody) // 转换失败，使用原始数据
	}

	var resp CatalogResponse
	decoder := xml.NewDecoder(bytes.NewReader(xmlBytes))
	// 设置 CharsetReader 以处理编码问题
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}
	if err := decoder.Decode(&resp); err != nil {
		xmlPreviewLen := 500
		if len(xmlBody) < xmlPreviewLen {
			xmlPreviewLen = len(xmlBody)
		}
		Log.Warnf("parse catalog response XML failed. device_id=%s, err=%+v, xml_preview=%s", deviceId, err, xmlBody[:xmlPreviewLen])
		Log.Debugf("full xml body (first 1000 chars): %s", func() string {
			if len(xmlBody) > 1000 {
				return xmlBody[:1000]
			}
			return xmlBody
		}())
		return
	}

	// 获取Item列表（支持DeviceList和ItemList两种格式）
	// 注意：某些设备可能使用 ItemList，某些使用 DeviceList
	items := resp.DeviceList.Items
	if len(items) == 0 {
		items = resp.ItemList.Items
	}

	// 如果两种格式都没有数据，记录警告
	if len(items) == 0 {
		Log.Warnf("catalog response has no items. device_id=%s, sum_num=%s", deviceId, resp.SumNum)
		// 不直接返回，继续处理，因为可能是设备没有通道
	}

	Log.Infof("========== Catalog 响应解析结果 ==========")
	Log.Infof("设备ID: %s", deviceId)
	Log.Infof("CmdType: %s", resp.CmdType)
	Log.Infof("SN: %s", resp.SN)
	Log.Infof("DeviceID: %s", resp.DeviceID)
	Log.Infof("SumNum: %s", resp.SumNum)
	Log.Infof("Result: %s", resp.Result)
	Log.Infof("DeviceList Items 数量: %d", len(resp.DeviceList.Items))
	Log.Infof("ItemList Items 数量: %d", len(resp.ItemList.Items))
	Log.Infof("最终 Items 数量: %d", len(items))
	Log.Infof("==========================================")

	// 检查结果（某些设备可能不返回 Result 字段，所以只在有值且不为 OK 时才返回）
	if resp.Result != "" && resp.Result != "OK" {
		Log.Warnf("catalog query failed. device_id=%s, result=%s", deviceId, resp.Result)
		return
	}

	// 打印所有Item的详细信息用于调试
	for i, item := range items {
		Log.Infof("catalog item[%d]: device_id=%s, name=%s, parent_id=%s, status=%s", i, item.DeviceID, item.Name, item.ParentID, item.Status)
	}

	// 检查 CmdType（大小写不敏感）
	if !strings.EqualFold(resp.CmdType, "Catalog") {
		Log.Debugf("not catalog response, cmd_type=%s, device_id=%s", resp.CmdType, deviceId)
		return
	}

	// 检查 SumNum 和实际数量是否一致（用于调试）
	if resp.SumNum != "" {
		sumNum, err := strconv.Atoi(resp.SumNum)
		if err == nil && sumNum > 0 && sumNum != len(items) {
			Log.Warnf("catalog item count mismatch. device_id=%s, sum_num=%d, actual_count=%d", deviceId, sumNum, len(items))
		}
	}

	s.mutex.Lock()
	device, exists := s.devices[deviceId]
	if !exists {
		s.mutex.Unlock()
		Log.Warnf("device not found when parsing catalog. device_id=%s", deviceId)
		return
	}

	device.mutex.Lock()
	// 清空旧通道列表
	device.Channels = make(map[string]*Channel)

	// 更新通道列表
	channelCount := 0
	for _, item := range items {
		Log.Debugf("processing catalog item. device_id=%s, item_device_id=%s, item_name=%s, parent_id=%s, status=%s", deviceId, item.DeviceID, item.Name, item.ParentID, item.Status)

		// 排除设备自身
		if item.DeviceID == deviceId {
			Log.Debugf("skip device itself. device_id=%s", deviceId)
			continue
		}

		// 通道判断逻辑（GB28181 标准）：
		// 1. 通道的DeviceID不等于设备ID（已排除）
		// 2. ParentID等于设备ID（标准情况）
		// 3. ParentID为空或"0"时，如果DeviceID前10位（区域编码）与设备ID相同，也认为是通道
		// 4. 某些设备可能不填ParentID，需要根据DeviceID的前缀判断
		isChannel := false

		// 标准情况：ParentID 明确指向设备ID
		if item.ParentID == deviceId {
			isChannel = true
			Log.Debugf("item is channel (parent_id matches device_id). device_id=%s, item_device_id=%s", deviceId, item.DeviceID)
		} else if item.ParentID == "" || item.ParentID == "0" {
			// ParentID为空或"0"时，根据DeviceID前缀判断
			if len(deviceId) >= 10 && len(item.DeviceID) >= 10 {
				// 检查前10位（区域编码）是否相同
				if item.DeviceID[:10] == deviceId[:10] {
					isChannel = true
					Log.Debugf("item is channel (region code matches, parent_id empty). device_id=%s, item_device_id=%s", deviceId, item.DeviceID)
				}
			} else {
				// 如果长度不足，只要DeviceID不同就认为是通道（兼容处理）
				isChannel = true
				Log.Debugf("item is channel (parent_id empty, device_id different). device_id=%s, item_device_id=%s", deviceId, item.DeviceID)
			}
		}

		if isChannel {
			channel := &Channel{
				ChannelId:   item.DeviceID,
				ChannelName: item.Name,
				Status:      item.Status,
			}
			if channel.ChannelName == "" {
				channel.ChannelName = item.DeviceID
			}
			device.Channels[item.DeviceID] = channel
			channelCount++
			Log.Infof("add channel SUCCESS. device_id=%s, channel_id=%s, channel_name=%s, status=%s, parent_id=%s", deviceId, channel.ChannelId, channel.ChannelName, channel.Status, item.ParentID)
		} else {
			Log.Debugf("skip item (not a channel). device_id=%s, item_device_id=%s, parent_id=%s, name=%s", deviceId, item.DeviceID, item.ParentID, item.Name)
		}
	}
	device.mutex.Unlock()
	s.mutex.Unlock()

	Log.Infof("parse catalog response success. device_id=%s, total_items=%d, channel_count=%d", deviceId, len(items), channelCount)
}

// scheduleCatalogQuery 定时查询Catalog（后台策略）
func (s *Gb28181Server) scheduleCatalogQuery() {
	interval := time.Duration(s.config.CatalogQueryInterval) * time.Second
	if interval <= 0 {
		return
	}

	// 首次延迟5秒执行，避免与设备注册时的查询冲突
	time.Sleep(5 * time.Second)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	Log.Infof("start catalog query scheduler. interval=%v", interval)

	for {
		select {
		case <-ticker.C:
			s.queryAllOnlineDevicesCatalog()
		case <-s.stopChan:
			Log.Infof("catalog query scheduler stopped")
			return
		}
	}
}

// queryAllOnlineDevicesCatalog 查询所有在线设备的Catalog
func (s *Gb28181Server) queryAllOnlineDevicesCatalog() {
	s.mutex.RLock()
	onlineDevices := make([]string, 0)
	for deviceId, device := range s.devices {
		if device.Status == "online" {
			onlineDevices = append(onlineDevices, deviceId)
		}
	}
	s.mutex.RUnlock()

	if len(onlineDevices) == 0 {
		return
	}

	Log.Debugf("scheduled catalog query. online_device_count=%d", len(onlineDevices))

	// 异步查询每个设备的Catalog，避免阻塞
	for _, deviceId := range onlineDevices {
		go func(devId string) {
			if err := s.QueryCatalog(devId); err != nil {
				Log.Debugf("scheduled catalog query failed. device_id=%s, err=%+v", devId, err)
			}
		}(deviceId)
	}
}

// monitorDevices 监控设备状态，检测超时设备
func (s *Gb28181Server) monitorDevices() {
	// 每10秒检查一次，确保能及时检测到设备离线
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.checkDeviceTimeout()
		case <-s.stopChan:
			return
		}
	}
}

// checkDeviceTimeout 检查设备超时
func (s *Gb28181Server) checkDeviceTimeout() {
	now := time.Now()

	s.mutex.Lock()
	offlineDevices := make([]*Device, 0)
	offlineDeviceStreams := make(map[string][]*Stream) // 设备ID -> 流列表

	for deviceId, device := range s.devices {
		// 根据客户端实际心跳间隔确定超时阈值
		var timeoutThreshold time.Duration
		if device.KeepaliveInterval > 0 {
			// 如果已计算出心跳间隔，使用心跳间隔的3倍作为超时阈值
			timeoutThreshold = device.KeepaliveInterval * 3
			// 设置最小和最大阈值，避免异常值
			if timeoutThreshold < 60*time.Second {
				timeoutThreshold = 60 * time.Second // 最小60秒
			}
			if timeoutThreshold > 600*time.Second {
				timeoutThreshold = 600 * time.Second // 最大600秒（10分钟）
			}
		} else {
			// 如果还没有计算出心跳间隔，使用过期时间作为阈值
			timeout := time.Duration(s.config.Expires) * time.Second
			if timeout == 0 {
				timeout = 3600 * time.Second // 默认1小时
			}
			timeoutThreshold = timeout + timeout/2 // 过期时间的1.5倍
		}

		// 检查心跳超时（根据客户端实际心跳间隔确定）
		// 注意：只更新状态为offline，不删除设备，以便API接口能查询到离线设备信息
		timeSinceLastKeepalive := now.Sub(device.KeepaliveTime)
		if timeSinceLastKeepalive > timeoutThreshold {
			if device.Status != "offline" {
				offlineDevices = append(offlineDevices, device)
				device.Status = "offline"
				Log.Warnf("device timeout detected. device_id=%s, last_keepalive=%v, time_since_last=%v, timeout_threshold=%v, keepalive_interval=%v",
					deviceId, device.KeepaliveTime, timeSinceLastKeepalive, timeoutThreshold, device.KeepaliveInterval)

				// 收集该设备的所有流信息，用于后续清理
				if deviceStreams, ok := s.deviceStreams[deviceId]; ok {
					streams := make([]*Stream, 0, len(deviceStreams))
					for _, stream := range deviceStreams {
						streams = append(streams, stream)
					}
					offlineDeviceStreams[deviceId] = streams
				}
			}
		}
	}
	s.mutex.Unlock()

	// 处理离线设备：清理流信息、更新通道状态并调用回调
	for _, device := range offlineDevices {
		deviceId := device.DeviceId
		Log.Infof("device marked as offline. device_id=%s", deviceId)

		// 清理该设备的所有流信息并更新通道状态
		if streams, ok := offlineDeviceStreams[deviceId]; ok {
			s.mutex.Lock()
			// 更新设备通道状态
			device.mutex.Lock()
			for _, stream := range streams {
				// 更新通道状态为 idle
				if channel, exists := device.Channels[stream.ChannelId]; exists {
					channel.Status = "idle"
					channel.StreamName = ""
					Log.Debugf("device offline, channel status updated to idle. device_id=%s, channel_id=%s",
						deviceId, stream.ChannelId)
				}

				// 从流列表中删除
				delete(s.streams, stream.StreamName)
				// 从设备流索引中删除
				if deviceStreams, exists := s.deviceStreams[deviceId]; exists {
					delete(deviceStreams, stream.StreamName)
					if len(deviceStreams) == 0 {
						delete(s.deviceStreams, deviceId)
					}
				}
			}
			device.mutex.Unlock()
			s.mutex.Unlock()

			// 调用 onBye 回调，通知上层停止流（在锁外调用，避免死锁）
			if s.onBye != nil {
				for _, stream := range streams {
					Log.Infof("device offline, stopping stream. device_id=%s, channel_id=%s, stream_name=%s",
						deviceId, stream.ChannelId, stream.StreamName)
					if err := s.onBye(deviceId, stream.ChannelId, stream.StreamName); err != nil {
						Log.Warnf("onBye callback failed for offline device. device_id=%s, stream_name=%s, err=%+v",
							deviceId, stream.StreamName, err)
					}
				}
			}

			Log.Infof("device offline, cleaned up %d streams and updated channel status. device_id=%s", len(streams), deviceId)
		} else {
			// 即使没有流，也更新所有通道状态为 idle
			s.mutex.Lock()
			device.mutex.Lock()
			for _, channel := range device.Channels {
				if channel.Status == "streaming" {
					channel.Status = "idle"
					channel.StreamName = ""
				}
			}
			device.mutex.Unlock()
			s.mutex.Unlock()
			Log.Infof("device offline, updated all channel status to idle. device_id=%s", deviceId)
		}
	}
}

// Dispose 释放资源
func (s *Gb28181Server) Dispose() error {
	// 停止监控goroutine
	if s.stopChan != nil {
		close(s.stopChan)
	}

	if s.udpConn != nil {
		s.udpConn.Close()
	}
	if s.conn != nil {
		return s.conn.Dispose()
	}
	return nil
}
