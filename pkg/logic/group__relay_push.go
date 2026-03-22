// Copyright 2022, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"fmt"

	"github.com/q191201771/lal/pkg/rtmp"
)

// TODO(chef): [refactor] 参照relay pull，整体重构一次relay push 202205

func (group *Group) AddRtmpPushSession(url string, session *rtmp.PushSession) {
	Log.Debugf("[%s] [%s] add rtmp PushSession into group.", group.UniqueKey, session.UniqueKey())
	group.mutex.Lock()
	defer group.mutex.Unlock()
	if group.url2PushProxy != nil {
		group.url2PushProxy[url].pushSession = session
	}
}

func (group *Group) DelRtmpPushSession(url string, session *rtmp.PushSession) {
	Log.Debugf("[%s] [%s] del rtmp PushSession into group.", group.UniqueKey, session.UniqueKey())
	group.mutex.Lock()
	defer group.mutex.Unlock()
	if group.url2PushProxy != nil {
		group.url2PushProxy[url].pushSession = nil
		group.url2PushProxy[url].isPushing = false
	}
}

// ---------------------------------------------------------------------------------------------------------------------

type pushProxy struct {
	isPushing   bool
	pushSession *rtmp.PushSession
}

func (group *Group) initRelayPushByConfig() {
	enable := group.config.RelayPushConfig.Enable
	addrList := group.config.RelayPushConfig.AddrList
	appName := group.appName
	streamName := group.streamName

	url2PushProxy := make(map[string]*pushProxy)
	if enable {
		for _, addr := range addrList {
			pushUrl := fmt.Sprintf("rtmp://%s/%s/%s", addr, appName, streamName)
			url2PushProxy[pushUrl] = &pushProxy{
				isPushing:   false,
				pushSession: nil,
			}
		}
	}

	group.pushEnable = group.config.RelayPushConfig.Enable
	group.url2PushProxy = url2PushProxy
}

// startPushIfNeeded 必要时进行replay push转推
func (group *Group) startPushIfNeeded() {
	// push转推功能没开
	if !group.pushEnable {
		return
	}
	// 没有pub发布者
	// TODO(chef): [refactor] 判断所有pub是否存在的方式 202208
	if group.rtmpPubSession == nil && group.rtspPubSession == nil {
		return
	}

	// relay push时携带rtmp pub的参数
	// TODO chef: 这个逻辑放这里不太好看
	var urlParam string
	if group.rtmpPubSession != nil {
		urlParam = group.rtmpPubSession.RawQuery()
	}

	for url, v := range group.url2PushProxy {
		// 正在转推中
		if v.isPushing {
			continue
		}
		v.isPushing = true

		urlWithParam := url
		if urlParam != "" {
			urlWithParam += "?" + urlParam
		}
		Log.Infof("[%s] start relay push. url=%s", group.UniqueKey, urlWithParam)

		go func(u, u2 string) {
			var pushSession *rtmp.PushSession
			acquired := false
			releaseSlotIfNeeded := func() {
				if acquired {
					releasePullStartSlot()
					acquired = false
				}
			}
			// 排队超时或 panic 在尚未 Add 时，需把 isPushing 复位，否则该目标 URL 永远不会再触发转推
			clearPushingFlag := func() {
				group.mutex.Lock()
				if group.url2PushProxy != nil {
					if p := group.url2PushProxy[u]; p != nil {
						p.isPushing = false
					}
				}
				group.mutex.Unlock()
			}
			defer func() {
				if r := recover(); r != nil {
					Log.Errorf("[%s] relay push goroutine panic recovered, url=%s panic=%+v", group.UniqueKey, u2, r)
					if pushSession != nil {
						group.DelRtmpPushSession(u, pushSession)
					} else {
						clearPushingFlag()
					}
				}
				releaseSlotIfNeeded()
			}()
			// 与回源拉流共用出站握手槽位（relay_pull_concurrency），避免大量静态 relay_push 同时 TCP/RTMP 握手打满本机端口或压垮上游
			if ok := acquirePullStartSlot(maxWaitPullStartSlot); !ok {
				Log.Warnf("[%s] relay push: wait outbound handshake slot timeout, abort. url=%s", group.UniqueKey, u2)
				clearPushingFlag()
				return
			}
			acquired = true

			pushSession = rtmp.NewPushSession(func(option *rtmp.PushSessionOption) {
				option.PushTimeoutMs = RelayPushTimeoutMs
				option.WriteAvTimeoutMs = RelayPushWriteAvTimeoutMs
			})
			err := pushSession.Start(u2)
			// 握手阶段结束立即释放槽位（与 relay pull 一致，不占用到推流会话结束）
			releaseSlotIfNeeded()

			if err != nil {
				Log.Errorf("[%s] relay push done. err=%v", pushSession.UniqueKey(), err)
				// Start 失败时，确保释放底层资源，避免在目标不可达/频繁触发时累积。
				_ = pushSession.Dispose()
				group.DelRtmpPushSession(u, pushSession)
				return
			}
			group.AddRtmpPushSession(u, pushSession)
			err = <-pushSession.WaitChan()
			Log.Infof("[%s] relay push done. err=%v", pushSession.UniqueKey(), err)
			// WaitChan 返回后也主动 Dispose 一次，确保连接与内部 goroutine 及时退出（幂等）。
			_ = pushSession.Dispose()
			group.DelRtmpPushSession(u, pushSession)
		}(url, urlWithParam)
	}
}

func (group *Group) stopPushIfNeeded() {
	if !group.pushEnable {
		return
	}
	for _, v := range group.url2PushProxy {
		if v.pushSession != nil {
			v.pushSession.Dispose()
		}
		v.pushSession = nil
	}
}
