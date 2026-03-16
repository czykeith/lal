package mediaserver

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/q191201771/lal/pkg/base"
)

type IGbObserver interface {
	CheckSsrc(ssrc uint32) (*MediaInfo, bool)
	GetMediaInfoByKey(key string) (*MediaInfo, bool)
	NotifyClose(streamName string)
	// GetPlaybackScale 返回指定回放流当前配置的倍速，未配置时返回 1.0。
	GetPlaybackScale(streamName string) float64
}

type GB28181MediaServer struct {
	listenPort int
	lalServer  ILalServer

	listener net.Listener

	disposeOnce sync.Once
	observer    IGbObserver
	mediaKey    string

	// conns 保存所有活跃连接（key=remoteAddr string）。
	// stop(BYE) 时按 streamName 遍历关闭，避免同 streamName 多连接时覆盖/误删导致停不掉。
	conns sync.Map

	// upstreamSinks 记录额外向上级平台转推的目标（key=sinkID）。
	upstreamSinks sync.Map // map[string]*UpstreamSink
}

// UpstreamSink 描述一条向上级平台转推的 PS/RTP 输出。
// 目前仅保存元信息，真正的 RTP 发送逻辑需要在 Conn/OnFrame 里按实际需求补充。
type UpstreamSink struct {
	ID         string
	StreamName string
	RemoteIP   string
	RemotePort int
	// 懒加载的 UDP 连接，用于发送 RTP 包
	conn *net.UDPConn
}

func NewGB28181MediaServer(listenPort int, mediaKey string, observer IGbObserver, lal ILalServer) *GB28181MediaServer {
	return &GB28181MediaServer{
		listenPort: listenPort,
		lalServer:  lal,
		observer:   observer,
		mediaKey:   mediaKey,
	}
}
func (s *GB28181MediaServer) GetListenerPort() uint16 {
	return uint16(s.listenPort)
}
func (s *GB28181MediaServer) GetMediaKey() string {
	return s.mediaKey
}
func (s *GB28181MediaServer) Start(listener net.Listener) (err error) {
	s.listener = listener
	if s.listener != nil {
		go func() {
			for {
				if s.listener == nil {
					return
				}
				conn, err := s.listener.Accept()
				if err != nil {
					var ne net.Error
					if ok := errors.As(err, &ne); ok && ne.Timeout() {
						base.Log.Error("Accept failed: timeout error, retrying...")
						time.Sleep(time.Second / 20)
					} else {
						break
					}
				}

				c := NewConn(conn, s.observer, s.lalServer)
				c.SetKey(s.mediaKey)
				c.SetMediaServer(s)
				go func(conn *Conn) {
					defer func() {
						if r := recover(); r != nil {
							base.Log.Errorf("gb28181 mediaserver conn goroutine panic recovered, connKey=%s panic=%v", conn.connKey, r)
						}
						s.conns.Delete(conn.connKey)
					}()
					conn.Serve()
				}(c)
			}
		}()
	}
	return
}
func (s *GB28181MediaServer) CloseConn(streamName string) {
	if streamName == "" {
		return
	}
	s.conns.Range(func(_, value any) bool {
		c := value.(*Conn)
		if c.streamName == streamName {
			c.Close()
		}
		return true
	})
}

func (s *GB28181MediaServer) Dispose() {
	s.disposeOnce.Do(func() {
		s.conns.Range(func(_, value any) bool {
			conn := value.(*Conn)
			conn.Close()
			return true
		})
		if s.listener != nil {
			s.listener.Close()
			s.listener = nil
		}
	})
}

// AddUpstreamSink 为指定 streamName 增加一个向上级平台转推的目标地址。
// 这里只是记录 sink 信息，具体 RTP 转发逻辑需在 Conn 中按实际使用场景实现。
func (s *GB28181MediaServer) AddUpstreamSink(streamName, remoteIP string, remotePort int) (string, error) {
	if streamName == "" || remoteIP == "" || remotePort <= 0 {
		return "", fmt.Errorf("invalid upstream sink params")
	}
	id := fmt.Sprintf("%s-%s:%d-%d", streamName, remoteIP, remotePort, time.Now().UnixNano())
	sink := &UpstreamSink{
		ID:         id,
		StreamName: streamName,
		RemoteIP:   remoteIP,
		RemotePort: remotePort,
	}
	s.upstreamSinks.Store(id, sink)
	base.Log.Infof("gb28181 mediaserver add upstream sink. key=%s stream=%s remote=%s:%d", id, streamName, remoteIP, remotePort)
	return id, nil
}

// RemoveUpstreamSink 移除一个向上级转推目标。
func (s *GB28181MediaServer) RemoveUpstreamSink(id string) {
	if id == "" {
		return
	}
	if v, ok := s.upstreamSinks.LoadAndDelete(id); ok {
		if sink, ok2 := v.(*UpstreamSink); ok2 && sink.conn != nil {
			_ = sink.conn.Close()
		}
		base.Log.Infof("gb28181 mediaserver remove upstream sink. key=%s", id)
	}
}

// ForwardRtp 将指定 streamName 的 RTP 包额外转发到已注册的上级 Sink。
func (s *GB28181MediaServer) ForwardRtp(streamName string, pkt *rtp.Packet) {
	if pkt == nil {
		return
	}
	s.upstreamSinks.Range(func(_, v any) bool {
		sink, ok := v.(*UpstreamSink)
		if !ok || sink.StreamName != streamName {
			return true
		}
		if sink.RemoteIP == "" || sink.RemotePort <= 0 {
			return true
		}

		// 确保 UDP 连接已建立（懒创建）
		if sink.conn == nil {
			addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", sink.RemoteIP, sink.RemotePort))
			if err != nil {
				base.Log.Warnf("gb28181 ForwardRtp resolve udp addr failed. sink=%s err=%+v", sink.ID, err)
				return true
			}
			c, err := net.DialUDP("udp", nil, addr)
			if err != nil {
				base.Log.Warnf("gb28181 ForwardRtp dial udp failed. sink=%s err=%+v", sink.ID, err)
				return true
			}
			sink.conn = c
		}

		// 复用设备的 RTP 头部与负载，直接转发给上级
		buf, err := pkt.Marshal()
		if err != nil {
			base.Log.Warnf("gb28181 ForwardRtp marshal rtp failed. sink=%s err=%+v", sink.ID, err)
			return true
		}
		if _, err = sink.conn.Write(buf); err != nil {
			base.Log.Warnf("gb28181 ForwardRtp write udp failed. sink=%s err=%+v", sink.ID, err)
		}
		return true
	})
}
