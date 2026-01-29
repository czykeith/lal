// Copyright 2020, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package rtsp

import (
	"net"
	"sync"

	"github.com/q191201771/naza/pkg/nazaatomic"
	"github.com/q191201771/naza/pkg/nazaerrors"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/rtprtcp"
	"github.com/q191201771/lal/pkg/sdp"
	"github.com/q191201771/naza/pkg/nazanet"
)

// 聚合PubSession和PullSession，也即流数据是输入类型的session

// IBaseInSessionObserver
//
// BaseInSession会向上层回调两种格式的数据(本质上是一份数据，业务方可自由选择使用)：
// 1. 原始的rtp packet
// 2. rtp合并后的av packet
type IBaseInSessionObserver interface {
	OnSdp(sdpCtx sdp.LogicContext)

	// OnRtpPacket 回调收到的RTP包
	//
	OnRtpPacket(pkt rtprtcp.RtpPacket)

	// OnAvPacket
	//
	// @param pkt: pkt结构体中字段含义见 rtprtcp.OnAvPacket
	//
	OnAvPacket(pkt base.AvPacket)
}

type BaseInSession struct {
	cmdSession IInterleavedPacketWriter

	observer IBaseInSessionObserver

	audioRtpConn     *nazanet.UdpConnection
	videoRtpConn     *nazanet.UdpConnection
	audioRtcpConn    *nazanet.UdpConnection
	videoRtcpConn    *nazanet.UdpConnection
	audioRtpChannel  int
	audioRtcpChannel int
	videoRtpChannel  int
	videoRtcpChannel int

	sessionStat base.BasicSessionStat

	mu              sync.Mutex
	sdpCtx          sdp.LogicContext // const after set
	avPacketQueue   *AvPacketQueue
	audioRrProducer *rtprtcp.RrProducer
	videoRrProducer *rtprtcp.RrProducer

	audioUnpacker rtprtcp.IRtpUnpacker
	videoUnpacker rtprtcp.IRtpUnpacker

	audioSsrc nazaatomic.Uint32
	videoSsrc nazaatomic.Uint32

	scale float64 // 客户端侧变速播放倍数，统一使用代码实现倍速，通过调整时间戳间隔来实现倍速效果

	disposeOnce sync.Once
	waitChan    chan error

	dumpReadAudioRtp base.LogDump
	dumpReadVideoRtp base.LogDump
	dumpReadRtcp     base.LogDump
	dumpReadSr       base.LogDump
}

func NewBaseInSession(sessionType base.SessionType, cmdSession IInterleavedPacketWriter) *BaseInSession {
	s := &BaseInSession{
		sessionStat:      base.NewBasicSessionStat(sessionType, ""),
		cmdSession:       cmdSession,
		waitChan:         make(chan error, 1),
		dumpReadAudioRtp: base.NewLogDump(Log, 1),
		dumpReadVideoRtp: base.NewLogDump(Log, 1),
		dumpReadSr:       base.NewLogDump(Log, 2),
	}
	Log.Infof("[%s] lifecycle new rtsp BaseInSession. session=%p", s.UniqueKey(), s)
	return s
}

func NewBaseInSessionWithObserver(sessionType base.SessionType, cmdSession IInterleavedPacketWriter, observer IBaseInSessionObserver) *BaseInSession {
	s := NewBaseInSession(sessionType, cmdSession)
	s.observer = observer
	return s
}

// SetScale 设置客户端侧变速播放倍数
// 统一使用代码实现倍速，通过调整时间戳间隔来实现倍速效果
func (session *BaseInSession) SetScale(scale float64) {
	// 健壮性：检查session是否有效
	if session == nil {
		return
	}

	// 健壮性：防止panic
	defer func() {
		if r := recover(); r != nil {
			Log.Errorf("[%s] panic in SetScale: %v", session.UniqueKey(), r)
		}
	}()

	session.mu.Lock()
	defer session.mu.Unlock()
	session.scale = scale
	if session.avPacketQueue != nil {
		session.avPacketQueue.SetScale(scale)
	}
	Log.Infof("[%s] set client-side scale. scale=%.1f", session.UniqueKey(), scale)
}

