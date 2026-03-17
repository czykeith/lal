// Copyright 2019, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/hls"
	"github.com/q191201771/lal/pkg/httpflv"
	"github.com/q191201771/lal/pkg/httpts"
	"github.com/q191201771/lal/pkg/mpegts"
	"github.com/q191201771/lal/pkg/remux"
	"github.com/q191201771/lal/pkg/rtmp"
	"github.com/q191201771/lal/pkg/rtsp"
	"github.com/q191201771/lal/pkg/sdp"
)

// ---------------------------------------------------------------------------------------------------------------------
// 输入流需要做的事情
// TODO(chef): [refactor] 考虑抽象出通用接口 202208
//
// checklist表格
// | .                                           | rtmp pub | ps pub |
// | 添加到group中                                | Y        | Y      |
// | 到输出流的转换路径关系                         | Y        | Y      |
// | 删除                                        | Y        | Y      |
// | group.hasPubSession()                       | Y        | Y      |
// | group.disposeInactiveSessions()检查超时并清理 | Y        | Y      |
// | group.Dispose()时销毁                        | Y        | Y      |
// | group.GetStat()时获取信息                     | Y        | Y      |
// | group.KickSession()时踢出                    | Y        | Y      |
// | group.updateAllSessionStat()更新信息         | Y        | Y      |
// | group.inSessionUniqueKey()                  | Y        | Y      |

// TODO(chef): [refactor] 整理sub类型流接入需要做的事情的文档 202211

// ---------------------------------------------------------------------------------------------------------------------
// 输入流到输出流的转换路径关系（一共6种输入）：
//
// rtmpPullSession.WithOnReadRtmpAvMsg  ->
// rtmpPubSession.SetPubSessionObserver ->
//    customizePubSession.WithOnRtmpMsg -> OnReadRtmpAvMsg(enter Lock) -> [dummyAudioFilter] -> broadcastByRtmpMsg -> rtmp, http-flv
//                                                                                                                 -> rtmp2RtspRemuxer -> rtsp
//                                                                                                                 -> rtmp2MpegtsRemuxer -> ts, hls
//
// ---------------------------------------------------------------------------------------------------------------------
// rtspPullSession ->
//  rtspPubSession -> OnRtpPacket(enter Lock) -> rtsp
//                 -> OnAvPacket(enter Lock) -> rtsp2RtmpRemuxer -> onRtmpMsgFromRemux -> [dummyAudioFilter] -> broadcastByRtmpMsg -> rtmp, http-flv
//                                                                                                           -> rtmp2MpegtsRemuxer -> ts, hls
//
// ---------------------------------------------------------------------------------------------------------------------
// psPubSession -> OnAvPacketFromPsPubSession(enter Lock) -> rtsp2RtmpRemuxer -> onRtmpMsgFromRemux -> [dummyAudioFilter] -> broadcastByRtmpMsg -> ...
//                                                                                                                                              -> ...
//                                                                                                                                              -> ...

type GroupOption struct {
	onHookSession func(uniqueKey string, streamName string) ICustomizeHookSessionContext
}

type IGroupObserver interface {
	CleanupHlsIfNeeded(appName string, streamName string, path string)
	OnHlsMakeTs(info base.HlsMakeTsInfo)
	OnRelayPullStart(info base.PullStartInfo) // TODO(chef): refactor me
	OnRelayPullStop(info base.PullStopInfo)
	// UpdateSnapshot 将指定 streamName 的最新关键帧写入全局缓存。
	UpdateSnapshot(streamName string, pkt base.AvPacket)
}

