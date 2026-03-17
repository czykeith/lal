// Copyright 2020, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/q191201771/naza/pkg/nazajson"

	"github.com/q191201771/naza/pkg/nazahttp"

	"github.com/q191201771/lal/pkg/base"
)

//go:embed http_an__lal.html
var webUITpl string

type HttpApiServer struct {
	addr string
	sm   *ServerManager

	ln net.Listener
}

func NewHttpApiServer(addr string, sm *ServerManager) *HttpApiServer {
	return &HttpApiServer{
		addr: addr,
		sm:   sm,
	}
}

func (h *HttpApiServer) Listen() (err error) {
	if h.ln, err = net.Listen("tcp", h.addr); err != nil {
		return
	}
	Log.Infof("start http-api server listen. addr=%s", h.addr)
	return
}

func (h *HttpApiServer) RunLoop() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/lal.html", h.webUIHandler)

	mux.HandleFunc("/api/stat/group", h.statGroupHandler)
	mux.HandleFunc("/api/stat/all_group", h.statAllGroupHandler)
	mux.HandleFunc("/api/stat/lal_info", h.statLalInfoHandler)

	mux.HandleFunc("/api/ctrl/start_relay_pull", h.ctrlStartRelayPullHandler)
	mux.HandleFunc("/api/ctrl/stop_relay_pull", h.ctrlStopRelayPullHandler)
	mux.HandleFunc("/api/ctrl/kick_session", h.ctrlKickSessionHandler)
	mux.HandleFunc("/api/ctrl/add_ip_blacklist", h.ctrlAddIpBlacklistHandler)
	mux.HandleFunc("/api/ctrl/start_relay", h.ctrlStartRelayHandler)
	mux.HandleFunc("/api/ctrl/stop_relay", h.ctrlStopRelayHandler)
	mux.HandleFunc("/api/ctrl/start_relay_from_stream", h.ctrlStartRelayFromStreamHandler)

	mux.HandleFunc("/api/ctrl/gb28181_invite", h.ctrlGb28181InviteHandler)
	mux.HandleFunc("/api/ctrl/gb28181_bye", h.ctrlGb28181ByeHandler)
	mux.HandleFunc("/api/ctrl/gb28181_playback", h.ctrlGb28181PlaybackHandler)
	mux.HandleFunc("/api/ctrl/gb28181_playback_scale", h.ctrlGb28181PlaybackScaleHandler)
	mux.HandleFunc("/api/ctrl/gb28181_ptz", h.ctrlGb28181PtzHandler)
	mux.HandleFunc("/api/ctrl/gb28181_devices", h.statGb28181DevicesHandler)
	mux.HandleFunc("/api/ctrl/gb28181_streams", h.statGb28181StreamsHandler)
	mux.HandleFunc("/api/ctrl/gb28181_device_info", h.queryGb28181DeviceInfoHandler)
	mux.HandleFunc("/api/ctrl/gb28181_device_status", h.queryGb28181DeviceStatusHandler)
	mux.HandleFunc("/api/ctrl/gb28181_channels", h.queryGb28181ChannelsHandler)

	// GB28181 上级平台（中间平台）管理
	mux.HandleFunc("/api/stat/gb28181_upstreams", h.statGb28181UpstreamsHandler)
	mux.HandleFunc("/api/stat/gb28181_upstream_subs", h.statGb28181UpstreamSubsHandler)
	mux.HandleFunc("/api/ctrl/gb28181_upstream_sub_add", h.ctrlGb28181UpstreamSubAddHandler)
	mux.HandleFunc("/api/ctrl/gb28181_upstream_sub_del", h.ctrlGb28181UpstreamSubDelHandler)
	mux.HandleFunc("/api/ctrl/gb28181_upstreams_conf_set", h.ctrlGb28181UpstreamsConfSetHandler)
	mux.HandleFunc("/api/ctrl/gb28181_upstreams_reload", h.ctrlGb28181UpstreamsReloadHandler)

	// 通用截图接口：根据 stream_name 返回最近一帧关键帧的 JPEG 图片。
	mux.HandleFunc("/api/ctrl/snapshot", h.ctrlSnapshotHandler)

	// 所有没有注册路由的走下面这个处理函数
	mux.HandleFunc("/", h.notFoundHandler)

	var srv http.Server
	srv.Handler = mux
	return srv.Serve(h.ln)
}

// TODO chef: dispose

// ---------------------------------------------------------------------------------------------------------------------

