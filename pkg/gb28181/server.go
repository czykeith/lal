package gb28181

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ghettovoice/gosip"
	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
	udpTransport "github.com/pion/transport/v3/udp"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/gb28181/mediaserver"
	"golang.org/x/net/html/charset"
)

type IMediaOpObserver interface {
	OnStartMediaServer(netWork string, singlePort bool, deviceId string, channelId string) *mediaserver.GB28181MediaServer
	OnStopMediaServer(netWork string, singlePort bool, deviceId string, channelId string, StreamName string) error
}
type GB28181Server struct {
	conf              GB28181Config
	RegisterValidity  time.Duration // 注册有效期，单位秒，默认 3600
	HeartbeatInterval time.Duration // 心跳间隔，单位秒，默认 60
	RemoveBanInterval time.Duration // 移除禁止设备间隔,默认600s
	// PlaybackSessionTTL 回放会话默认有效期，超过该时间自动结束回放，防止资源长期占用
	PlaybackSessionTTL time.Duration
	keepaliveInterval  int

	lalServer mediaserver.ILalServer

	udpAvailConnPool *AvailConnPool
	tcpAvailConnPool *AvailConnPool

	sipUdpSvr gosip.Server
	sipTcpSvr gosip.Server

	MediaServerMap sync.Map
	disposeOnce    sync.Once

	// playbackSessions 记录回放会话的过期时间，key 为 streamName
	playbackSessions sync.Map
	// playbackScales 记录回放会话的倍速配置，key 为 streamName，value 为 float64 倍速（>0，1 表示正常速度）
	playbackScales sync.Map
}

const MaxRegisterCount = 3

func sipMaybeStringTrim(v sip.MaybeString) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(v.String())
}

var (
	logger log.Logger
	sipsvr gosip.Server
)

func init() {
	logger = log.NewDefaultLogrusLogger().WithPrefix("LalMaxServer")
}

func NewGB28181Server(conf GB28181Config, lal mediaserver.ILalServer) *GB28181Server {
	if conf.ListenAddr == "" {
		conf.ListenAddr = "0.0.0.0"
	}
	if conf.SipPort == 0 {
		conf.SipPort = 5060
	}
	if conf.KeepaliveInterval == 0 {
		conf.KeepaliveInterval = 60
	}
	if conf.Serial == "" {
		conf.Serial = "34020000002000000001"
	}

	if conf.Realm == "" {
		conf.Realm = "3402000000"
	}

	if conf.MediaConfig.MediaIp == "" {
		conf.MediaConfig.MediaIp = "0.0.0.0"
	}

	if conf.MediaConfig.ListenPort == 0 {
		conf.MediaConfig.ListenPort = 30000
	}
	if conf.MediaConfig.MultiPortMaxIncrement == 0 {
		conf.MediaConfig.MultiPortMaxIncrement = 3000
	}
	gb28181Server := &GB28181Server{
		conf:               conf,
		RegisterValidity:   3600 * time.Second,
		HeartbeatInterval:  60 * time.Second,
		RemoveBanInterval:  600 * time.Second,
		PlaybackSessionTTL: 3 * time.Hour,
		keepaliveInterval:  conf.KeepaliveInterval,
		lalServer:          lal,
		udpAvailConnPool:   NewAvailConnPool(conf.MediaConfig.ListenPort+1, conf.MediaConfig.ListenPort+conf.MediaConfig.MultiPortMaxIncrement),
		tcpAvailConnPool:   NewAvailConnPool(conf.MediaConfig.ListenPort+1, conf.MediaConfig.ListenPort+conf.MediaConfig.MultiPortMaxIncrement),
	}
	gb28181Server.tcpAvailConnPool.onListenWithPort = func(port uint16) (net.Listener, error) {
		return net.Listen("tcp", fmt.Sprintf(":%d", port))
	}

	gb28181Server.udpAvailConnPool.onListenWithPort = func(port uint16) (net.Listener, error) {
		addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
		if err != nil {
			base.Log.Error("gb28181 media server udp listen failed,err:", err)
			return nil, err
		}

		return udpTransport.Listen("udp", addr)
	}
	return gb28181Server
}

// GetConfig 返回当前 GB28181 配置（用于生成 SDP 时保证使用最新配置）
func (s *GB28181Server) GetConfig() GB28181Config {
	return s.conf
}