type Group struct {
	UniqueKey  string // const after init
	appName    string // const after init
	streamName string // const after init TODO chef: 和stat里的字段重复，可以删除掉
	config     *Config

	option                      GroupOption
	customizeHookSessionContext ICustomizeHookSessionContext

	observer IGroupObserver

	exitChan chan struct{}

	mutex sync.Mutex
	// pub
	rtmpPubSession      *rtmp.ServerSession
	rtspPubSession      *rtsp.PubSession
	customizePubSession *CustomizePubSessionContext
	rtsp2RtmpRemuxer    *remux.AvPacket2RtmpRemuxer
	rtmp2RtspRemuxer    *remux.Rtmp2RtspRemuxer
	rtmp2MpegtsRemuxer  *remux.Rtmp2MpegtsRemuxer
	// rtmp2AvPacketRemuxer 将广播路径上的 RTMP 视频转换为 AnnexB AvPacket，用于全局截图缓存。
	rtmp2AvPacketRemuxer *remux.Rtmp2AvPacketRemuxer
	// avPacketSubs 订阅当前 Group 视频 AvPacket 的回调（用于如 GB28181 上级转推等场景）。
	// 使用独立的读写锁，避免与 group.mutex 形成死锁。
	avPacketSubsMu sync.RWMutex
	// key 为订阅 ID，由调用方自定义保证唯一性。
	avPacketSubs map[string]func(base.AvPacket)
	// pull
	pullProxy *pullProxy
	// relay
	relayProxy *relayProxy
	// rtmp pub使用 TODO(chef): [doc] 更新这个注释，是共同使用 202210
	dummyAudioFilter *remux.DummyAudioFilter
	// rtmp sub使用
	rtmpGopCache *remux.GopCache
	// httpflv sub使用
	httpflvGopCache *remux.GopCache
	// httpts sub使用
	httptsGopCache *remux.GopCacheMpegts
	// rtsp使用
	sdpCtx *sdp.LogicContext
	// mpegts使用
	patpmt []byte
	// sub
	rtmpSubSessionSet    map[*rtmp.ServerSession]struct{}
	httpflvSubSessionSet map[*httpflv.SubSession]struct{}
	httptsSubSessionSet  map[*httpts.SubSession]struct{}
	rtspSubSessionSet    map[*rtsp.SubSession]struct{} // 注意，使用这个容器时，一定要注意 session 的 Stage 属性
	hlsSubSessionSet     map[*hls.SubSession]struct{}
	// push
	pushEnable    bool
	url2PushProxy map[string]*pushProxy
	// hls
	hlsMuxer *hls.Muxer
	// record
	recordFlv    *httpflv.FlvFileWriter
	recordMpegts *mpegts.FileWriter
	// rtmp sub使用
	rtmpMergeWriter *base.MergeWriter // TODO(chef): 后面可以在业务层加一个定时Flush
	//
	stat base.StatGroup
	//
	inVideoFpsRecords base.PeriodRecord
	//
	hlsCalcSessionStatIntervalSec uint32
	//
	psPubDumpFile    *base.DumpFile
	rtspPullDumpFile *base.DumpFile
}

// HasHlsSubSession 返回当前 Group 是否还有 HLS 订阅者。
func (group *Group) HasHlsSubSession() bool {
	return len(group.hlsSubSessionSet) > 0
}

// AddAvPacketSubscriber 为当前 Group 增加一个视频 AvPacket 订阅者。
// id 需由调用方保证在该 Group 内唯一；返回的 cancel 用于取消订阅。
func (group *Group) AddAvPacketSubscriber(id string, cb func(base.AvPacket)) (cancel func()) {
	if id == "" || cb == nil {
		return func() {}
	}
	group.avPacketSubsMu.Lock()
	group.avPacketSubs[id] = cb
	group.avPacketSubsMu.Unlock()
	return func() {
		group.avPacketSubsMu.Lock()
		delete(group.avPacketSubs, id)
		group.avPacketSubsMu.Unlock()
	}
}