func (session *BaseInSession) InitWithSdp(sdpCtx sdp.LogicContext) {
	session.mu.Lock()
	session.sdpCtx = sdpCtx
	session.mu.Unlock()

	if session.sdpCtx.IsAudioUnpackable() {
		session.audioUnpacker = rtprtcp.DefaultRtpUnpackerFactory(session.sdpCtx.GetAudioPayloadTypeBase(), session.sdpCtx.AudioClockRate, unpackerItemMaxSize, session.onAvPacketUnpacked)
	} else {
		Log.Warnf("[%s] audio unpacker not support for this type yet. logicCtx=%+v", session.UniqueKey(), session.sdpCtx)
	}
	if session.sdpCtx.IsVideoUnpackable() {
		session.videoUnpacker = rtprtcp.DefaultRtpUnpackerFactory(session.sdpCtx.GetVideoPayloadTypeBase(), session.sdpCtx.VideoClockRate, unpackerItemMaxSize, session.onAvPacketUnpacked)
	} else {
		Log.Warnf("[%s] video unpacker not support this type yet. logicCtx=%+v", session.UniqueKey(), session.sdpCtx)
	}

	session.audioRrProducer = rtprtcp.NewRrProducer(session.sdpCtx.AudioClockRate)
	session.videoRrProducer = rtprtcp.NewRrProducer(session.sdpCtx.VideoClockRate)

	if session.sdpCtx.IsAudioUnpackable() && session.sdpCtx.IsVideoUnpackable() {
		// 优化：在锁外创建对象，减少锁持有时间
		var avPacketQueue *AvPacketQueue
		if BaseInSessionTimestampFilterFlag {
			avPacketQueue = NewAvPacketQueue(session.onAvPacket)
		}

		session.mu.Lock()
		if BaseInSessionTimestampFilterFlag {
			session.avPacketQueue = avPacketQueue
			// 如果已经设置了 scale，应用到新创建的 avPacketQueue
			if session.scale > 0 {
				session.avPacketQueue.SetScale(session.scale)
			}
		}
		session.mu.Unlock()
	}

	if session.observer != nil {
		session.observer.OnSdp(session.sdpCtx)
	}
}

// SetObserver 如果没有设置回调监听对象，可以通过该函数设置，调用方保证调用该函数发生在调用InitWithSdp之后
func (session *BaseInSession) SetObserver(observer IBaseInSessionObserver) {
	session.observer = observer

	// 避免在当前协程回调，降低业务方使用负担，不必担心设置监听对象和回调函数中锁重入 TODO(chef): 更好的方式
	go func() {
		session.observer.OnSdp(session.sdpCtx)
	}()
}

func (session *BaseInSession) SetupWithConn(uri string, rtpConn, rtcpConn *nazanet.UdpConnection) error {
	if session.sdpCtx.IsAudioUri(uri) {
		session.audioRtpConn = rtpConn
		session.audioRtcpConn = rtcpConn
	} else if session.sdpCtx.IsVideoUri(uri) {
		session.videoRtpConn = rtpConn
		session.videoRtcpConn = rtcpConn
	} else {
		return nazaerrors.Wrap(base.ErrRtsp)
	}

	go rtpConn.RunLoop(session.onReadRtpPacket)
	go rtcpConn.RunLoop(session.onReadRtcpPacket)

	return nil
}

func (session *BaseInSession) SetupWithChannel(uri string, rtpChannel, rtcpChannel int) error {
	if session.sdpCtx.IsAudioUri(uri) {
		session.audioRtpChannel = rtpChannel
		session.audioRtcpChannel = rtcpChannel
		return nil
	} else if session.sdpCtx.IsVideoUri(uri) {
		session.videoRtpChannel = rtpChannel
		session.videoRtcpChannel = rtcpChannel
		return nil
	}
	return nazaerrors.Wrap(base.ErrRtsp)
}

