// Copyright 2020, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package base

// 文档见： https://pengrl.com/lal/#/HTTPAPI

// ----- request -------------------------------------------------------------------------------------------------------

const (
	PullRetryNumForever = -1 // 永远重试
	PullRetryNumNever   = 0  // 不重试

	AutoStopPullAfterNoOutMsNever       = -1
	AutoStopPullAfterNoOutMsImmediately = 0

	RtspModeTcp = 0
	RtspModeUdp = 1
)

type ApiCtrlStartRelayPullReq struct {
	Url                      string  `json:"url"`
	StreamName               string  `json:"stream_name"`
	PullTimeoutMs            int     `json:"pull_timeout_ms"`
	PullRetryNum             int     `json:"pull_retry_num"`
	AutoStopPullAfterNoOutMs int     `json:"auto_stop_pull_after_no_out_ms"`
	RtspMode                 int     `json:"rtsp_mode"`
	DebugDumpPacket          string  `json:"debug_dump_packet"`
	Scale                    float64 `json:"scale"` // RTSP拉流时的播放速度倍数，合法范围 [1,8]，1 表示正常速度，>1 表示加速
}

type ApiCtrlKickSessionReq struct {
	StreamName string `json:"stream_name"`
	SessionId  string `json:"session_id"`
}

type ApiCtrlStartRtpPubReq struct {
	StreamName      string `json:"stream_name"`
	Port            int    `json:"port"`
	TimeoutMs       int    `json:"timeout_ms"`
	IsTcpFlag       int    `json:"is_tcp_flag"`
	DebugDumpPacket string `json:"debug_dump_packet"`
}

type ApiCtrlAddIpBlacklistReq struct {
	Ip          string `json:"ip"`
	DurationSec int    `json:"duration_sec"`
}

// ApiCtrlStartRelayReq 转推请求
// 从 RTSP 或 RTMP 拉流，然后转推到 RTMP 或 RTSP
// 注意：转推模式下，auto_stop_pull_after_no_out_ms 参数会被忽略，始终设置为 -1（不自动停止）
type ApiCtrlStartRelayReq struct {
	PullUrl                  string  `json:"pull_url"`                       // 拉流地址，支持 rtmp:// 或 rtsp://
	PushUrl                  string  `json:"push_url"`                       // 推流地址，支持 rtmp:// 或 rtsp://
	StreamName               string  `json:"stream_name"`                    // 流名称（可选，如果不提供则从 pull_url 解析）
	TimeoutMs                int     `json:"timeout_ms"`                     // 拉流和推流的超时时间（毫秒），默认 10000
	RetryNum                 int     `json:"retry_num"`                      // 拉流和推流的重试次数，-1表示永远重试，大于0表示重试次数，0表示不重试，默认 0
	AutoStopPullAfterNoOutMs int     `json:"auto_stop_pull_after_no_out_ms"` // 转推模式下此参数会被忽略，始终为 -1（不自动停止）
	RtspMode                 int     `json:"rtsp_mode"`                      // RTSP 模式，0=TCP，1=UDP，默认 0
	DebugDumpPacket          string  `json:"debug_dump_packet"`              // 转推模式下此参数会被忽略，数据不会落盘，直接转发
	Scale                    float64 `json:"scale"`                          // RTSP拉流时的播放速度倍数，合法范围 [1,8]，1 表示正常速度，>1 表示加速
}

// ApiCtrlStartRelayFromStreamReq 从已有流转推到其他协议（仅推流，不再额外拉流）
// 支持将当前 lal 内部已有的 stream_name 直接推送到 RTMP/RTSP 目标。
type ApiCtrlStartRelayFromStreamReq struct {
	StreamName string `json:"stream_name"` // 已存在的内部流名称（必填）
	PushUrl    string `json:"push_url"`    // 推流地址，支持 rtmp:// 或 rtsp://
	TimeoutMs  int    `json:"timeout_ms"`  // 推流超时时间（毫秒），默认 10000
	RetryNum   int    `json:"retry_num"`   // 推流重试次数，-1表示永远重试，大于0表示重试次数，0表示不重试
}