func NewGroup(appName string, streamName string, config *Config, option GroupOption, observer IGroupObserver) *Group {
	uk := base.GenUkGroup()

	g := &Group{
		UniqueKey:  uk,
		appName:    appName,
		streamName: streamName,
		config:     config,
		option:     option,
		observer:   observer,
		stat: base.StatGroup{
			StreamName: streamName,
			AppName:    appName,
		},
		exitChan:             make(chan struct{}, 1),
		rtmpSubSessionSet:    make(map[*rtmp.ServerSession]struct{}),
		httpflvSubSessionSet: make(map[*httpflv.SubSession]struct{}),
		httptsSubSessionSet:  make(map[*httpts.SubSession]struct{}),
		rtspSubSessionSet:    make(map[*rtsp.SubSession]struct{}),
		hlsSubSessionSet:     make(map[*hls.SubSession]struct{}),
		rtmpGopCache:         remux.NewGopCache("rtmp", uk, config.RtmpConfig.GopNum, config.RtmpConfig.SingleGopMaxFrameNum),
		httpflvGopCache:      remux.NewGopCache("httpflv", uk, config.HttpflvConfig.GopNum, config.HttpflvConfig.SingleGopMaxFrameNum),
		httptsGopCache:       remux.NewGopCacheMpegts(uk, config.HttptsConfig.GopNum, config.HttptsConfig.SingleGopMaxFrameNum),
		inVideoFpsRecords:    base.NewPeriodRecord(32),
		avPacketSubs:         make(map[string]func(base.AvPacket)),
	}

	g.hlsCalcSessionStatIntervalSec = uint32(config.HlsConfig.FragmentDurationMs / 100) // equals to (ms/1000) * 10
	if g.hlsCalcSessionStatIntervalSec == 0 {
		g.hlsCalcSessionStatIntervalSec = defaultHlsCalcSessionStatIntervalSec
	}

	g.initRelayPushByConfig()
	g.initRelayPullByConfig()

	if config.RtmpConfig.MergeWriteSize > 0 {
		g.rtmpMergeWriter = base.NewMergeWriter(g.writev2RtmpSubSessions, config.RtmpConfig.MergeWriteSize)
	}

	// 初始化用于截图的 AvPacket Remuxer：只在有视频关键帧时更新全局缓存，对主流程影响极小。
	g.rtmp2AvPacketRemuxer = remux.NewRtmp2AvPacketRemuxer().WithOnAvPacket(
		func(pkt base.AvPacket, _ interface{}) {
			if !pkt.IsVideo() {
				return
			}
			if g.observer != nil {
				g.observer.UpdateSnapshot(g.streamName, pkt)
			}
			// 将视频 AvPacket 分发给所有订阅者（如上级转推 PS/RTP）。
			g.avPacketSubsMu.RLock()
			subs := make([]func(base.AvPacket), 0, len(g.avPacketSubs))
			for _, cb := range g.avPacketSubs {
				subs = append(subs, cb)
			}
			g.avPacketSubsMu.RUnlock()
			for _, cb := range subs {
				cb(pkt)
			}
		},
	)

	Log.Infof("[%s] lifecycle new group. group=%p, appName=%s, streamName=%s", uk, g, appName, streamName)
	return g
}

func (group *Group) RunLoop() {
	<-group.exitChan
}

// Tick 定时器
//
// @param tickCount 当前时间，单位秒。注意，不一定是Unix时间戳，可以是从0开始+1秒递增的时间
func (group *Group) Tick(tickCount uint32) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	group.tickPullModule()
	group.startPushIfNeeded()

	// 定时关闭没有数据的session
	group.disposeInactiveSessions(tickCount)

	// 定时计算session bitrate
	if tickCount%calcSessionStatIntervalSec == 0 {
		group.updateAllSessionStat()
	}

	// because hls make multiple separate http request to get stream content and gap between request base on hls segment duration
	// if we update every 5s can cause bitrateKbit equal to 0 if within 5s do not have any ts http request is make
	if tickCount%group.hlsCalcSessionStatIntervalSec == 0 {
		for session := range group.hlsSubSessionSet {
			session.UpdateStat(group.hlsCalcSessionStatIntervalSec)
		}
	}
}

// Dispose ...
func (group *Group) Dispose() {
	Log.Infof("[%s] lifecycle dispose group.", group.UniqueKey)
	group.exitChan <- struct{}{}

	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.rtmpPubSession != nil {
		group.rtmpPubSession.Dispose()
	}
	if group.rtspPubSession != nil {
		group.rtspPubSession.Dispose()
	}
	for session := range group.rtmpSubSessionSet {
		session.Dispose()
	}
	group.rtmpSubSessionSet = nil

	for session := range group.rtspSubSessionSet {
		session.Dispose()
	}
	group.rtspSubSessionSet = nil

	for session := range group.httpflvSubSessionSet {
		session.Dispose()
	}
	group.httpflvSubSessionSet = nil

	for session := range group.httptsSubSessionSet {
		session.Dispose()
	}
	group.httptsSubSessionSet = nil

	group.delIn()
}

