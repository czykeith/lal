package mediaserver

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/q191201771/lal/pkg/avc"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/gb28181/mpegps"
	"github.com/q191201771/lal/pkg/rtprtcp"
	"github.com/q191201771/lal/pkg/rtsp"
)

var (
	ErrInvalidPsData = errors.New("invalid mpegps data")
	ErrInvalidSsrc   = errors.New("invalid ssrc")
)

type Frame struct {
	buffer  *bytes.Buffer
	pts     uint64
	dts     uint64
	initPts uint64
	initDts uint64
}

// CustomizePubSession 是对 logic.CustomizePubSessionContext 的最小接口抽象，避免直接依赖 logic 包。
type CustomizePubSession interface {
	WithOption(modOption func(option *base.AvPacketStreamOption))
	FeedAvPacket(packet base.AvPacket) error
}

// ILalServer 是对 logic.ILalServer 的最小接口抽象，仅包含 GB28181 使用到的能力。
type ILalServer interface {
	AddCustomizePubSession(streamName string) (CustomizePubSession, error)
	DelCustomizePubSession(session CustomizePubSession)
	// StatGroup 用于判断某个 streamName 是否在逻辑层存在且处于在线状态（有 pub）。
	StatGroup(streamName string) *base.StatGroup
}

type Conn struct {
	conn         net.Conn
	r            io.Reader
	check        bool
	demuxer      *mpegps.PsDemuxer
	avcUnpacker  rtprtcp.IRtpUnpacker
	hevcUnpacker rtprtcp.IRtpUnpacker
	streamName   string
	connKey      string
	lalServer    ILalServer
	lalSession   CustomizePubSession
	videoFrame   Frame
	audioFrame   Frame

	observer IGbObserver

	rtpPts         uint64
	psPtsZeroTimes int64

	psDumpFile *base.DumpFile

	buffer *bytes.Buffer
	key    string

	mediaServer *GB28181MediaServer
	one         sync.Once
	oneSaveConn sync.Once

	// scaleQueue 复用 RTSP 侧的 AvPacketQueue 变速算法，根据回放倍速对时间戳进行适配。
	scaleQueue *rtsp.AvPacketQueue
	hasAudio   bool
	hasVideo   bool

	// hevcFallbackLogAt 限制“99 payload type 但兜底为 AVC”日志刷屏。
	hevcFallbackLogAt time.Time

	// psFormatLastUpdated 记录 PS 格式缓存的最近一次刷新时间，用于避免 GetStreamFormat TTL 误清理。
	// 避免在高帧率下对同一 streamName 反复调用 SetStreamFormat，但仍需要定期刷新 UpdatedAt，
	// 否则 GetStreamFormat 的 TTL 可能导致“流还在却格式缓存被清理”的问题。
	psFormatLastUpdated time.Time
}