// ---------------------------------------------------------------------------------------------------------------------
// IClientSessionLifecycle interface
// ---------------------------------------------------------------------------------------------------------------------

// Dispose 文档请参考： IClientSessionLifecycle interface
func (session *BaseInSession) Dispose() error {
	return session.dispose(nil)
}

// WaitChan 文档请参考： IClientSessionLifecycle interface
//
// 注意，目前只有一种情况，即上层主动调用Dispose函数，此时error为nil
func (session *BaseInSession) WaitChan() <-chan error {
	return session.waitChan
}

// ---------------------------------------------------------------------------------------------------------------------

func (session *BaseInSession) GetSdp() sdp.LogicContext {
	// 优化：sdpCtx 在 InitWithSdp 后不再修改，可以不加锁读取
	// 但为了线程安全，保留锁（如果确定不会并发修改，可以移除）
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.sdpCtx
}

func (session *BaseInSession) HandleInterleavedPacket(b []byte, channel int) {
	// 健壮性：防止panic
	defer func() {
		if r := recover(); r != nil {
			Log.Errorf("[%s] panic in HandleInterleavedPacket: %v, channel=%d", session.UniqueKey(), r, channel)
		}
	}()

	// 健壮性：检查输入参数
	if session == nil {
		return
	}
	if b == nil || len(b) == 0 {
		Log.Warnf("[%s] HandleInterleavedPacket: empty packet, channel=%d", session.UniqueKey(), channel)
		return
	}

	switch channel {
	case session.audioRtpChannel:
		fallthrough
	case session.videoRtpChannel:
		_ = session.handleRtpPacket(b)
	case session.audioRtcpChannel:
		fallthrough
	case session.videoRtcpChannel:
		_ = session.handleRtcpPacket(b, nil)
	default:
		Log.Errorf("[%s] read interleaved packet but channel invalid. channel=%d", session.UniqueKey(), channel)
	}
}

// WriteRtpRtcpDummy 发现pull时，需要先给对端发送数据，才能收到数据
func (session *BaseInSession) WriteRtpRtcpDummy() {
	if session.videoRtpConn != nil {
		_ = session.videoRtpConn.Write(dummyRtpPacket)
	}
	if session.videoRtcpConn != nil {
		_ = session.videoRtcpConn.Write(dummyRtcpPacket)
	}
	if session.audioRtpConn != nil {
		_ = session.audioRtpConn.Write(dummyRtpPacket)
	}
	if session.audioRtcpConn != nil {
		_ = session.audioRtcpConn.Write(dummyRtcpPacket)
	}
}

// ----- ISessionStat --------------------------------------------------------------------------------------------------

func (session *BaseInSession) GetStat() base.StatSession {
	return session.sessionStat.GetStat()
}

func (session *BaseInSession) UpdateStat(intervalSec uint32) {
	session.sessionStat.UpdateStat(intervalSec)
}

func (session *BaseInSession) IsAlive() (readAlive, writeAlive bool) {
	return session.sessionStat.IsAlive()
}

// ---------------------------------------------------------------------------------------------------------------------

func (session *BaseInSession) UniqueKey() string {
	return session.sessionStat.UniqueKey()
}

// callback by RTPUnpacker
func (session *BaseInSession) onAvPacketUnpacked(pkt base.AvPacket) {
	// 健壮性：防止panic
	defer func() {
		if r := recover(); r != nil {
			Log.Errorf("[%s] panic in onAvPacketUnpacked: %v", session.UniqueKey(), r)
		}
	}()

	// 健壮性：检查session是否有效
	if session == nil {
		return
	}

	// 优化：减少锁持有时间，先读取指针，然后释放锁
	session.mu.Lock()
	avPacketQueue := session.avPacketQueue
	observer := session.observer
	session.mu.Unlock()

	// 在锁外处理，减少锁竞争
	// 健壮性：检查observer是否为nil
	if avPacketQueue != nil {
		avPacketQueue.Feed(pkt)
	} else if observer != nil {
		observer.OnAvPacket(pkt)
	}
}