// ---------------------------------------------------------------------------------------------------------------------

func (group *Group) StringifyDebugStats(maxsub int) string {
	b, _ := json.Marshal(group.GetStat(maxsub))
	return string(b)
}

func (group *Group) GetStat(maxsub int) base.StatGroup {
	// TODO(chef): [refactor] param maxsub

	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.rtmpPubSession != nil {
		group.stat.StatPub = base.Session2StatPub(group.rtmpPubSession)
	} else if group.rtspPubSession != nil {
		group.stat.StatPub = base.Session2StatPub(group.rtspPubSession)
	} else if group.customizePubSession != nil {
		// 用于 GB28181 / 自定义接入场景，通过 CustomizePubSessionContext 统计为 PS/CUSTOMIZE 类型的 Pub
		group.stat.StatPub = base.Session2StatPub(group.customizePubSession)
	} else {
		group.stat.StatPub = base.StatPub{}
	}

	// 创建时未带 appName（如 GB28181 通过 AddCustomizePubSession 仅传 streamName）的 Group，在统计展示时补全默认 app_name，不影响拉流/播放（SimpleGroupManager 仅按 streamName 路由）
	if group.appName == "" {
		if group.customizePubSession != nil {
			group.stat.AppName = group.customizePubSession.AppName() // "gb28181"
		} else if group.rtmpPubSession != nil {
			group.stat.AppName = group.rtmpPubSession.AppName()
		} else if group.rtspPubSession != nil {
			group.stat.AppName = group.rtspPubSession.AppName()
		} else {
			group.stat.AppName = "live"
		}
	}

	group.stat.StatPull = group.getStatPull()

	group.stat.StatSubs = nil
	var statSubCount int
	for s := range group.rtmpSubSessionSet {
		statSubCount++
		if statSubCount > maxsub {
			break
		}
		group.stat.StatSubs = append(group.stat.StatSubs, base.Session2StatSub(s))
	}
	for s := range group.httpflvSubSessionSet {
		statSubCount++
		if statSubCount > maxsub {
			break
		}
		group.stat.StatSubs = append(group.stat.StatSubs, base.Session2StatSub(s))
	}
	for s := range group.httptsSubSessionSet {
		statSubCount++
		if statSubCount > maxsub {
			break
		}
		group.stat.StatSubs = append(group.stat.StatSubs, base.Session2StatSub(s))
	}
	for s := range group.rtspSubSessionSet {
		statSubCount++
		if statSubCount > maxsub {
			break
		}
		group.stat.StatSubs = append(group.stat.StatSubs, base.Session2StatSub(s))
	}

	//Log.Debugf("GetStat. len(group.hlsSubSessionSet)=%d", len(group.hlsSubSessionSet))
	for s := range group.hlsSubSessionSet {
		statSubCount++
		if statSubCount > maxsub {
			break
		}
		group.stat.StatSubs = append(group.stat.StatSubs, base.Session2StatSub(s))
	}

	group.stat.GetFpsFrom(&group.inVideoFpsRecords, time.Now().Unix())

	return group.stat
}

