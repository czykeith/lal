package gb28181

import (
	"bytes"
	"context"
	"crypto/md5"
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
	"github.com/pion/rtp"
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

	// 上级 GB28181 平台专用 SIP Server（仅 UDP），监听 upstream_sip_port
	upstreamSipSvr gosip.Server

	MediaServerMap sync.Map
	disposeOnce    sync.Once

	// streamMediasvrIndex 维护 streamName -> GB28181MediaServer 的快速索引，
	// 用于加速上级 INVITE 查找 mediaserver 以及 Catalog 状态判断。
	streamMediasvrIndexMu sync.RWMutex
	streamMediasvrIndex   map[string]*mediaserver.GB28181MediaServer

	// playbackSessions 记录回放会话的过期时间，key 为 streamName
	playbackSessions sync.Map
	// playbackScales 记录回放会话的倍速配置，key 为 streamName，value 为 float64 倍速（>0，1 表示正常速度）
	playbackScales sync.Map
	// recoveryPending 断线待恢复的实时点播，key 为 streamName，value 为 *streamRecovery
	recoveryPending sync.Map

	// 上级 GB28181 平台（级联）运行时状态，key = upstreamConfig.ID
	upstreamsMu sync.RWMutex
	upstreams   map[string]*upstreamServer

	// upstreamsByIP 额外维护 SipIP -> upstreamID 的索引，加速通过 Source IP 反查上级平台。
	upstreamsByIPMu sync.RWMutex
	upstreamsByIP   map[string]string

	// 上级播放会话，key = Call-ID
	upstreamSessions sync.Map // map[string]*UpstreamSession

	// upstreamSessionsByChannel 维护 (upstreamID, channelID) -> Call-ID 集合 的索引，
	// 用于按通道维度快速清理上级会话和对应的转发 sink。
	upstreamSessionsByChannelMu sync.RWMutex
	upstreamSessionsByChannel   map[string]map[string]*UpstreamSession

	// upstreamCatalogCache 缓存为上级构造的 Catalog 设备列表，key = upstreamID。
	// 由于下级设备目录和订阅关系不会在毫秒级频繁变化，可以做一个短 TTL 的只读缓存，
	// 避免大量上级平台高频 Catalog 查询时，每次都全量遍历 Devices/channelMap。
	upstreamCatalogCacheMu sync.RWMutex
	upstreamCatalogCache   map[string]struct {
		list      []upstreamChannelInfo
		updatedAt time.Time
	}

	// virtualMediaserver 用于非 GB28181 流（如 RTMP/RTSP）的上级转推：仅承载 upstreamSinks，不监听端口；
	// logic 层通过 UpstreamRtpFeeder 向其中喂 RTP，由 ForwardRtp 发往上级。
	virtualMediaserverMu sync.Mutex
	virtualMediaserver   *mediaserver.GB28181MediaServer
}

const virtualMediaKey = "virtual_non_gb"

// hasActiveMediaStream 判断给定 streamName 是否在当前节点“有实际媒体数据”：包括
// - mediaserver 中存在活跃 PS 流（包含虚拟 mediaserver）；
// - 或 logic 层 Group 中存在有读流量/码率的 pub 或 pull。
func (s *GB28181Server) hasActiveMediaStream(streamName string) bool {
	if streamName == "" {
		return false
	}
	// 1) 优先用 streamMediasvrIndex 命中
	s.streamMediasvrIndexMu.RLock()
	if s.streamMediasvrIndex[streamName] != nil {
		s.streamMediasvrIndexMu.RUnlock()
		return true
	}
	s.streamMediasvrIndexMu.RUnlock()

	// 2) 回退遍历 MediaServerMap + HasStream
	found := false
	s.MediaServerMap.Range(func(_, v any) bool {
		ms, ok := v.(*mediaserver.GB28181MediaServer)
		if !ok {
			return true
		}
		if ms.HasStream(streamName) {
			found = true
			return false
		}
		return true
	})
	if found {
		return true
	}

	// 3) 逻辑层：StatGroup 中有 pub 或 pull 且有读流量/码率
	if s.lalServer != nil {
		if st := s.lalServer.StatGroup(streamName); st != nil {
			if st.StatPub.SessionId != "" && (st.StatPub.ReadBytesSum > 0 || st.StatPub.BitrateKbits > 0) {
				return true
			}
			if st.StatPull.SessionId != "" && (st.StatPull.ReadBytesSum > 0 || st.StatPull.BitrateKbits > 0) {
				return true
			}
		}
	}
	return false
}

// devIDFromSerial 根据平台 Serial 简单推导一个行政区划编码（CivilCode）的兜底值。
// 若 Serial 长度不少于 6，则取前 6 位，否则返回空字符串。
func devIDFromSerial(serial string) string {
	if len(serial) >= 6 {
		return serial[:6]
	}
	return ""
}

// streamRecovery 用于断线后自动重试 Invite（仅实时点播）
type streamRecovery struct {
	StreamName  string
	PlayInfo    PlayInfo
	RetryCount  int
	NextRetryAt time.Time
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
		conf:                      conf,
		RegisterValidity:          3600 * time.Second,
		HeartbeatInterval:         60 * time.Second,
		RemoveBanInterval:         600 * time.Second,
		PlaybackSessionTTL:        3 * time.Hour,
		keepaliveInterval:         conf.KeepaliveInterval,
		lalServer:                 lal,
		udpAvailConnPool:          NewAvailConnPool(conf.MediaConfig.ListenPort+1, conf.MediaConfig.ListenPort+conf.MediaConfig.MultiPortMaxIncrement),
		tcpAvailConnPool:          NewAvailConnPool(conf.MediaConfig.ListenPort+1, conf.MediaConfig.ListenPort+conf.MediaConfig.MultiPortMaxIncrement),
		upstreams:                 make(map[string]*upstreamServer),
		streamMediasvrIndex:       make(map[string]*mediaserver.GB28181MediaServer),
		upstreamsByIP:             make(map[string]string),
		upstreamSessionsByChannel: make(map[string]map[string]*UpstreamSession),
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
	// 下级设备侧 SIP Server（原有逻辑）
	s.sipUdpSvr = s.newSipServer("udp", s.conf.SipPort)
	s.sipTcpSvr = s.newSipServer("tcp", s.conf.SipPort)

	// 上级平台侧（中间平台）仅在启用时启动。
	if s.conf.UpstreamEnable {
		// 上级平台侧 SIP Server（仅 UDP，端口与下级分离）
		if s.conf.UpstreamSipPort == 0 {
			s.conf.UpstreamSipPort = 5061
		}
		s.upstreamSipSvr = s.newUpstreamSipServer("udp", s.conf.UpstreamSipPort)

		// 初始化上级 GB28181 平台（级联）客户端
		s.initUpstreams()
	}
	go s.startJob()
}

// upstreamServer 表示一个上级 GB28181 平台的运行时状态。
type upstreamServer struct {
	conf GB28181UpstreamConfig

	// 最近一次 REGISTER 成功时间
	lastRegisterOKAt time.Time
	// 最近一次心跳时间（采用单独 Keepalive MESSAGE，与 REGISTER 分离）
	lastKeepaliveAt time.Time

	// WWW-Authenticate 返回的 nonce 等认证信息
	nonce string

	// 用于生成上报中的 SN（简单自增即可）
	sn int

	subs sync.Map // key=streamName, value=*UpstreamStreamSub

	// 用于优雅退出 runUpstreamLoop 的控制通道。
	stopCh chan struct{}
}

// UpstreamStreamSub 表示某个上级平台对本节点某一路 streamName 的订阅关系。
type UpstreamStreamSub struct {
	UpstreamID string
	StreamName string
	ChannelID  string
	CallID     string
	CreateAt   time.Time
}

// UpstreamSession 记录一次上级点播会话的信息（仅信令侧，不含实际 RTP 转发实现）。
type UpstreamSession struct {
	UpstreamID string
	CallID     string
	DeviceID   string // 上级请求的 DeviceID（目录里的通道ID）
	StreamName string // 映射到本地的 streamName
	RemoteIP   string // 上级 SDP 中的媒体 IP
	RemotePort int    // 上级 SDP 中的媒体端口
	SinkID     string // mediaserver 中对应的上级转推目标 ID
	MediaKey   string // 对应 mediaserver 的 mediaKey，便于 Bye 时精确定位
	CreatedAt  time.Time
	// CancelFeed 用于非 GB28181 流：停止 logic 层向虚拟 mediaserver 喂 RTP。会话结束时调用。
	CancelFeed func()
}

// UpstreamRtpFeeder 由 logic 层适配器实现，用于为非 GB28181 流向虚拟 mediaserver 喂 RTP，以支持上级转推。
// feedFn 每次传入一个 RTP 包（原始字节）；cancel 由调用方在会话结束时调用以停止喂流。
type UpstreamRtpFeeder interface {
	RequestUpstreamRtpFeed(streamName string, feedFn func(rawRtp []byte)) (cancel func(), err error)
}

// upstreamChannelInfo 为上级平台返回的目录通道元素。
// 与内部 Channel/ChannelInfo 解耦，仅暴露 GB28181 目录中常用字段，显式去掉 MediaKey、Port 等实现细节。
type upstreamChannelInfo struct {
	DeviceID     string        `xml:"DeviceID"`
	ParentID     string        `xml:"ParentID"`
	Name         string        `xml:"Name"`
	Manufacturer string        `xml:"Manufacturer"`
	Model        string        `xml:"Model"`
	Owner        string        `xml:"Owner"`
	CivilCode    string        `xml:"CivilCode"`
	Address      string        `xml:"Address"`
	Parental     int           `xml:"Parental"`
	SafetyWay    int           `xml:"SafetyWay"`
	RegisterWay  int           `xml:"RegisterWay"`
	Secrecy      int           `xml:"Secrecy"`
	Status       ChannelStatus `xml:"Status"`
	Longitude    string        `xml:"Longitude"`
	Latitude     string        `xml:"Latitude"`

	// 媒体相关的可选字段：保留 IsInvite/SSRC/StreamName/SinglePort/DumpFileName，去除 MediaKey。
	IsInvite bool   `xml:"IsInvite,omitempty"`
	Ssrc     string `xml:"Ssrc,omitempty"`
	// StreamName 仅用于内部状态判断，不再对上级输出。
	StreamName   string `xml:"-"`
	SinglePort   bool   `xml:"SinglePort,omitempty"`
	DumpFileName string `xml:"DumpFileName,omitempty"`
}