// callback by avpacket queue
func (session *BaseInSession) onAvPacket(pkt base.AvPacket) {
	session.observer.OnAvPacket(pkt)
}

// callback by UDPConnection
func (session *BaseInSession) onReadRtpPacket(b []byte, rAddr *net.UDPAddr, err error) bool {
	if err != nil {
		// TODO(chef):
		// read udp [::]:30008: use of closed network connection
		// 可以退出loop，看是在上层退还是下层退，但是要注意每次read都判断的开销
		Log.Warnf("[%s] read udp packet failed. err=%+v", session.UniqueKey(), err)
		return true
	}

	_ = session.handleRtpPacket(b)
	return true
}

// callback by UDPConnection
func (session *BaseInSession) onReadRtcpPacket(b []byte, rAddr *net.UDPAddr, err error) bool {
	if err != nil {
		Log.Warnf("[%s] read udp packet failed. err=%+v", session.UniqueKey(), err)
		return true
	}

	_ = session.handleRtcpPacket(b, rAddr)
	return true
}

// @param rAddr 对端地址，往对端发送数据时使用，注意，如果nil，则表示是interleaved模式，我们直接往TCP连接发数据
func (session *BaseInSession) handleRtcpPacket(b []byte, rAddr *net.UDPAddr) error {
	// 健壮性：防止panic
	defer func() {
		if r := recover(); r != nil {
			Log.Errorf("[%s] panic in handleRtcpPacket: %v", session.UniqueKey(), r)
		}
	}()

	// 健壮性：检查输入参数
	if session == nil {
		return nazaerrors.Wrap(base.ErrRtsp)
	}
	if b == nil || len(b) == 0 {
		Log.Errorf("[%s] handleRtcpPacket but data is nil or empty", session.UniqueKey())
		return nazaerrors.Wrap(base.ErrRtsp)
	}

	// 健壮性：检查RTCP包最小长度（至少4字节头部）
	if len(b) < 4 {
		Log.Errorf("[%s] handleRtcpPacket but length invalid. len=%d", session.UniqueKey(), len(b))
		return nazaerrors.Wrap(base.ErrRtsp)
	}

	session.sessionStat.AddReadBytes(len(b))

	packetType := b[1]

	switch packetType {
	case rtprtcp.RtcpPacketTypeSr:
		sr := rtprtcp.ParseSr(b)
		if session.dumpReadSr.ShouldDump() {
			session.dumpReadSr.Outf("[%s] READ_RTCP. sr=%+v", session.UniqueKey(), sr)
		}
		var rrBuf []byte
		switch sr.SenderSsrc {
		case session.audioSsrc.Load():
			// 优化：减少锁持有时间
			session.mu.Lock()
			audioRrProducer := session.audioRrProducer
			audioRtcpConn := session.audioRtcpConn
			audioRtcpChannel := session.audioRtcpChannel
			cmdSession := session.cmdSession
			session.mu.Unlock()

			rrBuf = audioRrProducer.Produce(sr.GetMiddleNtp())
			if rrBuf != nil {
				if rAddr != nil {
					_ = audioRtcpConn.Write2Addr(rrBuf, rAddr)
				} else {
					_ = cmdSession.WriteInterleavedPacket(rrBuf, audioRtcpChannel)
				}
				session.sessionStat.AddWriteBytes(len(b))
			}
		case session.videoSsrc.Load():
			// 优化：减少锁持有时间
			session.mu.Lock()
			videoRrProducer := session.videoRrProducer
			videoRtcpConn := session.videoRtcpConn
			videoRtcpChannel := session.videoRtcpChannel
			cmdSession := session.cmdSession
			session.mu.Unlock()

			rrBuf = videoRrProducer.Produce(sr.GetMiddleNtp())
			if rrBuf != nil {
				if rAddr != nil {
					_ = videoRtcpConn.Write2Addr(rrBuf, rAddr)
				} else {
					_ = cmdSession.WriteInterleavedPacket(rrBuf, videoRtcpChannel)
				}
				session.sessionStat.AddWriteBytes(len(b))
			}
		default:
			// noop
			//
			// ffmpeg推流时，会在发送第一个RTP包之前就发送一个SR，所以关闭这个警告日志
			//Log.Warnf("[%s] read rtcp sr but senderSsrc invalid. senderSsrc=%d, audio=%d, video=%d",
			//	p.uniqueKey, sr.SenderSsrc, p.audioSsrc, p.videoSsrc)
		}
	default:
		// 优化：减少hex.Dump调用，只在需要时输出（性能开销大）
		Log.Warnf("[%s] handleRtcpPacket but type unknown. type=%d, len=%d",
			session.UniqueKey(), b[1], len(b))
		// 只在调试时输出hex dump: hex.Dump(nazabytes.Prefix(b, 32))
		return nazaerrors.Wrap(base.ErrRtsp)
	}

	return nil
}