func NewConn(conn net.Conn, observer IGbObserver, lal ILalServer) *Conn {
	c := &Conn{
		conn:      conn,
		r:         conn,
		demuxer:   mpegps.NewPsDemuxer(),
		observer:  observer,
		lalServer: lal,
		buffer:    bytes.NewBuffer(nil),
	}
	if conn != nil && conn.RemoteAddr() != nil {
		c.connKey = conn.RemoteAddr().String()
	}

	c.demuxer.OnFrame = c.OnFrame

	return c
}
func (c *Conn) StreamName() string {
	return c.streamName
}
func (c *Conn) SetMediaServer(mediaServer *GB28181MediaServer) {
	c.mediaServer = mediaServer
}
func (c *Conn) SetKey(key string) {
	c.key = key
}
func (c *Conn) Serve() (err error) {
	defer func() {
		if r := recover(); r != nil {
			base.Log.Errorf("gb28181 conn Serve panic recovered, streamName=%s connKey=%s panic=%v", c.streamName, c.connKey, r)
			err = fmt.Errorf("panic recovered: %v", r)
		}
	}()
	defer func() {
		// 频繁的无效 SSRC 多数来自端口扫描或非预期设备推流，降噪避免刷屏。
		if err != nil && errors.Is(err, ErrInvalidSsrc) {
			base.Log.Debug("conn close, err:", err)
		} else {
			base.Log.Info("conn close, err:", err)
		}
		c.Close()

		if c.observer != nil {
			if c.streamName != "" {
				c.observer.NotifyClose(c.streamName)
				if c.key != "" {
					c.observer.OnStreamInactive(c.streamName, c.key)
				}
			}
		}
		if c.psDumpFile != nil {
			c.psDumpFile.Close()
		}
		if c.lalSession != nil {
			c.lalServer.DelCustomizePubSession(c.lalSession)
		}
	}()

	// 单端口模式下 UDP 端口可能会被外部探测，连接建立日志容易刷屏；改为 Debug。
	base.Log.Debug("gb28181 conn, remoteaddr:", c.conn.RemoteAddr().String(), " localaddr:", c.conn.LocalAddr().String())

	// GB28181 设备/网络可能数秒无包（切换码流、短暂丢包）；10s 过短会误杀连接并触发 replace，收流抖动。
	// 用较长超时，真正断流再由设备 BYE 或业务 stop 关闭。
	const gb28181ReadTimeout = 60 * time.Second
	for {
		c.conn.SetReadDeadline(time.Now().Add(gb28181ReadTimeout))
		pkt := &rtp.Packet{}
		if c.conn.RemoteAddr().Network() == "udp" {
			buf := make([]byte, 1472*4)
			n, err := c.conn.Read(buf)
			if err != nil {
				base.Log.Error("conn read failed, err:", err)
				return err
			}

			err = pkt.Unmarshal(buf[:n])
			if err != nil {
				return err
			}
		} else {
			len := make([]byte, 2)
			_, err := io.ReadFull(c.r, len)
			if err != nil {
				return err
			}

			size := binary.BigEndian.Uint16(len)
			buf := make([]byte, size)
			_, err = io.ReadFull(c.r, buf)
			if err != nil {
				return err
			}

			err = pkt.Unmarshal(buf)
			if err != nil {
				return err
			}
		}

		if !c.check && c.observer != nil {
			var mediaInfo *MediaInfo
			var ok bool
			if pkt.SSRC != 0 {
				// 先按 SSRC 精确匹配
				mediaInfo, ok = c.observer.CheckSsrc(pkt.SSRC)
				if !ok {
					// 兼容部分设备：INVITE 时未约定/未记录 SSRC，首个 RTP 包携带的 SSRC 才是真实值。
					// 此时按 mediaKey 兜底：如果找到 MediaInfo 且 Ssrc 仍为 0，则把当前 SSRC 绑定进去并视为有效。
					if miByKey, ok2 := c.observer.GetMediaInfoByKey(c.key); ok2 {
						if miByKey.Ssrc == 0 {
							// 同步更新上层索引，避免后续 conn/重连再次失败。
							c.observer.BindMediaKeySsrc(c.key, pkt.SSRC)
							miByKey.Ssrc = pkt.SSRC
							mediaInfo = miByKey
							ok = true
						}
					}
				}
				if !ok {
					// 降噪：未知 SSRC 直接关闭连接（多为扫描或非预期推流）
					base.Log.Debug("invalid ssrc:", pkt.SSRC, " remoteaddr:", c.conn.RemoteAddr().String(), " localaddr:", c.conn.LocalAddr().String())
					return fmt.Errorf("%w:%d", ErrInvalidSsrc, pkt.SSRC)
				}
			} else {
				mediaInfo, ok = c.observer.GetMediaInfoByKey(c.key)
				if !ok {
					base.Log.Error("get mediaInfo :", c.key)
					return fmt.Errorf("get mediaInfo:%d", c.key)
				}
			}
			c.check = true
			c.streamName = mediaInfo.StreamName
			// 同 streamName 已在 group 内存在 customize pub 时不替换；此处不再 CloseOtherConns，避免误关正在收流的第一路。
			c.oneSaveConn.Do(func() {
				if c.mediaServer != nil {
					// 按 remoteAddr 保存连接，避免同一 streamName 覆盖导致 stop(BYE) 关不掉旧连接。
					// stop 时将遍历并关闭所有 streamName 匹配的连接。
					c.mediaServer.conns.Store(c.connKey, c)
				}
				// 通知上层有新的 streamName 活跃，用于维护 streamName -> mediaserver 索引。
				if c.observer != nil && c.streamName != "" && c.key != "" {
					c.observer.OnStreamActive(c.streamName, c.key)
				}
			})
			if len(mediaInfo.DumpFileName) > 0 {
				c.psDumpFile = base.NewDumpFile()
				if err = c.psDumpFile.OpenToWrite(mediaInfo.DumpFileName); err != nil {
					base.Log.Errorf("gb con dump file:%s", err.Error())
				}
			}
			base.Log.Info("gb28181 ssrc check success, streamName:", c.streamName)

			session, err := c.lalServer.AddCustomizePubSession(mediaInfo.StreamName)
			if err != nil {
				// 同 streamName 已存在输入源时拒绝第二路，属预期，避免 ERROR 刷屏
				if errors.Is(err, base.ErrDupInStream) {
					base.Log.Debug("lal server AddCustomizePubSession skipped (dup in stream), streamName:", mediaInfo.StreamName)
				} else {
					base.Log.Error("lal server AddCustomizePubSession failed, err:", err)
				}
				return err
			}

			session.WithOption(func(option *base.AvPacketStreamOption) {
				option.VideoFormat = base.AvPacketStreamVideoFormatAnnexb
				option.AudioFormat = base.AvPacketStreamAudioFormatAdtsAac
			})

			c.lalSession = session
		}
		c.rtpPts = uint64(pkt.Header.Timestamp)

		// GB28181 下级常见两种承载：
		// 1) RTP 承载 PS（最常见，PS 内再封装 H264/H265/AAC/G711 等）
		// 2) RTP 直接承载 H264/H265（少数平台/设备或级联场景）
		//
		// 我们在接收端按负载/PT 判定并缓存 streamName->格式，供上级级联 INVITE 动态生成匹配 SDP，
		// 并在 ForwardRtp 转发时按上级期望重写 PT/SSRC。
		// PS 识别策略：
		// - 优先用 pack start code 快速判断（isPsPayload）
		// - 若 payload type=96（你下发 SDP 里对应 PS/90000），即使单包不一定以 pack start 开头，
		//   也先尝试走 PS demux，只有 demux 明确“不像 PS”（且非 NeedMore）时才回退到 AVC/HEVC。
		tryPs := isPsPayload(pkt.Payload) || pkt.PayloadType == 96
		if tryPs && c.demuxer != nil {
			acceptedAsPs := false
			var inputErr error
			if c.psDumpFile != nil {
				c.psDumpFile.WriteWithType(pkt.Payload, base.DumpTypePsRtpData)
			}
			if inputErr = c.demuxer.Input(pkt.Payload); inputErr != nil {
				var psErr mpegps.Error
				// NeedMore 表示这是“正常的分片”，可以继续当作 PS 累积解封装。
				if errors.As(inputErr, &psErr) {
					if psErr.NeedMore() {
						acceptedAsPs = true
					} else {
						// 若设备在 PT=96 宣称为 PS，但 demux 返回非 NeedMore 错误：
						// - 更像 H264：回退 AVC 解封装
						// - 更不像 H264：仍按 PS 继续（避免错误回退导致无法恢复）
						if pkt.PayloadType == 96 && looksLikeH264RtpPayload(pkt.Payload) {
							acceptedAsPs = false
						} else {
							acceptedAsPs = true
						}
					}
				}
			} else {
				acceptedAsPs = true
			}

			if acceptedAsPs {
				if c.mediaServer != nil && c.streamName != "" {
					// streamName->format 对上级 SDP 生成是“按需缓存”，不需要每包都覆盖 UpdatedAt。
					// 但仍要定期刷新 UpdatedAt，避免 GetStreamFormat TTL 清理导致级联 INVITE SDP 生成失败。
					// 这里以 30 秒刷新一次，平衡性能与稳定性（TTL=10min）。
					if c.psFormatLastUpdated.IsZero() || time.Since(c.psFormatLastUpdated) > 30*time.Second {
						c.mediaServer.SetStreamFormat(c.streamName, StreamPayloadFormatPs)
						c.psFormatLastUpdated = time.Now()
					}
					c.mediaServer.ForwardRtp(c.streamName, pkt)
				}
				continue
			}
			// 若未被接受为 PS，则继续按 PT 走后续 AVC/HEVC 解封装。
		}

		// 非 PS：按 RTP PayloadType 尝试解包 H264/H265。
		switch pkt.PayloadType {
		case 98: // channel.go 当前 INVITE SDP: a=rtpmap:98 H264/90000
			if c.mediaServer != nil && c.streamName != "" {
				c.mediaServer.SetStreamFormat(c.streamName, StreamPayloadFormatH264)
				c.mediaServer.ForwardRtp(c.streamName, pkt)
			}
			c.feedRtpAvc(*pkt)
		case 99: // channel.go 当前 INVITE SDP: a=rtpmap:99 H265/90000
			// 部分下游会“把 H264 包错误地标成 99”，导致 HEVC unpacker 报 unknown nalu type。
			// 这里按负载形态兜底：若更像 H264，则走 AVC 解封装器。
			if looksLikeH264RtpPayload(pkt.Payload) {
				// 每秒最多打一条，避免高码率设备导致日志刷屏。
				if c.hevcFallbackLogAt.IsZero() || time.Since(c.hevcFallbackLogAt) > time.Second {
					c.hevcFallbackLogAt = time.Now()
					base.Log.Warnf("gb28181 payload type=99 but looks like H264, fallback to AVC. streamName=%s remote=%s", c.streamName, c.connKey)
				}
				if c.mediaServer != nil && c.streamName != "" {
					c.mediaServer.SetStreamFormat(c.streamName, StreamPayloadFormatH264)
					c.mediaServer.ForwardRtp(c.streamName, pkt)
				}
				c.feedRtpAvc(*pkt)
			} else {
				if c.mediaServer != nil && c.streamName != "" {
					c.mediaServer.SetStreamFormat(c.streamName, StreamPayloadFormatH265)
					c.mediaServer.ForwardRtp(c.streamName, pkt)
				}
				c.feedRtpHevc(*pkt)
			}
		case 96: // 兼容：部分实现使用 96 表示 H264；若不是 PS 则按 H264 处理
			if c.mediaServer != nil && c.streamName != "" {
				c.mediaServer.SetStreamFormat(c.streamName, StreamPayloadFormatH264)
				c.mediaServer.ForwardRtp(c.streamName, pkt)
			}
			c.feedRtpAvc(*pkt)
		default:
			// 若上级存在 sink，也可以选择转发原包，但无法保证 SDP 匹配；这里先不转发未知格式。
			// 其它 PT 暂不支持
		}
	}
	return
}

