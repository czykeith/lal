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
	"math"
	"strings"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/gb28181"
	"github.com/q191201771/naza/pkg/bininfo"
)

// server_manager__api.go
//
// 支持http-api功能的部分
//

func (sm *ServerManager) StatLalInfo() base.LalInfo {
	var lalInfo base.LalInfo
	lalInfo.BinInfo = bininfo.StringifySingleLine()
	lalInfo.LalVersion = base.LalVersion
	lalInfo.ApiVersion = base.HttpApiVersion
	lalInfo.NotifyVersion = base.HttpNotifyVersion
	lalInfo.WebUiVersion = base.HttpWebUiVersion
	lalInfo.StartTime = sm.serverStartTime
	lalInfo.ServerId = sm.config.ServerId
	return lalInfo
}

func (sm *ServerManager) StatAllGroup() (sgs []base.StatGroup) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	sm.groupManager.Iterate(func(group *Group) bool {
		sgs = append(sgs, group.GetStat(math.MaxInt32))
		return true
	})
	return
}

func (sm *ServerManager) StatGroup(streamName string) *base.StatGroup {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	g := sm.getGroup("", streamName)
	if g == nil {
		return nil
	}
	// copy
	var ret base.StatGroup
	ret = g.GetStat(math.MaxInt32)
	return &ret
}

func (sm *ServerManager) CtrlStartRelayPull(info base.ApiCtrlStartRelayPullReq) (ret base.ApiCtrlStartRelayPullResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	streamName := info.StreamName
	if streamName == "" {
		ctx, err := base.ParseUrl(info.Url, -1)
		if err != nil {
			ret.ErrorCode = base.ErrorCodeStartRelayPullFail
			ret.Desp = err.Error()
			return
		}
		streamName = ctx.LastItemOfPath
	}

	// 注意，如果group不存在，我们依然relay pull
	g := sm.getOrCreateGroup("", streamName)

	sessionId, err := g.StartPull(info)
	if err != nil {
		ret.ErrorCode = base.ErrorCodeStartRelayPullFail
		ret.Desp = err.Error()
	} else {
		ret.ErrorCode = base.ErrorCodeSucc
		ret.Desp = base.DespSucc
		ret.Data.StreamName = streamName
		ret.Data.SessionId = sessionId
	}
	return
}