// ApiCtrlGb28181InviteReq GB28181拉流请求
type ApiCtrlGb28181InviteReq struct {
	DeviceId   string `json:"device_id"`   // 设备ID（国标编码）
	ChannelId  string `json:"channel_id"`  // 通道ID（国标编码）
	StreamName string `json:"stream_name"` // 流名称（必填，全局唯一）
	Port       int    `json:"port"`        // RTP接收端口（可选，0表示自动分配）
	IsTcpFlag  int    `json:"is_tcp_flag"` // 是否使用TCP传输（0=UDP，1=TCP，默认0）
	// StreamIndex 码流索引（可选；海康等设备常用 0=主，1=子，2=第三...）
	StreamIndex int `json:"stream_index"`
}

// ApiCtrlGb28181ByeReq GB28181停止拉流请求
type ApiCtrlGb28181ByeReq struct {
	DeviceId   string `json:"device_id"`   // 设备ID（国标编码，可选，不再用于停止逻辑）
	ChannelId  string `json:"channel_id"`  // 通道ID（国标编码，可选，不再用于停止逻辑）
	StreamName string `json:"stream_name"` // 流名称（必填，全局唯一）
}

// ApiCtrlGb28181PlaybackReq GB28181回放请求
type ApiCtrlGb28181PlaybackReq struct {
	DeviceId    string `json:"device_id"`    // 设备ID（国标编码，20位，必填）
	ChannelId   string `json:"channel_id"`   // 通道ID（国标编码，20位，必填）
	StreamName  string `json:"stream_name"`  // 流名称（必填，全局唯一，用于拉流与停止时标识）
	StartTime   string `json:"start_time"`   // 开始时间（必填）。格式：2006-01-02T15:04:05 或 2006-01-02 15:04:05；无时区按服务器本地时区解析（如东八区）
	EndTime     string `json:"end_time"`     // 结束时间（必填）。格式同上，须晚于 start_time
	Port        int    `json:"port"`         // RTP接收端口（可选，0=自动分配）
	IsTcpFlag   int    `json:"is_tcp_flag"`  // 传输方式：0=UDP（默认），1=TCP
	StreamIndex int    `json:"stream_index"` // 码流索引：0=主码流，1=子码流，2=第三码流…（默认0）
}

// ApiCtrlGb28181PlaybackScaleReq GB28181回放倍速控制请求（通过控制指令调整设备端推流速率，不改变本地拉流逻辑）
//
// 注意：
// - 该接口要求对应 stream_name 已经处于回放会话中（已建立 INVITE/ACK 对话），否则会返回失败。
// - 有效倍速范围为 [1,8]，1 表示正常速度，>1 表示加速播放。
type ApiCtrlGb28181PlaybackScaleReq struct {
	StreamName string  `json:"stream_name"` // 回放会话对应的流名称（必填）
	Scale      float64 `json:"scale"`       // 倍速（必填），合法值区间 [1,8]，例如 1/2/4/8
}

// ApiCtrlGb28181PtzReq GB28181 PTZ控制请求
type ApiCtrlGb28181PtzReq struct {
	DeviceId  string `json:"device_id"`  // 设备ID（国标编码）
	ChannelId string `json:"channel_id"` // 通道ID（国标编码）
	Command   string `json:"command"`    // PTZ命令：Up/Down/Left/Right/UpLeft/UpRight/DownLeft/DownRight/ZoomIn/ZoomOut/FocusNear/FocusFar/IrisOpen/IrisClose/Stop/SetPreset/CallPreset/DelPreset/StartCruise/StopCruise
	Speed     int    `json:"speed"`      // 速度 1-8（默认5，仅用于方向控制）
	Preset    int    `json:"preset"`     // 预置位编号（用于预置位相关命令）
}

// ApiQueryGb28181DeviceInfoReq GB28181查询设备信息请求
type ApiQueryGb28181DeviceInfoReq struct {
	DeviceId string `json:"device_id"` // 设备ID（国标编码）
}

// ApiQueryGb28181DeviceStatusReq GB28181查询设备状态请求
type ApiQueryGb28181DeviceStatusReq struct {
	DeviceId string `json:"device_id"` // 设备ID（国标编码）
}

