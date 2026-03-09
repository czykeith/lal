// Copyright 2022, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"github.com/q191201771/naza/pkg/nazalog"
	"time"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/remux"
	"github.com/q191201771/lal/pkg/rtmp"
	"github.com/q191201771/lal/pkg/rtsp"
)

func (group *Group) AddCustomizePubSession(streamName string) (ICustomizePubSessionContext, error) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.hasInSession() {
		Log.Errorf("[%s] in stream already exist at group. add customize pub session, exist=%s",
			group.UniqueKey, group.inSessionUniqueKey())
		return nil, base.ErrDupInStream
	}

	group.customizePubSession = NewCustomizePubSessionContext(streamName)
	Log.Debugf("[%s] [%s] add customize pub session into group.", group.UniqueKey, group.customizePubSession.UniqueKey())

	group.addIn()

	if group.shouldStartRtspRemuxer() {
		group.rtmp2RtspRemuxer = remux.NewRtmp2RtspRemuxer(
			group.onSdpFromRemux,
			group.onRtpPacketFromRemux,
		)
	}

	group.customizePubSession.WithOnRtmpMsg(group.OnReadRtmpAvMsg)

	return group.customizePubSession, nil
}

func (group *Group) AddRtmpPubSession(session *rtmp.ServerSession) error {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.hasInSession() {
		Log.Errorf("[%s] in stream already exist at group. add=%s, exist=%s",
			group.UniqueKey, session.UniqueKey(), group.inSessionUniqueKey())
		return base.ErrDupInStream
	}

	Log.Debugf("[%s] [%s] add rtmp pub session into group.", group.UniqueKey, session.UniqueKey())

	group.rtmpPubSession = session
	group.addIn()

	if group.shouldStartRtspRemuxer() {
		group.rtmp2RtspRemuxer = remux.NewRtmp2RtspRemuxer(
			group.onSdpFromRemux,
			group.onRtpPacketFromRemux,
		)
	}

	session.SetPubSessionObserver(group)

	return nil
}

// AddRtspPubSession TODO chef: rtsp package中，增加回调返回值判断，如果是false，将连接关掉
func (group *Group) AddRtspPubSession(session *rtsp.PubSession) error {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.hasInSession() {
		Log.Errorf("[%s] in stream already exist at group. wanna add=%s", group.UniqueKey, session.UniqueKey())
		return base.ErrDupInStream
	}

	Log.Debugf("[%s] [%s] add RTSP PubSession into group.", group.UniqueKey, session.UniqueKey())

	group.rtspPubSession = session
	group.addIn()

	group.rtsp2RtmpRemuxer = remux.NewAvPacket2RtmpRemuxer().WithOnRtmpMsg(group.onRtmpMsgFromRemux)
	session.SetObserver(group)

	return nil
}

// StartRtpPub 已废弃：RTP 收流仅通过 GB28181 Invite（lalmax）接入，请使用 /api/ctrl/gb28181_invite
func (group *Group) StartRtpPub(req base.ApiCtrlStartRtpPubReq) (ret base.ApiCtrlStartRtpPubResp) {
	ret.ErrorCode = base.ErrorCodeListenUdpPortFail
	ret.Desp = "RTP pub only supported via GB28181 invite, use /api/ctrl/gb28181_invite"
	return
}

// StopRtpPub 已废弃：与 StartRtpPub 配套，现由 GB28181 Bye 处理
func (group *Group) StopRtpPub(streamName string) string {
	return ""
}

func (group *Group) AddRtmpPullSession(session *rtmp.PullSession) error {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.hasInSession() {
		Log.Errorf("[%s] in stream already exist. wanna add=%s", group.UniqueKey, session.UniqueKey())
		// 可能是pull过程中的竞态（例如同时有pub进来），避免isSessionPulling卡死
		if group.pullProxy != nil && group.pullProxy.isSessionPulling && group.pullProxy.pullingSessionId == session.UniqueKey() {
			group.pullProxy.isSessionPulling = false
			group.pullProxy.pullingSessionId = ""
		}
		return base.ErrDupInStream
	}

	Log.Debugf("[%s] [%s] add PullSession into group.", group.UniqueKey, session.UniqueKey())

	// Pull成功进入in session后，清理“正在pull”的attempt状态，并重置退避
	if group.pullProxy != nil && group.pullProxy.isSessionPulling && group.pullProxy.pullingSessionId == session.UniqueKey() {
		group.pullProxy.isSessionPulling = false
		group.pullProxy.pullingSessionId = ""
		group.resetPullBackoffLocked()
	}

	group.setRtmpPullSession(session)
	group.addIn()

	if group.shouldStartRtspRemuxer() {
		group.rtmp2RtspRemuxer = remux.NewRtmp2RtspRemuxer(
			group.onSdpFromRemux,
			group.onRtpPacketFromRemux,
		)
	}

	var info base.PullStartInfo
	info.SessionId = session.UniqueKey()
	info.Url = session.Url()
	info.Protocol = session.GetStat().Protocol
	info.RemoteAddr = session.GetStat().RemoteAddr
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.UrlParam = session.RawQuery()
	info.HasInSession = group.hasInSession()
	info.HasOutSession = group.hasOutSession()
	group.observer.OnRelayPullStart(info)

	return nil
}

