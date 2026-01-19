// Copyright 2020, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package rtsp

import (
	"bytes"
	"fmt"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/naza/pkg/circularqueue"
)

const maxQueueSize = 128

type OnAvPacket func(pkt base.AvPacket)

// AvPacketQueue
//
// 处理音频和视频的时间戳：
// 1. 让音频和视频的时间戳都从0开始（改变原时间戳）
// 2. 让音频和视频的时间戳交替递增输出（不改变原时间戳）
// 3. 当时间戳翻转时，保持时间戳的连续性（改变原时间戳）
//
// 注意，本模块默认音频和视频都存在，如果只有音频或只有视频，则不要使用该模块
//
// TODO(chef): [refactor] 重命名为filter 202305
type AvPacketQueue struct {
	onAvPacket OnAvPacket

	audioQueue *circularqueue.CircularQueue // TODO chef: 特化成AvPacket类型
	videoQueue *circularqueue.CircularQueue

	audioBaseTs int64 // audio base timestamp
	videoBaseTs int64 // video base timestamp

	audioPrevOriginTs   int64
	videoPrevOriginTs   int64
	audioPrevModTs      int64
	videoPrevModTs      int64
	audioPrevIntervalTs int64
	videoPrevIntervalTs int64

	scale float64 // 客户端侧变速播放倍数，用于调整时间戳间隔

	// 用于平滑变速的累积时间戳
	audioOriginBaseTs int64 // 音频原始时间戳基准（累积）
	videoOriginBaseTs int64 // 视频原始时间戳基准（累积）

	// 保存上一次的原始时间戳（未调整的），用于平滑计算
	audioPrevRawOriginTs int64
	videoPrevRawOriginTs int64
}

func NewAvPacketQueue(onAvPacket OnAvPacket) *AvPacketQueue {
	return &AvPacketQueue{
		onAvPacket: onAvPacket,
		audioQueue: circularqueue.New(maxQueueSize),
		videoQueue: circularqueue.New(maxQueueSize),

		audioBaseTs: -1,
		videoBaseTs: -1,

		audioPrevOriginTs:   -1,
		videoPrevOriginTs:   -1,
		audioPrevModTs:      -1,
		videoPrevModTs:      -1,
		audioPrevIntervalTs: -1,
		videoPrevIntervalTs: -1,

		scale: 1.0, // 默认正常速度

		audioOriginBaseTs: -1,
		videoOriginBaseTs: -1,

		audioPrevRawOriginTs: -1,
		videoPrevRawOriginTs: -1,
	}
}

// SetScale 设置客户端侧变速播放倍数
func (a *AvPacketQueue) SetScale(scale float64) {
	if scale > 0 {
		// 如果 scale 改变，需要调整基准以保持连续性
		oldScale := a.scale
		a.scale = scale

		// 如果之前已经有数据且 scale 改变，需要重新计算基准
		// 这样可以确保在改变 scale 时，时间戳能够平滑过渡
		if oldScale != scale && oldScale > 0 {
			// 将当前的原始时间戳作为新的基准，这样后续计算会基于新的 scale
			// 但保持时间戳的连续性
			if a.audioPrevOriginTs != -1 {
				// 保持原始基准不变，让后续计算自然适应新 scale
				// 不需要重置，因为我们会基于累积的原始时间戳计算
			}
			if a.videoPrevOriginTs != -1 {
				// 同上
			}
		}
	} else {
		a.scale = 1.0
	}
}