// ApiQueryGb28181ChannelsReq GB28181查询通道列表请求
type ApiQueryGb28181ChannelsReq struct {
	DeviceId string `json:"device_id"` // 设备ID（国标编码）
}

// ----- response ------------------------------------------------------------------------------------------------------

const (
	ErrorCodeSucc = 0
	DespSucc      = "succ"

	ErrorCodePageNotFound = 404

	ErrorCodeGroupNotFound   = 1001
	DespGroupNotFound        = "group not found"
	ErrorCodeParamMissing    = 1002
	DespParamMissing         = "param missing"
	ErrorCodeSessionNotFound = 1003
	DespSessionNotFound      = "session not found"

	ErrorCodeStartRelayPullFail = 2001
	ErrorCodeListenUdpPortFail  = 2002
	ErrorCodeStartRelayFail     = 2003
	DespStartRelayFail          = "start relay fail"
	ErrorCodeStopRelayFail      = 2004
	DespStopRelayFail           = "stop relay fail"

	ErrorCodeGb28181DeviceNotFound   = 3001
	DespGb28181DeviceNotFound        = "gb28181 device not found"
	ErrorCodeGb28181InviteFail       = 3002
	DespGb28181InviteFail            = "gb28181 invite fail"
	ErrorCodeGb28181ByeFail          = 3003
	DespGb28181ByeFail               = "gb28181 bye fail"
	ErrorCodeGb28181PtzFail          = 3004
	DespGb28181PtzFail               = "gb28181 ptz control fail"
	ErrorCodeGb28181QueryFail        = 3005
	DespGb28181QueryFail             = "gb28181 query fail"
	ErrorCodeGb28181PlaybackCtrlFail = 3006
	DespGb28181PlaybackCtrlFail      = "gb28181 playback control fail"
)

type ApiRespBasic struct {
	ErrorCode int    `json:"error_code"`
	Desp      string `json:"desp"`
}

func ApiNotFoundRespFn() ApiRespBasic {
	return ApiRespBasic{
		ErrorCode: ErrorCodePageNotFound,
		Desp:      DespPageNotFound,
	}
}

type ApiStatLalInfoResp struct {
	ApiRespBasic
	Data LalInfo `json:"data"`
}

type ApiStatAllGroupResp struct {
	ApiRespBasic
	Data struct {
		Groups []StatGroup `json:"groups"`
	} `json:"data"`
}

type ApiStatGroupResp struct {
	ApiRespBasic
	Data *StatGroup `json:"data"`
}

type ApiCtrlStartRelayPullResp struct {
	ApiRespBasic
	Data struct {
		StreamName string `json:"stream_name"`
		SessionId  string `json:"session_id"`
	} `json:"data"`
}

type ApiCtrlStopRelayPullResp struct {
	ApiRespBasic
	Data struct {
		SessionId string `json:"session_id"`
	} `json:"data"`
}

type ApiCtrlKickSessionResp struct {
	ApiRespBasic
}

type ApiCtrlStartRtpPubResp struct {
	ApiRespBasic
	Data struct {
		StreamName string `json:"stream_name"`
		SessionId  string `json:"session_id"`
		Port       int    `json:"port"`
	} `json:"data"`
}

type ApiCtrlAddIpBlacklistResp struct {
	ApiRespBasic
}

type ApiCtrlStartRelayResp struct {
	ApiRespBasic
	Data struct {
		StreamName    string `json:"stream_name"`
		PullSessionId string `json:"pull_session_id"`
		PushSessionId string `json:"push_session_id"`
	} `json:"data"`
}

// ApiCtrlStartRelayFromStreamResp 从已有流转推响应
type ApiCtrlStartRelayFromStreamResp struct {
	ApiRespBasic
	Data struct {
		StreamName    string `json:"stream_name"`
		PushSessionId string `json:"push_session_id"`
	} `json:"data"`
}

type ApiCtrlStopRelayResp struct {
	ApiRespBasic
	Data struct {
		StreamName    string `json:"stream_name"`
		PullSessionId string `json:"pull_session_id"`
		PushSessionId string `json:"push_session_id"`
	} `json:"data"`
}