func (s *GB28181Server) Start() {
	s.sipUdpSvr = s.newSipServer("udp")
	s.sipTcpSvr = s.newSipServer("tcp")
	go s.startJob()
}
func (s *GB28181Server) newSipServer(network string) gosip.Server {
	srvConf := gosip.ServerConfig{}

	if s.conf.SipIP != "" {
		srvConf.Host = s.conf.SipIP
	}
	sipSvr := gosip.NewServer(srvConf, nil, nil, logger)
	sipSvr.OnRequest(sip.REGISTER, s.OnRegister)
	sipSvr.OnRequest(sip.MESSAGE, s.OnMessage)
	sipSvr.OnRequest(sip.NOTIFY, s.OnNotify)
	sipSvr.OnRequest(sip.BYE, s.OnBye)

	addr := s.conf.ListenAddr + ":" + strconv.Itoa(int(s.conf.SipPort))
	err := sipSvr.Listen(network, addr)
	if err != nil {
		base.Log.Fatalf("%+v", err)
	}

	base.Log.Info(" start sip server listen. addr= " + addr + "  network:" + network)
	return sipSvr
}
func (s *GB28181Server) Dispose() {
	s.disposeOnce.Do(
		func() {
			s.MediaServerMap.Range(func(_, value any) bool {
				mediaServer := value.(*mediaserver.GB28181MediaServer)
				mediaServer.Dispose()
				return true
			})
			s.sipTcpSvr.Shutdown()
			s.sipUdpSvr.Shutdown()
		})
}
func (s *GB28181Server) OnStartMediaServer(netWork string, singlePort bool, deviceId string, channelId string) *mediaserver.GB28181MediaServer {
	isTcpFlag := false
	if netWork == "tcp" {
		isTcpFlag = true
	}
	var mediasvr *mediaserver.GB28181MediaServer
	if singlePort {
		if isTcpFlag {
			value, ok := s.MediaServerMap.Load(fmt.Sprintf("%s%d", "tcp", s.conf.MediaConfig.ListenPort))
			if ok {
				mediasvr = value.(*mediaserver.GB28181MediaServer)
			}
		} else {
			value, ok := s.MediaServerMap.Load(fmt.Sprintf("%s%d", "udp", s.conf.MediaConfig.ListenPort))
			if ok {
				mediasvr = value.(*mediaserver.GB28181MediaServer)
			}
		}
	} else {
		value, ok := s.MediaServerMap.Load(fmt.Sprintf("%s%s", deviceId, channelId))
		if ok {
			mediasvr = value.(*mediaserver.GB28181MediaServer)
		}
	}
	var listener net.Listener
	var err error
	var port uint16
	if mediasvr == nil {
		if singlePort {
			if isTcpFlag {
				mediasvr = mediaserver.NewGB28181MediaServer(int(s.conf.MediaConfig.ListenPort), fmt.Sprintf("%s%d", "tcp", s.conf.MediaConfig.ListenPort), s, s.lalServer)
				listener, err = s.tcpAvailConnPool.ListenWithPort(s.conf.MediaConfig.ListenPort)
				if err != nil {
					base.Log.Error("gb28181 media server tcp Listen failed:%s", err.Error())
					return nil
				}
				s.MediaServerMap.Store(fmt.Sprintf("%s%d", "tcp", s.conf.MediaConfig.ListenPort), mediasvr)
			} else {
				mediasvr = mediaserver.NewGB28181MediaServer(int(s.conf.MediaConfig.ListenPort), fmt.Sprintf("%s%d", "udp", s.conf.MediaConfig.ListenPort), s, s.lalServer)
				listener, err = s.udpAvailConnPool.ListenWithPort(s.conf.MediaConfig.ListenPort)
				if err != nil {
					base.Log.Error("gb28181 media server udp Listen failed:%s", err.Error())
					return nil
				}
				s.MediaServerMap.Store(fmt.Sprintf("%s%d", "udp", s.conf.MediaConfig.ListenPort), mediasvr)
			}
		} else {
			mediaKey := ""
			if isTcpFlag {
				listener, port, err = s.tcpAvailConnPool.Acquire()
				if err != nil {
					base.Log.Error("gb28181 media server tcp acquire failed:%s", err.Error())
					return nil
				}
				mediaKey = fmt.Sprintf("%s%d", "tcp", port)
			} else {
				listener, port, err = s.udpAvailConnPool.Acquire()
				if err != nil {
					base.Log.Error("gb28181 media server udp acquire failed:%s", err.Error())
					return nil
				}
				mediaKey = fmt.Sprintf("%s%d", "udp", port)
			}
			mediasvr = mediaserver.NewGB28181MediaServer(int(port), mediaKey, s, s.lalServer)
			s.MediaServerMap.Store(fmt.Sprintf("%s%s", deviceId, channelId), mediasvr)
		}
		go mediasvr.Start(listener)
	}
	return mediasvr
}
func (s *GB28181Server) OnStopMediaServer(netWork string, singlePort bool, deviceId string, channelId string, StreamName string) error {
	isTcpFlag := false
	if netWork == "tcp" {
		isTcpFlag = true
	}
	var mediasvr *mediaserver.GB28181MediaServer
	if singlePort {
		if isTcpFlag {
			key := fmt.Sprintf("%s%d", "tcp", s.conf.MediaConfig.ListenPort)
			value, ok := s.MediaServerMap.Load(key)
			if ok {
				mediasvr = value.(*mediaserver.GB28181MediaServer)
			}
		} else {
			key := fmt.Sprintf("%s%d", "udp", s.conf.MediaConfig.ListenPort)
			value, ok := s.MediaServerMap.Load(key)
			if ok {
				mediasvr = value.(*mediaserver.GB28181MediaServer)
			}
		}
	} else {
		key := fmt.Sprintf("%s%s", deviceId, channelId)
		value, ok := s.MediaServerMap.Load(key)
		if ok {
			mediasvr = value.(*mediaserver.GB28181MediaServer)
			s.MediaServerMap.Delete(key)
		}
	}
	if mediasvr != nil {
		if singlePort {
			mediasvr.CloseConn(StreamName)
		} else {
			mediasvr.Dispose()
		}
	}
	return nil
}
func (s *GB28181Server) CheckSsrc(ssrc uint32) (*mediaserver.MediaInfo, bool) {
	var isValidSsrc bool
	var mediaInfo *mediaserver.MediaInfo

	Devices.Range(func(_, value any) bool {
		d := value.(*Device)
		d.channelMap.Range(func(key, value any) bool {
			ch := value.(*Channel)
			if ch.MediaInfo.IsInvite && ch.MediaInfo.Ssrc == ssrc {
				isValidSsrc = true
				mediaInfo = &ch.MediaInfo
				return false
			}
			return true
		})
		if isValidSsrc {
			return false
		}
		return true
	})

	if isValidSsrc {
		return mediaInfo, true
	}

	return nil, false
}
func (s *GB28181Server) GetMediaInfoByKey(key string) (*mediaserver.MediaInfo, bool) {
	var isValidMediaInfo bool
	var mediaInfo *mediaserver.MediaInfo

	Devices.Range(func(_, value any) bool {
		d := value.(*Device)
		d.channelMap.Range(func(_, value any) bool {
			ch := value.(*Channel)
			if ch.MediaInfo.IsInvite && ch.MediaInfo.MediaKey == key {
				isValidMediaInfo = true
				mediaInfo = &ch.MediaInfo
				return false
			}
			return true
		})
		if isValidMediaInfo {
			return false
		}
		return true
	})

	if isValidMediaInfo {
		return mediaInfo, true
	}

	return nil, false
}