func (a *AvPacketQueue) adjustTsHandleRotate(pkt *base.AvPacket) {
	// TODO(chef): [refactor] adjustTsXxx 这一层换成fn这一层 202305
	fn := func(prevOriginTs, prevModTs, prevIntervalTs *int64, originBaseTs *int64, prevRawOriginTs *int64, isAudio bool) {
		if *prevOriginTs == -1 {
			// 第一次
			*prevOriginTs = pkt.Timestamp
			*originBaseTs = pkt.Timestamp
			*prevRawOriginTs = pkt.Timestamp
			pkt.Timestamp = 0
			*prevModTs = pkt.Timestamp
			// 没有prev，所以没法也不需要计算 prevIntervalTs
		} else {
			// 非第一次
			// 保存当前原始时间戳
			currentRawOriginTs := pkt.Timestamp
			interval := pkt.Timestamp - *prevRawOriginTs

			if interval < -1000 {
				// 时间戳翻滚，并且差值大于阈值了（为了避免B帧导致的小范围翻滚)
				Log.Warnf("[AVQ] ts rotate. isAudio=%v, interval=%d, prevModTs=%d, prevIntervalTs=%d, prevOriginTs=%d, pktTS=%d, audioQueue=%d, videoQueue=%d",
					isAudio, interval, *prevModTs, *prevIntervalTs, *prevOriginTs, pkt.Timestamp, a.audioQueue.Size(), a.videoQueue.Size())

				// 注意，这个不要放到if判断的前面，因为需要先打印旧值日志
				*prevOriginTs = pkt.Timestamp
				*originBaseTs = pkt.Timestamp
				*prevRawOriginTs = pkt.Timestamp

				// 用历史差值来更新，应用 scale
				adjustedInterval := int64(float64(*prevIntervalTs) / a.scale)
				pkt.Timestamp = *prevModTs + adjustedInterval
			} else {
				// 使用平滑的间隔计算，但保持累积基准以确保连续性
				// 计算原始时间戳间隔
				rawInterval := currentRawOriginTs - *prevRawOriginTs

				// 应用 scale：将间隔除以 scale
				adjustedInterval := int64(float64(rawInterval) / a.scale)

				// 确保间隔至少为 1，避免时间戳不递增
				// 但对于很小的间隔，允许为 0（通过累积误差处理）
				if adjustedInterval < 0 {
					// 如果为负，说明时间戳回退，使用上一次的间隔
					adjustedInterval = *prevIntervalTs
				} else if adjustedInterval == 0 && rawInterval > 0 {
					// 如果原始间隔大于 0 但调整后为 0，说明 scale 很大
					// 使用累积误差处理：累积小的间隔，当累积到一定值时再应用
					// 这里简化处理：至少设为 1
					adjustedInterval = 1
				} else if adjustedInterval == 0 {
					// 如果原始间隔就是 0，保持为 0
					adjustedInterval = 0
				}

				// 计算新的时间戳
				pkt.Timestamp = *prevModTs + adjustedInterval
				*prevOriginTs = pkt.Timestamp
				*prevIntervalTs = adjustedInterval
				*prevRawOriginTs = currentRawOriginTs
			}

			if pkt.Timestamp < 0 {
				Log.Warnf("[AVQ] ts negative. isAudio=%v, interval=%d, prevModTs=%d, prevIntervalTs=%d, prevOriginTs=%d, pktTS=%d, audioQueue=%d, videoQueue=%d",
					isAudio, interval, *prevModTs, *prevIntervalTs, *prevOriginTs, pkt.Timestamp, a.audioQueue.Size(), a.videoQueue.Size())
				pkt.Timestamp = 0
			}
			*prevModTs = pkt.Timestamp
		}
	}

	if pkt.IsVideo() {
		fn(&a.videoPrevOriginTs, &a.videoPrevModTs, &a.videoPrevIntervalTs, &a.videoOriginBaseTs, &a.videoPrevRawOriginTs, false)

		_ = a.videoQueue.PushBack(*pkt)
	} else {
		fn(&a.audioPrevOriginTs, &a.audioPrevModTs, &a.audioPrevIntervalTs, &a.audioOriginBaseTs, &a.audioPrevRawOriginTs, true)

		_ = a.audioQueue.PushBack(*pkt)
	}
}

func (a *AvPacketQueue) adjustTs(pkt *base.AvPacket) {
	if pkt.IsVideo() {
		// 时间戳回退了
		if pkt.Timestamp < a.videoBaseTs {
			Log.Warnf("[AVQ] video ts rotate. pktTS=%d, audioBaseTs=%d, videoBaseTs=%d, audioQueue=%d, videoQueue=%d",
				pkt.Timestamp, a.audioBaseTs, a.videoBaseTs, a.audioQueue.Size(), a.videoQueue.Size())
			a.PopAllByForce()
		}

		// 第一次
		if a.videoBaseTs == -1 {
			a.videoBaseTs = pkt.Timestamp
		}

		// 根据基准调节
		pkt.Timestamp -= a.videoBaseTs

		_ = a.videoQueue.PushBack(*pkt)
	} else {
		if pkt.Timestamp < a.audioBaseTs {
			Log.Warnf("[AVQ] audio ts rotate. pktTS=%d, audioBaseTs=%d, videoBaseTs=%d, audioQueue=%d, videoQueue=%d",
				pkt.Timestamp, a.audioBaseTs, a.videoBaseTs, a.audioQueue.Size(), a.videoQueue.Size())
			a.PopAllByForce()
		}
		if a.audioBaseTs == -1 {
			a.audioBaseTs = pkt.Timestamp
		}
		pkt.Timestamp -= a.audioBaseTs
		_ = a.audioQueue.PushBack(*pkt)
	}
}

