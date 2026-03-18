// Copyright 2022, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
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

	// 如果已有缓存，直接返回一份拷贝，避免修改底层切片。
	if sm.lastStatAllGroup != nil {
		sgs = make([]base.StatGroup, len(sm.lastStatAllGroup))
		copy(sgs, sm.lastStatAllGroup)
		return
	}

	// 首次调用或缓存尚未构建时，计算一次并写入缓存。
	sm.groupManager.Iterate(func(group *Group) bool {
		sgs = append(sgs, group.GetStat(math.MaxInt32))
		return true
	})
	sm.lastStatAllGroup = make([]base.StatGroup, len(sgs))
	copy(sm.lastStatAllGroup, sgs)
	sm.lastStatAllGroupAt = time.Now()
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

	// 同一 streamName 已在拉流：直接成功，不再 INVITE、不做 replace（与 AddCustomizePubSession 侧「同 streamName 不替换」一致）。
	if streamName != "" {
		if cur := sm.gb28181Server.FindChannelByStreamName(streamName); cur != nil && cur.MediaInfo.IsInvite {
			ret.ErrorCode = base.ErrorCodeSucc
			ret.Desp = base.DespSucc
			ret.Data.StreamName = streamName
			if port := parseGb28181MediaPort(cur.MediaInfo.MediaKey); port > 0 {
				ret.Data.Port = port
			}
			return
		}
	}

	// 同设备+通道+码流已在拉流（streamName 可能与请求不一致时仍避免重复 INVITE）
	if cur := sm.gb28181Server.FindInvitingChannelByRequest(info.DeviceId, info.ChannelId, streamIndex); cur != nil && cur.MediaInfo.IsInvite {
		ret.ErrorCode = base.ErrorCodeSucc
		ret.Desp = base.DespSucc
		ret.Data.StreamName = cur.MediaInfo.StreamName
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

	// 在发起 INVITE 前先检查设备与通道在线状态：
	// - 设备必须为 ONLINE；
	// - 通道必须为 ON（ChannelOnStatus）。
	if dev := sm.gb28181Server.GetDevice(info.DeviceId); dev != nil {
		if dev.Status != gb28181.DeviceOnlineStatus {
			ret.ErrorCode = base.ErrorCodeGb28181InviteFail
			ret.Desp = "device is offline"
			return
		}
	}
	if ch.Status != gb28181.ChannelOnStatus {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "channel is offline"
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

// StatGb28181Upstreams 查询 GB28181 上级平台配置列表（中间平台模式）。
// 注意：当前实现只读主配置与独立 upstream_config_file 中的内容，并不会通过此接口修改配置文件。
func (sm *ServerManager) StatGb28181Upstreams() (ret base.ApiGb28181UpstreamListResp) {
	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc

	conf := sm.config.Gb28181Config
	upstreams := make([]base.ApiGb28181Upstream, 0, len(conf.Upstreams))
	for _, u := range conf.Upstreams {
		upstreams = append(upstreams, base.ApiGb28181Upstream{
			ID:            u.ID,
			Enable:        u.Enable,
			SipID:         u.SipID,
			Realm:         u.Realm,
			SipIP:         u.SipIP,
			SipPort:       u.SipPort,
			LocalDeviceID: u.LocalDeviceID,
			Comment:       u.Comment,
		})
	}
	ret.Data.Upstreams = upstreams
	return
}

// StatGb28181UpstreamSubs 查询指定上级平台当前的订阅流列表（以 stream_name 为标识）。
// 数据来源为运行时 gb28181Server 内部的订阅表（而非配置文件）。
func (sm *ServerManager) StatGb28181UpstreamSubs(upstreamID string) (ret base.ApiGb28181UpstreamSubsResp) {
	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181QueryFail
		ret.Desp = "gb28181 server not enabled"
		return
	}

	subs := sm.gb28181Server.ListUpstreamSubs(upstreamID)
	list := make([]base.ApiGb28181UpstreamSub, 0, len(subs))
	for _, ssub := range subs {
		list = append(list, base.ApiGb28181UpstreamSub{
			UpstreamID: ssub.UpstreamID,
			StreamName: ssub.StreamName,
			ChannelID:  ssub.ChannelID,
		})
	}
	ret.Data.UpstreamID = upstreamID
	ret.Data.Subs = list
	return
}

// CtrlGb28181UpstreamSubAdd 为指定上级平台新增订阅流（stream_name 级别）。
// 注意：这是运行时行为，仅影响当前进程内的订阅表，不会自动写回 upstream_config_file。
func (sm *ServerManager) CtrlGb28181UpstreamSubAdd(info base.ApiGb28181UpstreamSub) (ret base.ApiRespBasic) {
	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "gb28181 server not enabled"
		return
	}
	if info.UpstreamID == "" || info.StreamName == "" || info.ChannelID == "" {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "upstream_id, stream_name and channel_id are required and must be non-empty"
		return
	}
	if err := sm.gb28181Server.AddUpstreamSub(info.UpstreamID, info.StreamName, info.ChannelID); err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = err.Error()
	}
	return
}