func (s *GB28181Server) NotifyClose(streamName string) {
	var ok bool
	Devices.Range(func(_, value any) bool {
		d := value.(*Device)
		d.channelMap.Range(func(key, value any) bool {
			ch := value.(*Channel)
			if ch.MediaInfo.StreamName == streamName {
				if ch.MediaInfo.IsInvite {
					ch.Bye(streamName)
				}
				ch.MediaInfo.Clear()
				ok = true
				// 主动关闭时移除回放会话
				s.UnregisterPlaybackSession(streamName)
				return false
			}
			return true
		})
		if ok {
			return false
		}
		return true
	})
}

func (s *GB28181Server) startJob() {
	statusTick := time.NewTicker(s.HeartbeatInterval / 2)
	banTick := time.NewTicker(s.RemoveBanInterval)
	for {
		select {
		case <-banTick.C:
			if s.conf.Username != "" || s.conf.Password != "" {
				s.removeBanDevice()
			}
		case <-statusTick.C:
			s.statusCheck()
			// 定期清理超时的回放会话
			s.clearExpiredPlaybackSessions()
		}
	}
}

func (s *GB28181Server) removeBanDevice() {
	DeviceRegisterCount.Range(func(key, value interface{}) bool {
		if value.(int) > MaxRegisterCount {
			DeviceRegisterCount.Delete(key)
		}
		return true
	})
}

// RegisterPlaybackSession 注册一次 GB28181 回放会话。
// 目前以 streamName 作为唯一标识，超时后会自动触发 BYE 关闭回放。
func (s *GB28181Server) RegisterPlaybackSession(streamName string) {
	if streamName == "" {
		return
	}
	expireAt := time.Now().Add(s.PlaybackSessionTTL)
	s.playbackSessions.Store(streamName, expireAt)
	base.Log.Infof("gb28181 playback session registered. streamName=%s expireAt=%s ttl=%s",
		streamName, expireAt.Format("2006-01-02 15:04:05"), s.PlaybackSessionTTL.String())
}

// UnregisterPlaybackSession 主动停止回放时移除会话。
func (s *GB28181Server) UnregisterPlaybackSession(streamName string) {
	if streamName == "" {
		return
	}
	s.playbackSessions.Delete(streamName)
	s.playbackScales.Delete(streamName)
}

// SetPlaybackScale 记录指定回放会话的倍速配置。
// scale<=0 或 scale==1.0 时视为关闭自定义倍速，恢复正常速度。
func (s *GB28181Server) SetPlaybackScale(streamName string, scale float64) {
	if streamName == "" {
		return
	}
	if scale <= 0 || scale == 1.0 {
		s.playbackScales.Delete(streamName)
		return
	}
	s.playbackScales.Store(streamName, scale)
}

// GetPlaybackScale 获取指定回放会话当前配置的倍速，默认返回 1.0。
func (s *GB28181Server) GetPlaybackScale(streamName string) float64 {
	if streamName == "" {
		return 1.0
	}
	if v, ok := s.playbackScales.Load(streamName); ok {
		if f, ok2 := v.(float64); ok2 && f > 0 {
			return f
		}
	}
	return 1.0
}

