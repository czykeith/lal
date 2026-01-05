// Copyright 2024, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package gb28181

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/q191201771/naza/pkg/nazanet"
)

// Gb28181Server GB28181信令服务器
type Gb28181Server struct {
	addr     string
	conn     *nazanet.UdpConnection
	udpConn  *net.UDPConn // 底层UDP连接，用于发送数据
	config   *ServerConfig
	devices  map[string]*Device // 设备ID -> 设备信息
	streams  map[string]*Stream // stream_name -> 流信息
	mutex    sync.RWMutex
	onInvite func(deviceId, channelId, streamName string, port int, isTcp bool) error
	onBye    func(deviceId, channelId, streamName string) error
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
		config:  config,
		devices: make(map[string]*Device),
		streams: make(map[string]*Stream),
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

	Log.Infof("device register. device_id=%s, ip=%s, port=%d, expires=%d", deviceId, ip, port, expires)

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

// handleKeepalive 处理MESSAGE请求（心跳）
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

	// 从SDP中提取信息
	sdp := msg.Body
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

	// 解析SDP获取RTP端口等信息（简化处理）
	// 实际应该解析SDP获取RTP信息，这里使用默认端口
	// 生成流名称
	streamName := fmt.Sprintf("%s_%s_%s", deviceId, callId, time.Now().Format("20060102150405"))

	// 发送200 OK响应
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
		"Content-Length": fmt.Sprintf("%d", len(sdp)),
	}

	// 构建SDP响应（简化处理，实际应该根据接收到的SDP构建响应）
	responseSdp := sdp // 这里简化处理，实际应该构建自己的SDP
	response := BuildSipResponse(200, "OK", headers, responseSdp)
	s.sendRaw(response, raddr)

	// 记录流信息（设备主动推流）
	s.mutex.Lock()
	s.streams[streamName] = &Stream{
		DeviceId:   deviceId,
		ChannelId:  deviceId, // 简化处理，使用设备ID作为通道ID
		StreamName: streamName,
		CallId:     callId,
		Port:       0, // 需要从SDP中解析
		IsTcp:      false,
		StartTime:  time.Now(),
	}
	s.mutex.Unlock()

	Log.Infof("accept INVITE from device. device_id=%s, stream_name=%s", deviceId, streamName)
}

// handleAck 处理ACK请求
func (s *Gb28181Server) handleAck(msg *SipMessage, raddr *net.UDPAddr) {
	// ACK通常不需要响应
	Log.Debugf("receive ACK from %s", raddr.String())
}

// handleBye 处理BYE请求
func (s *Gb28181Server) handleBye(msg *SipMessage, raddr *net.UDPAddr) {
	deviceId := ExtractDeviceId(msg.Headers["from"])
	if deviceId == "" {
		s.sendResponse(msg, 400, "Bad Request", raddr)
		return
	}

	Log.Infof("receive BYE from device. device_id=%s", deviceId)

	// 查找并停止对应的流
	s.mutex.Lock()
	for streamName, stream := range s.streams {
		if stream.DeviceId == deviceId {
			if s.onBye != nil {
				s.onBye(stream.DeviceId, stream.ChannelId, streamName)
			}
			delete(s.streams, streamName)
		}
	}
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
	s.streams[streamName] = &Stream{
		DeviceId:   deviceId,
		ChannelId:  channelId,
		StreamName: streamName,
		CallId:     callId,
		Port:       port,
		IsTcp:      isTcp,
		StartTime:  time.Now(),
	}
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