func (group *Group) KickSession(sessionId string) bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	Log.Infof("[%s] kick session. session id=%s", group.UniqueKey, sessionId)

	if strings.HasPrefix(sessionId, base.UkPreRtmpServerSession) {
		if group.rtmpPubSession != nil && group.rtmpPubSession.UniqueKey() == sessionId {
			group.rtmpPubSession.Dispose()
			return true
		}
		for s := range group.rtmpSubSessionSet {
			if s.UniqueKey() == sessionId {
				s.Dispose()
				return true
			}
		}
	} else if strings.HasPrefix(sessionId, base.UkPreRtmpPullSession) || strings.HasPrefix(sessionId, base.UkPreRtspPullSession) {
		return group.kickPull(sessionId)
	} else if strings.HasPrefix(sessionId, base.UkPreRtspPubSession) {
		if group.rtspPubSession != nil && group.rtspPubSession.UniqueKey() == sessionId {
			group.rtspPubSession.Dispose()
			return true
		}
	} else if strings.HasPrefix(sessionId, base.UkPreFlvSubSession) {
		// TODO chef: 考虑数据结构改成sessionIdzuokey的map
		for s := range group.httpflvSubSessionSet {
			if s.UniqueKey() == sessionId {
				s.Dispose()
				return true
			}
		}
	} else if strings.HasPrefix(sessionId, base.UkPreTsSubSession) {
		for s := range group.httptsSubSessionSet {
			if s.UniqueKey() == sessionId {
				s.Dispose()
				return true
			}
		}
	} else if strings.HasPrefix(sessionId, base.UkPreRtspSubSession) {
		for s := range group.rtspSubSessionSet {
			if s.UniqueKey() == sessionId {
				s.Dispose()
				return true
			}
		}
	} else if strings.HasPrefix(sessionId, base.UkPreHlsSubSession) {
		for s := range group.hlsSubSessionSet {
			if s.UniqueKey() == sessionId {
				s.Dispose()
				Log.Infof("[%s] kick hls session success. session id=%s", group.UniqueKey, sessionId)
				return true
			}
		}
		Log.Warnf("[%s] kick hls session failed, session not found in hlsSubSessionSet. session id=%s, set size=%d", group.UniqueKey, sessionId, len(group.hlsSubSessionSet))
	} else {
		Log.Errorf("[%s] kick session while session id format invalid. %s", group.UniqueKey, sessionId)
	}

	return false
}

func (group *Group) IsInactive() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.isTotalEmpty() && !group.isPullModuleAlive()
}

func (group *Group) HasInSession() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.hasInSession()
}

func (group *Group) HasOutSession() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.hasOutSession()
}

func (group *Group) OutSessionNum() int {
	// TODO(chef): 没有包含hls的播放者

	group.mutex.Lock()
	defer group.mutex.Unlock()

	pushNum := 0
	for _, item := range group.url2PushProxy {
		// TODO(chef): [refactor] 考虑只判断session是否为nil 202205
		if item.isPushing && item.pushSession != nil {
			pushNum++
		}
	}
	return len(group.rtmpSubSessionSet) + len(group.rtspSubSessionSet) +
		len(group.httpflvSubSessionSet) + len(group.httptsSubSessionSet) + pushNum
}

// ---------------------------------------------------------------------------------------------------------------------

// disposeInactiveSessions 关闭不活跃的session
func (group *Group) disposeInactiveSessions(tickCount uint32) {
	// 以下都是以 CheckSessionAliveIntervalSec 为间隔的清理逻辑

	if tickCount%base.LogicCheckSessionAliveIntervalSec != 0 {
		return
	}

	if group.rtmpPubSession != nil {
		if readAlive, _ := group.rtmpPubSession.IsAlive(); !readAlive {
			Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, group.rtmpPubSession.UniqueKey())
			group.rtmpPubSession.Dispose()
		}
	}
	if group.rtspPubSession != nil {
		if readAlive, _ := group.rtspPubSession.IsAlive(); !readAlive {
			Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, group.rtspPubSession.UniqueKey())
			group.rtspPubSession.Dispose()
		}
	}

	group.disposeInactivePullSession()

	// 收集需要 dispose 的 session，避免在遍历时修改 map
	var toDisposeRtmp []*rtmp.ServerSession
	for session := range group.rtmpSubSessionSet {
		if session != nil {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				toDisposeRtmp = append(toDisposeRtmp, session)
			}
		}
	}
	for _, session := range toDisposeRtmp {
		session.Dispose()
	}

	var toDisposeRtsp []*rtsp.SubSession
	for session := range group.rtspSubSessionSet {
		if session != nil {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				toDisposeRtsp = append(toDisposeRtsp, session)
			}
		}
	}
	for _, session := range toDisposeRtsp {
		session.Dispose()
	}

	var toDisposeHttpflv []*httpflv.SubSession
	for session := range group.httpflvSubSessionSet {
		if session != nil {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				toDisposeHttpflv = append(toDisposeHttpflv, session)
			}
		}
	}
	for _, session := range toDisposeHttpflv {
		session.Dispose()
	}

	var toDisposeHttpts []*httpts.SubSession
	for session := range group.httptsSubSessionSet {
		if session != nil {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				toDisposeHttpts = append(toDisposeHttpts, session)
			}
		}
	}
	for _, session := range toDisposeHttpts {
		session.Dispose()
	}
	for _, item := range group.url2PushProxy {
		session := item.pushSession
		if item.isPushing && session != nil {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				session.Dispose()
			}
		}
	}
}