// clearExpiredPlaybackSessions 清理已超过有效期的回放会话。
// 通过 streamName 反查 Channel，并发送 BYE 结束会话。
func (s *GB28181Server) clearExpiredPlaybackSessions() {
	now := time.Now()
	s.playbackSessions.Range(func(key, value any) bool {
		streamName, ok := key.(string)
		if !ok || streamName == "" {
			return true
		}
		expireAt, ok := value.(time.Time)
		if !ok {
			// 类型异常时防御性删除，避免泄漏
			s.playbackSessions.Delete(key)
			return true
		}
		if now.After(expireAt) {
			base.Log.Infof("gb28181 playback session expired, auto stop. streamName=%s expireAt=%s",
				streamName, expireAt.Format("2006-01-02 15:04:05"))
			// 删除会话记录
			s.playbackSessions.Delete(key)
			s.playbackScales.Delete(streamName)
			// 找到对应通道并发送 BYE
			if ch := s.FindChannelByStreamName(streamName); ch != nil {
				if err := ch.Bye(streamName); err != nil {
					base.Log.Warnf("gb28181 auto stop playback failed. streamName=%s err=%+v", streamName, err)
				}
			}
		}
		return true
	})
}

// statusCheck
// -  当设备超过 3 倍心跳时间未发送过心跳（通过 UpdateTime 判断）, 视为离线
// - 	当设备超过注册有效期内为发送过消息，则从设备列表中删除
// UpdateTime 在设备发送心跳之外的消息也会被更新，相对于 LastKeepaliveAt 更能体现出设备最会一次活跃的时间
func (s *GB28181Server) statusCheck() {
	Devices.Range(func(key, value any) bool {
		d := value.(*Device)
		ka := d.LastKeepaliveAt
		if ka.IsZero() {
			// 兼容：部分设备可能不发送 Keepalive MESSAGE，改用 UpdateTime 作为活跃时间。
			ka = d.UpdateTime
		}
		if int(time.Since(ka).Seconds()) > s.keepaliveInterval*3 {
			Devices.Delete(key)
			base.Log.Warn("Device Keepalive timeout, id:", d.ID, " LastKeepaliveAt:", d.LastKeepaliveAt, " updateTime:", d.UpdateTime)
		} else if time.Since(d.UpdateTime) > s.HeartbeatInterval*3 {
			d.Status = DeviceOfflineStatus
			d.channelMap.Range(func(key, value any) bool {
				ch := value.(*Channel)
				ch.Status = ChannelOffStatus
				return true
			})
			base.Log.Warn("Device offline, id:", d.ID, " registerTime:", d.RegisterTime, " updateTime:", d.UpdateTime)
		}
		return true
	})
}

// GetDeviceInfos 返回设备及通道列表，供 HTTP API 使用
func (s *GB28181Server) GetDeviceInfos() (deviceInfos *DeviceInfos) {
	return s.getDeviceInfos()
}

func (s *GB28181Server) getDeviceInfos() (deviceInfos *DeviceInfos) {
	deviceInfos = &DeviceInfos{
		DeviceItems: make([]*DeviceItem, 0),
	}
	Devices.Range(func(key, value any) bool {
		d := value.(*Device)
		keepaliveTime := ""
		if !d.LastKeepaliveAt.IsZero() {
			keepaliveTime = d.LastKeepaliveAt.Format("2006-01-02 15:04:05")
		}
		deviceItem := &DeviceItem{
			DeviceId:      d.ID,
			Name:          d.Name,
			RegisterTime:  d.RegisterTime.Format("2006-01-02 15:04:05"),
			KeepaliveTime: keepaliveTime,
			Status:        d.Status,
			Channels:      make([]*ChannelItem, 0),
		}
		d.channelMap.Range(func(key, value any) bool {
			ch := value.(*Channel)
			channel := &ChannelItem{
				ChannelId:    ch.ChannelId,
				Name:         ch.Name,
				Manufacturer: ch.Manufacturer,
				Owner:        ch.Owner,
				CivilCode:    ch.CivilCode,
				Address:      ch.Address,
				Status:       ch.Status,
				Longitude:    ch.Longitude,
				Latitude:     ch.Latitude,
				StreamName:   ch.StreamName,
			}
			deviceItem.Channels = append(deviceItem.Channels, channel)
			return true
		})
		deviceInfos.DeviceItems = append(deviceInfos.DeviceItems, deviceItem)
		return true
	})
	return deviceInfos
}
func (s *GB28181Server) GetAllSyncChannels() {
	Devices.Range(func(key, value any) bool {
		d := value.(*Device)
		d.syncChannels()
		return true
	})
}
func (s *GB28181Server) GetSyncChannels(deviceId string) bool {
	if v, ok := Devices.Load(deviceId); ok {
		d := v.(*Device)
		d.syncChannels()
		return true
	} else {
		return false
	}
}
func (s *GB28181Server) FindChannel(deviceId string, channelId string) (channel *Channel) {
	if v, ok := Devices.Load(deviceId); ok {
		d := v.(*Device)
		if ch, ok := d.channelMap.Load(channelId); ok {
			channel = ch.(*Channel)
			return channel
		} else {
			return nil
		}
	} else {
		return nil
	}
}