// catalogResponse 上级平台发起 Catalog 查询时的响应结构。
// DeviceList 使用 upstreamChannelInfo，避免对外暴露内部实现字段。
type catalogResponse struct {
	XMLName    xml.Name              `xml:"Response"`
	CmdType    string                `xml:"CmdType"`
	SN         int                   `xml:"SN"`
	DeviceID   string                `xml:"DeviceID"`
	SumNum     int                   `xml:"SumNum"`
	DeviceList []upstreamChannelInfo `xml:"DeviceList>Item"`
}

// AddUpstreamSub 在指定上级平台新增或更新一个基于 streamName 的订阅关系。
// 实际的 REGISTER / Invite / Bye 逻辑后续在 invite-bye-skeleton 步骤中补充。
func (s *GB28181Server) AddUpstreamSub(upstreamID, streamName, channelID string) error {
	if upstreamID == "" || streamName == "" || channelID == "" {
		return fmt.Errorf("upstreamID, streamName and channelID are required")
	}
	s.upstreamsMu.RLock()
	up, ok := s.upstreams[upstreamID]
	s.upstreamsMu.RUnlock()
	if !ok {
		return fmt.Errorf("upstream not found: %s", upstreamID)
	}

	sub := &UpstreamStreamSub{
		UpstreamID: upstreamID,
		StreamName: streamName,
		ChannelID:  channelID,
		CreateAt:   time.Now(),
	}
	up.subs.Store(streamName, sub)

	// 后续在 invite-bye-skeleton 中实现：这里触发向上级的 Invite。
	s.invalidateUpstreamCatalogCache(upstreamID)
	return nil
}

// RemoveUpstreamSub 取消上级平台对某一路流的订阅（By streamName）。
// 实际的 BYE 发送逻辑后续补充。
func (s *GB28181Server) RemoveUpstreamSub(upstreamID, streamName string) error {
	if upstreamID == "" || streamName == "" {
		return fmt.Errorf("upstreamID and streamName are required")
	}
	s.upstreamsMu.RLock()
	up, ok := s.upstreams[upstreamID]
	s.upstreamsMu.RUnlock()
	if !ok {
		return fmt.Errorf("upstream not found: %s", upstreamID)
	}

	if _, ok := up.subs.Load(streamName); ok {
		up.subs.Delete(streamName)
		// 后续在 invite-bye-skeleton 中实现：这里触发向上级的 BYE。
		s.invalidateUpstreamCatalogCache(upstreamID)
	}
	return nil
}

// ListUpstreamSubs 列出某个上级平台当前记录的订阅流列表。
func (s *GB28181Server) ListUpstreamSubs(upstreamID string) []UpstreamStreamSub {
	s.upstreamsMu.RLock()
	up, ok := s.upstreams[upstreamID]
	s.upstreamsMu.RUnlock()
	if !ok {
		return nil
	}
	var out []UpstreamStreamSub
	up.subs.Range(func(_, v any) bool {
		if sub, ok := v.(*UpstreamStreamSub); ok {
			out = append(out, *sub)
		}
		return true
	})
	return out
}

// ClearAllUpstreamSubs 清空所有上级平台当前记录的订阅关系。
// 用于从配置文件重新加载订阅时先清理旧的运行时状态。
func (s *GB28181Server) ClearAllUpstreamSubs() {
	s.upstreamsMu.RLock()
	defer s.upstreamsMu.RUnlock()
	for _, up := range s.upstreams {
		up.subs.Range(func(key, _ any) bool {
			up.subs.Delete(key)
			return true
		})
	}
}

// invalidateUpstreamCatalogCache 在订阅关系变更时，清除指定上级的目录缓存。
func (s *GB28181Server) invalidateUpstreamCatalogCache(upstreamID string) {
	s.upstreamCatalogCacheMu.Lock()
	defer s.upstreamCatalogCacheMu.Unlock()
	if s.upstreamCatalogCache == nil {
		return
	}
	delete(s.upstreamCatalogCache, upstreamID)
}

// runUpstreamLoop 负责单个上级平台的注册与心跳维护。
func (s *GB28181Server) runUpstreamLoop(up *upstreamServer) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		default:
		}

		select {
		case <-ticker.C:
		case <-up.stopCh:
			return
		}

		prevRegisterAt := up.lastRegisterOKAt

		// 注册与续注册
		if err := s.ensureUpstreamRegister(up); err != nil {
			base.Log.Warnf("gb28181 upstream register failed. id=%s err=%+v", up.conf.ID, err)
			continue
		}
		// 首次注册成功后，主动向上级上报一次 Catalog 信息，方便上级立即感知本节点可用通道。
		if prevRegisterAt.IsZero() && !up.lastRegisterOKAt.IsZero() {
			if err := s.sendUpstreamCatalog(up); err != nil {
				base.Log.Warnf("gb28181 upstream send catalog after register failed. id=%s err=%+v", up.conf.ID, err)
			}
		}
		// 单独 Keepalive MESSAGE（仅在已注册成功后进行）
		if err := s.ensureUpstreamKeepalive(up); err != nil {
			base.Log.Warnf("gb28181 upstream keepalive failed. id=%s err=%+v", up.conf.ID, err)
		}
	}
}

// ensureUpstreamRegister 确保与上级的平台注册处于有效期内。
// 实现：
//   - 首次或过期时发送一次不带 Authorization 的 REGISTER；
//   - 无论是否成功解析 401，都再发送一次带 Authorization 的 REGISTER。
func (s *GB28181Server) ensureUpstreamRegister(up *upstreamServer) error {
	now := time.Now()
	validSec := up.conf.RegisterValidity
	if validSec <= 0 {
		validSec = 3600
	}
	// 未到刷新时间则不用续注册
	if !up.lastRegisterOKAt.IsZero() && now.Sub(up.lastRegisterOKAt) < time.Duration(validSec/2)*time.Second {
		return nil
	}

	// 自定义 UDP REGISTER 客户端：先发一次裸 REGISTER，再发一次带 Authorization 的 REGISTER。
	ok, realm, nonce, err := doUpstreamRegisterOnce(&s.conf, &up.conf, "", "", false)
	if err != nil {
		return err
	}
	if ok {
		up.lastRegisterOKAt = now
		return nil
	}

	// 如果第一次返回 401 并成功解析出 realm/nonce，则使用解析到的值；否则使用本地配置尝试一次。
	if realm == "" {
		realm = up.conf.Realm
	}
	up.nonce = nonce
	ok2, _, _, err2 := doUpstreamRegisterOnce(&s.conf, &up.conf, realm, nonce, true)
	if err2 != nil {
		return err2
	}
	if !ok2 {
		return fmt.Errorf("upstream auth register failed")
	}
	up.lastRegisterOKAt = now
	return nil
}

// ensureUpstreamKeepalive 确保按配置间隔向上级发送 Keepalive MESSAGE。
// 仅在最近一次 REGISTER 成功后才会发送。
func (s *GB28181Server) ensureUpstreamKeepalive(up *upstreamServer) error {
	if up.lastRegisterOKAt.IsZero() {
		// 尚未注册成功，不发送心跳
		return nil
	}
	now := time.Now()
	interval := up.conf.KeepaliveInterval
	if interval <= 0 {
		interval = 60
	}
	if !up.lastKeepaliveAt.IsZero() && now.Sub(up.lastKeepaliveAt) < time.Duration(interval)*time.Second {
		return nil
	}

	req := s.buildUpstreamKeepaliveRequest(up)
	if req == nil {
		return fmt.Errorf("build upstream keepalive request failed")
	}

	resp, err := s.upstreamSipSvr.RequestWithContext(context.Background(), req)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("empty keepalive response")
	}
	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("keepalive failed, code=%d", resp.StatusCode())
	}
	up.lastKeepaliveAt = now
	return nil
}

