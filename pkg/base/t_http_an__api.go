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
	Url                      string `json:"url"`
	StreamName               string `json:"stream_name"`
	PullTimeoutMs            int    `json:"pull_timeout_ms"`
	PullRetryNum             int    `json:"pull_retry_num"`
	AutoStopPullAfterNoOutMs int    `json:"auto_stop_pull_after_no_out_ms"`
	RtspMode                 int    `json:"rtsp_mode"`
	DebugDumpPacket          string `json:"debug_dump_packet"`
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
	PullUrl                  string `json:"pull_url"`                       // 拉流地址，支持 rtmp:// 或 rtsp://
	PushUrl                  string `json:"push_url"`                       // 推流地址，支持 rtmp:// 或 rtsp://
	StreamName               string `json:"stream_name"`                    // 流名称（可选，如果不提供则从 pull_url 解析）
	TimeoutMs                int    `json:"timeout_ms"`                     // 拉流和推流的超时时间（毫秒），默认 10000
	RetryNum                 int    `json:"retry_num"`                      // 拉流和推流的重试次数，-1表示永远重试，大于0表示重试次数，0表示不重试，默认 0
	AutoStopPullAfterNoOutMs int    `json:"auto_stop_pull_after_no_out_ms"` // 转推模式下此参数会被忽略，始终为 -1（不自动停止）
	RtspMode                 int    `json:"rtsp_mode"`                      // RTSP 模式，0=TCP，1=UDP，默认 0
	DebugDumpPacket          string `json:"debug_dump_packet"`              // 转推模式下此参数会被忽略，数据不会落盘，直接转发
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

type ApiCtrlStopRelayResp struct {
	ApiRespBasic
	Data struct {
		StreamName    string `json:"stream_name"`
		PullSessionId string `json:"pull_session_id"`
		PushSessionId string `json:"push_session_id"`
	} `json:"data"`
}