// FindChannelByStreamName 根据当前拉流的 streamName 查找通道。
// 主要用于 stop(BYE) 时精确定位实际在拉流的通道（例如 stream_index 导致选择了不同的 channelId）。
func (s *GB28181Server) FindChannelByStreamName(streamName string) (channel *Channel) {
	if streamName == "" {
		return nil
	}
	var found *Channel
	Devices.Range(func(_, value any) bool {
		d := value.(*Device)
		d.channelMap.Range(func(_, v any) bool {
			ch := v.(*Channel)
			if ch.MediaInfo.IsInvite && ch.MediaInfo.StreamName == streamName {
				found = ch
				return false
			}
			return true
		})
		return found == nil
	})
	return found
}

// FindInvitingChannelByRequest 根据“请求视角”的 deviceId/channelId/streamIndex 查找正在拉流的通道。
// 说明：由于主/子码流可能通过 heuristic 选择到不同的实际 ChannelId，这里使用 playInfo 记录的原始请求参数来判定重复拉流。
func (s *GB28181Server) FindInvitingChannelByRequest(deviceId, channelId string, streamIndex int) (channel *Channel) {
	var found *Channel
	Devices.Range(func(_, value any) bool {
		d := value.(*Device)
		d.channelMap.Range(func(_, v any) bool {
			ch := v.(*Channel)
			if !ch.MediaInfo.IsInvite || ch.playInfo == nil {
				return true
			}
			if ch.playInfo.DeviceId == deviceId && ch.playInfo.ChannelId == channelId && ch.playInfo.StreamIndex == streamIndex {
				found = ch
				return false
			}
			return true
		})
		return found == nil
	})
	return found
}

// FindChannelWithStreamType 根据主/辅码流选择通道。
//
// streamType: 0=主码流，1=辅码流（默认仍推荐业务侧通过 ChannelId 精确指定）。
// 为兼容部分海康等设备的使用习惯，这里做一个简单的 heuristic：
// - 当 streamType=1 且 channelId 长度为20位时：
//   - 认为最后3位为通道中“码流序号”，例如 ...001 主、...002 辅；
//   - 在同一设备下查找同 base(前17位) 且后3位不同的其它通道，优先 suffix 为 002/102/202。
//
// 找不到匹配时，退回到原始 channelId。
func (s *GB28181Server) FindChannelWithStreamType(deviceId, channelId string, streamType int) *Channel {
	if streamType == 0 {
		return s.FindChannel(deviceId, channelId)
	}

	v, ok := Devices.Load(deviceId)
	if !ok {
		return nil
	}
	d := v.(*Device)

	// 仅在长度为20位的 channelId 上尝试 heuristic
	if len(channelId) != 20 {
		return s.FindChannel(deviceId, channelId)
	}

	baseId := channelId[:17]
	origSuffix := channelId[17:]
	preferSuffixes := []string{"002", "102", "202"}

	var candidate *Channel
	d.channelMap.Range(func(_, value any) bool {
		ch := value.(*Channel)
		if ch.ChannelId == channelId || len(ch.ChannelId) != 20 {
			return true
		}
		if !strings.HasPrefix(ch.ChannelId, baseId) {
			return true
		}
		suf := ch.ChannelId[17:]
		if suf == origSuffix {
			return true
		}
		for _, ps := range preferSuffixes {
			if suf == ps {
				candidate = ch
				return false
			}
		}
		if candidate == nil {
			candidate = ch
		}
		return true
	})

	if candidate != nil {
		return candidate
	}
	return s.FindChannel(deviceId, channelId)
}

// GetDevice 根据设备 ID 获取设备，供 API 使用
func (s *GB28181Server) GetDevice(deviceId string) *Device {
	if v, ok := Devices.Load(deviceId); ok {
		return v.(*Device)
	}
	return nil
}

// QueryDeviceInfo 向设备查询设备信息
func (s *GB28181Server) QueryDeviceInfo(deviceId string) {
	if d := s.GetDevice(deviceId); d != nil {
		d.QueryDeviceInfo(s.conf)
	}
}

// QueryCatalog 向设备查询通道目录
func (s *GB28181Server) QueryCatalog(deviceId string) int {
	if d := s.GetDevice(deviceId); d != nil {
		return d.Catalog(s.conf)
	}
	return 0
}

// StreamInfo 流信息，用于 GetAllStreams
type StreamInfo struct {
	DeviceId   string
	ChannelId  string
	StreamName string
}