// OnUpstreamInvite 处理上级平台对本节点的 INVITE（点播）请求，仅完成信令与会话记录。
func (s *GB28181Server) OnUpstreamInvite(req sip.Request, tx sip.ServerTransaction) {
	// 通过 Source IP 与 Upstreams 中的 SipIP 匹配上级，避免依赖 From.User 与 LocalDeviceID 完全一致。
	source := req.Source()
	srcHost := source
	if idx := strings.LastIndex(source, ":"); idx > 0 {
		srcHost = source[:idx]
	}

	var (
		up   *upstreamServer
		upID string
	)
	// 先通过 IP 索引快速定位上级 ID，失败再回退遍历。
	s.upstreamsByIPMu.RLock()
	if id, ok := s.upstreamsByIP[srcHost]; ok {
		upID = id
	}
	s.upstreamsByIPMu.RUnlock()
	s.upstreamsMu.RLock()
	if upID != "" {
		up = s.upstreams[upID]
	} else {
		for id, u := range s.upstreams {
			if u.conf.SipIP == srcHost {
				up = u
				upID = id
				break
			}
		}
	}
	s.upstreamsMu.RUnlock()
	if up == nil {
		base.Log.Warnf("gb28181 upstream invite from unknown source=%s", source)
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "unknown upstream", ""))
		return
	}

	base.Log.Infof("gb28181 upstream INVITE from=%s source=%s\n%s", upID, source, req.String())

	// 解析 INVITE 中的通道标识。
	// 上级通常使用两种方式：
	// 1) 传统 MANSCDP XML（Body 为 Application/MANSCDP+xml，包含 CmdType=Play 和 DeviceID）；
	// 2) 仅携带 SDP（常见为 application/sdp），通过 Subject 或 To/Request-URI 传递通道 ID。
	var channelID string
	bodyRaw := strings.TrimSpace(req.Body())
	if strings.HasPrefix(bodyRaw, "<") {
		// 方式 1：MANSCDP XML
		temp := &struct {
			XMLName  xml.Name
			CmdType  string
			DeviceID string
		}{}
		if bodyRaw != "" {
			decoder := xml.NewDecoder(bytes.NewReader([]byte(bodyRaw)))
			decoder.CharsetReader = charset.NewReaderLabel
			_ = decoder.Decode(temp)
		}
		if temp.CmdType != "Play" {
			base.Log.Warnf("gb28181 upstream invite unsupported CmdType=%s", temp.CmdType)
			_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "unsupported CmdType", ""))
			return
		}
		channelID = temp.DeviceID
	} else {
		// 方式 2：纯 SDP INVITE。这里以 Subject 中的“目标通道/平台 ID”为准：
		// Subject: <channelID>:0,<deviceID>:0
		var subjVal string
		for _, h := range req.Headers() {
			if strings.EqualFold(h.Name(), "Subject") {
				subjVal = h.Value()
				break
			}
		}
		if subjVal != "" {
			parts := strings.Split(subjVal, ",")
			if len(parts) > 0 {
				// 取 Subject 中的第一个 <channelID>:0 作为通道 ID，
				// 例如 "34020000002000000001:0,42010000101234567890:0" => 34020000002000000001
				first := parts[0]
				if idx := strings.IndexByte(first, ':'); idx > 0 {
					channelID = strings.TrimSpace(first[:idx])
				} else {
					channelID = strings.TrimSpace(first)
				}
			}
		}
	}
	if channelID == "" {
		base.Log.Warnf("gb28181 upstream invite missing DeviceID/Subject channel id")
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "missing DeviceID", ""))
		return
	}
	// 先尝试在上级订阅表中，通过订阅时配置的 channel_id 映射到本地 streamName。
	// 这样可支持“非下级 GB28181 流”的订阅播放。
	var chFound *Channel
	var streamName string
	up.subs.Range(func(_, v any) bool {
		sub, ok2 := v.(*UpstreamStreamSub)
		if !ok2 {
			return true
		}
		if sub.ChannelID == channelID && sub.StreamName != "" {
			streamName = sub.StreamName
			return false
		}
		return true
	})

	// 若订阅中未找到映射关系，再退回到“下级 GB28181 通道 DeviceID == channelId”的传统查找方式。
	if streamName == "" {
		Devices.Range(func(_, dv any) bool {
			d := dv.(*Device)
			if v, ok2 := d.channelMap.Load(channelID); ok2 {
				chFound = v.(*Channel)
				return false
			}
			return true
		})
		if chFound == nil {
			base.Log.Warnf("gb28181 upstream invite channel not found. deviceID=%s", channelID)
			_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusNotFound, "channel not found", ""))
			return
		}
		// 从通道信息中获取本地 streamName（优先 MediaInfo.StreamName，其次 Channel.StreamName）
		streamName = chFound.MediaInfo.StreamName
		if streamName == "" {
			streamName = chFound.StreamName
		}
		if streamName == "" {
			base.Log.Warnf("gb28181 upstream invite no streamName for channel=%s", channelID)
			_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusNotFound, "stream not found", ""))
			return
		}
	}

	// 校验该 streamName 是否在该上级的订阅列表中（作为权限控制）
	allowed := false
	up.subs.Range(func(_, v any) bool {
		sub, ok2 := v.(*UpstreamStreamSub)
		if !ok2 {
			return true
		}
		if sub.StreamName == streamName {
			allowed = true
			return false
		}
		return true
	})
	if !allowed {
		base.Log.Warnf("gb28181 upstream invite stream not in subs. upstream=%s stream=%s", upID, streamName)
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusForbidden, "stream not subscribed", ""))
		return
	}

	// 解析上级 SDP，获取媒体接收 IP/端口和 SSRC（y=）
	var remoteIP string
	var remotePort int
	var sdpLines []string
	var remoteSSRC string
	isPlayback := false
	if body := req.Body(); body != "" {
		sdpLines = strings.Split(body, "\r\n")
		for _, l := range sdpLines {
			if strings.HasPrefix(l, "c=IN IP4 ") {
				remoteIP = strings.TrimSpace(strings.TrimPrefix(l, "c=IN IP4 "))
			}
			if strings.HasPrefix(l, "m=video ") {
				parts := strings.Split(l, " ")
				if len(parts) >= 2 {
					if p, err := strconv.Atoi(parts[1]); err == nil {
						remotePort = p
					}
				}
			}
			if strings.HasPrefix(l, "y=") {
				remoteSSRC = strings.TrimSpace(strings.TrimPrefix(l, "y="))
			}
			// 回放场景通常会携带 u=<channelId>:0 字段（与下级点播保持一致约定）
			if strings.HasPrefix(l, "u=") && strings.HasSuffix(l, ":0") {
				isPlayback = true
			}
		}
	}
	if remoteIP == "" || remotePort == 0 {
		base.Log.Warnf("gb28181 upstream invite invalid SDP c/m line. ip=%s port=%d", remoteIP, remotePort)
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "invalid sdp", ""))
		return
	}

	// 在同一上级对同一通道多次发起 INVITE 的场景下，仅保留最后一次“实时播放”请求对应的转发。
	// 回放（包含 u=<channelId>:0）不受此限制，允许并行存在。
	if !isPlayback {
		s.cleanupUpstreamSessionForChannel(upID, channelID)
	}

	// 记录上级会话（后续通过 mediaserver 将该 streamName 的 PS/RTP 额外推送到上级）。
	callID, _ := req.CallID()
	callIDStr := ""
	if callID != nil {
		callIDStr = callID.Value()
	}
	if callIDStr == "" {
		callIDStr = RandNumString(10)
	}
	sess := &UpstreamSession{
		UpstreamID: upID,
		CallID:     callIDStr,
		DeviceID:   channelID,
		StreamName: streamName,
		RemoteIP:   remoteIP,
		RemotePort: remotePort,
		CreatedAt:  time.Now(),
	}

	// 尝试找到当前 streamName 对应的 GB28181MediaServer，并为其增加一个上级转推目标（Sink）。
	// 以 streamName 为唯一依据，优先通过 streamMediasvrIndex 命中，必要时再 fallback 到旧逻辑。
	var mediasvr *mediaserver.GB28181MediaServer
	s.streamMediasvrIndexMu.RLock()
	mediasvr = s.streamMediasvrIndex[streamName]
	s.streamMediasvrIndexMu.RUnlock()
	if mediasvr == nil {
		// fallback：遍历 MediaServerMap + HasStream，兼容索引尚未建立的场景。
		s.MediaServerMap.Range(func(_, v any) bool {
			ms, ok := v.(*mediaserver.GB28181MediaServer)
			if !ok {
				return true
			}
			if ms.HasStream(streamName) {
				mediasvr = ms
				return false
			}
			return true
		})
	}
	if mediasvr == nil {
		// 非 GB28181 流（如 RTMP/RTSP）：使用虚拟 mediaserver 承载 sink，并尝试向 logic 层请求 RTP 喂流。
		mediasvr = s.getVirtualMediaServer()
		if mediasvr == nil {
			base.Log.Warnf("gb28181 upstream invite: mediaserver not found for streamName=%s", streamName)
			_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusServiceUnavailable, "mediaserver not found", ""))
			return
		}
		// 将非 GB 流也纳入 streamMediasvrIndex，便于 Catalog 状态和后续查找。
		s.streamMediasvrIndexMu.Lock()
		s.streamMediasvrIndex[streamName] = mediasvr
		s.streamMediasvrIndexMu.Unlock()
		sess.MediaKey = virtualMediaKey
	}

	// 将上级期望的 SSRC 传入 mediaserver，后续转发时重写 RTP SSRC 与 y= 保持一致。
	var ssrcUint uint32
	if remoteSSRC != "" {
		if v, err := strconv.ParseUint(remoteSSRC, 10, 32); err == nil {
			ssrcUint = uint32(v)
		}
	}
	sinkID, err := mediasvr.AddUpstreamSink(streamName, remoteIP, remotePort, ssrcUint)
	if err != nil {
		base.Log.Warnf("gb28181 upstream invite: add sink failed. stream=%s remote=%s:%d err=%+v",
			streamName, remoteIP, remotePort, err)
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusInternalServerError, "add sink failed", ""))
		return
	}
	sess.SinkID = sinkID
	// 记录 mediaKey，便于 Bye 时精确找到 mediaserver。
	if sess.MediaKey == "" {
		sess.MediaKey = mediasvr.GetMediaKey()
	}
	s.upstreamSessions.Store(callIDStr, sess)

	// 非 GB28181 流：向 logic 层请求 RTP 喂流，以便上级能收到媒体。
	if sess.MediaKey == virtualMediaKey {
		if feeder, ok := s.lalServer.(UpstreamRtpFeeder); ok {
			feedFn := func(raw []byte) {
				var pkt rtp.Packet
				if err := pkt.Unmarshal(raw); err != nil {
					return
				}
				mediasvr.ForwardRtp(streamName, &pkt)
			}
			cancel, err := feeder.RequestUpstreamRtpFeed(streamName, feedFn)
			if err == nil && cancel != nil {
				sess.CancelFeed = cancel
				s.upstreamSessions.Store(callIDStr, sess)
			}
		}
	}

	// 构造 200 OK 响应，按 GB28181 标准返回 SDP（参考实际设备侧响应）：
	// v=0
	// o=<ChannelID> 0 0 IN IP4 <本机媒体IP>
	// s=Play
	// c=IN IP4 <本机媒体IP>
	// t=0 0
	// m=video <本机媒体端口> RTP/AVP 96
	// a=rtpmap:96 PS/90000
	// a=sendonly
	// y=<SSRC>
	// f=...（可选）
	// o=/c= 中的 IP 按 GB28181 标准，使用本设备的媒体 IP：
	// 1) 优先使用上级配置中的 media_ip；
	// 2) 其次使用本地 GB28181 媒体配置的 MediaIp；
	// 3) 最后回退使用 sip_ip。
	mediaIP := up.conf.MediaIP
	if mediaIP == "" {
		mediaIP = s.conf.MediaConfig.MediaIp
	}
	if mediaIP == "" {
		mediaIP = s.conf.SipIP
	}
	mediaPort := mediasvr.GetListenerPort()
	// y 行的 SSRC 与上级请求保持一致；若上级未提供则生成一个。
	ssrc := remoteSSRC
	if ssrc == "" {
		ssrc = RandNumString(10)
	}
	sdpResp := fmt.Sprintf("v=0\r\n"+
		"o=%s 0 0 IN IP4 %s\r\n"+
		"s=Play\r\n"+
		"c=IN IP4 %s\r\n"+
		"t=0 0\r\n"+
		"m=video %d RTP/AVP 96\r\n"+
		"a=rtpmap:96 PS/90000\r\n"+
		"a=sendonly\r\n"+
		"y=%s\r\n"+
		"f=v/////a/1/8/1\r\n",
		channelID, mediaIP,
		mediaIP,
		mediaPort,
		ssrc,
	)

	resp := sip.NewResponseFromRequest("", req, http.StatusOK, "OK", "")
	ct := sip.ContentType("Application/SDP")
	resp.AppendHeader(&ct)
	// 按 GB28181 设备侧习惯，补充一个以通道 ID 和本机 SIP IP 为主的 Contact，便于上级后续 BYE/UPDATE 等路由。
	contactAddr := sip.Address{
		Uri: &sip.SipUri{
			FUser: sip.String{Str: channelID},
			FHost: s.conf.SipIP,
			FPort: nil,
		},
	}
	resp.AppendHeader(contactAddr.AsContactHeader())
	resp.SetBody(sdpResp, true)
	_ = tx.Respond(resp)
}

