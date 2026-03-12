package mediaserver

import (
	"errors"
	"net"
	"sync"
	"time"

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
				go func() {
					c.Serve()
					s.conns.Delete(c.connKey)
				}()
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