type ApiCtrlGb28181InviteResp struct {
	ApiRespBasic
	Data struct {
		StreamName string `json:"stream_name"`
		SessionId  string `json:"session_id"`
		Port       int    `json:"port"`
	} `json:"data"`
}

type ApiCtrlGb28181ByeResp struct {
	ApiRespBasic
	Data struct {
		StreamName string `json:"stream_name"`
		SessionId  string `json:"session_id"`
	} `json:"data"`
}

// ApiCtrlGb28181PlaybackScaleResp GB28181回放倍速控制响应
type ApiCtrlGb28181PlaybackScaleResp struct {
	ApiRespBasic
	Data struct {
		StreamName string  `json:"stream_name"`
		Scale      float64 `json:"scale"`
		SessionId  string  `json:"session_id"` // 对话 Call-ID（若可获取），便于排查
	} `json:"data"`
}

// ApiCtrlGb28181PlaybackResp GB28181回放响应
type ApiCtrlGb28181PlaybackResp struct {
	ApiRespBasic
	Data struct {
		StreamName string `json:"stream_name"` // 与请求一致，用于后续 bye 或拉流
		Port       int    `json:"port"`        // RTP 接收端口，可用于拉流地址
		SessionId  string `json:"session_id"`  // 预留，兼容字段
	} `json:"data"`
}

type ApiStatGb28181DeviceResp struct {
	ApiRespBasic
	Data struct {
		Devices []Gb28181DeviceInfo `json:"devices"`
	} `json:"data"`
}

type Gb28181DeviceInfo struct {
	DeviceId      string               `json:"device_id"`      // 设备ID
	DeviceName    string               `json:"device_name"`    // 设备名称
	Status        string               `json:"status"`         // 状态：online/offline
	RegisterTime  string               `json:"register_time"`  // 注册时间
	KeepaliveTime string               `json:"keepalive_time"` // 最后心跳时间
	Channels      []Gb28181ChannelInfo `json:"channels"`       // 通道列表
}

type ApiCtrlGb28181PtzResp struct {
	ApiRespBasic
	Data struct {
		DeviceId  string `json:"device_id"`
		ChannelId string `json:"channel_id"`
		Command   string `json:"command"`
	} `json:"data"`
}

type ApiQueryGb28181DeviceInfoResp struct {
	ApiRespBasic
	Data struct {
		DeviceId     string `json:"device_id"`
		DeviceName   string `json:"device_name"`
		Manufacturer string `json:"manufacturer"`
		Model        string `json:"model"`
		Firmware     string `json:"firmware"`
	} `json:"data"`
}

type ApiQueryGb28181DeviceStatusResp struct {
	ApiRespBasic
	Data struct {
		DeviceId string `json:"device_id"`
		Status   string `json:"status"` // online/offline
	} `json:"data"`
}

type ApiQueryGb28181ChannelsResp struct {
	ApiRespBasic
	Data struct {
		DeviceId string               `json:"device_id"`
		Channels []Gb28181ChannelInfo `json:"channels"`
	} `json:"data"`
}

type ApiStatGb28181StreamsResp struct {
	ApiRespBasic
	Data struct {
		Streams []Gb28181StreamInfo `json:"streams"`
	} `json:"data"`
}

type Gb28181StreamInfo struct {
	DeviceId   string `json:"device_id"`   // 设备ID
	ChannelId  string `json:"channel_id"`  // 通道ID
	StreamName string `json:"stream_name"` // 流名称
	CallId     string `json:"call_id"`     // SIP Call-ID
	Port       int    `json:"port"`        // RTP端口
	IsTcp      bool   `json:"is_tcp"`      // 是否TCP传输
	StartTime  string `json:"start_time"`  // 开始时间
}

type Gb28181ChannelInfo struct {
	ChannelId   string `json:"channel_id"`   // 通道ID
	ChannelName string `json:"channel_name"` // 通道名称
	Status      string `json:"status"`       // 状态：idle/streaming
	StreamName  string `json:"stream_name"`  // 流名称（如果正在推流）
}