// updateAllSessionStat 更新所有session的状态
func (group *Group) updateAllSessionStat() {
	if group.rtmpPubSession != nil {
		group.rtmpPubSession.UpdateStat(calcSessionStatIntervalSec)
	}
	if group.rtspPubSession != nil {
		group.rtspPubSession.UpdateStat(calcSessionStatIntervalSec)
	}
	if group.customizePubSession != nil {
		group.customizePubSession.UpdateStat(calcSessionStatIntervalSec)
	}
	group.updatePullSessionStat()

	for session := range group.rtmpSubSessionSet {
		session.UpdateStat(calcSessionStatIntervalSec)
	}
	for session := range group.httpflvSubSessionSet {
		session.UpdateStat(calcSessionStatIntervalSec)
	}
	for session := range group.httptsSubSessionSet {
		session.UpdateStat(calcSessionStatIntervalSec)
	}
	for session := range group.rtspSubSessionSet {
		session.UpdateStat(calcSessionStatIntervalSec)
	}

	for _, item := range group.url2PushProxy {
		session := item.pushSession
		if item.isPushing && session != nil {
			session.UpdateStat(calcSessionStatIntervalSec)
		}
	}
}

func (group *Group) hasPubSession() bool {
	return group.rtmpPubSession != nil || group.rtspPubSession != nil || group.customizePubSession != nil
}

func (group *Group) hasSubSession() bool {
	return len(group.rtmpSubSessionSet) != 0 ||
		len(group.httpflvSubSessionSet) != 0 ||
		len(group.httptsSubSessionSet) != 0 ||
		len(group.rtspSubSessionSet) != 0 ||
		len(group.hlsSubSessionSet) != 0 ||
		group.customizeHookSessionContext != nil
}

func (group *Group) hasPushSession() bool {
	for _, item := range group.url2PushProxy {
		if item.isPushing && item.pushSession != nil {
			return true
		}
	}
	return false
}

func (group *Group) hasInSession() bool {
	return group.hasPubSession() || group.hasPullSession()
}

// hasOutSession 是否还有out往外发送音视频数据的session
func (group *Group) hasOutSession() bool {
	return group.hasSubSession() || group.hasPushSession()
}

// isTotalEmpty 当前group是否完全没有流了
func (group *Group) isTotalEmpty() bool {
	return !group.hasInSession() && !group.hasOutSession()
}

func (group *Group) inSessionUniqueKey() string {
	if group.rtmpPubSession != nil {
		return group.rtmpPubSession.UniqueKey()
	}
	if group.rtspPubSession != nil {
		return group.rtspPubSession.UniqueKey()
	}
	if group.customizePubSession != nil {
		return group.customizePubSession.UniqueKey()
	}
	return group.pullSessionUniqueKey()
}

func (group *Group) shouldStartRtspRemuxer() bool {
	return group.config.RtspConfig.Enable || group.config.RtspConfig.RtspsEnable
}

func (group *Group) shouldStartMpegtsRemuxer() bool {
	return (group.config.HlsConfig.Enable || group.config.HlsConfig.EnableHttps) ||
		(group.config.HttptsConfig.Enable || group.config.HttptsConfig.EnableHttps) ||
		group.config.RecordConfig.EnableMpegts
}

func (group *Group) OnHlsMakeTs(info base.HlsMakeTsInfo) {
	group.observer.OnHlsMakeTs(info)
}