// OnUpstreamRegister 预留上级侧 REGISTER 处理入口（通常由自定义 UDP 客户端主导，此处可作调试或扩展用）。
func (s *GB28181Server) OnUpstreamRegister(req sip.Request, tx sip.ServerTransaction) {
	base.Log.Info("SIP<-OnUpstreamRegister, source:", req.Source(), " req:", req.String())
	// 暂时简单回 200 OK，后续如需在上级视角管理中间平台状态，可在此完善认证/状态机。
	resp := sip.NewResponseFromRequest("", req, http.StatusOK, "OK", "")
	_ = tx.Respond(resp)
}

// OnUpstreamMessage 处理来自上级平台的 MESSAGE。
// 主要用于接收上级的 Catalog/DeviceInfo/Keepalive 等请求，采用“大设备”视角响应。
func (s *GB28181Server) OnUpstreamMessage(req sip.Request, tx sip.ServerTransaction) {
	base.Log.Info("SIP<-OnUpstreamMessage, source:", req.Source(), " req:", req.String())

	// 根据 Source IP 匹配上级配置（与 OnUpstreamInvite 一致）
	source := req.Source()
	srcHost := source
	if idx := strings.LastIndex(source, ":"); idx > 0 {
		srcHost = source[:idx]
	}
	var (
		up   *upstreamServer
		upID string
	)
	s.upstreamsByIPMu.RLock()
	if id, ok := s.upstreamsByIP[srcHost]; ok {
		upID = id
	}
	s.upstreamsByIPMu.RUnlock()
	s.upstreamsMu.RLock()
	if upID != "" {
		up = s.upstreams[upID]
	} else {
		for id, u := range s.upstreams {
			if u.conf.SipIP == srcHost {
				up = u
				upID = id
				break
			}
		}
	}
	s.upstreamsMu.RUnlock()
	if up == nil {
		base.Log.Warnf("gb28181 upstream MESSAGE from unknown source=%s", source)
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "unknown upstream", ""))
		return
	}

	// 解析上级 MANSCDP XML
	temp := &struct {
		XMLName  xml.Name
		CmdType  string
		SN       int
		DeviceID string
	}{}
	if body := req.Body(); body != "" {
		decoder := xml.NewDecoder(bytes.NewReader([]byte(body)))
		decoder.CharsetReader = charset.NewReaderLabel
		_ = decoder.Decode(temp)
	}

	deviceID := temp.DeviceID
	if deviceID == "" {
		deviceID = s.conf.Serial
	}

	switch temp.CmdType {
	case "Catalog":
		body, err := s.buildUpstreamCatalogBody(upID, temp.SN, deviceID)
		if err != nil {
			base.Log.Errorf("build upstream catalog body failed. upstream=%s err=%+v", upID, err)
			_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "build catalog failed", ""))
			return
		}
		resp := sip.NewResponseFromRequest("", req, http.StatusOK, "OK", body)
		_ = tx.Respond(resp)
	case "DeviceInfo":
		body := s.buildUpstreamDeviceInfoBody(temp.SN, deviceID)
		resp := sip.NewResponseFromRequest("", req, http.StatusOK, "OK", body)
		_ = tx.Respond(resp)
	case "Keepalive":
		// 上级对本平台的 Keepalive，可简单回 200 OK 或扩展为状态上报。
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", ""))
	default:
		base.Log.Warnf("gb28181 upstream MESSAGE unsupported CmdType=%s", temp.CmdType)
		_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "unsupported CmdType", ""))
	}
}

// OnUpstreamBye 处理来自上级平台的 BYE 请求。
// 目前直接复用现有 OnBye 逻辑（其中已先处理 upstreamSessions 再处理下级设备）。
func (s *GB28181Server) OnUpstreamBye(req sip.Request, tx sip.ServerTransaction) {
	base.Log.Info("SIP<-OnUpstreamBye, source:", req.Source(), " req:", req.String())

	// 先按 Call-ID 正常处理（标准路径）。
	callID, _ := req.CallID()
	if callID != nil {
		if v, ok := s.upstreamSessions.Load(callID.Value()); ok {
			if sess, ok2 := v.(*UpstreamSession); ok2 {
				if sess.CancelFeed != nil {
					sess.CancelFeed()
					sess.CancelFeed = nil
				}
				if sess.MediaKey != "" && sess.SinkID != "" {
					if mv, ok3 := s.MediaServerMap.Load(sess.MediaKey); ok3 {
						if mediasvr, ok4 := mv.(*mediaserver.GB28181MediaServer); ok4 {
							mediasvr.RemoveUpstreamSink(sess.SinkID)
						}
					}
				}
				s.upstreamSessions.Delete(callID.Value())
				// 若不再有任何会话使用虚拟 mediaserver，则尝试清理之。
				s.maybeCleanupVirtualMediaServer()
				_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", ""))
				return
			}
		}
	}

	// 若按 Call-ID 未找到，尝试根据 Subject/通道 ID 进行兜底清理。
	var channelID string
	subjVal := ""
	for _, h := range req.Headers() {
		if strings.EqualFold(h.Name(), "Subject") {
			subjVal = h.Value()
			break
		}
	}
	if subjVal != "" {
		parts := strings.Split(subjVal, ",")
		if len(parts) > 0 {
			first := parts[0]
			if idx := strings.IndexByte(first, ':'); idx > 0 {
				channelID = strings.TrimSpace(first[:idx])
			} else {
				channelID = strings.TrimSpace(first)
			}
		}
	}
	if channelID != "" {
		// 通过 source IP 推断上级 ID（与 OnUpstreamInvite/OnUpstreamMessage 一致）。
		source := req.Source()
		srcHost := source
		if idx := strings.LastIndex(source, ":"); idx > 0 {
			srcHost = source[:idx]
		}
		var upID string
		s.upstreamsByIPMu.RLock()
		if id, ok := s.upstreamsByIP[srcHost]; ok {
			upID = id
		}
		s.upstreamsByIPMu.RUnlock()
		if upID != "" {
			s.cleanupUpstreamSessionForChannel(upID, channelID)
		}
	}

	// 无论是否找到会话，都按标准返回 200 OK，避免上级重传 BYE。
	_ = tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", ""))
}

// buildUpstreamRegisterRequest 构造向上级平台发送的 REGISTER 请求。
// withAuth 为 true 时携带 Authorization 头。
func (s *GB28181Server) buildUpstreamRegisterRequest(up *upstreamServer, withAuth bool) sip.Request {
	uc := up.conf

	deviceID := uc.LocalDeviceID
	if deviceID == "" {
		deviceID = s.conf.Serial
	}
	realm := uc.Realm
	if realm == "" {
		realm = s.conf.Realm
	}

	callId := sip.CallID(RandNumString(10))
	userAgent := sip.UserAgentHeader(SipUserAgent)
	maxForwards := sip.MaxForwards(70)
	cseq := sip.CSeq{
		SeqNo:      uint32(time.Now().Unix() & 0x7fffffff),
		MethodName: sip.REGISTER,
	}

	// From/To/Contact: 使用 deviceID@realm 形式
	addr := sip.Address{
		Uri: &sip.SipUri{
			FUser: sip.String{Str: deviceID},
			FHost: realm,
			FPort: nil,
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: RandNumString(9)}),
	}

	req := sip.NewRequest(
		"",
		sip.REGISTER,
		addr.Uri,
		"SIP/2.0",
		[]sip.Header{
			addr.AsFromHeader(),
			addr.AsToHeader(),
			&callId,
			&userAgent,
			&cseq,
			&maxForwards,
			addr.AsContactHeader(),
		},
		"",
		nil,
	)

	req.SetTransport("udp")
	req.SetDestination(net.JoinHostPort(uc.SipIP, strconv.Itoa(int(uc.SipPort))))

	expiresSec := uc.RegisterValidity
	if expiresSec <= 0 {
		expiresSec = 3600
	}
	expires := sip.Expires(expiresSec)
	req.AppendHeader(&expires)

	if withAuth {
		username := uc.Username
		if username == "" {
			username = deviceID
		}
		// 构造 Authorization 头：这里直接手动拼 Digest 响应
		uri := fmt.Sprintf("sip:%s@%s", deviceID, realm)
		// r1 = MD5(username:realm:password)
		s1 := fmt.Sprintf("%s:%s:%s", username, realm, uc.Password)
		r1 := fmt.Sprintf("%x", md5.Sum([]byte(s1)))
		// r2 = MD5(method:uri)
		s2 := fmt.Sprintf("%s:%s", "REGISTER", uri)
		r2 := fmt.Sprintf("%x", md5.Sum([]byte(s2)))
		// response = MD5(r1:nonce:r2)
		s3 := fmt.Sprintf("%s:%s:%s", r1, up.nonce, r2)
		resp := fmt.Sprintf("%x", md5.Sum([]byte(s3)))

		auth := fmt.Sprintf(
			`Digest username="%s",realm="%s",nonce="%s",uri="%s",response="%s",algorithm=MD5`,
			username, realm, up.nonce, uri, resp,
		)
		req.AppendHeader(&sip.GenericHeader{
			HeaderName: "Authorization",
			Contents:   auth,
		})
	}

	return req
}