// CtrlGb28181UpstreamSubDel 删除指定上级平台的一条订阅关系。
// 同样仅修改运行时状态，如需持久化请同时更新 upstream_config_file。
func (sm *ServerManager) CtrlGb28181UpstreamSubDel(info base.ApiGb28181UpstreamSub) (ret base.ApiRespBasic) {
	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc

	if sm.gb28181Server == nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "gb28181 server not enabled"
		return
	}
	if err := sm.gb28181Server.RemoveUpstreamSub(info.UpstreamID, info.StreamName); err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = err.Error()
		return
	}
	// 订阅删除后，立即对当前所有上级会话做一次对齐：
	// 对于已不在订阅表中的会话（包括非 GB 流转推），主动发送 BYE、关闭转推 Sink，并停止对应的 RTP 喂流。
	sm.gb28181Server.ReconcileUpstreamSessionsWithSubs()
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

// CtrlGb28181UpstreamsConfSet 覆盖写入 gb28181 上级配置文件（例如 conf/gb28181_upstreams.json）。
// 注意：该接口只负责写盘，不会自动重启 gb28181Server。
func (sm *ServerManager) CtrlGb28181UpstreamsConfSet(confFile Gb28181UpstreamConfigFile) (ret base.ApiRespBasic) {
	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc

	path := sm.config.Gb28181Config.UpstreamConfigFile
	if path == "" {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "gb28181.upstream_config_file is empty"
		return
	}

	// 使用与 LoadConfAndInitLog 相同的结构，直接写回 JSON。
	data, err := json.MarshalIndent(confFile, "", "  ")
	if err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = fmt.Sprintf("marshal upstream config failed: %v", err)
		return
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = fmt.Sprintf("write upstream config file failed: %v", err)
		return
	}

	// 写盘成功后，直接应用到运行时（合并 reload 行为）
	// 覆盖内存配置
	sm.config.Gb28181Config.Upstreams = confFile.Upstreams
	sm.config.Gb28181Config.UpstreamSubs = confFile.Subs

	// 若 gb28181 未启用或服务未启动，仅更新配置即可
	if !sm.config.Gb28181Config.Enable || sm.gb28181Server == nil {
		return
	}
	// 刷新上级平台
	gbConf := gb28181ConfigFromLogic(sm.config.Gb28181Config)
	// 使用简单粗暴重载：先清空所有上级相关运行时资源，再按新配置重建。
	sm.gb28181Server.BrutalReloadUpstreams(gbConf.Upstreams)
	// 重建订阅（仅对已启用的上级）
	sm.gb28181Server.ClearAllUpstreamSubs()
	enabledUpstreamIDs := make(map[string]struct{})
	for _, u := range sm.config.Gb28181Config.Upstreams {
		if u.Enable && u.ID != "" {
			enabledUpstreamIDs[u.ID] = struct{}{}
		}
	}
	for _, sub := range sm.config.Gb28181Config.UpstreamSubs {
		if _, enabled := enabledUpstreamIDs[sub.UpstreamID]; !enabled {
			continue
		}
		if err := sm.gb28181Server.AddUpstreamSub(sub.UpstreamID, sub.StreamName, sub.ChannelID); err != nil {
			Log.Warnf("apply gb28181 upstream sub failed. upstream=%s stream=%s channel=%s err=%+v",
				sub.UpstreamID, sub.StreamName, sub.ChannelID, err)
		}
	}
	// brutal reload 已清空所有会话，此处无需对齐，但保留一次调用以防未来逻辑调整。
	sm.gb28181Server.ReconcileUpstreamSessionsWithSubs()
	return
}

