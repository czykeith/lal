// Copyright 2022, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"sync"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/remux"
	"github.com/q191201771/naza/pkg/nazaatomic"
	"github.com/q191201771/naza/pkg/nazalog"
)

type CustomizePubSessionOption struct {
	DebugDumpPacket string
}

type ModCustomizePubSessionOptionFn func(option *CustomizePubSessionOption)

type CustomizePubSessionContext struct {
	uniqueKey string

	streamName string
	remuxer    *remux.AvPacket2RtmpRemuxer
	onRtmpMsg  func(msg base.RtmpMsg)
	option     CustomizePubSessionOption
	dumpFile   *base.DumpFile

	disposeFlag nazaatomic.Bool

	// stat 用于在 /api/stat/all_group 中作为 Pub 统计展示（SessionTypePsPub / CUSTOMIZE+PUB）
	stat base.StatSession

	// 字节/码率统计：收流侧累计字节（FeedAvPacket/FeedRtmpMsg 写入量）
	readBytesSum   nazaatomic.Uint64
	prevReadBytes  uint64 // 上一轮 UpdateStat 时的读字节，用于算码率
	staleReadBytes uint64 // 用于 IsAlive 判断
	statMu         sync.Mutex
}

func NewCustomizePubSessionContext(streamName string) *CustomizePubSessionContext {
	s := &CustomizePubSessionContext{
		uniqueKey:  base.GenUkCustomizePubSession(),
		streamName: streamName,
		remuxer:    remux.NewAvPacket2RtmpRemuxer(),
	}
	// 初始化基础 Stat 信息，按 PS PUB 类型展示（供 GB28181/自定义接入在 HTTP API 中统计使用）
	s.stat = base.NewStatSessionForPsPub(s.uniqueKey, base.ReadableNowTime())
	nazalog.Infof("[%s] NewCustomizePubSessionContext.", s.uniqueKey)
	return s
}

func (ctx *CustomizePubSessionContext) WithOnRtmpMsg(onRtmpMsg func(msg base.RtmpMsg)) *CustomizePubSessionContext {
	ctx.onRtmpMsg = onRtmpMsg
	ctx.remuxer.WithOnRtmpMsg(onRtmpMsg)
	return ctx
}

func (ctx *CustomizePubSessionContext) WithCustomizePubSessionContextOption(modFn func(option *CustomizePubSessionOption)) *CustomizePubSessionContext {
	modFn(&ctx.option)
	if ctx.option.DebugDumpPacket != "" {
		ctx.dumpFile = base.NewDumpFile()
		err := ctx.dumpFile.OpenToWrite(ctx.option.DebugDumpPacket)
		nazalog.Assert(nil, err)
	}
	return ctx
}

func (ctx *CustomizePubSessionContext) UniqueKey() string {
	return ctx.uniqueKey
}

func (ctx *CustomizePubSessionContext) StreamName() string {
	return ctx.streamName
}

func (ctx *CustomizePubSessionContext) Dispose() {
	nazalog.Infof("[%s] CustomizePubSessionContext::Dispose.", ctx.uniqueKey)
	ctx.disposeFlag.Store(true)
}

// -----implement of base.ISessionUrlContext & base.ISessionStat------------------------------------

func (ctx *CustomizePubSessionContext) Url() string {
	// 仅用于统计展示，伪造一个 ps:// 前缀的 URL
	return "ps://gb28181/" + ctx.streamName
}

func (ctx *CustomizePubSessionContext) AppName() string {
	// 统计视角下，统一展示为 gb28181
	return "gb28181"
}

func (ctx *CustomizePubSessionContext) RawQuery() string {
	return ""
}

// UpdateStat 根据本周期内读字节增量计算读码率（kbps），并更新 stat。
func (ctx *CustomizePubSessionContext) UpdateStat(intervalSec uint32) {
	if intervalSec == 0 {
		return
	}
	ctx.statMu.Lock()
	defer ctx.statMu.Unlock()
	curr := ctx.readBytesSum.Load()
	rDiff := curr - ctx.prevReadBytes
	ctx.stat.ReadBitrateKbits = int(rDiff * 8 / 1024 / uint64(intervalSec))
	ctx.stat.WriteBitrateKbits = 0
	ctx.stat.BitrateKbits = ctx.stat.ReadBitrateKbits
	ctx.prevReadBytes = curr
}

func (ctx *CustomizePubSessionContext) GetStat() base.StatSession {
	ctx.statMu.Lock()
	ctx.stat.ReadBytesSum = ctx.readBytesSum.Load()
	ctx.statMu.Unlock()
	return ctx.stat
}

// IsAlive: 根据最近一次 UpdateStat 周期内是否有新读字节判断是否活跃。
func (ctx *CustomizePubSessionContext) IsAlive() (readAlive, writeAlive bool) {
	if ctx.disposeFlag.Load() {
		return false, false
	}
	ctx.statMu.Lock()
	defer ctx.statMu.Unlock()
	curr := ctx.readBytesSum.Load()
	readAlive = curr != ctx.staleReadBytes
	ctx.staleReadBytes = curr
	return readAlive, true
}

// -----implement of base.IAvPacketStream ------------------------------------------------------------------------------

func (ctx *CustomizePubSessionContext) WithOption(modOption func(option *base.AvPacketStreamOption)) {
	ctx.remuxer.WithOption(modOption)
}

func (ctx *CustomizePubSessionContext) FeedAudioSpecificConfig(asc []byte) error {
	if ctx.disposeFlag.Load() {
		nazalog.Errorf("[%s] FeedAudioSpecificConfig while CustomizePubSessionContext disposed.", ctx.uniqueKey)
		return base.ErrDisposedInStream
	}
	//nazalog.Debugf("[%s] FeedAudioSpecificConfig. asc=%s", ctx.uniqueKey, hex.Dump(asc))
	if ctx.dumpFile != nil {
		_ = ctx.dumpFile.WriteWithType(asc, base.DumpTypeCustomizePubAudioSpecificConfigData)
	}
	ctx.remuxer.InitWithAvConfig(asc, nil, nil, nil)
	return nil
}

func (ctx *CustomizePubSessionContext) FeedAvPacket(packet base.AvPacket) error {
	if ctx.disposeFlag.Load() {
		nazalog.Errorf("[%s] FeedAudioSpecificConfig while CustomizePubSessionContext disposed.", ctx.uniqueKey)
		return base.ErrDisposedInStream
	}
	ctx.readBytesSum.Add(uint64(len(packet.Payload)))
	if ctx.dumpFile != nil {
		_ = ctx.dumpFile.WriteAvPacket(packet, base.DumpTypeCustomizePubData)
	}
	ctx.remuxer.FeedAvPacket(packet)
	return nil
}

func (ctx *CustomizePubSessionContext) FeedRtmpMsg(msg base.RtmpMsg) error {
	if ctx.disposeFlag.Load() {
		return base.ErrDisposedInStream
	}
	// 直接喂 RTMP 的接入在此统计；走 FeedAvPacket 的已在 FeedAvPacket 统计，remuxer 内部回调 onRtmpMsg 不会经本函数
	ctx.readBytesSum.Add(uint64(len(msg.Payload)))
	ctx.onRtmpMsg(msg)
	return nil
}