func (h *HttpApiServer) statLalInfoHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiStatLalInfoResp
	v.ErrorCode = base.ErrorCodeSucc
	v.Desp = base.DespSucc
	v.Data = h.sm.StatLalInfo()
	feedback(v, w)
}

func (h *HttpApiServer) statAllGroupHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiStatAllGroupResp
	v.ErrorCode = base.ErrorCodeSucc
	v.Desp = base.DespSucc
	v.Data.Groups = h.sm.StatAllGroup()
	feedback(v, w)
}

func (h *HttpApiServer) statGroupHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiStatGroupResp

	q := req.URL.Query()
	streamName := q.Get("stream_name")
	if streamName == "" {
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	v.Data = h.sm.StatGroup(streamName)
	if v.Data == nil {
		v.ErrorCode = base.ErrorCodeGroupNotFound
		v.Desp = base.DespGroupNotFound
		feedback(v, w)
		return
	}

	v.ErrorCode = base.ErrorCodeSucc
	v.Desp = base.DespSucc
	feedback(v, w)
}

// ---------------------------------------------------------------------------------------------------------------------

// ctrlSnapshotHandler 截图接口：GET /api/ctrl/snapshot?stream_name=xxx
// 返回最近缓存的关键帧解码后的 JPEG 图片。
func (h *HttpApiServer) ctrlSnapshotHandler(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	streamName := q.Get("stream_name")
	if streamName == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("missing stream_name"))
		return
	}

	frame, ok := h.sm.GetSnapshotFrame(streamName)
	if !ok || frame == nil || len(frame.Data) == 0 {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no snapshot for stream"))
		return
	}

	jpg, err := convertAnnexbToJPEG(frame)
	if err != nil || len(jpg) == 0 {
		w.WriteHeader(http.StatusInternalServerError)
		if err != nil {
			_, _ = w.Write([]byte("snapshot decode failed: " + err.Error()))
		} else {
			_, _ = w.Write([]byte("snapshot decode failed"))
		}
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(jpg)
}

// ---------------------------------------------------------------------------------------------------------------------

