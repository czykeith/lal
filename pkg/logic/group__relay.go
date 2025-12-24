// Copyright 2024, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/rtmp"
	"github.com/q191201771/lal/pkg/rtsp"
)

// relayProxy 转推代理，管理拉流和推流
type relayProxy struct {
	mutex sync.Mutex

	// 拉流相关
	pullUrl                  string
	timeoutMs                int // 拉流和推流的超时时间
	retryNum                 int // 拉流和推流的重试次数
	autoStopPullAfterNoOutMs int
	rtspMode                 int
	debugDumpPacket          string
	pullSessionId            string // 拉流 session ID

	// 推流相关
	pushUrl        string
	pushStartCount int    // 推流启动次数，用于重试计数
	pushSessionId  string // 推流 session ID

	// 状态
	isRelaying      bool
	rtmpPushSession *rtmp.PushSession
	rtspPushSession *rtsp.PushSession
}

// StartRelay 启动转推
func (group *Group) StartRelay(info base.ApiCtrlStartRelayReq) (string, string, error) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	// 如果已经有转推任务在运行（同一个 streamName），直接返回已有信息
	if group.relayProxy != nil && group.relayProxy.isRelaying {
		Log.Infof("[%s] relay already exists for streamName. pull_url=%s, push_url=%s, pull_session_id=%s, push_session_id=%s",
			group.UniqueKey, group.relayProxy.pullUrl, group.relayProxy.pushUrl, group.relayProxy.pullSessionId, group.relayProxy.pushSessionId)
		return group.relayProxy.pullSessionId, group.relayProxy.pushSessionId, nil
	}

	// 创建转推代理
	// 注意：转推模式下不进行数据落盘，debugDumpPacket 强制设置为空字符串
	group.relayProxy = &relayProxy{
		pullUrl:                  info.PullUrl,
		timeoutMs:                info.TimeoutMs,
		retryNum:                 info.RetryNum,
		autoStopPullAfterNoOutMs: info.AutoStopPullAfterNoOutMs,
		rtspMode:                 info.RtspMode,
		debugDumpPacket:          "", // 转推模式下不落盘，直接转发
		pushUrl:                  info.PushUrl,
		pushStartCount:           0,
		isRelaying:               false,
	}

	// 设置默认值
	if group.relayProxy.timeoutMs == 0 {
		group.relayProxy.timeoutMs = DefaultApiCtrlStartRelayPullReqPullTimeoutMs
	}
	if group.relayProxy.retryNum == 0 {
		group.relayProxy.retryNum = base.PullRetryNumNever
	}
	// 转推模式下，auto_stop_pull_after_no_out_ms 强制设置为 -1（不自动停止）
	group.relayProxy.autoStopPullAfterNoOutMs = base.AutoStopPullAfterNoOutMsNever

	// 启动拉流
	// 转推模式下，auto_stop_pull_after_no_out_ms 强制设置为 -1（不自动停止）
	// 因为转推的目的是推送到远程服务器，而不是为了本地观看

	// 设置拉流参数（需要在释放锁前设置）
	group.pullProxy.apiEnable = true
	group.pullProxy.pullUrl = info.PullUrl
	group.pullProxy.pullTimeoutMs = group.relayProxy.timeoutMs
	group.pullProxy.pullRetryNum = group.relayProxy.retryNum
	group.pullProxy.autoStopPullAfterNoOutMs = base.AutoStopPullAfterNoOutMsNever // 转推模式下强制不自动停止
	group.pullProxy.rtspMode = group.relayProxy.rtspMode
	// 转推模式下，不进行数据落盘，强制设置为空字符串
	group.pullProxy.debugDumpPacket = ""

	// 调用 pullIfNeeded 启动拉流（不获取锁，因为我们已经持有锁）
	pullSessionId, err := group.pullIfNeeded()
	if err != nil {
		return "", "", fmt.Errorf("start pull failed: %w", err)
	}
	group.relayProxy.pullSessionId = pullSessionId

	// 启动推流（推流会从 Group 的 broadcastByRtmpMsg 获取数据）
	pushSessionId, err := group.startPushLocked(info.PushUrl, group.relayProxy.timeoutMs)
	if err != nil {
		// 如果推流失败，停止拉流（需要在释放锁后调用）
		group.pullProxy.apiEnable = false
		group.stopPull() // 使用不获取锁的内部方法
		return "", "", fmt.Errorf("start push failed: %w", err)
	}
	group.relayProxy.pushSessionId = pushSessionId
	group.relayProxy.isRelaying = true

	Log.Infof("[%s] start relay. pull_url=%s, push_url=%s, pull_session_id=%s, push_session_id=%s",
		group.UniqueKey, info.PullUrl, info.PushUrl, pullSessionId, pushSessionId)

	return pullSessionId, pushSessionId, nil
}

