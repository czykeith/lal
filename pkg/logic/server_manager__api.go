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
	"strconv"
	"strings"
	"time"

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

// CtrlStartRelayFromStream 从已有流启动转推（仅推流，不再额外拉流）
// 调用方需要保证 stream_name 已经在本机存在（例如通过 RTMP/RTSP/GB28181/自定义接入）。
func (sm *ServerManager) CtrlStartRelayFromStream(info base.ApiCtrlStartRelayFromStreamReq) (ret base.ApiCtrlStartRelayFromStreamResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	streamName := info.StreamName
	if streamName == "" {
		ret.ErrorCode = base.ErrorCodeParamMissing
		ret.Desp = base.DespParamMissing
		return
	}

	// 检查 group 是否存在
	g := sm.getGroup("", streamName)
	if g == nil {
		ret.ErrorCode = base.ErrorCodeGroupNotFound
		ret.Desp = base.DespGroupNotFound
		return
	}

	// 复用现有 startPush 逻辑，仅做推流：
	// - 不改动现有拉流配置，不新增额外拉流。
	timeoutMs := info.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = DefaultApiCtrlStartRelayPullReqPullTimeoutMs
	}

	pushUrl := info.PushUrl
	if pushUrl == "" {
		ret.ErrorCode = base.ErrorCodeParamMissing
		ret.Desp = base.DespParamMissing
		return
	}

	// 内部通过 group.startPushLocked 完成 RTMP/RTSP 推流
	pushSessionId, err := g.StartPushOnly(pushUrl, timeoutMs, info.RetryNum)
	if err != nil {
		ret.ErrorCode = base.ErrorCodeStartRelayFail
		ret.Desp = err.Error()
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
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

// CtrlGb28181Invite GB28181拉流（lalmax：FindChannel + channel.Invite）
func (sm *ServerManager) CtrlGb28181Invite(info base.ApiCtrlGb28181InviteReq) (ret base.ApiCtrlGb28181InviteResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	streamName := info.StreamName

	// 统一使用码流索引 stream_index：0=主，1=子，2=第三...
	streamIndex := info.StreamIndex
	if streamIndex < 0 {
		streamIndex = 0
	}

	// 重复拉流校验（全局）：
	// 1) stream_name 已存在：幂等返回（避免因为 heuristic 选到不同 ChannelId 导致重复 INVITE）
	if cur := sm.gb28181Server.FindChannelByStreamName(streamName); cur != nil && cur.MediaInfo.IsInvite {
		ret.ErrorCode = base.ErrorCodeSucc
		ret.Desp = base.DespSucc
		ret.Data.StreamName = streamName
		if port := parseGb28181MediaPort(cur.MediaInfo.MediaKey); port > 0 {
			ret.Data.Port = port
		}
		return
	}

	// 内部通道选择目前仅区分主/子（heuristic），因此将 index 映射为主(0) / 子(1)。
	streamType := 0
	if streamIndex != 0 {
		streamType = 1
	}
	ch := sm.gb28181Server.FindChannelWithStreamType(info.DeviceId, info.ChannelId, streamType)
	if ch == nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = base.DespGb28181DeviceNotFound
		return
	}

	network := "udp"
	if info.IsTcpFlag == 1 {
		network = "tcp"
	}
	playInfo := &gb28181.PlayInfo{
		NetWork:     network,
		DeviceId:    info.DeviceId,
		ChannelId:   info.ChannelId,
		StreamName:  streamName,
		SinglePort:  false,
		StreamIndex: streamIndex,
	}
	gbConf := sm.gb28181Server.GetConfig()
	code, err := ch.Invite(&gb28181.InviteOptions{}, streamName, playInfo, &gbConf)
	if err != nil || code != 200 {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		if err != nil {
			ret.Desp = err.Error()
		} else {
			ret.Desp = fmt.Sprintf("invite failed, code=%d", code)
		}
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
	// 从 Channel 的 MediaKey 中解析收流端口号，例如 \"udp30000\" / \"tcp30000\"
	if port := parseGb28181MediaPort(ch.MediaInfo.MediaKey); port > 0 {
		ret.Data.Port = port
	}
	return
}

// CtrlGb28181Playback GB28181回放（lalmax：FindChannel + channel.Invite 带 Start/End）
func (sm *ServerManager) CtrlGb28181Playback(info base.ApiCtrlGb28181PlaybackReq) (ret base.ApiCtrlGb28181PlaybackResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	// 无时区格式按本地时区解析，避免 "2006-01-02T15:04:05" 被当成 UTC 导致与北京时间差 8 小时
	timeLayouts := []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", time.RFC3339, time.RFC3339Nano}
	naiveLayouts := map[string]bool{"2006-01-02T15:04:05": true, "2006-01-02 15:04:05": true}
	var startTime, endTime time.Time
	var err error
	for _, layout := range timeLayouts {
		if naiveLayouts[layout] {
			startTime, err = time.ParseInLocation(layout, info.StartTime, time.Local)
		} else {
			startTime, err = time.Parse(layout, info.StartTime)
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		ret.ErrorCode = base.ErrorCodeParamMissing
		ret.Desp = fmt.Sprintf("invalid start_time: %s", info.StartTime)
		return
	}
	for _, layout := range timeLayouts {
		if naiveLayouts[layout] {
			endTime, err = time.ParseInLocation(layout, info.EndTime, time.Local)
		} else {
			endTime, err = time.Parse(layout, info.EndTime)
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		ret.ErrorCode = base.ErrorCodeParamMissing
		ret.Desp = fmt.Sprintf("invalid end_time: %s", info.EndTime)
		return
	}
	if !endTime.After(startTime) {
		ret.ErrorCode = base.ErrorCodeParamMissing
		ret.Desp = "end_time must be after start_time"
		return
	}

	streamName := info.StreamName
	// stream_name 作为唯一标识，必填（由 HTTP handler 校验）

	// 回放重复校验：stream_name 已存在则幂等返回
	if cur := sm.gb28181Server.FindChannelByStreamName(streamName); cur != nil && cur.MediaInfo.IsInvite {
		ret.ErrorCode = base.ErrorCodeSucc
		ret.Desp = base.DespSucc
		ret.Data.StreamName = streamName
		if port := parseGb28181MediaPort(cur.MediaInfo.MediaKey); port > 0 {
			ret.Data.Port = port
		}
		return
	}

	// 按设备+通道查找；码流由 stream_index 在 SDP/Subject 中指定
	ch := sm.gb28181Server.FindChannelWithStreamType(info.DeviceId, info.ChannelId, 0)
	if ch == nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = base.DespGb28181DeviceNotFound
		return
	}

	streamIndex := info.StreamIndex
	if streamIndex < 0 {
		streamIndex = 0
	}
	network := "udp"
	if info.IsTcpFlag == 1 {
		network = "tcp"
	}
	playInfo := &gb28181.PlayInfo{
		NetWork:     network,
		DeviceId:    info.DeviceId,
		ChannelId:   info.ChannelId,
		StreamName:  streamName,
		SinglePort:  false,
		StreamIndex: streamIndex,
	}
	opt := &gb28181.InviteOptions{
		Start: int(startTime.Unix()),
		End:   int(endTime.Unix()),
	}
	gbConf := sm.gb28181Server.GetConfig()
	code, err := ch.Invite(opt, streamName, playInfo, &gbConf)
	if err != nil || code != 200 {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		if err != nil {
			ret.Desp = err.Error()
		} else {
			ret.Desp = fmt.Sprintf("playback invite failed, code=%d", code)
		}
		return
	}

	// 注册回放会话，有效期默认 3 小时（可通过 GB28181Server.PlaybackSessionTTL 调整）
	if sm.gb28181Server != nil {
		sm.gb28181Server.RegisterPlaybackSession(streamName)
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
	if port := parseGb28181MediaPort(ch.MediaInfo.MediaKey); port > 0 {
		ret.Data.Port = port
	}
	return
}

// parseGb28181MediaPort 从 lalmax 的 MediaKey（形如 "udp30000" 或 "tcp30000"）中提取端口号。
func parseGb28181MediaPort(mediaKey string) int {
	if mediaKey == "" {
		return 0
	}
	for i := 0; i < len(mediaKey); i++ {
		c := mediaKey[i]
		if c >= '0' && c <= '9' {
			if p, err := strconv.Atoi(mediaKey[i:]); err == nil {
				return p
			}
			break
		}
	}
	return 0
}

// CtrlGb28181Bye GB28181停止拉流（lalmax：FindChannel + channel.Bye）
func (sm *ServerManager) CtrlGb28181Bye(info base.ApiCtrlGb28181ByeReq) (ret base.ApiCtrlGb28181ByeResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181ByeFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	streamName := info.StreamName
	// stream_name 作为唯一标识，停止时只按 stream_name 定位
	ch := sm.gb28181Server.FindChannelByStreamName(streamName)
	if ch == nil {
		ret.ErrorCode = base.ErrorCodeGb28181ByeFail
		ret.Desp = "gb28181 stream not found"
		return
	}

	err := ch.Bye(streamName)
	if err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181ByeFail
		ret.Desp = err.Error()
		return
	}

	// 主动停止时同步移除回放会话（若是回放的话）
	if sm.gb28181Server != nil {
		sm.gb28181Server.UnregisterPlaybackSession(streamName)
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
	return
}

// CtrlGb28181PlaybackScale GB28181回放倍速控制（发送控制指令，不改变本地拉流逻辑）
func (sm *ServerManager) CtrlGb28181PlaybackScale(info base.ApiCtrlGb28181PlaybackScaleReq) (ret base.ApiCtrlGb28181PlaybackScaleResp) {
	// 注意：不要在 sm.mutex 内同步等待 SIP 响应，否则会阻塞其它 HTTP 接口。
	streamName := info.StreamName

	sm.mutex.Lock()
	gb := sm.gb28181Server
	var ch *gb28181.Channel
	if gb != nil {
		ch = gb.FindChannelByStreamName(streamName)
	}
	sm.mutex.Unlock()

	if gb == nil {
		ret.ErrorCode = base.ErrorCodeGb28181PlaybackCtrlFail
		ret.Desp = "gb28181 server not enabled"
		return
	}
	if ch == nil {
		ret.ErrorCode = base.ErrorCodeGb28181PlaybackCtrlFail
		ret.Desp = "gb28181 stream not found"
		return
	}

	if err := ch.PlaybackScale(info.Scale); err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181PlaybackCtrlFail
		ret.Desp = err.Error()
		return
	}

	// 记录本地倍速配置，供 GB28181 RTP 收流侧进行时间戳适配。
	if gb != nil {
		gb.SetPlaybackScale(streamName, info.Scale)
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.StreamName = streamName
	ret.Data.Scale = info.Scale
	ret.Data.SessionId = ch.GetCallId()
	return
}

// StatGb28181Devices 获取GB28181设备列表（lalmax：GetDeviceInfos）
func (sm *ServerManager) StatGb28181Devices() (ret base.ApiStatGb28181DeviceResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181DeviceNotFound
		ret.Desp = "gb28181 server not enabled"
		return
	}

	infos := sm.gb28181Server.GetDeviceInfos()
	deviceInfos := make([]base.Gb28181DeviceInfo, 0, len(infos.DeviceItems))
	for _, item := range infos.DeviceItems {
		deviceInfo := base.Gb28181DeviceInfo{
			DeviceId:      item.DeviceId,
			DeviceName:    item.Name,
			Status:        string(item.Status),
			RegisterTime:  item.RegisterTime,
			KeepaliveTime: item.KeepaliveTime,
			Channels:      make([]base.Gb28181ChannelInfo, 0, len(item.Channels)),
		}
		for _, ch := range item.Channels {
			deviceInfo.Channels = append(deviceInfo.Channels, base.Gb28181ChannelInfo{
				ChannelId:   ch.ChannelId,
				ChannelName: ch.Name,
				Status:      string(ch.Status),
				StreamName:  ch.StreamName,
			})
		}
		deviceInfos = append(deviceInfos, deviceInfo)
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.Devices = deviceInfos
	return
}

// CtrlGb28181Ptz GB28181 PTZ控制（lalmax：FindChannel + channel.Ptz*）
func (sm *ServerManager) CtrlGb28181Ptz(info base.ApiCtrlGb28181PtzReq) (ret base.ApiCtrlGb28181PtzResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181PtzFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	ch := sm.gb28181Server.FindChannel(info.DeviceId, info.ChannelId)
	if ch == nil {
		ret.ErrorCode = base.ErrorCodeGb28181PtzFail
		ret.Desp = base.DespGb28181DeviceNotFound
		return
	}

	speed := byte(info.Speed)
	if speed == 0 {
		speed = 5
	}
	if speed > 8 {
		speed = 8
	}
	speed *= 25 // lalmax 约定

	var err error
	switch info.Command {
	case "Stop":
		err = ch.PtzStop(&gb28181.PtzStop{DeviceId: info.DeviceId, ChannelId: info.ChannelId})
	case "Up":
		err = ch.PtzDirection(&gb28181.PtzDirection{DeviceId: info.DeviceId, ChannelId: info.ChannelId, Up: true, Speed: speed})
	case "Down":
		err = ch.PtzDirection(&gb28181.PtzDirection{DeviceId: info.DeviceId, ChannelId: info.ChannelId, Down: true, Speed: speed})
	case "Left":
		err = ch.PtzDirection(&gb28181.PtzDirection{DeviceId: info.DeviceId, ChannelId: info.ChannelId, Left: true, Speed: speed})
	case "Right":
		err = ch.PtzDirection(&gb28181.PtzDirection{DeviceId: info.DeviceId, ChannelId: info.ChannelId, Right: true, Speed: speed})
	case "ZoomIn":
		err = ch.PtzZoom(&gb28181.PtzZoom{DeviceId: info.DeviceId, ChannelId: info.ChannelId, ZoomIn: true, Speed: speed})
	case "ZoomOut":
		err = ch.PtzZoom(&gb28181.PtzZoom{DeviceId: info.DeviceId, ChannelId: info.ChannelId, ZoomOut: true, Speed: speed})
	case "CallPreset":
		err = ch.PtzPreset(&gb28181.PtzPreset{DeviceId: info.DeviceId, ChannelId: info.ChannelId, Cmd: gb28181.PresetCallPoint, Point: byte(info.Preset)})
	case "SetPreset":
		err = ch.PtzPreset(&gb28181.PtzPreset{DeviceId: info.DeviceId, ChannelId: info.ChannelId, Cmd: gb28181.PresetEditPoint, Point: byte(info.Preset)})
	case "DelPreset":
		err = ch.PtzPreset(&gb28181.PtzPreset{DeviceId: info.DeviceId, ChannelId: info.ChannelId, Cmd: gb28181.PresetDelPoint, Point: byte(info.Preset)})
	default:
		ret.ErrorCode = base.ErrorCodeGb28181PtzFail
		ret.Desp = fmt.Sprintf("unsupported ptz command: %s", info.Command)
		return
	}
	if err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181PtzFail
		ret.Desp = err.Error()
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.DeviceId = info.DeviceId
	ret.Data.ChannelId = info.ChannelId
	ret.Data.Command = info.Command
	return
}

// QueryGb28181DeviceInfo 查询GB28181设备信息（lalmax：GetDevice + QueryDeviceInfo）
func (sm *ServerManager) QueryGb28181DeviceInfo(info base.ApiQueryGb28181DeviceInfoReq) (ret base.ApiQueryGb28181DeviceInfoResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181QueryFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	d := sm.gb28181Server.GetDevice(info.DeviceId)
	if d == nil {
		ret.ErrorCode = base.ErrorCodeGb28181DeviceNotFound
		ret.Desp = base.DespGb28181DeviceNotFound
		return
	}

	sm.gb28181Server.QueryDeviceInfo(info.DeviceId)

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.DeviceId = d.ID
	ret.Data.DeviceName = d.Name
	ret.Data.Manufacturer = d.Manufacturer
	ret.Data.Model = d.Model
	return
}

// QueryGb28181DeviceStatus 查询GB28181设备状态（lalmax：GetDevice 状态）
func (sm *ServerManager) QueryGb28181DeviceStatus(info base.ApiQueryGb28181DeviceStatusReq) (ret base.ApiQueryGb28181DeviceStatusResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181QueryFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	d := sm.gb28181Server.GetDevice(info.DeviceId)
	if d == nil {
		ret.ErrorCode = base.ErrorCodeGb28181DeviceNotFound
		ret.Desp = base.DespGb28181DeviceNotFound
		return
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.DeviceId = d.ID
	ret.Data.Status = string(d.Status)
	return
}

// QueryGb28181Channels 查询GB28181通道列表（lalmax：GetDevice + GetChannels，先拉取目录）
func (sm *ServerManager) QueryGb28181Channels(info base.ApiQueryGb28181ChannelsReq) (ret base.ApiQueryGb28181ChannelsResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181QueryFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	d := sm.gb28181Server.GetDevice(info.DeviceId)
	if d == nil {
		ret.ErrorCode = base.ErrorCodeGb28181DeviceNotFound
		ret.Desp = base.DespGb28181DeviceNotFound
		return
	}

	sm.gb28181Server.QueryCatalog(info.DeviceId)
	channels := d.GetChannels()
	channelInfos := make([]base.Gb28181ChannelInfo, 0, len(channels))
	for _, ch := range channels {
		channelInfos = append(channelInfos, base.Gb28181ChannelInfo{
			ChannelId:   ch.ChannelId,
			ChannelName: ch.Name,
			Status:      string(ch.Status),
			StreamName:  ch.StreamName,
		})
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.DeviceId = info.DeviceId
	ret.Data.Channels = channelInfos
	return
}

// StatGb28181Streams 获取GB28181流列表（lalmax：GetAllStreams）
func (sm *ServerManager) StatGb28181Streams() (ret base.ApiStatGb28181StreamsResp) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181QueryFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	streams := sm.gb28181Server.GetAllStreams()
	streamInfos := make([]base.Gb28181StreamInfo, 0, len(streams))
	for _, s := range streams {
		streamInfos = append(streamInfos, base.Gb28181StreamInfo{
			DeviceId:   s.DeviceId,
			ChannelId:  s.ChannelId,
			StreamName: s.StreamName,
		})
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.Streams = streamInfos
	return
}

// lalmax GB28181 通过 mediaserver Conn 直接调用 AddCustomizePubSession，无需 onInvite/onBye/onReconnect 回调