// GetAllStreams 返回当前已邀请（正在拉流）的通道列表
func (s *GB28181Server) GetAllStreams() (list []StreamInfo) {
	list = make([]StreamInfo, 0)
	Devices.Range(func(_, value any) bool {
		d := value.(*Device)
		d.channelMap.Range(func(_, value any) bool {
			ch := value.(*Channel)
			if ch.MediaInfo.StreamName != "" {
				list = append(list, StreamInfo{
					DeviceId:   d.ID,
					ChannelId:  ch.ChannelId,
					StreamName: ch.MediaInfo.StreamName,
				})
			}
			return true
		})
		return true
	})
	return list
}
func (s *GB28181Server) OnRegister(req sip.Request, tx sip.ServerTransaction) {
	from, ok := req.From()
	if !ok || from.Address == nil {
		base.Log.Error("OnRegister, no from")
		return
	}
	id := from.Address.User().String()

	base.Log.Info("OnRegister", " id:", id, " source:", req.Source(), " req:", req.String())

	isUnregister := false
	if exps := req.GetHeaders("Expires"); len(exps) > 0 {
		exp := exps[0]
		expSec, err := strconv.ParseInt(exp.Value(), 10, 32)
		if err != nil {
			base.Log.Error(err)
			return
		}
		if expSec == 0 {
			isUnregister = true
		}
	} else {
		base.Log.Warn("OnRegister has no expire header, source:", req.Source())
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "Missing Expires", ""))
		return
	}

	base.Log.Info("OnRegister", " isUnregister:", isUnregister, " id:", id, " source:", req.Source(), " destination:", req.Destination())

	if len(id) != 20 {
		if !s.conf.AllowNonStandardDeviceId {
			// 多为扫描或非国标客户端；不回复会堆积事务，刷 ERROR 也无助于排障
			base.Log.Debugf("gb28181 register rejected non-standard id=%s len=%d source=%s", id, len(id), req.Source())
			_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusForbidden, "Invalid device id", ""))
			return
		}
		base.Log.Warnf("gb28181 register non-standard device id accepted. id=%s len=%d source=%s", id, len(id), req.Source())
	}

	passAuth := false
	// 不需要密码情况
	if s.conf.Username == "" && s.conf.Password == "" {
		passAuth = true
	} else {
		// 需要密码情况 设备第一次上报，返回401和加密算法
		if hdrs := req.GetHeaders("Authorization"); len(hdrs) > 0 {
			authenticateHeader := hdrs[0].(*sip.GenericHeader)
			auth := &Authorization{sip.AuthFromValue(authenticateHeader.Contents)}

			// Digest 校验用 r1 = MD5(username:realm:password)，username 必须与客户端计算时一致。
			// 有些摄像头没有配置用户名，只用密码注册，Digest 里用户名往往是设备国标 ID，或 Authorization 不带 username。
			// 服务端若也只配密码未配 Username，不能用空字符串算 r1，否则与客户端不一致导致校验失败。
			var username string
			switch {
			case auth.Username() == id:
				username = id
			case auth.Username() == "":
				username = id // 客户端未带用户名时按国标惯例用设备 ID
			case s.conf.Username != "":
				username = s.conf.Username
			default:
				username = id // 服务端仅密码、未配 Username 时与客户端「只填密码」一致，用设备 ID
			}

			if dc, ok := DeviceRegisterCount.LoadOrStore(id, 1); ok && dc.(int) > MaxRegisterCount {
				response := sip.NewResponseFromRequest("", req, http.StatusForbidden, "Forbidden", "")
				tx.Respond(response)
				return
			} else {
				// 设备第二次上报，校验
				_nonce, loaded := DeviceNonce.Load(id)
				if loaded && auth.Verify(username, s.conf.Password, s.conf.Realm, _nonce.(string)) {
					passAuth = true
				} else {
					DeviceRegisterCount.Store(id, dc.(int)+1)
				}
			}
		}
	}

	if passAuth {
		var d *Device
		if isUnregister {
			tmpd, ok := Devices.LoadAndDelete(id)
			if ok {
				base.Log.Info("Unregister Device, id:", id)
				d = tmpd.(*Device)
			} else {
				return
			}
		} else {
			if v, ok := Devices.Load(id); ok {
				d = v.(*Device)
				s.RecoverDevice(d, req)
			} else {
				d = s.StoreDevice(id, req)
			}
		}

		// REGISTER 成功即视为在线与活跃（兼容不发 Keepalive MESSAGE 的设备）
		if d != nil && !isUnregister {
			now := time.Now()
			d.Status = DeviceOnlineStatus
			d.UpdateTime = now
			d.LastKeepaliveAt = now
		}

		DeviceNonce.Delete(id)
		DeviceRegisterCount.Delete(id)
		resp := sip.NewResponseFromRequest("", req, http.StatusOK, "OK", "")
		to, _ := resp.To()
		resp.ReplaceHeaders("To", []sip.Header{&sip.ToHeader{Address: to.Address, Params: sip.NewParams().Add("tag", sip.String{Str: RandNumString(9)})}})
		resp.RemoveHeader("Allow")
		expires := sip.Expires(3600)
		resp.AppendHeader(&expires)
		resp.AppendHeader(&sip.GenericHeader{
			HeaderName: "Date",
			Contents:   time.Now().Format(TIME_LAYOUT),
		})
		_ = tx.Respond(resp)

		if !isUnregister {
			//订阅设备更新
			go d.syncChannels()
		}
	} else {
		base.Log.Info("OnRegister unauthorized, id:", id, " source:", req.Source(), " destination:", req.Destination())
		response := sip.NewResponseFromRequest("", req, http.StatusUnauthorized, "Unauthorized", "")
		_nonce, _ := DeviceNonce.LoadOrStore(id, RandNumString(32))
		auth := fmt.Sprintf(
			`Digest realm="%s",algorithm=%s,nonce="%s"`,
			s.conf.Realm,
			"MD5",
			_nonce.(string),
		)
		response.AppendHeader(&sip.GenericHeader{
			HeaderName: "WWW-Authenticate",
			Contents:   auth,
		})
		_ = tx.Respond(response)
	}
}