func (session *BaseInSession) handleRtpPacket(b []byte) error {
	// 健壮性：防止panic
	defer func() {
		if r := recover(); r != nil {
			Log.Errorf("[%s] panic in handleRtpPacket: %v", session.UniqueKey(), r)
		}
	}()

	// 健壮性：检查输入参数
	if session == nil {
		return nazaerrors.Wrap(base.ErrRtsp)
	}
	if b == nil || len(b) == 0 {
		return nazaerrors.Wrap(base.ErrRtsp)
	}

	session.sessionStat.AddReadBytes(len(b))

	// 健壮性：检查RTP包最小长度
	if len(b) < rtprtcp.RtpFixedHeaderLength {
		Log.Errorf("[%s] handleRtpPacket but length invalid. len=%d", session.UniqueKey(), len(b))
		return nazaerrors.Wrap(base.ErrRtsp)
	}

	packetType := int(b[1] & 0x7F)

	// 健壮性：检查sdpCtx是否有效（通过检查是否有有效的payload type）
	// 注意：不能直接比较包含[]byte的结构体，所以通过方法调用检查
	// 由于IsPayloadTypeOrigin已经会检查sdpCtx，这里直接使用即可
	if !session.sdpCtx.IsPayloadTypeOrigin(packetType) {
		//Log.Errorf("[%s] handleRtpPacket but type invalid. type=%d", session.UniqueKey(), packetType)
		return nazaerrors.Wrap(base.ErrRtsp)
	}

	h, err := rtprtcp.ParseRtpHeader(b)
	if err != nil {
		Log.Errorf("[%s] handleRtpPacket invalid rtp packet. err=%+v", session.UniqueKey(), err)
		return err
	}

	// 健壮性：检查解析出的header是否有效
	// 注意：RtpHeader是值类型，不是指针，所以检查关键字段
	if h.Ssrc == 0 && h.Seq == 0 && h.Timestamp == 0 {
		// 如果所有关键字段都是0，可能是解析失败
		Log.Warnf("[%s] handleRtpPacket: parsed header has zero values, may be invalid", session.UniqueKey())
	}

	var pkt rtprtcp.RtpPacket
	pkt.Header = h
	pkt.Raw = b

	// 接收数据时，保证了sdp的原始类型对应
	if session.sdpCtx.IsAudioPayloadTypeOrigin(packetType) {
		// 优化：减少hex.Dump调用，只在需要时输出（性能开销大）
		if session.dumpReadAudioRtp.ShouldDump() {
			session.dumpReadAudioRtp.Outf("[%s] READ_RTP. audio, h=%+v, len=%d",
				session.UniqueKey(), h, len(b))
			// 只在调试时输出hex dump: hex.Dump(nazabytes.Prefix(b, 32))
		}

		session.audioSsrc.Store(h.Ssrc)
		// 健壮性：检查observer是否有效
		if session.observer != nil {
			session.observer.OnRtpPacket(pkt)
		}
		// 优化：减少锁持有时间，只锁定必要的操作
		session.mu.Lock()
		audioRrProducer := session.audioRrProducer
		session.mu.Unlock()
		if audioRrProducer != nil {
			audioRrProducer.FeedRtpPacket(h.Seq)
		}

		// 健壮性：检查unpacker是否有效
		if session.audioUnpacker != nil {
			session.audioUnpacker.Feed(pkt)
		}
	} else if session.sdpCtx.IsVideoPayloadTypeOrigin(packetType) {
		// 优化：减少hex.Dump调用，只在需要时输出（性能开销大）
		if session.dumpReadVideoRtp.ShouldDump() {
			session.dumpReadVideoRtp.Outf("[%s] READ_RTP. video, h=%+v, len=%d",
				session.UniqueKey(), h, len(b))
			// 只在调试时输出hex dump: hex.Dump(nazabytes.Prefix(b, 32))
		}

		session.videoSsrc.Store(h.Ssrc)
		// 健壮性：检查observer是否有效
		if session.observer != nil {
			session.observer.OnRtpPacket(pkt)
		}
		// 优化：减少锁持有时间，只锁定必要的操作
		session.mu.Lock()
		videoRrProducer := session.videoRrProducer
		session.mu.Unlock()
		if videoRrProducer != nil {
			videoRrProducer.FeedRtpPacket(h.Seq)
		}

		// 健壮性：检查unpacker是否有效
		if session.videoUnpacker != nil {
			session.videoUnpacker.Feed(pkt)
		}
	} else {
		// noop 因为前面已经判断过type了，所以永远不会走到这
	}

	return nil
}