// sendUpstreamBye 主动向上级发送 BYE，结束一次已存在的上级播放会话。
// 仅在同一上级+通道重复 INVITE 时使用，以通道为维度保证只保留最后一次转发。
func (s *GB28181Server) sendUpstreamBye(sess *UpstreamSession) {
	if sess == nil || sess.UpstreamID == "" || sess.CallID == "" {
		return
	}
	s.upstreamsMu.RLock()
	up, ok := s.upstreams[sess.UpstreamID]
	s.upstreamsMu.RUnlock()
	if !ok {
		return
	}

	deviceID := up.conf.LocalDeviceID
	if deviceID == "" {
		deviceID = s.conf.Serial
	}
	realm := up.conf.Realm
	if realm == "" {
		realm = s.conf.Realm
	}

	callId := sip.CallID(sess.CallID)
	userAgent := sip.UserAgentHeader(SipUserAgent)
	maxForwards := sip.MaxForwards(70)
	cseq := sip.CSeq{
		SeqNo:      uint32(time.Now().Unix() & 0x7fffffff),
		MethodName: sip.BYE,
	}

	fromAddr := sip.Address{
		Uri: &sip.SipUri{
			FUser: sip.String{Str: deviceID},
			FHost: realm,
			FPort: nil,
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: RandNumString(9)}),
	}
	toAddr := fromAddr

	req := sip.NewRequest(
		"",
		sip.BYE,
		toAddr.Uri,
		"SIP/2.0",
		[]sip.Header{
			fromAddr.AsFromHeader(),
			toAddr.AsToHeader(),
			&callId,
			&userAgent,
			&cseq,
			&maxForwards,
			fromAddr.AsContactHeader(),
		},
		"",
		nil,
	)
	req.SetTransport("udp")
	req.SetDestination(net.JoinHostPort(up.conf.SipIP, strconv.Itoa(int(up.conf.SipPort))))

	if _, err := s.upstreamSipSvr.RequestWithContext(context.Background(), req); err != nil {
		base.Log.Warnf("gb28181 upstream send BYE failed. upstream=%s callID=%s err=%+v", sess.UpstreamID, sess.CallID, err)
	}
}

// buildUpstreamKeepaliveRequest 构造向上级发送的 Keepalive MESSAGE。
// 按 GB28181 规范使用 MANSCDP Notify：CmdType=Keepalive, DeviceID=本节点在上级注册的ID。
func (s *GB28181Server) buildUpstreamKeepaliveRequest(up *upstreamServer) sip.Request {
	uc := up.conf

	deviceID := uc.LocalDeviceID
	if deviceID == "" {
		deviceID = s.conf.Serial
	}
	realm := uc.Realm
	if realm == "" {
		realm = s.conf.Realm
	}

	up.sn++

	// Keepalive Notify XML
	body := fmt.Sprintf(`<?xml version="1.0"?>
<Notify>
  <CmdType>Keepalive</CmdType>
  <SN>%d</SN>
  <DeviceID>%s</DeviceID>
  <Status>OK</Status>
</Notify>`, up.sn, deviceID)

	callId := sip.CallID(RandNumString(10))
	userAgent := sip.UserAgentHeader(SipUserAgent)
	maxForwards := sip.MaxForwards(70)
	cseq := sip.CSeq{
		SeqNo:      uint32(time.Now().Unix() & 0x7fffffff),
		MethodName: sip.MESSAGE,
	}

	fromAddr := sip.Address{
		Uri: &sip.SipUri{
			FUser: sip.String{Str: deviceID},
			FHost: realm,
			FPort: nil,
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: RandNumString(9)}),
	}
	toAddr := fromAddr

	req := sip.NewRequest(
		"",
		sip.MESSAGE,
		toAddr.Uri,
		"SIP/2.0",
		[]sip.Header{
			fromAddr.AsFromHeader(),
			toAddr.AsToHeader(),
			&callId,
			&userAgent,
			&cseq,
			&maxForwards,
			fromAddr.AsContactHeader(),
		},
		"",
		nil,
	)

	req.SetTransport("udp")
	req.SetDestination(net.JoinHostPort(uc.SipIP, strconv.Itoa(int(uc.SipPort))))

	contentType := sip.ContentType("Application/MANSCDP+xml")
	req.AppendHeader(&contentType)
	req.SetBody(body, true)

	return req
}

// sendUpstreamCatalog 在上级注册成功后，主动向上级平台上报一次 Catalog 信息（MESSAGE）。
// 这样即便上级暂未主动发起 Catalog 查询，也能立即拿到本节点对其开放的通道列表。
func (s *GB28181Server) sendUpstreamCatalog(up *upstreamServer) error {
	uc := up.conf

	deviceID := uc.LocalDeviceID
	if deviceID == "" {
		deviceID = s.conf.Serial
	}
	realm := uc.Realm
	if realm == "" {
		realm = s.conf.Realm
	}

	up.sn++

	body, err := s.buildUpstreamCatalogBody(uc.ID, up.sn, deviceID)
	if err != nil {
		return fmt.Errorf("build upstream catalog body failed: %w", err)
	}

	callId := sip.CallID(RandNumString(10))
	userAgent := sip.UserAgentHeader(SipUserAgent)
	maxForwards := sip.MaxForwards(70)
	cseq := sip.CSeq{
		SeqNo:      uint32(time.Now().Unix() & 0x7fffffff),
		MethodName: sip.MESSAGE,
	}

	fromAddr := sip.Address{
		Uri: &sip.SipUri{
			FUser: sip.String{Str: deviceID},
			FHost: realm,
			FPort: nil,
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: RandNumString(9)}),
	}
	toAddr := fromAddr

	req := sip.NewRequest(
		"",
		sip.MESSAGE,
		toAddr.Uri,
		"SIP/2.0",
		[]sip.Header{
			fromAddr.AsFromHeader(),
			toAddr.AsToHeader(),
			&callId,
			&userAgent,
			&cseq,
			&maxForwards,
			fromAddr.AsContactHeader(),
		},
		"",
		nil,
	)

	req.SetTransport("udp")
	req.SetDestination(net.JoinHostPort(uc.SipIP, strconv.Itoa(int(uc.SipPort))))

	contentType := sip.ContentType("Application/MANSCDP+xml")
	req.AppendHeader(&contentType)
	req.SetBody(body, true)

	resp, err := s.upstreamSipSvr.RequestWithContext(context.Background(), req)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("empty catalog response")
	}
	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("catalog notify failed, code=%d", resp.StatusCode())
	}
	return nil
}

const upstreamCatalogCacheTTL = 3 * time.Second

// buildUpstreamCatalogBody 为上级平台生成 Catalog 响应 XML。
// upstreamID 通常取 SIP From 中的 User 字段，要求与配置中的 Upstreams.ID 对齐。
// 若找不到对应上级，则返回所有上级的订阅通道列表。
func (s *GB28181Server) buildUpstreamCatalogBody(upstreamID string, sn int, deviceID string) (string, error) {
	var list []upstreamChannelInfo

	now := time.Now()

	// 辅助函数：根据 streamName 反查通道信息，构造 ChannelInfo。
	// 若未找到下级 GB28181 通道且提供了 fallbackChannelID，则构造一个“虚拟通道”，用于向上级暴露任意本地流。
	buildChannel := func(streamName, fallbackChannelID string) *upstreamChannelInfo {
		var chFound *Channel
		var dev *Device
		Devices.Range(func(_, dv any) bool {
			d := dv.(*Device)
			var hit bool
			d.channelMap.Range(func(_, cv any) bool {
				ch := cv.(*Channel)
				name := ch.MediaInfo.StreamName
				if name == "" && ch.StreamName != "" {
					name = ch.StreamName
				}
				if name == streamName {
					chFound = ch
					dev = d
					hit = true
					return false
				}
				return true
			})
			return !hit
		})
		if chFound == nil || dev == nil {
			// 非下级 GB28181 流：根据订阅中提供的 channel_id 构造一个虚拟通道。
			// 当前 streamName 在本地流列表中不存在时，根据“是否有实际媒体数据（pub/pull）”决定 ON/OFF。
			if fallbackChannelID == "" {
				return nil
			}
			var status ChannelStatus = ChannelOffStatus
			if s.hasActiveMediaStream(streamName) {
				status = ChannelOnStatus
			}
			return &upstreamChannelInfo{
				DeviceID:   fallbackChannelID,
				ParentID:   s.conf.Serial,
				Name:       streamName,
				Owner:      s.conf.Serial, // 归属平台自身
				CivilCode:  devIDFromSerial(s.conf.Serial),
				Status:     status,
				StreamName: streamName,
			}
		}

		// 基于下级真实通道构造对上级可见的目录元素。
		// 注意：返回给上级的平台的 ChannelID（DeviceID 字段）以订阅配置中的 channel_id 为准，
		// 这样无论是主动上报还是被动查询，看到的通道 ID 都与订阅保持一致。
		channelIDForUp := chFound.ChannelId
		if fallbackChannelID != "" {
			channelIDForUp = fallbackChannelID
		}
		ci := upstreamChannelInfo{
			DeviceID:     channelIDForUp,
			ParentID:     dev.ID,
			Name:         chFound.Name,
			Manufacturer: chFound.Manufacturer,
			Model:        chFound.Model,
			Owner:        chFound.Owner,
			CivilCode:    chFound.CivilCode,
			Address:      chFound.Address,
			Parental:     chFound.Parental,
			SafetyWay:    chFound.SafetyWay,
			RegisterWay:  chFound.RegisterWay,
			Secrecy:      chFound.Secrecy,
			Status:       chFound.Status,
			Longitude:    chFound.Longitude,
			Latitude:     chFound.Latitude,
		}

		// 为上级填充 StreamName：以订阅时配置的 streamName 为准，避免因下级内部映射差异导致状态不一致。
		ci.StreamName = streamName
		ci.IsInvite = chFound.MediaInfo.IsInvite
		if chFound.MediaInfo.Ssrc != 0 {
			ci.Ssrc = fmt.Sprintf("%d", chFound.MediaInfo.Ssrc)
		}
		ci.SinglePort = chFound.MediaInfo.SinglePort
		ci.DumpFileName = chFound.MediaInfo.DumpFileName
		// 结合设备在线状态 + 实际媒体是否在本地拉取，综合判断对上级的在线状态。
		// 1) 设备本身离线 => 一律 OFF；
		// 2) 设备在线：以 hasActiveMediaStream(streamName) + IsInvite 综合判断。
		hasActiveMedia := s.hasActiveMediaStream(streamName)
		// 如果当前通道处于 Invite 状态，也视为存在实时拉流需求。
		if chFound.MediaInfo.IsInvite {
			hasActiveMedia = true
		}
		if dev.Status == DeviceOnlineStatus && hasActiveMedia {
			ci.Status = ChannelOnStatus
		} else {
			ci.Status = ChannelOffStatus
		}
		// 若 Owner/CivilCode 为空，则回退使用平台默认值。
		if ci.Owner == "" {
			ci.Owner = s.conf.Serial
		}
		if ci.CivilCode == "" {
			ci.CivilCode = devIDFromSerial(s.conf.Serial)
		}
		return &ci
	}

	// 先确认上级是否存在
	s.upstreamsMu.RLock()
	up, ok := s.upstreams[upstreamID]
	s.upstreamsMu.RUnlock()
	if !ok {
		// 未找到对应上级，直接返回空列表。
		resp := catalogResponse{
			CmdType:    "Catalog",
			SN:         sn,
			DeviceID:   deviceID,
			SumNum:     0,
			DeviceList: nil,
		}
		return XmlEncode(&resp)
	}

	// 命中缓存则直接复用缓存中的列表，避免频繁遍历 Devices/channelMap。
	s.upstreamCatalogCacheMu.RLock()
	if s.upstreamCatalogCache != nil {
		if cached, ok2 := s.upstreamCatalogCache[upstreamID]; ok2 {
			if now.Sub(cached.updatedAt) < upstreamCatalogCacheTTL {
				list = append(list, cached.list...)
				s.upstreamCatalogCacheMu.RUnlock()
				resp := catalogResponse{
					CmdType:    "Catalog",
					SN:         sn,
					DeviceID:   deviceID,
					SumNum:     len(list),
					DeviceList: list,
				}
				return XmlEncode(&resp)
			}
		}
	}
	s.upstreamCatalogCacheMu.RUnlock()

	// 未命中缓存，重新根据订阅关系构造列表。
	seen := make(map[string]struct{})
	up.subs.Range(func(_, v any) bool {
		sub, ok2 := v.(*UpstreamStreamSub)
		if !ok2 || sub.StreamName == "" {
			return true
		}
		if _, exists := seen[sub.StreamName]; exists {
			return true
		}
		if ch := buildChannel(sub.StreamName, sub.ChannelID); ch != nil {
			list = append(list, *ch)
			seen[sub.StreamName] = struct{}{}
		}
		return true
	})

	// 更新缓存
	if len(list) > 0 {
		s.upstreamCatalogCacheMu.Lock()
		if s.upstreamCatalogCache == nil {
			s.upstreamCatalogCache = make(map[string]struct {
				list      []upstreamChannelInfo
				updatedAt time.Time
			})
		}
		s.upstreamCatalogCache[upstreamID] = struct {
			list      []upstreamChannelInfo
			updatedAt time.Time
		}{
			list:      append([]upstreamChannelInfo(nil), list...),
			updatedAt: now,
		}
		s.upstreamCatalogCacheMu.Unlock()
	}

	resp := catalogResponse{
		CmdType:    "Catalog",
		SN:         sn,
		DeviceID:   deviceID,
		SumNum:     len(list),
		DeviceList: list,
	}
	return XmlEncode(&resp)
}