func isPsPayload(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}
	// MPEG-PS pack start code: 0x000001BA
	return payload[0] == 0x00 && payload[1] == 0x00 && payload[2] == 0x01 && payload[3] == 0xBA
}

// looksLikeH264RtpPayload 做一个非常轻量的“形态判断”，用于应对部分设备把 H264
// 包错误地标成了我们期望的 H265 payload type（例如 payload type=99）。
// 这里只在兜底场景下使用，尽量避免误判导致走错解封装器。
func looksLikeH264RtpPayload(payload []byte) bool {
	// single NAL unit or FU-A both have meaningful nal_unit_type bits in the first/second byte.
	if len(payload) < 1 {
		return false
	}
	b0 := payload[0]
	// forbidden_zero_bit should be 0 for H264
	if (b0 & 0x80) != 0 {
		return false
	}
	nt := b0 & 0x1f // nal_unit_type (5 bits)
	// common single NALU types: 1..23 (24..31 reserved)
	if nt >= 1 && nt <= 23 {
		return true
	}
	// STAP-A
	if nt == 24 {
		return true
	}
	// FU-A indicator has nal_unit_type = 28
	if nt == 28 && len(payload) >= 2 {
		innerType := payload[1] & 0x1f
		return innerType >= 1 && innerType <= 23
	}
	return false
}