// StopRelay 停止转推
func (group *Group) StopRelay() (string, string) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	return group.stopRelayLocked()
}

func (group *Group) stopRelayLocked() (string, string) {
	if group.relayProxy == nil || !group.relayProxy.isRelaying {
		return "", ""
	}

	pullSessionId := group.relayProxy.pullSessionId
	pushSessionId := group.relayProxy.pushSessionId

	// 停止拉流（使用不获取锁的内部方法，因为我们已经持有锁）
	if pullSessionId != "" {
		group.pullProxy.apiEnable = false
		group.stopPull() // 使用不获取锁的内部方法
	}

	// 停止推流
	if group.relayProxy.rtmpPushSession != nil {
		group.relayProxy.rtmpPushSession.Dispose()
		group.relayProxy.rtmpPushSession = nil
	}
	if group.relayProxy.rtspPushSession != nil {
		group.relayProxy.rtspPushSession.Dispose()
		group.relayProxy.rtspPushSession = nil
	}

	group.relayProxy.isRelaying = false
	group.relayProxy.pullSessionId = ""
	group.relayProxy.pushSessionId = ""
	group.relayProxy.pushStartCount = 0 // 重置推流重试计数

	Log.Infof("[%s] stop relay. pull_session_id=%s, push_session_id=%s",
		group.UniqueKey, pullSessionId, pushSessionId)

	return pullSessionId, pushSessionId
}

// startPushLocked 启动推流（内部方法，需要持有锁）
// 推流会从 Group 的 broadcastByRtmpMsg 获取数据
func (group *Group) startPushLocked(pushUrl string, timeoutMs int) (string, error) {
	if strings.HasPrefix(pushUrl, "rtmp://") {
		return group.startRtmpPushLocked(pushUrl, timeoutMs)
	} else if strings.HasPrefix(pushUrl, "rtsp://") {
		return group.startRtspPushLocked(pushUrl, timeoutMs)
	}
	return "", fmt.Errorf("unsupported push url protocol: %s", pushUrl)
}