// CtrlGb28181UpstreamsReload 从 upstream_config_file 重新加载上级平台配置，并重启 gb28181 中间平台服务。
// 实际流程：
// 1) 读取并解析配置文件到 Gb28181UpstreamConfigFile；
// 2) 覆盖 sm.config.Gb28181Config.Upstreams / UpstreamSubs；
// 3) Dispose 旧的 gb28181Server（若存在），再按当前配置新建并 Start；
// 4) 将 UpstreamSubs 通过 AddUpstreamSub 注入运行时订阅表。
func (sm *ServerManager) CtrlGb28181UpstreamsReload() (ret base.ApiRespBasic) {
	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc

	if !sm.config.Gb28181Config.UpstreamEnable {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "gb28181.upstream_enable is false, middle platform is disabled, reload not allowed"
		return
	}

	path := sm.config.Gb28181Config.UpstreamConfigFile
	if path == "" {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = "gb28181.upstream_config_file is empty"
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = fmt.Sprintf("read upstream config file failed: %v", err)
		return
	}

	var file Gb28181UpstreamConfigFile
	if err := json.Unmarshal(data, &file); err != nil {
		ret.ErrorCode = base.ErrorCodeGb28181InviteFail
		ret.Desp = fmt.Sprintf("unmarshal upstream config file failed: %v", err)
		return
	}

	// 覆盖逻辑配置中的 Upstreams / UpstreamSubs
	sm.config.Gb28181Config.Upstreams = file.Upstreams
	sm.config.Gb28181Config.UpstreamSubs = file.Subs

	// 若 gb28181 未启用，仅更新配置即可。
	if !sm.config.Gb28181Config.Enable || sm.gb28181Server == nil {
		return
	}

	// 仅刷新上级平台相关配置，不重启其他 GB28181 服务。
	gbConf := gb28181ConfigFromLogic(sm.config.Gb28181Config)
	// 使用简单粗暴重载：先清空所有上级相关运行时资源，再按新配置重建。
	sm.gb28181Server.BrutalReloadUpstreams(gbConf.Upstreams)

	// 重建运行时订阅表：先清空，再按配置文件重建；仅对已启用的上级注入订阅。
	sm.gb28181Server.ClearAllUpstreamSubs()
	enabledUpstreamIDs := make(map[string]struct{})
	for _, u := range sm.config.Gb28181Config.Upstreams {
		if u.Enable && u.ID != "" {
			enabledUpstreamIDs[u.ID] = struct{}{}
		}
	}
	for _, sub := range sm.config.Gb28181Config.UpstreamSubs {
		if _, enabled := enabledUpstreamIDs[sub.UpstreamID]; !enabled {
			continue
		}
		if err := sm.gb28181Server.AddUpstreamSub(sub.UpstreamID, sub.StreamName, sub.ChannelID); err != nil {
			Log.Warnf("reload gb28181 upstream sub failed. upstream=%s stream=%s channel=%s err=%+v",
				sub.UpstreamID, sub.StreamName, sub.ChannelID, err)
		}
	}

	// 订阅关系变化后，对当前正在转发的上级播放会话做一次对齐：
	// 对于已存在但不再被订阅允许的会话，主动发 BYE 并关闭对应的转发 Sink。
	sm.gb28181Server.ReconcileUpstreamSessionsWithSubs()
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
// 仅使用 GB28181Server 自身维护的流信息，保证 Active Streams 中展示的是 GB 层的真实、完整信息。
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
		port := parseGb28181MediaPort(s.MediaKey)
		isTcp := strings.HasPrefix(s.MediaKey, "tcp")
		streamInfos = append(streamInfos, base.Gb28181StreamInfo{
			DeviceId:   s.DeviceId,
			ChannelId:  s.ChannelId,
			StreamName: s.StreamName,
			CallId:     s.CallId,
			Port:       port,
			IsTcp:      isTcp,
			StartTime:  s.StartTime,
		})
	}

	ret.ErrorCode = base.ErrorCodeSucc
	ret.Desp = base.DespSucc
	ret.Data.Streams = streamInfos
	return
}

// lalmax GB28181 通过 mediaserver Conn 直接调用 AddCustomizePubSession，无需 onInvite/onBye/onReconnect 回调