// Feed 注意，调用方保证，音频相较于音频，视频相较于视频，时间戳是线性递增的。
func (a *AvPacketQueue) Feed(pkt base.AvPacket) {
	//Log.Debugf("[AVQ] Feed. t=%d, ts=%d, Q(%d,%d), %s, base(%d,%d)", pkt.PayloadType, pkt.Timestamp, a.audioQueue.Size(), a.videoQueue.Size(), packetsReadable(peekQueuePackets(a)), a.audioBaseTs, a.videoBaseTs)

	if TimestampFilterHandleRotateFlag {
		a.adjustTsHandleRotate(&pkt)
	} else {
		a.adjustTs(&pkt)
	}

	// 如果音频和视频都存在，则按序输出，直到其中一个为空
	// 添加最大循环次数限制，防止在时间戳异常时无限循环
	maxIterations := 100
	iterations := 0
	for !a.audioQueue.Empty() && !a.videoQueue.Empty() && iterations < maxIterations {
		iterations++
		apkt, _ := a.audioQueue.Front()
		vpkt, _ := a.videoQueue.Front()
		aapkt := apkt.(base.AvPacket)
		vvpkt := vpkt.(base.AvPacket)

		// 计算时间戳差值，如果差值很小（小于等于1），视为相等
		tsDiff := aapkt.Timestamp - vvpkt.Timestamp
		if tsDiff < -1 {
			// 音频时间戳更小
			_, _ = a.audioQueue.PopFront()
			//Log.Debugf("[AVQ] pop audio by video. a=%d, v=%d", aapkt.Timestamp, vvpkt.Timestamp)
			a.onAvPacket(aapkt)
		} else if tsDiff > 1 {
			// 视频时间戳更小
			_, _ = a.videoQueue.PopFront()
			//Log.Debugf("[AVQ] pop video by audio. a=%d, v=%d", aapkt.Timestamp, vvpkt.Timestamp)
			a.onAvPacket(vvpkt)
		} else {
			// 时间戳差值很小（-1, 0, 1），视为相等，优先输出视频
			// 这样可以避免在变速播放时因为时间戳过于接近而卡住
			_, _ = a.videoQueue.PopFront()
			//Log.Debugf("[AVQ] pop video (similar ts). a=%d, v=%d, diff=%d", aapkt.Timestamp, vvpkt.Timestamp, tsDiff)
			a.onAvPacket(vvpkt)
		}
	}

	// 如果达到最大循环次数，强制输出一个队列，防止卡住
	if iterations >= maxIterations && !a.audioQueue.Empty() && !a.videoQueue.Empty() {
		Log.Warnf("[AVQ] max iterations reached, force pop. audioQueue=%d, videoQueue=%d",
			a.audioQueue.Size(), a.videoQueue.Size())
		// 优先输出视频队列
		if !a.videoQueue.Empty() {
			vpkt, _ := a.videoQueue.Front()
			vvpkt := vpkt.(base.AvPacket)
			_, _ = a.videoQueue.PopFront()
			a.onAvPacket(vvpkt)
		}
	}

	// 如果视频满了，则全部输出
	if a.videoQueue.Full() {
		Log.Assert(true, a.audioQueue.Empty())
		a.popAllVideo()
		return
	}

	// 如果音频满了，则全部输出
	if a.audioQueue.Full() {
		Log.Assert(true, a.videoQueue.Empty())
		a.popAllAudio()
		return
	}
}

func (a *AvPacketQueue) PopAllByForce() {
	a.videoBaseTs = -1
	a.audioBaseTs = -1

	if a.audioQueue.Empty() && a.videoQueue.Empty() {
		// noop
	} else if a.audioQueue.Empty() && !a.videoQueue.Empty() {
		a.popAllVideo()
	} else if !a.audioQueue.Empty() && a.videoQueue.Empty() {
		a.popAllAudio()
	} else {
		// 这种情况不可能发生
		// 因为每次排出时，都会排空一个队列
	}
}

func (a *AvPacketQueue) popAllAudio() {
	//Log.Debugf("[AVQ] pop all audio. audioQueue=%d, videoQueue=%d", a.audioQueue.Size(), a.videoQueue.Size())
	for !a.audioQueue.Empty() {
		pkt, _ := a.audioQueue.Front()
		ppkt := pkt.(base.AvPacket)
		_, _ = a.audioQueue.PopFront()
		a.onAvPacket(ppkt)
	}
}

func (a *AvPacketQueue) popAllVideo() {
	//Log.Debugf("[AVQ] pop all video. audioQueue=%d, videoQueue=%d", a.audioQueue.Size(), a.videoQueue.Size())
	for !a.videoQueue.Empty() {
		pkt, _ := a.videoQueue.Front()
		ppkt := pkt.(base.AvPacket)
		_, _ = a.videoQueue.PopFront()
		a.onAvPacket(ppkt)
	}
}

// ---------------------------------------------------------------------------------------------------------------------

func packetsReadable(pkts []base.AvPacket) string {
	var buf bytes.Buffer
	buf.WriteString("[")
	for _, pkt := range pkts {
		if pkt.IsAudio() {
			buf.WriteString(fmt.Sprintf(" A(%d) ", pkt.Timestamp))
		} else if pkt.IsVideo() {
			buf.WriteString(fmt.Sprintf(" V(%d) ", pkt.Timestamp))
		} else {
			buf.WriteString(fmt.Sprintf(" U(%d) ", pkt.Timestamp))
		}
	}
	buf.WriteString("]")
	return buf.String()
}

func peekQueuePackets(q *AvPacketQueue) []base.AvPacket {
	var out []base.AvPacket
	for i := 0; i < q.audioQueue.Size(); i++ {
		pkt, _ := q.audioQueue.At(i)
		ppkt := pkt.(base.AvPacket)
		out = append(out, ppkt)
	}
	for i := 0; i < q.videoQueue.Size(); i++ {
		pkt, _ := q.videoQueue.At(i)
		ppkt := pkt.(base.AvPacket)
		out = append(out, ppkt)
	}
	return out
}