func (s *GB28181Server) OnMessage(req sip.Request, tx sip.ServerTransaction) {
	from, _ := req.From()
	id := from.Address.User().String()
	base.Log.Info("SIP<-OnMessage, id:", id, " source:", req.Source(), " req:", req.String())
	temp := &struct {
		XMLName      xml.Name
		CmdType      string
		SN           int // 请求序列号，一般用于对应 request 和 response
		DeviceID     string
		DeviceName   string
		Manufacturer string
		Model        string
		Channel      string
		DeviceList   []ChannelInfo `xml:"DeviceList>Item"`
		SumNum       int           // 录像结果的总数 SumNum，录像结果会按照多条消息返回，可用于判断是否全部返回
	}{}
	decoder := xml.NewDecoder(bytes.NewReader([]byte(req.Body())))
	decoder.CharsetReader = charset.NewReaderLabel
	err := decoder.Decode(temp)
	if err != nil {
		err = DecodeGbk(temp, []byte(req.Body()))
		if err != nil {
			base.Log.Error("decode catelog err:", err)
		}
	}
	if v, ok := Devices.Load(id); ok {
		d := v.(*Device)
		switch d.Status {
		case DeviceOfflineStatus, DeviceRecoverStatus:
			s.RecoverDevice(d, req)
			//go d.syncChannels(s.conf)
		case DeviceRegisterStatus:
			d.Status = DeviceOnlineStatus
		}
		d.UpdateTime = time.Now()

		var body string
		switch temp.CmdType {
		case "Keepalive":
			d.LastKeepaliveAt = time.Now()
			//callID !="" 说明是订阅的事件类型信息
			//if d.lastSyncTime.IsZero() {
			//	go d.syncChannels(s.conf)
			//}
		case "Catalog":
			d.UpdateChannels(temp.DeviceList...)
		case "DeviceInfo":
			// 主设备信息
			d.Name = temp.DeviceName
			d.Manufacturer = temp.Manufacturer
			d.Model = temp.Model
		case "Alarm":
			d.Status = DeviceAlarmedStatus
			body = BuildAlarmResponseXML(d.ID)
		default:
			base.Log.Warn("Not supported CmdType, CmdType:", temp.CmdType, " body:", req.Body())
			response := sip.NewResponseFromRequest("", req, http.StatusBadRequest, "", "")
			tx.Respond(response)
			return
		}

		tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", body))
	} else {
		if s.conf.QuickLogin {
			switch temp.CmdType {
			case "Keepalive":
				d := s.StoreDevice(id, req)
				d.LastKeepaliveAt = time.Now()
				tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", ""))
				go d.syncChannels()
				return
			}
		}

		base.Log.Warn("Unauthorized message, device not found, id:", id)
		tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "device not found", ""))
	}
}

func (s *GB28181Server) OnNotify(req sip.Request, tx sip.ServerTransaction) {
	from, _ := req.From()
	id := from.Address.User().String()
	if v, ok := Devices.Load(id); ok {
		d := v.(*Device)
		d.UpdateTime = time.Now()
		temp := &struct {
			XMLName    xml.Name
			CmdType    string
			DeviceID   string
			Time       string           //位置订阅-GPS时间
			Longitude  string           //位置订阅-经度
			Latitude   string           //位置订阅-维度
			DeviceList []*notifyMessage `xml:"DeviceList>Item"` //目录订阅
		}{}
		decoder := xml.NewDecoder(bytes.NewReader([]byte(req.Body())))
		decoder.CharsetReader = charset.NewReaderLabel
		err := decoder.Decode(temp)
		if err != nil {
			err = DecodeGbk(temp, []byte(req.Body()))
			if err != nil {
				base.Log.Error("decode catelog failed, err:", err)
			}
		}
		var body string
		switch temp.CmdType {
		case "Catalog":
			//目录状态
			d.UpdateChannelStatus(temp.DeviceList, s.conf)
		case "MobilePosition":
			//更新channel的坐标
			d.UpdateChannelPosition(temp.DeviceID, temp.Time, temp.Longitude, temp.Latitude)
		default:
			base.Log.Warn("Not supported CmdType, cmdType:", temp.CmdType, " body:", req.Body())
			response := sip.NewResponseFromRequest("", req, http.StatusBadRequest, "", "")
			tx.Respond(response)
			return
		}

		tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", body))
	} else {
		tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "device not found", ""))
	}
}