func (session *BaseInSession) dispose(err error) error {
	// 健壮性：防止panic
	defer func() {
		if r := recover(); r != nil {
			Log.Errorf("panic in dispose: %v", r)
		}
	}()

	// 健壮性：检查session是否有效
	if session == nil {
		return nil
	}

	var retErr error
	session.disposeOnce.Do(func() {
		Log.Infof("[%s] lifecycle dispose rtsp BaseInSession. session=%p", session.UniqueKey(), session)
		var e1, e2, e3, e4 error

		// 健壮性：安全地释放所有资源，即使某个资源释放失败也继续释放其他资源
		if session.audioRtpConn != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						Log.Errorf("[%s] panic disposing audioRtpConn: %v", session.UniqueKey(), r)
					}
				}()
				e1 = session.audioRtpConn.Dispose()
			}()
		}
		if session.audioRtcpConn != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						Log.Errorf("[%s] panic disposing audioRtcpConn: %v", session.UniqueKey(), r)
					}
				}()
				e2 = session.audioRtcpConn.Dispose()
			}()
		}
		if session.videoRtpConn != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						Log.Errorf("[%s] panic disposing videoRtpConn: %v", session.UniqueKey(), r)
					}
				}()
				e3 = session.videoRtpConn.Dispose()
			}()
		}
		if session.videoRtcpConn != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						Log.Errorf("[%s] panic disposing videoRtcpConn: %v", session.UniqueKey(), r)
					}
				}()
				e4 = session.videoRtcpConn.Dispose()
			}()
		}

		// 健壮性：清理其他资源
		session.mu.Lock()
		session.audioRtpConn = nil
		session.audioRtcpConn = nil
		session.videoRtpConn = nil
		session.videoRtcpConn = nil
		session.avPacketQueue = nil
		session.audioUnpacker = nil
		session.videoUnpacker = nil
		session.mu.Unlock()

		// 健壮性：安全地发送错误到waitChan
		select {
		case session.waitChan <- err:
		default:
			// 如果waitChan已满，不阻塞
		}

		retErr = nazaerrors.CombineErrors(e1, e2, e3, e4)
	})
	return retErr
}