// buildUpstreamDeviceInfoBody 构造作为“大设备”暴露给上级平台的 DeviceInfo 响应。
// Mode A：中间平台对上级抽象为一个逻辑设备，而不直接泄露所有下级设备细节。
func (s *GB28181Server) buildUpstreamDeviceInfoBody(sn int, deviceID string) string {
	type deviceInfoResp struct {
		XMLName      xml.Name `xml:"Response"`
		CmdType      string   `xml:"CmdType"`
		SN           int      `xml:"SN"`
		DeviceID     string   `xml:"DeviceID"`
		DeviceName   string   `xml:"DeviceName"`
		Manufacturer string   `xml:"Manufacturer"`
		Model        string   `xml:"Model"`
		Result       string   `xml:"Result"`
	}
	name := "LalServer-GB28181-Gateway"
	if s.conf.Serial != "" {
		name = s.conf.Serial
	}
	resp := deviceInfoResp{
		CmdType:      "DeviceInfo",
		SN:           sn,
		DeviceID:     deviceID,
		DeviceName:   name,
		Manufacturer: "LalServer",
		Model:        "GB28181-Gateway",
		Result:       "OK",
	}
	body, err := XmlEncode(&resp)
	if err != nil {
		// 兜底：出错时返回一个最简单的 XML，避免上级异常。
		return fmt.Sprintf(`<?xml version="1.0"?>
<Response>
  <CmdType>DeviceInfo</CmdType>
  <SN>%d</SN>
  <DeviceID>%s</DeviceID>
  <Result>OK</Result>
</Response>`, sn, deviceID)
	}
	return body
}

// getVirtualMediaServer 返回用于非 GB28181 流的虚拟 mediaserver（仅承载 upstreamSinks，不监听端口）。
func (s *GB28181Server) getVirtualMediaServer() *mediaserver.GB28181MediaServer {
	s.virtualMediaserverMu.Lock()
	defer s.virtualMediaserverMu.Unlock()
	if s.virtualMediaserver != nil {
		return s.virtualMediaserver
	}
	port := int(s.conf.MediaConfig.ListenPort)
	if port == 0 {
		port = 30000
	}
	s.virtualMediaserver = mediaserver.NewGB28181MediaServer(port, virtualMediaKey, s, s.lalServer)
	s.MediaServerMap.Store(virtualMediaKey, s.virtualMediaserver)
	return s.virtualMediaserver
}

// maybeCleanupVirtualMediaServer 在没有任何上级会话使用虚拟 mediaserver 时，清理其索引并释放资源。
// 仅在上级会话（包括非 GB 流转推）全部结束后调用，避免影响原始流。
func (s *GB28181Server) maybeCleanupVirtualMediaServer() {
	s.virtualMediaserverMu.Lock()
	defer s.virtualMediaserverMu.Unlock()
	vm := s.virtualMediaserver
	if vm == nil {
		return
	}

	// 若仍有会话引用虚拟 mediaserver，则不清理。
	hasVirtualSession := false
	s.upstreamSessions.Range(func(_, v any) bool {
		if sess, ok := v.(*UpstreamSession); ok && sess.MediaKey == virtualMediaKey {
			hasVirtualSession = true
			return false
		}
		return true
	})
	if hasVirtualSession {
		return
	}

	// 清理 streamMediasvrIndex 中指向虚拟 mediaserver 的条目。
	s.streamMediasvrIndexMu.Lock()
	for name, ms := range s.streamMediasvrIndex {
		if ms == vm {
			delete(s.streamMediasvrIndex, name)
		}
	}
	s.streamMediasvrIndexMu.Unlock()

	// 从 MediaServerMap 中移除并释放虚拟 mediaserver 资源。
	s.MediaServerMap.Delete(virtualMediaKey)
	// 先清空所有上级 sink（关闭 UDP 连接），再释放实例。
	vm.ClearAllUpstreamSinks()
	vm.Dispose()
	s.virtualMediaserver = nil
}

// initUpstreams 根据配置初始化上级平台结构。
// 目前仅构建内存结构与订阅表，注册/心跳及 Invite 逻辑后续补充。
func (s *GB28181Server) initUpstreams() {
	s.upstreamsMu.Lock()
	defer s.upstreamsMu.Unlock()

	if s.upstreams == nil {
		s.upstreams = make(map[string]*upstreamServer)
	}
	if s.upstreamsByIP == nil {
		s.upstreamsByIP = make(map[string]string)
	} else {
		// 保留现有上级的同时重建 IP 索引。
		s.upstreamsByIPMu.Lock()
		s.upstreamsByIP = make(map[string]string)
		s.upstreamsByIPMu.Unlock()
	}

	for _, uc := range s.conf.Upstreams {
		if !uc.Enable || uc.ID == "" {
			continue
		}
		if up, ok := s.upstreams[uc.ID]; ok {
			// 已存在的上级仅更新配置，不重复启动循环。
			up.conf = uc
		} else {
			up = &upstreamServer{
				conf:   uc,
				stopCh: make(chan struct{}),
			}
			s.upstreams[uc.ID] = up
			// 启动上级注册/心跳循环
			go s.runUpstreamLoop(up)
		}
		if uc.SipIP != "" {
			s.upstreamsByIP[uc.SipIP] = uc.ID
		}
	}
}

// ReloadUpstreams 仅根据新的上级配置刷新 upstreams，不影响下级 GB28181 设备接入。
// - 新增的上级：创建 upstreamServer 并启动 runUpstreamLoop；
// - 已存在的上级：仅更新 conf；
// - 被删除的上级：发 stop 信号结束其 runUpstreamLoop，并从映射表中移除。
func (s *GB28181Server) ReloadUpstreams(newConfs []GB28181UpstreamConfig) {
	s.upstreamsMu.Lock()
	defer s.upstreamsMu.Unlock()

	old := s.upstreams
	if old == nil {
		old = make(map[string]*upstreamServer)
	}
	s.upstreams = make(map[string]*upstreamServer)

	// 重建 IP 索引
	s.upstreamsByIPMu.Lock()
	s.upstreamsByIP = make(map[string]string)
	s.upstreamsByIPMu.Unlock()

	for _, uc := range newConfs {
		if !uc.Enable || uc.ID == "" {
			continue
		}
		if upOld, ok := old[uc.ID]; ok {
			upOld.conf = uc
			s.upstreams[uc.ID] = upOld
		} else {
			up := &upstreamServer{
				conf:   uc,
				stopCh: make(chan struct{}),
			}
			s.upstreams[uc.ID] = up
			go s.runUpstreamLoop(up)
		}
		if uc.SipIP != "" {
			s.upstreamsByIPMu.Lock()
			s.upstreamsByIP[uc.SipIP] = uc.ID
			s.upstreamsByIPMu.Unlock()
		}
	}

	// 对于不再存在的上级，发 stop 信号结束其循环。
	for id, up := range old {
		if _, ok := s.upstreams[id]; !ok {
			// 清理该上级下所有上级播放会话和转发 Sink。
			s.cleanupUpstreamSessionsForUpstream(id, true)
			if up.stopCh != nil {
				close(up.stopCh)
			}
		}
	}
}