func (group *Group) AddRtspPullSession(session *rtsp.PullSession) error {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.hasInSession() {
		Log.Errorf("[%s] in stream already exist. wanna add=%s", group.UniqueKey, session.UniqueKey())
		// 可能是pull过程中的竞态（例如同时有pub进来），避免isSessionPulling卡死
		if group.pullProxy != nil && group.pullProxy.isSessionPulling && group.pullProxy.pullingSessionId == session.UniqueKey() {
			group.pullProxy.isSessionPulling = false
			group.pullProxy.pullingSessionId = ""
		}
		return base.ErrDupInStream
	}

	Log.Debugf("[%s] [%s] add PullSession into group.", group.UniqueKey, session.UniqueKey())

	// Pull成功进入in session后，清理“正在pull”的attempt状态，并重置退避
	if group.pullProxy != nil && group.pullProxy.isSessionPulling && group.pullProxy.pullingSessionId == session.UniqueKey() {
		group.pullProxy.isSessionPulling = false
		group.pullProxy.pullingSessionId = ""
		group.resetPullBackoffLocked()
	}

	group.setRtspPullSession(session)
	group.addIn()

	group.rtsp2RtmpRemuxer = remux.NewAvPacket2RtmpRemuxer().WithOnRtmpMsg(group.onRtmpMsgFromRemux)

	var info base.PullStartInfo
	info.SessionId = session.UniqueKey()
	info.Url = session.Url()
	info.Protocol = session.GetStat().Protocol
	info.RemoteAddr = session.GetStat().RemoteAddr
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.UrlParam = session.RawQuery()
	info.HasInSession = group.hasInSession()
	info.HasOutSession = group.hasOutSession()
	group.observer.OnRelayPullStart(info)

	return nil
}

// ---------------------------------------------------------------------------------------------------------------------

func (group *Group) DelCustomizePubSession(sessionCtx ICustomizePubSessionContext) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delCustomizePubSession(sessionCtx)
}

func (group *Group) DelRtmpPubSession(session *rtmp.ServerSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delRtmpPubSession(session)
}

func (group *Group) DelRtspPubSession(session *rtsp.PubSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delRtspPubSession(session)
}

func (group *Group) DelRtmpPullSession(session *rtmp.PullSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	removed := group.delPullSession(session)
	if !removed {
		return
	}

	var info base.PullStopInfo
	info.SessionId = session.UniqueKey()
	info.Url = session.Url()
	info.Protocol = session.GetStat().Protocol
	info.RemoteAddr = session.GetStat().RemoteAddr
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.UrlParam = session.RawQuery()
	info.HasInSession = group.hasInSession()
	info.HasOutSession = group.hasOutSession()
	group.observer.OnRelayPullStop(info)
}

func (group *Group) DelRtspPullSession(session *rtsp.PullSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	removed := group.delPullSession(session)
	if !removed {
		return
	}

	var info base.PullStopInfo
	info.SessionId = session.UniqueKey()
	info.Url = session.Url()
	info.Protocol = session.GetStat().Protocol
	info.RemoteAddr = session.GetStat().RemoteAddr
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.UrlParam = session.RawQuery()
	info.HasInSession = group.hasInSession()
	info.HasOutSession = group.hasOutSession()
	group.observer.OnRelayPullStop(info)
}

// ---------------------------------------------------------------------------------------------------------------------

func (group *Group) delCustomizePubSession(sessionCtx ICustomizePubSessionContext) {
	Log.Debugf("[%s] [%s] del customize PubSession from group.", group.UniqueKey, sessionCtx.UniqueKey())

	if sessionCtx != group.customizePubSession {
		Log.Warnf("[%s] del customize pub session but not match. del session=%s, group session=%p",
			group.UniqueKey, sessionCtx.UniqueKey(), group.customizePubSession)
		return
	}

	group.delIn()
}