// CtrlStopRelayPull
//
// TODO(chef): 整理错误值
func (sm *ServerManager) CtrlStopRelayPull(streamName string) (ret base.ApiCtrlStopRelayPullResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	g := sm.getGroup("", streamName)
	if g == nil {
		ret.ErrorCode = base.ErrorCodeGroupNotFound
		ret.Desp = base.DespGroupNotFound
		return
	}

	ret.Data.SessionId = g.StopPull()
	if ret.Data.SessionId == "" {
		ret.ErrorCode = base.ErrorCodeSessionNotFound
		ret.Desp = base.DespSessionNotFound
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	return
}

// CtrlKickSession
//
// TODO(chef): refactor 不要返回http结果，返回error吧
func (sm *ServerManager) CtrlKickSession(info base.ApiCtrlKickSessionReq) (ret base.ApiCtrlKickSessionResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	g := sm.getGroup("", info.StreamName)
	if g == nil {
		ret.ErrorCode = base.ErrorCodeGroupNotFound
		ret.Desp = base.DespGroupNotFound
		return
	}

	if !g.KickSession(info.SessionId) {
		// 如果是 HLS session，尝试在 ServerHandler 中查找并关闭
		if strings.HasPrefix(info.SessionId, base.UkPreHlsSubSession) && sm.hlsServerHandler != nil {
			if sm.hlsServerHandler.CloseSubSessionByUniqueKey(info.SessionId) {
				ret.ErrorCode = base.ErrorCodeSucc
				ret.Desp = base.DespSucc
				return
			}
		}
		ret.ErrorCode = base.ErrorCodeSessionNotFound
		ret.Desp = base.DespSessionNotFound
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	return
}

func (sm *ServerManager) CtrlAddIpBlacklist(info base.ApiCtrlAddIpBlacklistReq) (ret base.ApiCtrlAddIpBlacklistResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	sm.ipBlacklist.Add(info.Ip, info.DurationSec)

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	return
}

func (sm *ServerManager) CtrlStartRtpPub(info base.ApiCtrlStartRtpPubReq) (ret base.ApiCtrlStartRtpPubResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	// 注意，如果group不存在，我们依然relay pull
	g := sm.getOrCreateGroup("", info.StreamName)
	ret = g.StartRtpPub(info)

	return
}

// CtrlStartRelay 启动转推
func (sm *ServerManager) CtrlStartRelay(info base.ApiCtrlStartRelayReq) (ret base.ApiCtrlStartRelayResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	// 解析 stream_name
	streamName := info.StreamName
	if streamName == "" {
		ctx, err := base.ParseUrl(info.PullUrl, -1)
		if err != nil {
			ret.ErrorCode = base.ErrorCodeStartRelayFail
			ret.Desp = fmt.Sprintf("parse pull url failed: %v", err)
			return
		}
		streamName = ctx.LastItemOfPath
	}

	// 创建或获取 Group
	g := sm.getOrCreateGroup("", streamName)

	// 启动转推
	pullSessionId, pushSessionId, err := g.StartRelay(info)
	if err != nil {
		ret.ErrorCode = base.ErrorCodeStartRelayFail
		ret.Desp = err.Error()
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
	ret.Data.PullSessionId = pullSessionId
	ret.Data.PushSessionId = pushSessionId
	return
}

// CtrlStopRelay 停止转推
func (sm *ServerManager) CtrlStopRelay(streamName string) (ret base.ApiCtrlStopRelayResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	g := sm.getGroup("", streamName)
	if g == nil {
		ret.ErrorCode = base.ErrorCodeGroupNotFound
		ret.Desp = base.DespGroupNotFound
		return
	}

	pullSessionId, pushSessionId := g.StopRelay()
	if pullSessionId == "" && pushSessionId == "" {
		ret.ErrorCode = base.ErrorCodeStopRelayFail
		ret.Desp = base.DespStopRelayFail
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
	ret.Data.PullSessionId = pullSessionId
	ret.Data.PushSessionId = pushSessionId
	return
}

// CtrlGb28181Invite GB28181拉流
func (sm *ServerManager) CtrlGb28181Invite(info base.ApiCtrlGb28181InviteReq) (ret base.ApiCtrlGb28181InviteResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	// 生成流名称
	streamName := info.StreamName
	if streamName == "" {
		streamName = gb28181.GenerateStreamName(info.DeviceId, info.ChannelId)
	}

	// 检查拉流任务是否已经存在
	if sm.gb28181Server.HasStream(streamName) {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = fmt.Sprintf("stream already exists: %s", streamName)
		return
	}

	// 检查Group是否已经有输入流
	group := sm.getGroup("", streamName)
	if group != nil && group.HasInSession() {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = fmt.Sprintf("stream group already has input session: %s", streamName)
		return
	}

	// 获取或创建Group
	if group == nil {
		group = sm.getOrCreateGroup("", streamName)
	}

	// 启动RTP Pub
	var port int
	if info.Port > 0 {
		port = info.Port
	}
	isTcp := info.IsTcpFlag == 1

	rtpPubReq := base.ApiCtrlStartRtpPubReq{
		StreamName: streamName,
		Port:       port,
		TimeoutMs:  10000,
		IsTcpFlag:  info.IsTcpFlag,
	}

	rtpPubResp := group.StartRtpPub(rtpPubReq)
	if rtpPubResp.ErrorCode != base.ErrorCodeSucc {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = rtpPubResp.Desp
		return
	}

	// 发送INVITE信令
	err := sm.gb28181Server.Invite(info.DeviceId, info.ChannelId, streamName, rtpPubResp.Data.Port, isTcp)
	if err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = err.Error()
		// 如果INVITE失败，需要停止RTP Pub
		group.StopRtpPub(streamName)
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
	ret.Data.SessionId = rtpPubResp.Data.SessionId
	ret.Data.Port = rtpPubResp.Data.Port
	return
}

// CtrlGb28181Bye GB28181停止拉流
func (sm *ServerManager) CtrlGb28181Bye(info base.ApiCtrlGb28181ByeReq) (ret base.ApiCtrlGb28181ByeResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181ByeFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	// 确定流名称
	streamName := info.StreamName
	if streamName == "" {
		streamName = gb28181.GenerateStreamName(info.DeviceId, info.ChannelId)
	}

	// 发送BYE信令
	err := sm.gb28181Server.Bye(info.DeviceId, info.ChannelId, streamName)
	if err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181ByeFail
		ret.Desp = err.Error()
		return
	}

	// 停止RTP Pub
	group := sm.getGroup("", streamName)
	if group != nil {
		sessionId := group.StopRtpPub(streamName)
		ret.Data.SessionId = sessionId
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
	return
}

// StatGb28181Devices 获取GB28181设备列表
func (sm *ServerManager) StatGb28181Devices() (ret base.ApiStatGb28181DeviceResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181DeviceNotFound
		ret.Desp = "gb28181 server not enabled"
		return
	}

	devices := sm.gb28181Server.GetDevices()
	deviceInfos := make([]base.Gb28181DeviceInfo, 0, len(devices))

	for _, device := range devices {
		deviceInfo := base.Gb28181DeviceInfo{
			DeviceId:      device.DeviceId,
			DeviceName:    device.DeviceName,
			Status:        device.Status,
			RegisterTime:  device.RegisterTime.Format("2006-01-02 15:04:05"),
			KeepaliveTime: device.KeepaliveTime.Format("2006-01-02 15:04:05"),
			Channels:      make([]base.Gb28181ChannelInfo, 0),
		}

		// 使用线程安全的方法获取通道列表
		channels := device.GetChannels()
		for _, channel := range channels {
			channelInfo := base.Gb28181ChannelInfo{
				ChannelId:   channel.ChannelId,
				ChannelName: channel.ChannelName,
				Status:      channel.Status,
				StreamName:  channel.StreamName,
			}
			deviceInfo.Channels = append(deviceInfo.Channels, channelInfo)
		}

		deviceInfos = append(deviceInfos, deviceInfo)
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.Devices = deviceInfos
	return
}

// onGb28181Invite GB28181 INVITE回调
func (sm *ServerManager) onGb28181Invite(deviceId, channelId, streamName string, port int, isTcp bool) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	group := sm.getOrCreateGroup("", streamName)

	// 检查Group是否已经有输入会话（服务器主动拉流时，RTP Pub Session已经在API调用时创建）
	if group.HasInSession() {
		Log.Debugf("group already has input session, skip StartRtpPub. stream_name=%s", streamName)
		return nil
	}

	// 设备主动推流时，需要在这里创建RTP Pub Session
	rtpPubReq := base.ApiCtrlStartRtpPubReq{
		StreamName: streamName,
		Port:       port,
		TimeoutMs:  10000,
		IsTcpFlag:  0,
	}
	if isTcp {
		rtpPubReq.IsTcpFlag = 1
	}

	rtpPubResp := group.StartRtpPub(rtpPubReq)
	if rtpPubResp.ErrorCode != base.ErrorCodeSucc {
		return fmt.Errorf("start rtp pub failed: %s", rtpPubResp.Desp)
	}

	return nil
}

// onGb28181Bye GB28181 BYE回调
func (sm *ServerManager) onGb28181Bye(deviceId, channelId, streamName string) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	group := sm.getGroup("", streamName)
	if group != nil {
		group.StopRtpPub(streamName)
	}

	return nil
}