func (c *Conn) feedRtpAvc(pkt rtp.Packet) {
	if c.lalSession == nil {
		return
	}
	if c.avcUnpacker == nil {
		c.avcUnpacker = rtprtcp.DefaultRtpUnpackerFactory(base.AvPacketPtAvc, 90000, 1024, func(ap base.AvPacket) {
			annexb, err := avc.Avcc2Annexb(ap.Payload)
			if err != nil {
				return
			}
			ap.Payload = annexb
			ap.Pts = ap.Timestamp
			c.feedRtpVideoAvPacket(ap)
		})
	}
	h := rtprtcp.MakeDefaultRtpHeader()
	h.PacketType = uint8(pkt.PayloadType)
	h.Seq = pkt.SequenceNumber
	h.Timestamp = pkt.Timestamp
	h.Ssrc = pkt.SSRC
	h.Mark = boolToUint8(pkt.Marker)

	rp := rtprtcp.MakeRtpPacket(h, pkt.Payload)
	c.avcUnpacker.Feed(rp)
}

func (c *Conn) feedRtpHevc(pkt rtp.Packet) {
	if c.lalSession == nil {
		return
	}
	if c.hevcUnpacker == nil {
		c.hevcUnpacker = rtprtcp.DefaultRtpUnpackerFactory(base.AvPacketPtHevc, 90000, 1024, func(ap base.AvPacket) {
			// rtprtcp 解包器输出的是长度前缀格式（AVCC/HVCC 类似结构）；转成 AnnexB 供下游统一处理。
			annexb, err := avc.Avcc2Annexb(ap.Payload)
			if err != nil {
				return
			}
			ap.Payload = annexb
			ap.Pts = ap.Timestamp
			c.feedRtpVideoAvPacket(ap)
		})
	}
	h := rtprtcp.MakeDefaultRtpHeader()
	h.PacketType = uint8(pkt.PayloadType)
	h.Seq = pkt.SequenceNumber
	h.Timestamp = pkt.Timestamp
	h.Ssrc = pkt.SSRC
	h.Mark = boolToUint8(pkt.Marker)

	rp := rtprtcp.MakeRtpPacket(h, pkt.Payload)
	c.hevcUnpacker.Feed(rp)
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func (c *Conn) feedRtpVideoAvPacket(pkt base.AvPacket) {
	// rtprtcp 解包器回调的 Timestamp 单位为毫秒，这里保持与 PS→OnFrame 类似的“从0开始”时间戳。
	c.hasVideo = true
	ts := uint64(pkt.Timestamp)
	pts := uint64(pkt.Pts)
	if c.videoFrame.initDts == 0 {
		c.videoFrame.initDts = ts
	}
	if c.videoFrame.initPts == 0 {
		c.videoFrame.initPts = pts
	}

	pkt.Timestamp = int64(ts - c.videoFrame.initDts)
	pkt.Pts = int64(pts - c.videoFrame.initPts)
	c.feedAvPacketWithScale(pkt)
}

func (c *Conn) Demuxer(data []byte) error {
	c.buffer.Write(data)

	buf := c.buffer.Bytes()
	if len(buf) < 4 {
		return nil
	}

	if buf[0] != 0x00 && buf[1] != 0x00 && buf[2] != 0x01 && buf[3] != 0xBA {
		return ErrInvalidPsData
	}

	packets := splitPsPackets(buf)
	if len(packets) <= 1 {
		return nil
	}

	for i, packet := range packets {
		if i == len(packets)-1 {
			c.buffer = bytes.NewBuffer(packet)
			return nil
		}

		if c.demuxer != nil {
			c.demuxer.Input(packet)
		}
	}

	return nil
}

func (c *Conn) OnFrame(frame []byte, cid mpegps.PsStreamType, pts uint64, dts uint64) {
	payloadType := getPayloadType(cid)
	if payloadType == base.AvPacketPtUnknown {
		return
	}
	//当ps流解析出pts为0时，计数超过10则用rtp的时间戳
	if pts == 0 {
		if c.psPtsZeroTimes >= 0 {
			c.psPtsZeroTimes++
		}
		if c.psPtsZeroTimes > 10 {
			pts = c.rtpPts
			dts = c.rtpPts
		}
	} else {
		c.psPtsZeroTimes = -1
	}
	if payloadType == base.AvPacketPtAac || payloadType == base.AvPacketPtG711A || payloadType == base.AvPacketPtG711U {
		c.hasAudio = true
		if c.audioFrame.initDts == 0 {
			c.audioFrame.initDts = dts
		}

		if c.audioFrame.initPts == 0 {
			c.audioFrame.initPts = pts
		}

		var pkt base.AvPacket
		pkt.PayloadType = payloadType
		pkt.Timestamp = int64(dts - c.audioFrame.initDts)
		pkt.Pts = int64(pts - c.audioFrame.initPts)
		pkt.Payload = append(pkt.Payload, frame...)
		c.feedAvPacketWithScale(pkt)

	} else {
		c.hasVideo = true
		if c.videoFrame.initPts == 0 {
			c.videoFrame.initPts = pts
		}

		if c.videoFrame.initDts == 0 {
			c.videoFrame.initDts = dts
		}

		// 塞入lal中
		c.videoFrame.pts = pts - c.videoFrame.initPts
		c.videoFrame.dts = dts - c.videoFrame.initDts
		var pkt base.AvPacket
		pkt.PayloadType = payloadType
		pkt.Timestamp = int64(c.videoFrame.dts)
		pkt.Pts = int64(c.videoFrame.pts)
		pkt.Payload = frame
		c.feedAvPacketWithScale(pkt)
	}
}

// feedAvPacketWithScale 根据当前回放倍速（若有）选择是否走变速过滤器。
// - 当未配置倍速或倍速<=1 时，直接透传到 lalSession。
// - 当倍速>1 且同时存在音频和视频时，使用 RTSP 侧的 AvPacketQueue 做时间戳适配与音视频对齐。
func (c *Conn) feedAvPacketWithScale(pkt base.AvPacket) {
	if c.lalSession == nil {
		return
	}

	// 默认正常速度
	scale := 1.0
	if c.observer != nil && c.streamName != "" {
		scale = c.observer.GetPlaybackScale(c.streamName)
	}

	// 未启用倍速或缺少音视频其一时，直接透传，避免 AvPacketQueue 在单流场景下导致长时延。
	if scale <= 1.0 {
		_ = c.lalSession.FeedAvPacket(pkt)
		return
	}

	// 按需懒加载变速队列
	if c.scaleQueue == nil {
		c.scaleQueue = rtsp.NewAvPacketQueue(func(out base.AvPacket) {
			_ = c.lalSession.FeedAvPacket(out)
		})
	}

	// 注意：PsDemuxer.OnFrame 中传入的 frame 可能复用底层缓冲区。
	// 为避免在 AvPacketQueue 中排队期间底层数据被覆盖，这里对负载做一次深拷贝。
	if len(pkt.Payload) > 0 {
		buf := make([]byte, len(pkt.Payload))
		copy(buf, pkt.Payload)
		pkt.Payload = buf
	}

	c.scaleQueue.SetScale(scale)
	c.scaleQueue.Feed(pkt)
}
func (c *Conn) Close() {
	c.one.Do(func() {
		c.conn.Close()
	})
}
func getPayloadType(cid mpegps.PsStreamType) base.AvPacketPt {
	switch cid {
	case mpegps.PsStreamAac:
		return base.AvPacketPtAac
	case mpegps.PsStreamG711A:
		return base.AvPacketPtG711A
	case mpegps.PsStreamG711U:
		return base.AvPacketPtG711U
	case mpegps.PsStreamH264:
		return base.AvPacketPtAvc
	case mpegps.PsStreamH265:
		return base.AvPacketPtHevc
	}

	return base.AvPacketPtUnknown
}

func splitPsPackets(data []byte) [][]byte {
	startCode := []byte{0x00, 0x00, 0x01, 0xBA}
	start := 0
	var packets [][]byte
	for i := 0; i < len(data); i++ {
		if i+len(startCode) <= len(data) && bytes.Equal(data[i:i+len(startCode)], startCode) {
			if i == 0 {
				continue
			}
			packets = append(packets, data[start:i])
			start = i
		}
	}
	packets = append(packets, data[start:])

	return packets
}