func (group *Group) delRtmpPubSession(session *rtmp.ServerSession) {
	Log.Debugf("[%s] [%s] del rtmp PubSession from group.", group.UniqueKey, session.UniqueKey())

	if session != group.rtmpPubSession {
		Log.Warnf("[%s] del rtmp pub session but not match. del session=%s, group session=%p",
			group.UniqueKey, session.UniqueKey(), group.rtmpPubSession)
		return
	}

	group.delIn()

}

func (group *Group) delRtspPubSession(session *rtsp.PubSession) {
	Log.Debugf("[%s] [%s] del rtsp PubSession from group.", group.UniqueKey, session.UniqueKey())

	if session != group.rtspPubSession {
		Log.Warnf("[%s] del rtmp pub session but not match. del session=%s, group session=%p",
			group.UniqueKey, session.UniqueKey(), group.rtspPubSession)
		return
	}

	group.delIn()
}

func (group *Group) delPullSession(session base.IObject) bool {
	Log.Debugf("[%s] [%s] del PullSession from group.", group.UniqueKey, session.UniqueKey())

	delUk := session.UniqueKey()
	if group.pullProxy != nil {
		if group.pullProxy.rtmpSession != nil && group.pullProxy.rtmpSession.UniqueKey() == delUk {
			group.resetRelayPullSession()
			group.delIn()
			return true
		}
		if group.pullProxy.rtspSession != nil && group.pullProxy.rtspSession.UniqueKey() == delUk {
			group.resetRelayPullSession()
			group.delIn()
			return true
		}

		// 若该session从未成功Add进group（仅attempt阶段），不要触发delIn；只需清理attempt状态。
		if group.pullProxy.isSessionPulling && group.pullProxy.pullingSessionId == delUk {
			group.pullProxy.isSessionPulling = false
			group.pullProxy.pullingSessionId = ""
		}
	}

	Log.Warnf("[%s] del pull session ignored (not match current). del=%s, current=%s",
		group.UniqueKey, delUk, group.pullSessionUniqueKey())
	return false
}

// ---------------------------------------------------------------------------------------------------------------------

// addIn 有pub或pull的输入型session加入时，需要调用该函数
func (group *Group) addIn() {
	now := time.Now().Unix()

	if group.shouldStartMpegtsRemuxer() {
		group.rtmp2MpegtsRemuxer = remux.NewRtmp2MpegtsRemuxer(group)
		nazalog.Debugf("[%s] [%s] NewRtmp2MpegtsRemuxer in group.", group.UniqueKey, group.rtmp2MpegtsRemuxer.UniqueKey())
	}

	if group.config.InSessionConfig.AddDummyAudioEnable {
		group.dummyAudioFilter = remux.NewDummyAudioFilter(group.UniqueKey, group.config.InSessionConfig.AddDummyAudioWaitAudioMs, group.broadcastByRtmpMsg)
	}

	if group.option.onHookSession != nil {
		group.customizeHookSessionContext = group.option.onHookSession(group.inSessionUniqueKey(), group.streamName)
	}

	group.startPushIfNeeded()

	// 转推模式下不启动存储功能（HLS、录制等），只做数据转发
	if group.relayProxy == nil || !group.relayProxy.isRelaying {
		group.startHlsIfNeeded()
		group.startRecordFlvIfNeeded(now)
		group.startRecordMpegtsIfNeeded(now)
	}
}

// delIn 有pub或pull的输入型session离开时，需要调用该函数
func (group *Group) delIn() {
	// 注意，remuxer放前面，使得有机会将内部缓存的数据吐出来
	if group.rtmp2MpegtsRemuxer != nil {
		group.rtmp2MpegtsRemuxer.Dispose()
		group.rtmp2MpegtsRemuxer = nil
	}

	if group.customizeHookSessionContext != nil {
		group.customizeHookSessionContext.OnStop()
		group.customizeHookSessionContext = nil
	}

	group.stopPushIfNeeded()
	group.stopHlsIfNeeded()
	group.stopRecordFlvIfNeeded()
	group.stopRecordMpegtsIfNeeded()

	group.rtmpPubSession = nil
	group.rtspPubSession = nil
	group.customizePubSession = nil
	group.rtsp2RtmpRemuxer = nil
	group.rtmp2RtspRemuxer = nil
	group.dummyAudioFilter = nil

	if group.psPubDumpFile != nil {
		group.psPubDumpFile.Close()
		group.psPubDumpFile = nil
	}
	group.rtmpGopCache.Clear()
	group.httpflvGopCache.Clear()
	group.httptsGopCache.Clear()
	group.sdpCtx = nil
	group.patpmt = nil
}
