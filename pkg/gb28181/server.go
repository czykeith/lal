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
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
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
	mutex         sync.RWMutex
	onInvite      func(deviceId, channelId, streamName string, port int, isTcp bool) error
	onBye         func(deviceId, channelId, streamName string) error
	onReconnect   func(deviceId string) error
}

// ServerConfig GB28181服务器配置
type ServerConfig struct {
	LocalSipId     string // 本地SIP ID
	LocalSipIp     string // 本地SIP IP
	LocalSipPort   int    // 本地SIP端口
	LocalSipDomain string // 本地SIP域
	Username       string // 认证用户名（可选）
	Password       string // 认证密码（可选）
	Expires        int    // 注册过期时间（秒）
}

// Device 设备信息
type Device struct {
	DeviceId      string
	DeviceName    string
	Ip            string
	Port          int
	Status        string // online/offline
	RegisterTime  time.Time
	KeepaliveTime time.Time
	Channels      map[string]*Channel
	mutex         sync.RWMutex
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

	return &Gb28181Server{
		config:        config,
		devices:       make(map[string]*Device),
		streams:       make(map[string]*Stream),
		deviceStreams: make(map[string]map[string]*Stream),
	}
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
	msg, err := ParseSipMessage(data)
	if err != nil {
		Log.Warnf("parse sip message failed. err=%+v", err)
		return
	}

	Log.Debugf("receive sip message. method=%s, from=%s", msg.Method, msg.Headers["from"])

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
		"Contact":        fmt.Sprintf("<sip:%s@%s:%d>", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort),
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
	if exists {
		device.KeepaliveTime = time.Now()
		device.Status = "online"
	}
	s.mutex.Unlock()

	// 检查是否是通道列表响应（Body中包含XML）
	body := msg.Body
	if len(body) > 0 {
		// 记录收到的消息内容用于调试
		previewLen := 200
		if len(body) < previewLen {
			previewLen = len(body)
		}
		Log.Debugf("receive MESSAGE from device. device_id=%s, body_len=%d, body_preview=%s", deviceId, len(body), body[:previewLen])

		// 检查是否是Catalog响应（包含<CmdType>Catalog</CmdType>）
		if strings.Contains(body, "<?xml") && strings.Contains(body, "<CmdType>Catalog</CmdType>") {
			// 这是通道列表响应，解析XML
			Log.Infof("detect catalog response XML. device_id=%s", deviceId)
			s.parseCatalogResponse(deviceId, body)
		} else if strings.Contains(body, "<?xml") {
			// 其他XML消息（如Notify心跳），不需要解析通道列表
			Log.Debugf("receive other XML message (not Catalog). device_id=%s", deviceId)
		}
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
		"Contact":        fmt.Sprintf("<sip:%s@%s:%d>", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort),
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
	Log.Debugf("receive sip response. status_code=%d", msg.StatusCode)

	// 处理INVITE的200 OK响应
	if msg.StatusCode == 200 {
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

			// 获取设备信息用于发送ACK
			s.mutex.RLock()
			device, deviceExists := s.devices[targetStream.DeviceId]
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
						requestUri = fmt.Sprintf("sip:%s@%s:%d", targetStream.ChannelId, device.Ip, device.Port)
					}
				}

				// 构建ACK请求
				from := fmt.Sprintf("<sip:%s@%s:%d>", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort)
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
func (s *Gb28181Server) Invite(deviceId, channelId, streamName string, port int, isTcp bool) error {
	s.mutex.RLock()
	device, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("device not found: %s", deviceId)
	}

	// 生成Call-ID和Tag
	callId := GenerateCallId()
	branch := GenerateBranch()

	// 构建INVITE请求
	requestUri := fmt.Sprintf("sip:%s@%s:%d", channelId, device.Ip, device.Port)
	from := fmt.Sprintf("<sip:%s@%s:%d>", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort)
	to := fmt.Sprintf("<sip:%s@%s:%d>", channelId, device.Ip, device.Port)

	// 构建SDP
	sdp := s.buildSdp(port, isTcp)

	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + GenerateTag(),
		"To":             to,
		"Call-ID":        callId,
		"CSeq":           "1 INVITE",
		"Contact":        fmt.Sprintf("<sip:%s@%s:%d>", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort),
		"Content-Type":   "application/sdp",
		"Content-Length": fmt.Sprintf("%d", len(sdp)),
		"Subject":        fmt.Sprintf("%s:0,%s:0", channelId, s.config.LocalSipId),
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

	// 构建BYE请求
	requestUri := fmt.Sprintf("sip:%s@%s:%d", channelId, device.Ip, device.Port)
	from := fmt.Sprintf("<sip:%s@%s:%d>", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort)
	to := fmt.Sprintf("<sip:%s@%s:%d>", channelId, device.Ip, device.Port)

	branch := GenerateBranch()
	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + GenerateTag(),
		"To":             to,
		"Call-ID":        stream.CallId,
		"CSeq":           "1 BYE",
		"Content-Length": "0",
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

// buildSdp 构建SDP消息
func (s *Gb28181Server) buildSdp(port int, isTcp bool) string {
	protocol := "RTP/AVP"
	if isTcp {
		protocol = "TCP/RTP/AVP"
	}

	sdp := fmt.Sprintf(`v=0
o=%s 0 0 IN IP4 %s
s=Play
c=IN IP4 %s
t=0 0
m=video %d %s 96 98 97
a=recvonly
a=rtpmap:96 PS/90000
a=rtpmap:98 H264/90000
a=rtpmap:97 MPEG4/90000
`, s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipIp, port, protocol)

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

// queryCatalog 查询设备通道列表
func (s *Gb28181Server) queryCatalog(deviceId string) {
	time.Sleep(100 * time.Millisecond) // 等待注册完成

	s.mutex.RLock()
	device, exists := s.devices[deviceId]
	s.mutex.RUnlock()

	if !exists {
		return
	}

	// 构建CATALOG查询XML（GB28181标准格式）
	catalogXml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Query>
<CmdType>Catalog</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
</Query>`, time.Now().Unix(), deviceId)

	// 构建MESSAGE请求
	requestUri := fmt.Sprintf("sip:%s@%s:%d", deviceId, device.Ip, device.Port)
	from := fmt.Sprintf("<sip:%s@%s:%d>", s.config.LocalSipId, s.config.LocalSipIp, s.config.LocalSipPort)
	to := fmt.Sprintf("<sip:%s@%s:%d>", deviceId, device.Ip, device.Port)

	branch := GenerateBranch()
	callId := GenerateCallId()
	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + GenerateTag(),
		"To":             to,
		"Call-ID":        callId,
		"CSeq":           "1 MESSAGE",
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

	Log.Infof("send CATALOG query to device. device_id=%s, ip=%s, port=%d, xml=%s", deviceId, device.Ip, device.Port, catalogXml)
}

// parseCatalogResponse 解析通道列表响应
func (s *Gb28181Server) parseCatalogResponse(deviceId string, xmlBody string) {
	Log.Debugf("parse catalog response. device_id=%s, xml_body=%s", deviceId, xmlBody)

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
		SumNum     string   `xml:"SumNum"`
		DeviceList struct {
			Items []Item `xml:"Item"`
		} `xml:"DeviceList"`
		ItemList struct {
			Items []Item `xml:"Item"`
		} `xml:"ItemList"`
	}

	// 检查XML编码，如果是GB2312或GBK，转换为UTF-8
	xmlBytes := []byte(xmlBody)
	if strings.Contains(xmlBody, "encoding=\"GB2312\"") || strings.Contains(xmlBody, "encoding='GB2312'") ||
		strings.Contains(xmlBody, "encoding=\"GBK\"") || strings.Contains(xmlBody, "encoding='GBK'") {
		// 转换GB2312/GBK到UTF-8
		reader := transform.NewReader(bytes.NewReader(xmlBytes), simplifiedchinese.GBK.NewDecoder())
		utf8Bytes, err := io.ReadAll(reader)
		if err != nil {
			Log.Warnf("convert GB2312/GBK to UTF-8 failed. device_id=%s, err=%+v", deviceId, err)
			// 如果转换失败，尝试直接解析
		} else {
			xmlBytes = utf8Bytes
			// 替换XML声明中的编码为UTF-8
			xmlBody = strings.ReplaceAll(string(xmlBytes), "encoding=\"GB2312\"", "encoding=\"UTF-8\"")
			xmlBody = strings.ReplaceAll(xmlBody, "encoding='GB2312'", "encoding='UTF-8'")
			xmlBody = strings.ReplaceAll(xmlBody, "encoding=\"GBK\"", "encoding=\"UTF-8\"")
			xmlBody = strings.ReplaceAll(xmlBody, "encoding='GBK'", "encoding='UTF-8'")
			xmlBytes = []byte(xmlBody)
		}
	}

	var resp CatalogResponse
	decoder := xml.NewDecoder(bytes.NewReader(xmlBytes))
	if err := decoder.Decode(&resp); err != nil {
		xmlPreviewLen := 500
		if len(xmlBody) < xmlPreviewLen {
			xmlPreviewLen = len(xmlBody)
		}
		Log.Warnf("parse catalog response XML failed. device_id=%s, err=%+v, xml=%s", deviceId, err, xmlBody[:xmlPreviewLen])
		return
	}

	// 获取Item列表（支持DeviceList和ItemList两种格式）
	items := resp.DeviceList.Items
	if len(items) == 0 {
		items = resp.ItemList.Items
	}

	Log.Infof("parsed catalog response. device_id=%s, cmd_type=%s, sum_num=%s, item_count=%d", deviceId, resp.CmdType, resp.SumNum, len(items))

	// 打印所有Item的详细信息用于调试
	for i, item := range items {
		Log.Infof("catalog item[%d]: device_id=%s, name=%s, parent_id=%s, status=%s", i, item.DeviceID, item.Name, item.ParentID, item.Status)
	}

	if resp.CmdType != "Catalog" {
		Log.Debugf("not catalog response, cmd_type=%s", resp.CmdType)
		return
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
		Log.Infof("processing catalog item. device_id=%s, item_device_id=%s, item_name=%s, parent_id=%s, status=%s", deviceId, item.DeviceID, item.Name, item.ParentID, item.Status)

		// 通道判断逻辑（更宽松的判断）：
		// 1. 通道的DeviceID不等于设备ID（排除设备自身）
		// 2. ParentID等于设备ID，或者是空/"0"（某些设备不填ParentID）
		// 3. 对于ParentID为空的情况，如果DeviceID和deviceId不同，也认为是通道
		isChannel := false
		if item.DeviceID != deviceId {
			// 首先检查ParentID是否明确指向设备ID
			if item.ParentID == deviceId {
				isChannel = true
				Log.Infof("item is channel (parent_id matches device_id). device_id=%s, item_device_id=%s, parent_id=%s", deviceId, item.DeviceID, item.ParentID)
			} else if item.ParentID == "" || item.ParentID == "0" || item.ParentID == deviceId {
				// ParentID为空、0或等于设备ID，都认为是通道（只要DeviceID不同）
				isChannel = true
				Log.Infof("item is channel (parent_id is empty/0/device_id and device_id different). device_id=%s, item_device_id=%s, parent_id=%s", deviceId, item.DeviceID, item.ParentID)
			} else if len(deviceId) >= 10 && len(item.DeviceID) >= 10 {
				// 如果ParentID不为空，检查前10位（区域编码）是否相同
				if item.DeviceID[:10] == deviceId[:10] && item.DeviceID != deviceId {
					isChannel = true
					Log.Infof("item is channel (region code matches). device_id=%s, item_device_id=%s, parent_id=%s", deviceId, item.DeviceID, item.ParentID)
				}
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
			Log.Infof("skip item (not a channel). device_id=%s, item_device_id=%s, parent_id=%s, name=%s", deviceId, item.DeviceID, item.ParentID, item.Name)
		}
	}
	device.mutex.Unlock()
	s.mutex.Unlock()

	Log.Infof("parse catalog response success. device_id=%s, total_items=%d, channel_count=%d", deviceId, len(items), channelCount)
}

// Dispose 释放资源
func (s *Gb28181Server) Dispose() error {
	if s.udpConn != nil {
		s.udpConn.Close()
	}
	if s.conn != nil {
		return s.conn.Dispose()
	}
	return nil
}