// BrutalReloadUpstreams 使用“简单粗暴”的方式重载上级配置：
// - 清理所有上级相关运行时资源（会话、sink、虚拟 mediaserver、缓存、索引、上级循环）；
// - 再按 newConfs 重建上级平台运行时结构并启动循环。
// 该方法不会影响下级 GB28181 设备接入侧（sip_port 等）。
func (s *GB28181Server) BrutalReloadUpstreams(newConfs []GB28181UpstreamConfig) {
	// 1) 停止并清理所有上级播放会话（不等待、尽量不阻塞）
	s.upstreamSessions.Range(func(key, value any) bool {
		sess, ok := value.(*UpstreamSession)
		if !ok {
			s.upstreamSessions.Delete(key)
			return true
		}
		// 不发送 BYE：粗暴重载以快速清理为主，避免因网络不可达阻塞或拖慢重载。
		if sess.CancelFeed != nil {
			sess.CancelFeed()
			sess.CancelFeed = nil
		}
		if sess.MediaKey != "" && sess.SinkID != "" {
			if mv, ok2 := s.MediaServerMap.Load(sess.MediaKey); ok2 {
				if mediasvr, ok3 := mv.(*mediaserver.GB28181MediaServer); ok3 {
					mediasvr.RemoveUpstreamSink(sess.SinkID)
				}
			}
		}
		s.upstreamSessions.Delete(key)
		return true
	})
	// 兜底清场：即便会话表与 sink 状态不同步，也要清空所有 mediaserver 中的 upstreamSinks，避免残留转推连接。
	s.MediaServerMap.Range(func(_, v any) bool {
		if ms, ok := v.(*mediaserver.GB28181MediaServer); ok && ms != nil {
			ms.ClearAllUpstreamSinks()
		}
		return true
	})
	// 清理按通道索引
	s.upstreamSessionsByChannelMu.Lock()
	s.upstreamSessionsByChannel = make(map[string]map[string]*UpstreamSession)
	s.upstreamSessionsByChannelMu.Unlock()
	// 清理目录缓存
	s.upstreamCatalogCacheMu.Lock()
	s.upstreamCatalogCache = make(map[string]struct {
		list      []upstreamChannelInfo
		updatedAt time.Time
	})
	s.upstreamCatalogCacheMu.Unlock()

	// 2) 停止所有上级循环并清空 upstreams / upstreamsByIP
	s.upstreamsMu.Lock()
	old := s.upstreams
	s.upstreams = make(map[string]*upstreamServer)
	s.upstreamsMu.Unlock()

	// 重建 IP 索引（直接清空）
	s.upstreamsByIPMu.Lock()
	s.upstreamsByIP = make(map[string]string)
	s.upstreamsByIPMu.Unlock()

	// 停止旧上级循环
	for _, up := range old {
		if up == nil || up.stopCh == nil {
			continue
		}
		func() {
			defer func() {
				_ = recover()
			}()
			close(up.stopCh)
		}()
	}

	// 3) 清理虚拟 mediaserver（此时已无会话引用，可安全销毁）
	s.maybeCleanupVirtualMediaServer()

	// 4) 按新配置重建上级运行时并启动循环
	s.upstreamsMu.Lock()
	if s.upstreams == nil {
		s.upstreams = make(map[string]*upstreamServer)
	}
	for _, uc := range newConfs {
		if !uc.Enable || uc.ID == "" {
			continue
		}
		up := &upstreamServer{
			conf:   uc,
			stopCh: make(chan struct{}),
		}
		s.upstreams[uc.ID] = up
		go s.runUpstreamLoop(up)
		if uc.SipIP != "" {
			s.upstreamsByIPMu.Lock()
			s.upstreamsByIP[uc.SipIP] = uc.ID
			s.upstreamsByIPMu.Unlock()
		}
	}
	s.upstreamsMu.Unlock()
}
func (s *GB28181Server) newSipServer(network string, port uint16) gosip.Server {
	srvConf := gosip.ServerConfig{}

	if s.conf.SipIP != "" {
		srvConf.Host = s.conf.SipIP
	}
	sipSvr := gosip.NewServer(srvConf, nil, nil, logger)
	sipSvr.OnRequest(sip.REGISTER, s.OnRegister)
	sipSvr.OnRequest(sip.MESSAGE, s.OnMessage)
	sipSvr.OnRequest(sip.NOTIFY, s.OnNotify)
	sipSvr.OnRequest(sip.BYE, s.OnBye)

	addr := s.conf.ListenAddr + ":" + strconv.Itoa(int(port))
	err := sipSvr.Listen(network, addr)
	if err != nil {
		base.Log.Fatalf("%+v", err)
	}

	base.Log.Info(" start sip server listen. addr= " + addr + "  network:" + network)
	return sipSvr
}

// newUpstreamSipServer 创建仅用于上级 GB28181 平台交互的 SIP Server，监听 upstream_sip_port。
// 目前仅使用 UDP 协议，挂载上级专用的 Handler（后续可根据需要扩展）。
func (s *GB28181Server) newUpstreamSipServer(network string, port uint16) gosip.Server {
	srvConf := gosip.ServerConfig{}
	if s.conf.SipIP != "" {
		srvConf.Host = s.conf.SipIP
	}
	sipSvr := gosip.NewServer(srvConf, nil, nil, logger)

	// 上级侧的 REGISTER/MESSAGE/INVITE/BYE 入口。
	// 目前 REGISTER 主要由自定义 UDP 客户端处理，这里的 OnUpstreamRegister 作为兜底或后续扩展。
	sipSvr.OnRequest(sip.REGISTER, s.OnUpstreamRegister)
	sipSvr.OnRequest(sip.MESSAGE, s.OnUpstreamMessage)
	sipSvr.OnRequest(sip.INVITE, s.OnUpstreamInvite)
	sipSvr.OnRequest(sip.BYE, s.OnUpstreamBye)

	addr := s.conf.ListenAddr + ":" + strconv.Itoa(int(port))
	if err := sipSvr.Listen(network, addr); err != nil {
		base.Log.Fatalf("%+v", err)
	}
	base.Log.Info(" start upstream sip server listen. addr= " + addr + "  network:" + network)
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
			if s.upstreamSipSvr != nil {
				s.upstreamSipSvr.Shutdown()
			}
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
		// 主动 Stop 时移除该流的断线恢复任务，避免关闭后仍被重试
		if StreamName != "" {
			s.recoveryPending.Delete(StreamName)
		}
		if singlePort {
			mediasvr.CloseConn(StreamName)
		} else {
			mediasvr.Dispose()
		}
	}
	return nil
}

// OnStreamActive 由 mediaserver.Conn 在首次建立指定 streamName 的媒体连接时回调。
// 这里通过 mediaKey 反查 mediaserver，并维护 streamName -> mediaserver 的快速索引。
func (s *GB28181Server) OnStreamActive(streamName string, mediaKey string) {
	if streamName == "" || mediaKey == "" {
		return
	}
	v, ok := s.MediaServerMap.Load(mediaKey)
	if !ok {
		return
	}
	ms, ok := v.(*mediaserver.GB28181MediaServer)
	if !ok {
		return
	}
	s.streamMediasvrIndexMu.Lock()
	s.streamMediasvrIndex[streamName] = ms
	s.streamMediasvrIndexMu.Unlock()
}

