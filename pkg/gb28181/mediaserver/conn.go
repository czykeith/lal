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
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/gb28181/mpegps"
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
	conn       net.Conn
	r          io.Reader
	check      bool
	demuxer    *mpegps.PsDemuxer
	streamName string
	connKey    string
	lalServer  ILalServer
	lalSession CustomizePubSession
	videoFrame Frame
	audioFrame Frame

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
		// 将该 RTP 包额外转发给已注册的上级 Sink（级联转推）。
		if c.mediaServer != nil && c.streamName != "" {
			c.mediaServer.ForwardRtp(c.streamName, pkt)
		}

		if c.demuxer != nil {
			if c.psDumpFile != nil {
				c.psDumpFile.WriteWithType(pkt.Payload, base.DumpTypePsRtpData)
			}
			if err := c.demuxer.Input(pkt.Payload); err != nil {
				// 单包 PS 解析失败不关闭连接，仅打日志并继续收包，提高拉流稳定性
				var psErr mpegps.Error
				if errors.As(err, &psErr) && psErr.NeedMore() {
					// 正常“需要更多数据”，不打印
				} else {
					base.Log.Debug("gb28181 ps demux input err, skip packet. streamName=%s err=%v", c.streamName, err)
				}
			}
		}
	}
	return
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