func (h *HttpApiServer) ctrlStartRelayPullHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlStartRelayPullResp
	var info base.ApiCtrlStartRelayPullReq

	j, err := unmarshalRequestJsonBody(req, &info, "url")
	if err != nil {
		Log.Warnf("http api start pull error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	if !j.Exist("pull_timeout_ms") {
		info.PullTimeoutMs = DefaultApiCtrlStartRelayPullReqPullTimeoutMs
	}
	if !j.Exist("pull_retry_num") {
		info.PullRetryNum = base.PullRetryNumNever
	}
	if !j.Exist("auto_stop_pull_after_no_out_ms") {
		info.AutoStopPullAfterNoOutMs = base.AutoStopPullAfterNoOutMsNever
	}
	if !j.Exist("rtsp_mode") {
		info.RtspMode = base.RtspModeTcp
	}
	// scale 可选，默认 1，合法范围 [1,8]，超出则视为参数错误
	if !j.Exist("scale") {
		info.Scale = 1
	}
	if info.Scale < 1 || info.Scale > 8 {
		Log.Warnf("http api start pull invalid scale. scale=%v", info.Scale)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = "scale must be in [1,8]"
		feedback(v, w)
		return
	}

	Log.Infof("http api start pull. req info=%+v", info)

	resp := h.sm.CtrlStartRelayPull(info)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlStopRelayPullHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlStopRelayPullResp

	q := req.URL.Query()
	streamName := q.Get("stream_name")
	if streamName == "" {
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	Log.Infof("http api stop pull. stream_name=%s", streamName)

	resp := h.sm.CtrlStopRelayPull(streamName)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlKickSessionHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlKickSessionResp
	var info base.ApiCtrlKickSessionReq

	_, err := unmarshalRequestJsonBody(req, &info, "stream_name", "session_id")
	if err != nil {
		Log.Warnf("http api kick session error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	Log.Infof("http api kick session. req info=%+v", info)

	resp := h.sm.CtrlKickSession(info)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlAddIpBlacklistHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlAddIpBlacklistResp
	var info base.ApiCtrlAddIpBlacklistReq

	_, err := unmarshalRequestJsonBody(req, &info, "ip", "duration_sec")
	if err != nil {
		Log.Warnf("http api add ip blacklist error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	Log.Infof("http api add ip blacklist. req info=%+v", info)

	resp := h.sm.CtrlAddIpBlacklist(info)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlStartRelayHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlStartRelayResp
	var info base.ApiCtrlStartRelayReq

	j, err := unmarshalRequestJsonBody(req, &info, "pull_url", "push_url")
	if err != nil {
		Log.Warnf("http api start relay error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	// 设置默认值
	if !j.Exist("timeout_ms") {
		info.TimeoutMs = DefaultApiCtrlStartRelayPullReqPullTimeoutMs
	}
	if !j.Exist("retry_num") {
		info.RetryNum = base.PullRetryNumNever
	}
	if !j.Exist("auto_stop_pull_after_no_out_ms") {
		info.AutoStopPullAfterNoOutMs = base.AutoStopPullAfterNoOutMsNever
	}
	if !j.Exist("rtsp_mode") {
		info.RtspMode = base.RtspModeTcp
	}
	// scale 可选，默认 1，合法范围 [1,8]，超出则视为参数错误
	if !j.Exist("scale") {
		info.Scale = 1
	}
	if info.Scale < 1 || info.Scale > 8 {
		Log.Warnf("http api start relay invalid scale. scale=%v", info.Scale)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = "scale must be in [1,8]"
		feedback(v, w)
		return
	}

	Log.Infof("http api start relay. req info=%+v", info)

	resp := h.sm.CtrlStartRelay(info)
	feedback(resp, w)
}

// /api/ctrl/start_relay_from_stream
// 从已有的内部流（stream_name）发起转推，仅做推流，不再额外拉流。
func (h *HttpApiServer) ctrlStartRelayFromStreamHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlStartRelayFromStreamResp
	var info base.ApiCtrlStartRelayFromStreamReq

	j, err := unmarshalRequestJsonBody(req, &info, "stream_name", "push_url")
	if err != nil {
		Log.Warnf("http api start relay from stream error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	// 默认值
	if !j.Exist("timeout_ms") {
		info.TimeoutMs = DefaultApiCtrlStartRelayPullReqPullTimeoutMs
	}
	if !j.Exist("retry_num") {
		info.RetryNum = base.PullRetryNumNever
	}

	Log.Infof("http api start relay from stream. req info=%+v", info)

	resp := h.sm.CtrlStartRelayFromStream(info)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlStopRelayHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlStopRelayResp

	q := req.URL.Query()
	streamName := q.Get("stream_name")
	if streamName == "" {
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	Log.Infof("http api stop relay. stream_name=%s", streamName)

	resp := h.sm.CtrlStopRelay(streamName)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlGb28181InviteHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlGb28181InviteResp
	var info base.ApiCtrlGb28181InviteReq

	j, err := unmarshalRequestJsonBody(req, &info, "device_id", "channel_id", "stream_name")
	if err != nil {
		Log.Warnf("http api gb28181 invite error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	if !j.Exist("port") {
		info.Port = 0
	}
	if !j.Exist("is_tcp_flag") {
		info.IsTcpFlag = 0
	}
	// 统一使用 stream_index；未传时默认使用子码流索引 1（保持历史默认行为）
	if !j.Exist("stream_index") {
		info.StreamIndex = 1
	}

	Log.Infof("http api gb28181 invite. req info=%+v", info)

	resp := h.sm.CtrlGb28181Invite(info)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlGb28181ByeHandler(w http.ResponseWriter, req *http.Request) {
	var info base.ApiCtrlGb28181ByeReq

	_, err := unmarshalRequestJsonBody(req, &info, "stream_name")
	if err != nil {
		Log.Warnf("http api gb28181 bye error. err=%+v", err)
		var v base.ApiCtrlGb28181ByeResp
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	Log.Infof("http api gb28181 bye. req info=%+v", info)

	resp := h.sm.CtrlGb28181Bye(info)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlGb28181PlaybackHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlGb28181PlaybackResp
	var info base.ApiCtrlGb28181PlaybackReq

	j, err := unmarshalRequestJsonBody(req, &info, "device_id", "channel_id", "stream_name", "start_time", "end_time")
	if err != nil {
		Log.Warnf("http api gb28181 playback error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	if !j.Exist("port") {
		info.Port = 0
	}
	if !j.Exist("is_tcp_flag") {
		info.IsTcpFlag = 0
	}
	if !j.Exist("stream_index") {
		info.StreamIndex = 0
	}

	Log.Infof("http api gb28181 playback. req info=%+v", info)

	resp := h.sm.CtrlGb28181Playback(info)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlGb28181PlaybackScaleHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlGb28181PlaybackScaleResp
	var info base.ApiCtrlGb28181PlaybackScaleReq

	j, err := unmarshalRequestJsonBody(req, &info, "stream_name")
	if err != nil {
		Log.Warnf("http api gb28181 playback scale error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	// 默认 scale=1（正常速度），如果调用方未显式传入则按 1 处理。
	if !j.Exist("scale") {
		info.Scale = 1
	}
	// 接口层校验：合法范围 [1,8]
	if info.Scale < 1 || info.Scale > 8 {
		Log.Warnf("http api gb28181 playback scale invalid param. scale=%v", info.Scale)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = "scale must be in [1,8]"
		feedback(v, w)
		return
	}

	Log.Infof("http api gb28181 playback scale. req info=%+v", info)

	resp := h.sm.CtrlGb28181PlaybackScale(info)
	feedback(resp, w)
}

func (h *HttpApiServer) statGb28181DevicesHandler(w http.ResponseWriter, req *http.Request) {
	Log.Infof("http api stat gb28181 devices")

	resp := h.sm.StatGb28181Devices()
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlGb28181PtzHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlGb28181PtzResp
	var info base.ApiCtrlGb28181PtzReq

	j, err := unmarshalRequestJsonBody(req, &info, "device_id", "channel_id", "command")
	if err != nil {
		Log.Warnf("http api gb28181 ptz error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	if !j.Exist("speed") {
		info.Speed = 5 // 默认速度
	}
	if !j.Exist("preset") {
		info.Preset = 0
	}

	Log.Infof("http api gb28181 ptz. req info=%+v", info)

	resp := h.sm.CtrlGb28181Ptz(info)
	feedback(resp, w)
}

func (h *HttpApiServer) statGb28181StreamsHandler(w http.ResponseWriter, req *http.Request) {
	Log.Infof("http api stat gb28181 streams")

	resp := h.sm.StatGb28181Streams()
	feedback(resp, w)
}

func (h *HttpApiServer) queryGb28181DeviceInfoHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiQueryGb28181DeviceInfoResp
	var info base.ApiQueryGb28181DeviceInfoReq

	_, err := unmarshalRequestJsonBody(req, &info, "device_id")
	if err != nil {
		Log.Warnf("http api query gb28181 device info error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	Log.Infof("http api query gb28181 device info. req info=%+v", info)

	resp := h.sm.QueryGb28181DeviceInfo(info)
	feedback(resp, w)
}

func (h *HttpApiServer) queryGb28181DeviceStatusHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiQueryGb28181DeviceStatusResp
	var info base.ApiQueryGb28181DeviceStatusReq

	_, err := unmarshalRequestJsonBody(req, &info, "device_id")
	if err != nil {
		Log.Warnf("http api query gb28181 device status error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	Log.Infof("http api query gb28181 device status. req info=%+v", info)

	resp := h.sm.QueryGb28181DeviceStatus(info)
	feedback(resp, w)
}

func (h *HttpApiServer) queryGb28181ChannelsHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiQueryGb28181ChannelsResp
	var info base.ApiQueryGb28181ChannelsReq

	_, err := unmarshalRequestJsonBody(req, &info, "device_id")
	if err != nil {
		Log.Warnf("http api query gb28181 channels error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	Log.Infof("http api query gb28181 channels. req info=%+v", info)

	resp := h.sm.QueryGb28181Channels(info)
	feedback(resp, w)
}

// GET /api/stat/gb28181_upstreams
func (h *HttpApiServer) statGb28181UpstreamsHandler(w http.ResponseWriter, req *http.Request) {
	resp := h.sm.StatGb28181Upstreams()
	feedback(resp, w)
}

// GET /api/stat/gb28181_upstream_subs?upstream_id=xxx
func (h *HttpApiServer) statGb28181UpstreamSubsHandler(w http.ResponseWriter, req *http.Request) {
	upstreamID := req.URL.Query().Get("upstream_id")
	if upstreamID == "" {
		var v base.ApiGb28181UpstreamSubsResp
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = "upstream_id is required"
		feedback(v, w)
		return
	}
	resp := h.sm.StatGb28181UpstreamSubs(upstreamID)
	feedback(resp, w)
}

// POST /api/ctrl/gb28181_upstream_sub_add
func (h *HttpApiServer) ctrlGb28181UpstreamSubAddHandler(w http.ResponseWriter, req *http.Request) {
	var info base.ApiGb28181UpstreamSub
	_, err := unmarshalRequestJsonBody(req, &info, "upstream_id", "stream_name", "channel_id")
	if err != nil {
		Log.Warnf("http api gb28181 upstream sub add error. err=%+v", err)
		var v base.ApiRespBasic
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}
	resp := h.sm.CtrlGb28181UpstreamSubAdd(info)
	feedback(resp, w)
}

// POST /api/ctrl/gb28181_upstream_sub_del
func (h *HttpApiServer) ctrlGb28181UpstreamSubDelHandler(w http.ResponseWriter, req *http.Request) {
	var info base.ApiGb28181UpstreamSub
	_, err := unmarshalRequestJsonBody(req, &info, "upstream_id", "stream_name")
	if err != nil {
		Log.Warnf("http api gb28181 upstream sub del error. err=%+v", err)
		var v base.ApiRespBasic
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}
	resp := h.sm.CtrlGb28181UpstreamSubDel(info)
	feedback(resp, w)
}

// POST /api/ctrl/gb28181_upstreams_conf_set
func (h *HttpApiServer) ctrlGb28181UpstreamsConfSetHandler(w http.ResponseWriter, req *http.Request) {
	var confFile Gb28181UpstreamConfigFile
	body, err := io.ReadAll(req.Body)
	if err != nil {
		Log.Warnf("http api gb28181 upstreams conf set read body error. err=%+v", err)
		var v base.ApiRespBasic
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = err.Error()
		feedback(v, w)
		return
	}
	if err := json.Unmarshal(body, &confFile); err != nil {
		Log.Warnf("http api gb28181 upstreams conf set unmarshal error. err=%+v", err)
		var v base.ApiRespBasic
		v.ErrorCode = base.ErrorCodeGb28181InviteFail
		v.Desp = "invalid json: " + err.Error()
		feedback(v, w)
		return
	}
	resp := h.sm.CtrlGb28181UpstreamsConfSet(confFile)
	feedback(resp, w)
}

// POST /api/ctrl/gb28181_upstreams_reload
func (h *HttpApiServer) ctrlGb28181UpstreamsReloadHandler(w http.ResponseWriter, req *http.Request) {
	resp := h.sm.CtrlGb28181UpstreamsReload()
	feedback(resp, w)
}

func (h *HttpApiServer) webUIHandler(w http.ResponseWriter, req *http.Request) {
	t, err := template.New("webUI").Parse(webUITpl)
	if err != nil {
		Log.Errorf("invaild html template: %v", err)
		return
	}

	lalInfo := h.sm.StatLalInfo()
	data := map[string]interface{}{
		"ServerID":      lalInfo.ServerId,
		"LalVersion":    lalInfo.LalVersion,
		"ApiVersion":    lalInfo.ApiVersion,
		"NotifyVersion": lalInfo.NotifyVersion,
		"WebUiVersion":  lalInfo.WebUiVersion,
		"StartTime":     lalInfo.StartTime,
	}
	for _, item := range strings.Split(lalInfo.BinInfo, ". ") {
		if index := strings.Index(item, "="); index != -1 {
			k := item[:index]
			v := strings.TrimPrefix(strings.TrimSuffix(item[index:], "."), "=")
			data[k] = v
		}
	}
	t.Execute(w, data)
}

func (h *HttpApiServer) notFoundHandler(w http.ResponseWriter, req *http.Request) {
	Log.Warnf("invalid http-api request. uri=%s, raddr=%s", req.RequestURI, req.RemoteAddr)
	//w.WriteHeader(http.StatusNotFound)
	feedback(base.ApiNotFoundRespFn(), w)
}

// ---------------------------------------------------------------------------------------------------------------------

func feedback(v interface{}, w http.ResponseWriter) {
	resp, _ := json.Marshal(v)
	w.Header().Add("Server", base.LalHttpApiServer)
	base.AddCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// unmarshalRequestJsonBody
//
// TODO(chef): [refactor] 搬到naza中 202205
func unmarshalRequestJsonBody(r *http.Request, info interface{}, keyFieldList ...string) (nazajson.Json, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nazajson.Json{}, err
	}

	j, err := nazajson.New(body)
	if err != nil {
		return j, err
	}
	for _, kf := range keyFieldList {
		if !j.Exist(kf) {
			return j, nazahttp.ErrParamMissing
		}
	}

	return j, json.Unmarshal(body, info)
}
