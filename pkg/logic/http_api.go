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
	mux.HandleFunc("/api/ctrl/start_rtp_pub", h.ctrlStartRtpPubHandler)
	mux.HandleFunc("/api/ctrl/add_ip_blacklist", h.ctrlAddIpBlacklistHandler)
	mux.HandleFunc("/api/ctrl/start_relay", h.ctrlStartRelayHandler)
	mux.HandleFunc("/api/ctrl/stop_relay", h.ctrlStopRelayHandler)

	mux.HandleFunc("/api/ctrl/gb28181_invite", h.ctrlGb28181InviteHandler)
	mux.HandleFunc("/api/ctrl/gb28181_bye", h.ctrlGb28181ByeHandler)
	mux.HandleFunc("/api/ctrl/gb28181_playback", h.ctrlGb28181PlaybackHandler)
	mux.HandleFunc("/api/ctrl/gb28181_ptz", h.ctrlGb28181PtzHandler)
	mux.HandleFunc("/api/ctrl/gb28181_devices", h.statGb28181DevicesHandler)
	mux.HandleFunc("/api/ctrl/gb28181_streams", h.statGb28181StreamsHandler)
	mux.HandleFunc("/api/ctrl/gb28181_device_info", h.queryGb28181DeviceInfoHandler)
	mux.HandleFunc("/api/ctrl/gb28181_device_status", h.queryGb28181DeviceStatusHandler)
	mux.HandleFunc("/api/ctrl/gb28181_channels", h.queryGb28181ChannelsHandler)

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

func (h *HttpApiServer) ctrlStartRtpPubHandler(w http.ResponseWriter, req *http.Request) {
	var v base.ApiCtrlStartRtpPubResp
	var info base.ApiCtrlStartRtpPubReq

	j, err := unmarshalRequestJsonBody(req, &info, "stream_name")
	if err != nil {
		Log.Warnf("http api start rtp pub error. err=%+v", err)
		v.ErrorCode = base.ErrorCodeParamMissing
		v.Desp = base.DespParamMissing
		feedback(v, w)
		return
	}

	if !j.Exist("timeout_ms") {
		info.TimeoutMs = DefaultApiCtrlStartRtpPubReqTimeoutMs
	}
	// 不存在时默认0值的，不需要手动写了
	//if !j.Exist("port") {
	//	info.Port = 0
	//}
	//if !j.Exist("is_tcp_flag") {
	//	info.IsTcpFlag = 0
	//}

	Log.Infof("http api start rtp pub. req info=%+v", info)

	resp := h.sm.CtrlStartRtpPub(info)
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

	Log.Infof("http api start relay. req info=%+v", info)

	resp := h.sm.CtrlStartRelay(info)
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

	j, err := unmarshalRequestJsonBody(req, &info, "device_id", "channel_id")
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
	if !j.Exist("stream_type") {
		info.StreamType = 1 // 默认辅码流
	}

	Log.Infof("http api gb28181 invite. req info=%+v", info)

	resp := h.sm.CtrlGb28181Invite(info)
	feedback(resp, w)
}

func (h *HttpApiServer) ctrlGb28181ByeHandler(w http.ResponseWriter, req *http.Request) {
	var info base.ApiCtrlGb28181ByeReq

	_, err := unmarshalRequestJsonBody(req, &info, "device_id", "channel_id")
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

	j, err := unmarshalRequestJsonBody(req, &info, "device_id", "channel_id", "start_time", "end_time")
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
	if !j.Exist("scale") {
		info.Scale = 1.0 // 默认正常速度
	}

	Log.Infof("http api gb28181 playback. req info=%+v", info)

	resp := h.sm.CtrlGb28181Playback(info)
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
