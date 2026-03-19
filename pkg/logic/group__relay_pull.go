// Copyright 2022, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/rtsp"

	"github.com/q191201771/lal/pkg/rtmp"
)

// StartPull 外部命令主动触发pull拉流
func (group *Group) StartPull(info base.ApiCtrlStartRelayPullReq) (string, error) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	// 手动触发时，避免被上一次失败的退避窗口影响
	group.resetPullBackoffLocked()

	group.pullProxy.apiEnable = true
	group.pullProxy.pullUrl = info.Url
	group.pullProxy.pullTimeoutMs = info.PullTimeoutMs
	group.pullProxy.pullRetryNum = info.PullRetryNum
	group.pullProxy.autoStopPullAfterNoOutMs = info.AutoStopPullAfterNoOutMs
	group.pullProxy.rtspMode = info.RtspMode
	group.pullProxy.debugDumpPacket = info.DebugDumpPacket
	group.pullProxy.scale = info.Scale

	return group.pullIfNeeded()
}

// StopPull
//
// @return 如果PullSession存在，返回它的unique key
func (group *Group) StopPull() string {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	group.pullProxy.apiEnable = false
	return group.stopPull()
}

// ---------------------------------------------------------------------------------------------------------------------

type pullProxy struct {
	staticRelayPullEnable    bool // 是否开启pull TODO(chef): refactor 这两个bool可以考虑合并成一个
	apiEnable                bool
	pullUrl                  string
	pullTimeoutMs            int
	pullRetryNum             int
	autoStopPullAfterNoOutMs int // 没有观看者时，是否自动停止pull
	rtspMode                 int
	debugDumpPacket          string
	scale                    float64 // RTSP拉流时的播放速度倍数

	startCount   int
	lastHasOutTs int64

	isSessionPulling bool // 是否正在pull，注意，这是一个内部状态，表示的是session的状态，而不是整体任务应该处于的状态
	pullingSessionId string
	nextPullTsMs     int64 // 允许下一次触发pull的最早时间（毫秒）
	backoffMs        int   // 失败退避（毫秒）
	rtmpSession      *rtmp.PullSession
	rtspSession      *rtsp.PullSession
}

const (
	// 避免断线/失败后每秒疯狂重连，造成CPU和网络抖动
	minPullBackoffMs    = 1000
	maxPullBackoffMs    = 30 * 1000
	pullBackoffJitterMs = 250
)

const (
	// 控制“握手阶段”同时在做 Start() 的拉流数量（rtmp/rtsp pull 都算）。
	//
	// 当拉流规模很大（比如400+）时，如果所有拉流同时tcp connect/handshake，
	// Windows 上很容易出现临时端口耗尽、TIME_WAIT堆积、上游accept/backlog扛不住等问题，
	// 表现为：大量拉流失败或拉到一半频繁中断。
	//
	// 通过限并发让拉流分批建立，整体成功率和稳定性会明显提升。
	defaultMaxConcurrentPullStart = 128
	// 获取并发slot的最长等待时间，避免大量goroutine永久阻塞在排队上（会影响停止/切流的及时性）。
	maxWaitPullStartSlot = 15 * time.Second
)

var pullStartSem = make(chan struct{}, defaultMaxConcurrentPullStart)