// OnStreamInactive 由 mediaserver.Conn 在 streamName 对应的连接关闭时回调，移除索引。
func (s *GB28181Server) OnStreamInactive(streamName string, mediaKey string) {
	if streamName == "" {
		return
	}
	s.streamMediasvrIndexMu.Lock()
	delete(s.streamMediasvrIndex, streamName)
	s.streamMediasvrIndexMu.Unlock()
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
	var chFound *Channel
	Devices.Range(func(_, value any) bool {
		d := value.(*Device)
		d.channelMap.Range(func(key, value any) bool {
			ch := value.(*Channel)
			if ch.MediaInfo.StreamName == streamName {
				chFound = ch
				// 断线重试：仅对实时点播、且启用自动重试时登记恢复任务（回放不自动重试）
				if s.conf.AutoRetryOnDisconnect && s.conf.RetryMaxCount != 0 && ch.playInfo != nil {
					if _, inPlayback := s.playbackSessions.Load(streamName); !inPlayback {
						firstDelay := time.Duration(s.conf.RetryFirstDelayMs) * time.Millisecond
						if firstDelay <= 0 {
							firstDelay = 3 * time.Second
						}
						s.recoveryPending.Store(streamName, &streamRecovery{
							StreamName:  streamName,
							PlayInfo:    *ch.playInfo,
							RetryCount:  0,
							NextRetryAt: time.Now().Add(firstDelay),
						})
						base.Log.Infof("gb28181 stream disconnect, schedule retry. streamName=%s firstRetryIn=%v maxRetry=%d",
							streamName, firstDelay, s.conf.RetryMaxCount)
					}
				}
				if ch.MediaInfo.IsInvite {
					ch.Bye(streamName)
				}
				ch.MediaInfo.Clear()
				ok = true
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
	_ = chFound
}

func (s *GB28181Server) startJob() {
	statusTick := time.NewTicker(s.HeartbeatInterval / 2)
	banTick := time.NewTicker(s.RemoveBanInterval)
	recoveryTick := time.NewTicker(2 * time.Second)
	// 下级设备目录（Catalog）定时查询。
	// 若未在配置中显式设置，默认每 180 秒查询一次。
	catalogIntervalSec := s.conf.CatalogQueryInterval
	if catalogIntervalSec <= 0 {
		catalogIntervalSec = 180
	}
	catalogTick := time.NewTicker(time.Duration(catalogIntervalSec) * time.Second)
	for {
		select {
		case <-banTick.C:
			if s.conf.Username != "" || s.conf.Password != "" {
				s.removeBanDevice()
			}
		case <-statusTick.C:
			s.statusCheck()
			s.clearExpiredPlaybackSessions()
		case <-recoveryTick.C:
			s.processRecoveryPending()
		case <-catalogTick.C:
			// 定时触发下级 Catalog 同步（包含间隔保护，避免过于频繁）。
			s.GetAllSyncChannels()
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

// processRecoveryPending 处理断线待恢复的实时点播：到点则重新 Invite，成功则移除，失败则退避后再次重试。
func (s *GB28181Server) processRecoveryPending() {
	if !s.conf.AutoRetryOnDisconnect || s.conf.RetryMaxCount == 0 {
		return
	}
	now := time.Now()
	s.recoveryPending.Range(func(key, value any) bool {
		streamName, _ := key.(string)
		rec, _ := value.(*streamRecovery)
		if rec == nil || now.Before(rec.NextRetryAt) {
			return true
		}
		ch := s.FindChannel(rec.PlayInfo.DeviceId, rec.PlayInfo.ChannelId)
		if ch == nil {
			base.Log.Warnf("gb28181 retry: channel not found, remove recovery. streamName=%s deviceId=%s channelId=%s",
				streamName, rec.PlayInfo.DeviceId, rec.PlayInfo.ChannelId)
			s.recoveryPending.Delete(streamName)
			return true
		}
		opt := &InviteOptions{}
		code, err := ch.Invite(opt, rec.StreamName, &rec.PlayInfo, &s.conf)
		if code == http.StatusOK && err == nil {
			s.recoveryPending.Delete(streamName)
			base.Log.Infof("gb28181 retry success. streamName=%s", streamName)
			return true
		}
		rec.RetryCount++
		backoffMs := s.conf.RetryFirstDelayMs
		for i := 0; i < rec.RetryCount && i < 10; i++ {
			backoffMs *= 2
		}
		if maxMs := s.conf.RetryMaxDelayMs; maxMs > 0 && backoffMs > maxMs {
			backoffMs = maxMs
		}
		rec.NextRetryAt = now.Add(time.Duration(backoffMs) * time.Millisecond)
		s.recoveryPending.Store(streamName, rec)
		if s.conf.RetryMaxCount > 0 && rec.RetryCount >= s.conf.RetryMaxCount {
			base.Log.Warnf("gb28181 retry limit reached, remove recovery. streamName=%s retries=%d err=%v",
				streamName, rec.RetryCount, err)
			s.recoveryPending.Delete(streamName)
			return true
		}
		base.Log.Infof("gb28181 retry failed, will retry again. streamName=%s retries=%d nextIn=%v err=%v",
			streamName, rec.RetryCount, time.Duration(backoffMs)*time.Millisecond, err)
		return true
	})
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
	CallId     string
	MediaKey   string
	StartTime  string
}

// GetAllStreams 返回当前已邀请（正在拉流）的通道列表
//
// 优先使用 Channel.MediaInfo.StreamName；为兼容个别路径下 MediaInfo 被清理但 playInfo 仍存在的情况，
// 当 MediaInfo.StreamName 为空、但 playInfo 非空时，回退使用 playInfo.StreamName，避免 Active Streams 漏报。
func (s *GB28181Server) GetAllStreams() (list []StreamInfo) {
	list = make([]StreamInfo, 0)
	Devices.Range(func(_, value any) bool {
		d := value.(*Device)
		d.channelMap.Range(func(_, value any) bool {
			ch := value.(*Channel)
			streamName := ch.MediaInfo.StreamName
			if streamName == "" && ch.playInfo != nil {
				streamName = ch.playInfo.StreamName
			}
			if streamName != "" {
				list = append(list, StreamInfo{
					DeviceId:   d.ID,
					ChannelId:  ch.ChannelId,
					StreamName: streamName,
					CallId:     ch.GetCallId(),
					MediaKey:   ch.MediaInfo.MediaKey,
					StartTime:  ch.getStartTime(),
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
			case "Catalog":
				// 设备未在 Devices 中，但上报了 Catalog（例如服务端重启或设备长时间超时被删除后，
				// 仍对之前的 Catalog 请求进行响应）。在 QuickLogin 场景下，自动创建设备并更新通道列表，
				// 避免 Channels 信息丢失、页面显示为 0。
				d := s.StoreDevice(id, req)
				d.UpdateTime = time.Now()
				if len(temp.DeviceList) > 0 {
					d.UpdateChannels(temp.DeviceList...)
				}
				tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", ""))
				return
			}
		}

		base.Log.Warn("Unauthorized message, device not found, id:", id)
		// 作为中间平台，大设备视角处理中枢 GB28181 消息（Mode A）。
		deviceID := temp.DeviceID
		if deviceID == "" {
			deviceID = s.conf.Serial
		}
		switch temp.CmdType {
		case "Catalog":
			// 上级平台发起的 Catalog 查询：返回该上级已订阅的通道列表。
			body, buildErr := s.buildUpstreamCatalogBody(id, temp.SN, deviceID)
			if buildErr != nil {
				base.Log.Errorf("build upstream catalog body failed. from=%s err=%+v", id, buildErr)
				tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "build catalog failed", ""))
				return
			}
			resp := sip.NewResponseFromRequest("", req, http.StatusOK, "OK", body)
			tx.Respond(resp)
			return
		case "DeviceInfo":
			// 上级查询本平台（大设备）的设备信息。
			body := s.buildUpstreamDeviceInfoBody(temp.SN, deviceID)
			resp := sip.NewResponseFromRequest("", req, http.StatusOK, "OK", body)
			tx.Respond(resp)
			return
		default:
			// 其他 CmdType 暂不支持，保持兼容返回 400。
			tx.Respond(sip.NewResponseFromRequest("", req, http.StatusBadRequest, "device not found", ""))
			return
		}
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

	// 优先判断是否为上级 Bye（停止上级播放）
	if v, ok := s.upstreamSessions.LoadAndDelete(callIdStr); ok {
		if sess, ok2 := v.(*UpstreamSession); ok2 {
			if sess.CancelFeed != nil {
				sess.CancelFeed()
				sess.CancelFeed = nil
			}
			if sess.SinkID == "" {
				tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", ""))
				return
			}
			var mediasvr *mediaserver.GB28181MediaServer
			// 优先使用会话中记录的 MediaKey 精确定位 mediaserver
			if sess.MediaKey != "" {
				if v2, ok2 := s.MediaServerMap.Load(sess.MediaKey); ok2 {
					mediasvr = v2.(*mediaserver.GB28181MediaServer)
				}
			}
			// 回退：根据当前通道的 MediaKey 查找（若会话中未记录）
			if mediasvr == nil && sess.DeviceID != "" {
				if ch := s.FindChannel(sess.DeviceID, sess.DeviceID); ch != nil && ch.MediaInfo.MediaKey != "" {
					if v2, ok2 := s.MediaServerMap.Load(ch.MediaInfo.MediaKey); ok2 {
						mediasvr = v2.(*mediaserver.GB28181MediaServer)
					}
				}
			}
			if mediasvr != nil {
				mediasvr.RemoveUpstreamSink(sess.SinkID)
			}
		}
		tx.Respond(sip.NewResponseFromRequest("", req, http.StatusOK, "OK", ""))
		return
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

// cleanupUpstreamSessionForChannel 清理指定上级平台对某一路通道的现有上级会话和转发 Sink。
// 用于保证“同一上级对同一 channel 多次 INVITE 时，仅保留最后一次转发”。
func (s *GB28181Server) cleanupUpstreamSessionForChannel(upstreamID, channelID string) {
	if upstreamID == "" || channelID == "" {
		return
	}
	s.upstreamSessions.Range(func(key, value any) bool {
		sess, ok := value.(*UpstreamSession)
		if !ok {
			return true
		}
		if sess.UpstreamID != upstreamID || sess.DeviceID != channelID {
			return true
		}

		// 同一上级+通道重复 INVITE 时，先主动向上级发送 BYE，结束上一条会话。
		s.sendUpstreamBye(sess)

		if sess.CancelFeed != nil {
			sess.CancelFeed()
			sess.CancelFeed = nil
		}
		if sess.MediaKey != "" && sess.SinkID != "" {
			if v, ok2 := s.MediaServerMap.Load(sess.MediaKey); ok2 {
				if mediasvr, ok3 := v.(*mediaserver.GB28181MediaServer); ok3 {
					mediasvr.RemoveUpstreamSink(sess.SinkID)
				}
			}
		}

		s.upstreamSessions.Delete(key)
		// 某一路通道的会话被清理后，若虚拟 mediaserver 已无会话引用，则尝试一起清理。
		s.maybeCleanupVirtualMediaServer()
		return true
	})
}

// cleanupUpstreamSessionsForUpstream 清理指定上级平台下所有上级播放会话和转发 Sink。
// 当上级配置被删除或重启时，用于优雅结束现有播放转发，避免上级继续推流到无效会话。
func (s *GB28181Server) cleanupUpstreamSessionsForUpstream(upstreamID string, sendBye bool) {
	if upstreamID == "" {
		return
	}
	s.upstreamSessions.Range(func(key, value any) bool {
		sess, ok := value.(*UpstreamSession)
		if !ok || sess.UpstreamID != upstreamID {
			return true
		}

		// 可选：先向上级发送 BYE，结束 SIP 会话。
		if sendBye {
			s.sendUpstreamBye(sess)
		}

		if sess.CancelFeed != nil {
			sess.CancelFeed()
			sess.CancelFeed = nil
		}
		if sess.MediaKey != "" && sess.SinkID != "" {
			if v, ok2 := s.MediaServerMap.Load(sess.MediaKey); ok2 {
				if mediasvr, ok3 := v.(*mediaserver.GB28181MediaServer); ok3 {
					mediasvr.RemoveUpstreamSink(sess.SinkID)
				}
			}
		}

		s.upstreamSessions.Delete(key)
		return true
	})
	// 所属上级下的所有会话被清理后，若虚拟 mediaserver 已无会话引用，则尝试一起清理。
	s.maybeCleanupVirtualMediaServer()
}

// reconcileUpstreamSessionsWithSubs 在订阅关系发生变化后，对当前所有上级播放会话进行对齐：
// - 若某会话对应的 (UpstreamID, DeviceID, StreamName) 不再出现在订阅表中，则主动结束该会话并关闭转发。
func (s *GB28181Server) reconcileUpstreamSessionsWithSubs() {
	s.upstreamSessions.Range(func(key, value any) bool {
		sess, ok := value.(*UpstreamSession)
		if !ok {
			return true
		}
		upID := sess.UpstreamID
		channelID := sess.DeviceID
		streamName := sess.StreamName
		if upID == "" || channelID == "" || streamName == "" {
			return true
		}

		// 检查当前会话是否仍被订阅允许。
		allowed := false
		s.upstreamsMu.RLock()
		up, ok2 := s.upstreams[upID]
		s.upstreamsMu.RUnlock()
		if ok2 && up != nil {
			up.subs.Range(func(_, v any) bool {
				if sub, ok3 := v.(*UpstreamStreamSub); ok3 {
					if sub.StreamName == streamName && sub.ChannelID == channelID {
						allowed = true
						return false
					}
				}
				return true
			})
		}

		if !allowed {
			s.sendUpstreamBye(sess)
			if sess.CancelFeed != nil {
				sess.CancelFeed()
				sess.CancelFeed = nil
			}
			if sess.MediaKey != "" && sess.SinkID != "" {
				if v, ok2 := s.MediaServerMap.Load(sess.MediaKey); ok2 {
					if mediasvr, ok3 := v.(*mediaserver.GB28181MediaServer); ok3 {
						mediasvr.RemoveUpstreamSink(sess.SinkID)
					}
				}
			}
			s.upstreamSessions.Delete(key)
		}
		return true
	})
	// 订阅对齐后，若虚拟 mediaserver 已无会话引用，则尝试一起清理。
	s.maybeCleanupVirtualMediaServer()
}

// ReconcileUpstreamSessionsWithSubs 为导出封装，供 logic 层调用，用于在订阅关系重载后对正在转发的会话做一次对齐。
func (s *GB28181Server) ReconcileUpstreamSessionsWithSubs() {
	s.reconcileUpstreamSessionsWithSubs()
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