func (s *GB28181Server) OnBye(req sip.Request, tx sip.ServerTransaction) {
	callIdStr := ""
	if callId, ok := req.CallID(); ok {
		callIdStr = callId.Value()
	}
	from, _ := req.From()
	devId := from.Address.User().String()
	if _d, ok := Devices.Load(devId); ok {
		d := _d.(*Device)
		d.channelMap.Range(func(key, value any) bool {
			ch := value.(*Channel)
			if ch.GetCallId() == callIdStr {
				// 记录当前流名用于同步清理回放会话（byeClear 会清空 MediaInfo）
				streamName := ch.MediaInfo.StreamName
				ch.byeClear()
				if streamName != "" {
					s.UnregisterPlaybackSession(streamName)
				}
				return false
			}
			return true
		})
	}
	tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", ""))
}

func (s *GB28181Server) StoreDevice(id string, req sip.Request) (d *Device) {
	from, _ := req.From()
	deviceAddr := sip.Address{
		DisplayName: from.DisplayName,
		Uri:         from.Address,
	}
	deviceIp := req.Source()
	if _d, ok := Devices.Load(id); ok {
		d = _d.(*Device)
		now := time.Now()
		d.UpdateTime = now
		// 兼容部分设备仅通过 REGISTER 续期，不发送 Keepalive MESSAGE。
		// 将 REGISTER 视为一次活跃/心跳。
		d.LastKeepaliveAt = now
		d.NetAddr = deviceIp
		d.addr = deviceAddr
		d.network = strings.ToLower(req.Transport())
		if d.network == "udp" {
			d.sipSvr = s.sipUdpSvr
		} else {
			d.sipSvr = s.sipTcpSvr
		}
		// REGISTER 的 From DisplayName 作为设备名（未收过 DeviceInfo 时优先显示）
		if dn := sipMaybeStringTrim(from.DisplayName); dn != "" && d.Name == "" {
			d.Name = dn
		}
		base.Log.Info("UpdateDevice, netaddr:", d.NetAddr)
	} else {
		servIp := req.Recipient().Host()

		sipIp := s.conf.SipIP
		mediaIp := s.conf.MediaConfig.MediaIp
		name := sipMaybeStringTrim(from.DisplayName)
		now := time.Now()
		d = &Device{
			ID:              id,
			Name:            name,
			RegisterTime:    now,
			UpdateTime:      now,
			LastKeepaliveAt: now,
			Status:          DeviceRegisterStatus,
			addr:            deviceAddr,
			sipIP:           sipIp,
			mediaIP:         mediaIp,
			NetAddr:         deviceIp,
			conf:            s.conf,
			network:         strings.ToLower(req.Transport()),
		}
		if d.network == "udp" {
			d.sipSvr = s.sipUdpSvr
		} else {
			d.sipSvr = s.sipTcpSvr
		}
		d.WithMediaServer(s)
		base.Log.Info("StoreDevice, deviceIp:", deviceIp, " serverIp:", servIp, " mediaIp:", mediaIp, " sipIP:", sipIp)
		Devices.Store(id, d)
	}

	return d
}

func (s *GB28181Server) RecoverDevice(d *Device, req sip.Request) {
	from, _ := req.From()
	d.addr = sip.Address{
		DisplayName: from.DisplayName,
		Uri:         from.Address,
	}
	if dn := sipMaybeStringTrim(from.DisplayName); dn != "" && d.Name == "" {
		d.Name = dn
	}
	deviceIp := req.Source()
	servIp := req.Recipient().Host()
	sipIp := s.conf.SipIP
	mediaIp := sipIp
	d.Status = DeviceRegisterStatus
	d.sipIP = sipIp
	d.mediaIP = mediaIp
	d.NetAddr = deviceIp
	d.network = strings.ToLower(req.Transport())
	if d.network == "udp" {
		d.sipSvr = s.sipUdpSvr
	} else {
		d.sipSvr = s.sipTcpSvr
	}
	now := time.Now()
	d.UpdateTime = now
	// 同 StoreDevice：REGISTER/Recover 视为活跃/心跳
	d.LastKeepaliveAt = now

	base.Log.Info("RecoverDevice, deviceIp:", deviceIp, " serverIp:", servIp, " mediaIp:", mediaIp, " sipIP:", sipIp)
}

type notifyMessage struct {
	ChannelInfo

	//状态改变事件 ON:上线,OFF:离线,VLOST:视频丢失,DEFECT:故障,ADD:增加,DEL:删除,UPDATE:更新(必选)
	Event string
}