func acquirePullStartSlot(timeout time.Duration) bool {
	if timeout <= 0 {
		pullStartSem <- struct{}{}
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case pullStartSem <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func releasePullStartSlot() {
	select {
	case <-pullStartSem:
	default:
		// 理论上不应发生，防御性处理，避免因误用导致panic
	}
}

// initRelayPullByConfig 根据配置文件中的静态回源配置来初始化回源设置
func (group *Group) initRelayPullByConfig() {
	enable := group.config.StaticRelayPullConfig.Enable
	addr := group.config.StaticRelayPullConfig.Addr
	appName := group.appName
	streamName := group.streamName

	group.pullProxy = &pullProxy{
		startCount:   0,
		lastHasOutTs: time.Now().UnixNano() / 1e6,
	}

	var pullUrl string
	if enable {
		pullUrl = fmt.Sprintf("rtmp://%s/%s/%s", addr, appName, streamName)
	}

	group.pullProxy.pullUrl = pullUrl
	group.pullProxy.staticRelayPullEnable = enable
	group.pullProxy.pullTimeoutMs = StaticRelayPullTimeoutMs
	group.pullProxy.pullRetryNum = staticRelayPullRetryNum
	group.pullProxy.autoStopPullAfterNoOutMs = staticRelayPullAutoStopPullAfterNoOutMs
}

func (group *Group) resetPullBackoffLocked() {
	group.pullProxy.backoffMs = 0
	group.pullProxy.nextPullTsMs = 0
}

func (group *Group) schedulePullBackoffLocked() {
	nowMs := time.Now().UnixNano() / 1e6
	if group.pullProxy.backoffMs <= 0 {
		group.pullProxy.backoffMs = minPullBackoffMs
	} else {
		group.pullProxy.backoffMs *= 2
		if group.pullProxy.backoffMs > maxPullBackoffMs {
			group.pullProxy.backoffMs = maxPullBackoffMs
		}
	}
	// 简单jitter，避免多个group同一秒同时重连
	jitter := int64(nowMs % pullBackoffJitterMs)
	group.pullProxy.nextPullTsMs = nowMs + int64(group.pullProxy.backoffMs) + jitter
	Log.Infof("[%s] relay pull backoff. backoff_ms=%d, jitter_ms=%d", group.UniqueKey, group.pullProxy.backoffMs, jitter)
}

func (group *Group) onPullStartFailedLocked(sessionId string, pullErr error) {
	if !group.pullProxy.isSessionPulling || group.pullProxy.pullingSessionId != sessionId {
		return
	}
	group.pullProxy.isSessionPulling = false
	group.pullProxy.pullingSessionId = ""
	group.schedulePullBackoffLocked()
	_ = pullErr // 预留：未来可按错误类型分类退避策略
}

func (group *Group) onPullSessionExitedLocked(sessionId string, pullErr error) {
	// 只有当前活跃的pull session退出，才影响退避窗口
	if sessionId != group.pullSessionUniqueKey() {
		return
	}
	if pullErr == nil {
		group.resetPullBackoffLocked()
	} else {
		group.schedulePullBackoffLocked()
	}
}

func (group *Group) setRtmpPullSession(session *rtmp.PullSession) {
	group.pullProxy.rtmpSession = session
}

func (group *Group) setRtspPullSession(session *rtsp.PullSession) {
	group.pullProxy.rtspSession = session
	if group.pullProxy.debugDumpPacket != "" {
		group.rtspPullDumpFile = base.NewDumpFile()
		if err := group.rtspPullDumpFile.OpenToWrite(group.pullProxy.debugDumpPacket); err != nil {
			Log.Errorf("%+v", err)
		}
	}
}

func (group *Group) resetRelayPullSession() {
	group.pullProxy.isSessionPulling = false
	group.pullProxy.pullingSessionId = ""
	group.pullProxy.rtmpSession = nil
	group.pullProxy.rtspSession = nil
	if group.rtspPullDumpFile != nil {
		group.rtspPullDumpFile.Close()
		group.rtspPullDumpFile = nil
	}
}

func (group *Group) getStatPull() base.StatPull {
	if group.pullProxy.rtmpSession != nil {
		return base.Session2StatPull(group.pullProxy.rtmpSession)
	}
	if group.pullProxy.rtspSession != nil {
		return base.Session2StatPull(group.pullProxy.rtspSession)
	}
	return base.StatPull{}
}

func (group *Group) disposeInactivePullSession() {
	if group.pullProxy.rtmpSession != nil {
		if readAlive, _ := group.pullProxy.rtmpSession.IsAlive(); !readAlive {
			Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, group.pullProxy.rtmpSession.UniqueKey())
			group.pullProxy.rtmpSession.Dispose()
		}
	}
	if group.pullProxy.rtspSession != nil {
		if readAlive, _ := group.pullProxy.rtspSession.IsAlive(); !readAlive {
			Log.Warnf("[%s] session timeout. session=%s", group.UniqueKey, group.pullProxy.rtspSession.UniqueKey())
			group.pullProxy.rtspSession.Dispose()
		}
	}
}

func (group *Group) updatePullSessionStat() {
	if group.pullProxy.rtmpSession != nil {
		group.pullProxy.rtmpSession.UpdateStat(calcSessionStatIntervalSec)
	}
	if group.pullProxy.rtspSession != nil {
		group.pullProxy.rtspSession.UpdateStat(calcSessionStatIntervalSec)
	}
}

func (group *Group) isPullModuleAlive() bool {
	if group.hasPullSession() || group.pullProxy.isSessionPulling {
		return true
	}

	// 关键：即便当前处于backoff窗口，只要pull配置是开启的，并且未来仍会继续尝试拉流（例如永久重试），
	// 就应视为“存活”，否则Group会被ServerManager当作Inactive直接回收，导致控制台流消失。
	if group.pullProxy != nil {
		if (group.pullProxy.staticRelayPullEnable || group.pullProxy.apiEnable) &&
			group.pullProxy.pullUrl != "" &&
			!group.shouldAutoStopPull() {
			// 检查重试次数是否已达上限。-1表示永远重试。
			if group.pullProxy.pullRetryNum < 0 || group.pullProxy.startCount <= group.pullProxy.pullRetryNum {
				return true
			}
		}
	}

	flag, _ := group.shouldStartPull()
	return flag
}

func (group *Group) tickPullModule() {
	if group.hasSubSession() {
		group.pullProxy.lastHasOutTs = time.Now().UnixNano() / 1e6
	}

	if group.shouldAutoStopPull() {
		group.stopPull()
	} else {
		group.pullIfNeeded()
	}
}

func (group *Group) hasPullSession() bool {
	return group.pullProxy.rtmpSession != nil || group.pullProxy.rtspSession != nil
}

func (group *Group) pullSessionUniqueKey() string {
	if group.pullProxy.rtmpSession != nil {
		return group.pullProxy.rtmpSession.UniqueKey()
	}
	if group.pullProxy.rtspSession != nil {
		return group.pullProxy.rtspSession.UniqueKey()
	}
	return ""
}

// kickPull
//
// @return 返回true，表示找到对应的session，并关闭
func (group *Group) kickPull(sessionId string) bool {
	if (group.pullProxy.rtmpSession != nil && group.pullProxy.rtmpSession.UniqueKey() == sessionId) ||
		(group.pullProxy.rtspSession != nil && group.pullProxy.rtspSession.UniqueKey() == sessionId) {
		group.pullProxy.apiEnable = false
		group.stopPull()
		return true
	}
	return false
}

// 判断是否需要pull从远端拉流至本地，如果需要，则触发pull
//
// 当前调用时机：
// 1. 添加新sub session
// 2. 外部命令，比如http api
// 3. 定时器，比如pull的连接断了，通过定时器可以重启触发pull
func (group *Group) pullIfNeeded() (string, error) {
	if flag, err := group.shouldStartPull(); !flag {
		return "", err
	}

	Log.Infof("[%s] start relay pull. url=%s", group.UniqueKey, group.pullProxy.pullUrl)

	group.pullProxy.isSessionPulling = true

	isPullByRtmp := strings.HasPrefix(group.pullProxy.pullUrl, "rtmp")

	var rtmpSession *rtmp.PullSession
	var rtspSession *rtsp.PullSession
	var uk string

	if isPullByRtmp {
		rtmpSession = rtmp.NewPullSession(func(option *rtmp.PullSessionOption) {
			option.PullTimeoutMs = group.pullProxy.pullTimeoutMs
		}).WithOnPullSucc(func() {
			err := group.AddRtmpPullSession(rtmpSession)
			if err != nil {
				rtmpSession.Dispose()
				return
			}
		}).WithOnReadRtmpAvMsg(group.OnReadRtmpAvMsg)

		uk = rtmpSession.UniqueKey()
	} else {
		rtspSession = rtsp.NewPullSession(group, func(option *rtsp.PullSessionOption) {
			option.PullTimeoutMs = group.pullProxy.pullTimeoutMs
			option.OverTcp = group.pullProxy.rtspMode == 0
			option.Scale = group.pullProxy.scale
		}).WithOnDescribeResponse(func() {
			err := group.AddRtspPullSession(rtspSession)
			if err != nil {
				rtspSession.Dispose()
				return
			}
		})

		uk = rtspSession.UniqueKey()
	}

	// 记录attempt id，保证错误路径能正确清理状态，避免“卡死不再拉流”
	group.pullProxy.pullingSessionId = uk

	go func(rtPullUrl string, rtIsPullByRtmp bool, rtRtmpSession *rtmp.PullSession, rtRtspSession *rtsp.PullSession) {
		defer func() {
			if r := recover(); r != nil {
				Log.Errorf("[%s] relay pull goroutine panic recovered, panic=%+v", uk, r)
				releasePullStartSlot()
				if rtRtmpSession != nil {
					rtRtmpSession.Dispose()
					group.DelRtmpPullSession(rtRtmpSession)
				}
				if rtRtspSession != nil {
					rtRtspSession.Dispose()
				}
				group.mutex.Lock()
				group.onPullSessionExitedLocked(uk, fmt.Errorf("panic recovered: %v", r))
				group.mutex.Unlock()
			}
		}()
		// 全局限并发：只控制 Start 握手阶段的并发数量，不限制拉流总路数
		acquired := false
		if ok := acquirePullStartSlot(maxWaitPullStartSlot); !ok {
			// 排队超时：说明系统处于高压（或上游长期不可用），不要一直堵在这里
			err := errors.New("wait pull start slot timeout")
			Log.Warnf("[%s] relay pull start blocked too long, abort attempt. err=%v", uk, err)
			group.mutex.Lock()
			group.onPullStartFailedLocked(uk, err)
			group.mutex.Unlock()
			if rtRtmpSession != nil {
				rtRtmpSession.Dispose()
			}
			if rtRtspSession != nil {
				rtRtspSession.Dispose()
			}
			return
		}
		acquired = true
		defer func() {
			if acquired {
				releasePullStartSlot()
			}
		}()

		// 可能在排队期间被StopPull/autoStop取消，这里二次确认
		group.mutex.Lock()
		if !group.pullProxy.isSessionPulling || group.pullProxy.pullingSessionId != uk {
			group.mutex.Unlock()
			// 已被取消或被新attempt覆盖，直接返回
			// 注意：此时 session 已创建但尚未 Start/Add 进 group，需要主动 Dispose，避免 stop/start 频繁时累积对象。
			if rtRtmpSession != nil {
				rtRtmpSession.Dispose()
			}
			if rtRtspSession != nil {
				rtRtspSession.Dispose()
			}
			return
		}
		// 真正开始Start，才计入尝试次数（用于重试上限判断）
		group.pullProxy.startCount++
		group.mutex.Unlock()

		if rtIsPullByRtmp {
			// TODO(chef): 处理数据回调，是否应该等待Add成功之后。避免竞态条件中途加入了其他in session
			err := rtRtmpSession.Start(rtPullUrl)
			// Start 阶段结束，释放并发slot（由 defer 统一释放）
			acquired = false
			if err != nil {
				Log.Errorf("[%s] relay pull fail. err=%v", rtRtmpSession.UniqueKey(), err)
				group.mutex.Lock()
				group.onPullStartFailedLocked(rtRtmpSession.UniqueKey(), err)
				group.mutex.Unlock()
				return
			}

			err = <-rtRtmpSession.WaitChan()
			Log.Infof("[%s] relay pull done. err=%v", rtRtmpSession.UniqueKey(), err)
			group.mutex.Lock()
			group.onPullSessionExitedLocked(rtRtmpSession.UniqueKey(), err)
			group.mutex.Unlock()
			group.DelRtmpPullSession(rtRtmpSession)
			return
		}

		err := rtRtspSession.Start(rtPullUrl)
		// Start 阶段结束，释放并发slot（由 defer 统一释放）
		acquired = false
		if err != nil {
			Log.Errorf("[%s] relay pull fail. err=%v", rtRtspSession.UniqueKey(), err)
			group.mutex.Lock()
			group.onPullStartFailedLocked(rtRtspSession.UniqueKey(), err)
			group.mutex.Unlock()
			return
		}

		err = <-rtRtspSession.WaitChan()
		Log.Infof("[%s] relay pull done. err=%v", rtRtspSession.UniqueKey(), err)
		group.mutex.Lock()
		group.onPullSessionExitedLocked(rtRtspSession.UniqueKey(), err)
		group.mutex.Unlock()
		group.DelRtspPullSession(rtRtspSession)
		return
	}(group.pullProxy.pullUrl, isPullByRtmp, rtmpSession, rtspSession)

	return uk, nil
}

func (group *Group) stopPull() string {
	// 关闭时，清空用于重试的计数
	group.pullProxy.startCount = 0
	// 若当前仅处于attempt阶段（还没Add进group），也要清理状态，避免在高并发排队时卡死不再拉流
	group.pullProxy.isSessionPulling = false
	group.pullProxy.pullingSessionId = ""
	// stop属于“正常行为”，清理退避窗口，避免影响下一次手动/自动拉流
	group.resetPullBackoffLocked()

	if group.pullProxy.rtmpSession != nil {
		Log.Infof("[%s] stop pull session.", group.UniqueKey)
		group.pullProxy.rtmpSession.Dispose()
		return group.pullProxy.rtmpSession.UniqueKey()
	}
	if group.pullProxy.rtspSession != nil {
		Log.Infof("[%s] stop pull session.", group.UniqueKey)
		group.pullProxy.rtspSession.Dispose()
		return group.pullProxy.rtspSession.UniqueKey()
	}
	return ""
}

func (group *Group) shouldStartPull() (bool, error) {
	// 退避窗口内不触发pull，避免频繁重连导致抖动
	nowMs := time.Now().UnixNano() / 1e6
	if group.pullProxy.nextPullTsMs != 0 && nowMs < group.pullProxy.nextPullTsMs {
		return false, errors.New("relay pull in backoff")
	}

	// 如果本地已经有输入型的流，就不需要pull了
	if group.hasInSession() {
		return false, base.ErrDupInStream
	}

	// 已经在pull中，就不需要pull了
	if group.pullProxy.isSessionPulling {
		return false, base.ErrDupInStream
	}

	if !group.pullProxy.staticRelayPullEnable && !group.pullProxy.apiEnable {
		return false, errors.New("relay pull not enable")
	}

	// 没人观看自动停的逻辑，是否满足并且需要触发
	if group.shouldAutoStopPull() {
		return false, errors.New("should auto stop pull")
	}

	// 检查重试次数
	if group.pullProxy.pullRetryNum >= 0 {
		if group.pullProxy.startCount > group.pullProxy.pullRetryNum {
			return false, errors.New("relay pull retry limited")
		}
	} else {
		// 负数永远都重试
	}

	return true, nil
}

// shouldAutoStopPull 是否需要自动停，根据没人观看停的逻辑
func (group *Group) shouldAutoStopPull() bool {
	// 没开启
	if group.pullProxy.autoStopPullAfterNoOutMs < 0 {
		return false
	}

	// 还有观众
	if group.hasOutSession() {
		return false
	}

	// 没有观众，并且设置为立即关闭
	if group.pullProxy.autoStopPullAfterNoOutMs == 0 {
		return true
	}

	// 是否达到时间阈值
	return group.pullProxy.lastHasOutTs != -1 && time.Now().UnixNano()/1e6-group.pullProxy.lastHasOutTs >= int64(group.pullProxy.autoStopPullAfterNoOutMs)
}