// startRtmpPushLocked 启动 RTMP 推流
func (group *Group) startRtmpPushLocked(pushUrl string, timeoutMs int) (string, error) {
	pushSession := rtmp.NewPushSession(func(option *rtmp.PushSessionOption) {
		option.PushTimeoutMs = timeoutMs
		option.WriteAvTimeoutMs = RelayPushWriteAvTimeoutMs
	})

	// 在启动前增加计数（包括首次启动和重试）
	group.relayProxy.pushStartCount++

	err := pushSession.Start(pushUrl)
	if err != nil {
		// 如果启动失败，不减少计数，因为这次尝试已经计入
		return "", err
	}

	group.relayProxy.rtmpPushSession = pushSession
	sessionId := pushSession.UniqueKey()

	// 将推流 session 添加到 url2PushProxy，这样 broadcastByRtmpMsg 会自动转发数据
	if group.url2PushProxy == nil {
		group.url2PushProxy = make(map[string]*pushProxy)
	}
	group.url2PushProxy[pushUrl] = &pushProxy{
		isPushing:   true,
		pushSession: pushSession,
	}

	// 如果 GOP 缓存中已有序列头信息，立即发送给新的推流 session
	// 这样确保推流 session 能够立即开始推送完整的流数据
	if group.rtmpGopCache.MetadataEnsureWithSetDataFrame != nil {
		_ = pushSession.Write(group.rtmpGopCache.MetadataEnsureWithSetDataFrame)
	}
	if group.rtmpGopCache.VideoSeqHeader != nil {
		_ = pushSession.Write(group.rtmpGopCache.VideoSeqHeader)
	}
	if group.rtmpGopCache.AacSeqHeader != nil {
		_ = pushSession.Write(group.rtmpGopCache.AacSeqHeader)
	}
	// 发送 GOP 缓存中的所有数据
	for i := 0; i < group.rtmpGopCache.GetGopCount(); i++ {
		for _, item := range group.rtmpGopCache.GetGopDataAt(i) {
			_ = pushSession.Write(item)
		}
	}
	// 标记为非新鲜 session，避免在 broadcastByRtmpMsg 中重复发送
	pushSession.IsFresh = false

	// 监听推流结束，并处理重试
	go func() {
		err := <-pushSession.WaitChan()
		Log.Infof("[%s] rtmp push session done. session_id=%s, err=%v", group.UniqueKey, sessionId, err)

		group.mutex.Lock()

		// 清理当前推流 session
		if group.relayProxy != nil && group.relayProxy.rtmpPushSession != nil && group.relayProxy.rtmpPushSession.UniqueKey() == sessionId {
			group.relayProxy.rtmpPushSession = nil
		}
		if group.url2PushProxy != nil {
			delete(group.url2PushProxy, pushUrl)
		}

		// 如果转推任务还在运行，无论推流是正常断开还是异常断开，都尝试重试
		// 这样可以确保当目标服务器重启后，转推能够自动恢复
		var shouldRetry bool
		if group.relayProxy != nil && group.relayProxy.isRelaying {
			// 检查是否需要重试
			// pushStartCount 已经在 startRtmpPushLocked 中增加了，所以这里需要检查是否超过限制
			if group.relayProxy.retryNum < 0 {
				// -1 表示永远重试
				shouldRetry = true
			} else if group.relayProxy.retryNum > 0 {
				// 大于0表示重试次数，pushStartCount 已经包含了当前失败的这次，所以需要 <=
				if group.relayProxy.pushStartCount <= group.relayProxy.retryNum {
					shouldRetry = true
				}
			}

			if shouldRetry {
				if err != nil {
					Log.Infof("[%s] push session disconnected with error, retry push. err=%v, retry_count=%d, max_retry=%d",
						group.UniqueKey, err, group.relayProxy.pushStartCount, group.relayProxy.retryNum)
				} else {
					Log.Infof("[%s] push session disconnected normally, retry push. retry_count=%d, max_retry=%d",
						group.UniqueKey, group.relayProxy.pushStartCount, group.relayProxy.retryNum)
				}
			} else {
				Log.Warnf("[%s] push retry limit reached. retry_count=%d, max_retry=%d",
					group.UniqueKey, group.relayProxy.pushStartCount, group.relayProxy.retryNum)
			}
		}
		group.mutex.Unlock()

		// 在锁外执行重试，避免阻塞
		if shouldRetry {
			// 等待一段时间后重试（避免立即重试导致资源浪费和频繁连接）
			// 给目标服务器一些时间恢复
			time.Sleep(2 * time.Second)

			// 循环重试，直到成功或达到重试限制
			for {
				group.mutex.Lock()
				// 检查转推任务是否还在运行
				if group.relayProxy == nil || !group.relayProxy.isRelaying {
					group.mutex.Unlock()
					Log.Infof("[%s] relay task stopped, abort retry push", group.UniqueKey)
					break
				}

				// 检查是否还需要重试
				canRetry := false
				if group.relayProxy.retryNum < 0 {
					canRetry = true
				} else if group.relayProxy.retryNum > 0 {
					if group.relayProxy.pushStartCount <= group.relayProxy.retryNum {
						canRetry = true
					}
				}

				if !canRetry {
					Log.Warnf("[%s] push retry limit reached, abort retry. retry_count=%d, max_retry=%d",
						group.UniqueKey, group.relayProxy.pushStartCount, group.relayProxy.retryNum)
					group.mutex.Unlock()
					break
				}

				// 尝试启动推流
				retryPushUrl := group.relayProxy.pushUrl
				retryPushTimeoutMs := group.relayProxy.timeoutMs
				newSessionId, retryErr := group.startRtmpPushLocked(retryPushUrl, retryPushTimeoutMs)

				if retryErr != nil {
					// 启动失败，继续重试
					Log.Warnf("[%s] retry push failed, will retry again. err=%v, retry_count=%d",
						group.UniqueKey, retryErr, group.relayProxy.pushStartCount)
					group.mutex.Unlock()
					// 等待一段时间后继续重试
					time.Sleep(2 * time.Second)
					continue
				} else {
					// 启动成功
					group.relayProxy.pushSessionId = newSessionId
					Log.Infof("[%s] retry push success. new_session_id=%s", group.UniqueKey, newSessionId)
					group.mutex.Unlock()
					break
				}
			}
		}
	}()

	return sessionId, nil
}

// startRtspPushLocked 启动 RTSP 推流
func (group *Group) startRtspPushLocked(pushUrl string, timeoutMs int) (string, error) {
	// RTSP 推流需要 SDP，需要等待拉流建立后才能获取
	// 这里先创建 session，等拉流建立后再启动推流
	pushSession := rtsp.NewPushSession(func(option *rtsp.PushSessionOption) {
		option.PushTimeoutMs = timeoutMs
	})

	group.relayProxy.rtspPushSession = pushSession
	sessionId := pushSession.UniqueKey()

	// RTSP 推流需要等待拉流建立并获取 SDP 后才能启动
	// 这里先返回 session ID，实际的推流启动会在拉流建立后触发
	// TODO: 实现 RTSP 推流的完整逻辑

	return sessionId, nil
}
