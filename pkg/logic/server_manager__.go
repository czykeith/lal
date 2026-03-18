// Copyright 2019, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/q191201771/naza/pkg/taskpool"

	"github.com/q191201771/lal/pkg/avc"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/gb28181"
	"github.com/q191201771/lal/pkg/gb28181/mediaserver"
	"github.com/q191201771/lal/pkg/gb28181/mpegps"
	"github.com/q191201771/lal/pkg/hevc"
	"github.com/q191201771/lal/pkg/hls"
	"github.com/q191201771/lal/pkg/httpflv"
	"github.com/q191201771/lal/pkg/httpts"
	"github.com/q191201771/lal/pkg/rtmp"
	"github.com/q191201771/lal/pkg/rtsp"
	"github.com/q191201771/naza/pkg/defertaskthread"
	//"github.com/felixge/fgprof"
)

type ServerManager struct {
	option          Option
	serverStartTime string
	config          *Config

	httpServerManager *base.HttpServerManager
	httpServerHandler *HttpServerHandler
	hlsServerHandler  *hls.ServerHandler

	rtmpServer    *rtmp.Server
	rtmpsServer   *rtmp.Server
	rtspServer    *rtsp.Server
	rtspsServer   *rtsp.Server
	httpApiServer *HttpApiServer
	pprofServer   *http.Server
	wsrtspServer  *rtsp.WebsocketServer
	gb28181Server *gb28181.GB28181Server
	exitChan      chan struct{}

	mutex         sync.Mutex
	groupManager  IGroupManager
	snapshotStore *SnapshotStore
	// 缓存最近一次 StatAllGroup 的结果，避免高频 HTTP 调用时重复遍历所有 Group 计算统计。
	lastStatAllGroup   []base.StatGroup
	lastStatAllGroupAt time.Time

	onHookSession func(uniqueKey string, streamName string) ICustomizeHookSessionContext

	notifyHandlerThread taskpool.Pool

	ipBlacklist IpBlacklist
}

// gbLalAdapter 适配 logic.ILalServer 为 mediaserver.ILalServer，供 GB28181 媒体接入使用。
type gbLalAdapter struct {
	inner ILalServer
}

// AddCustomizePubSession 实现 mediaserver.ILalServer 接口，返回 mediaserver.CustomizePubSession。
func (a *gbLalAdapter) AddCustomizePubSession(streamName string) (mediaserver.CustomizePubSession, error) {
	return a.inner.AddCustomizePubSession(streamName)
}

// DelCustomizePubSession 实现 mediaserver.ILalServer 接口。
func (a *gbLalAdapter) DelCustomizePubSession(sess mediaserver.CustomizePubSession) {
	// 实际返回的是 logic.ICustomizePubSessionContext，这里做一次类型断言再删除，避免引入循环依赖。
	if ctx, ok := sess.(ICustomizePubSessionContext); ok {
		a.inner.DelCustomizePubSession(ctx)
		return
	}
	Log.Warnf("gb28181 del customize pub session ignored: unexpected type %T", sess)
}

// RequestUpstreamRtpFeed 实现 gb28181.UpstreamRtpFeeder，供非 GB28181 流上级转推时请求 RTP 喂流。
func (a *gbLalAdapter) RequestUpstreamRtpFeed(streamName string, feedFn func(rawRtp []byte)) (cancel func(), err error) {
	return a.inner.RequestUpstreamRtpFeed(streamName, feedFn)
}

// StatGroup 透传 logic 层的 StatGroup 能力，供 gb28181 用于查询任意 streamName 的在线状态。
func (a *gbLalAdapter) StatGroup(streamName string) *base.StatGroup {
	return a.inner.StatGroup(streamName)
}

func NewServerManager(modOption ...ModOption) *ServerManager {
	sm := &ServerManager{
		serverStartTime: base.ReadableNowTime(),
		exitChan:        make(chan struct{}, 1),
	}
	sm.groupManager = NewSimpleGroupManager(sm)
	sm.snapshotStore = NewSnapshotStore()

	sm.option = defaultOption
	for _, fn := range modOption {
		fn(&sm.option)
	}

	rawContent := sm.option.ConfRawContent
	if len(rawContent) == 0 {
		rawContent = base.WrapReadConfigFile(sm.option.ConfFilename, base.LalDefaultConfFilenameList, func() {
			_, _ = fmt.Fprintf(os.Stderr, `
Example:
  %s -c %s

Github: %s
Doc: %s
`, os.Args[0], filepath.FromSlash("./conf/"+base.LalDefaultConfigFilename), base.LalGithubSite, base.LalDocSite)
		})
	}
	sm.config = LoadConfAndInitLog(rawContent)
	base.LogoutStartInfo()

	if sm.config.HlsConfig.Enable && sm.config.HlsConfig.UseMemoryAsDiskFlag {
		Log.Infof("hls use memory as disk.")
		hls.SetUseMemoryAsDiskFlag(true)
	}

	if sm.config.RecordConfig.EnableFlv {
		if err := os.MkdirAll(sm.config.RecordConfig.FlvOutPath, 0777); err != nil {
			Log.Errorf("record flv mkdir error. path=%s, err=%+v", sm.config.RecordConfig.FlvOutPath, err)
		}
	}

	if sm.config.RecordConfig.EnableMpegts {
		if err := os.MkdirAll(sm.config.RecordConfig.MpegtsOutPath, 0777); err != nil {
			Log.Errorf("record mpegts mkdir error. path=%s, err=%+v", sm.config.RecordConfig.MpegtsOutPath, err)
		}
	}

	sm.nhInitNotifyHandler()

	if sm.config.HttpflvConfig.Enable || sm.config.HttpflvConfig.EnableHttps ||
		sm.config.HttptsConfig.Enable || sm.config.HttptsConfig.EnableHttps ||
		sm.config.HlsConfig.Enable || sm.config.HlsConfig.EnableHttps {
		sm.httpServerManager = base.NewHttpServerManager()
		sm.httpServerHandler = NewHttpServerHandler(sm)
		sm.hlsServerHandler = hls.NewServerHandler(sm.config.HlsConfig.OutPath, sm.config.HlsConfig.UrlPattern, sm.config.HlsConfig.SubSessionHashKey, sm.config.HlsConfig.SubSessionTimeoutMs, sm)
	}

	if sm.config.RtmpConfig.Enable {
		sm.rtmpServer = rtmp.NewServer(sm.config.RtmpConfig.Addr, sm)
	}
	if sm.config.RtmpConfig.RtmpsEnable {
		sm.rtmpsServer = rtmp.NewServer(sm.config.RtmpConfig.RtmpsAddr, sm)
	}
	if sm.config.RtspConfig.Enable {
		sm.rtspServer = rtsp.NewServer(sm.config.RtspConfig.Addr, sm, sm.config.RtspConfig.ServerAuthConfig)
	}
	if sm.config.RtspConfig.RtspsEnable {
		sm.rtspsServer = rtsp.NewServer(sm.config.RtspConfig.RtspsAddr, sm, sm.config.RtspConfig.ServerAuthConfig)
	}
	if sm.config.RtspConfig.WsRtspEnable {
		sm.wsrtspServer = rtsp.NewWebsocketServer(sm.config.RtspConfig.WsRtspAddr, sm, sm.config.RtspConfig.ServerAuthConfig)
	}
	if sm.config.HttpApiConfig.Enable {
		sm.httpApiServer = NewHttpApiServer(sm.config.HttpApiConfig.Addr, sm)
	}

	if sm.config.Gb28181Config.Enable {
		gb28181Conf := gb28181ConfigFromLogic(sm.config.Gb28181Config)
		adapter := &gbLalAdapter{inner: sm}
		sm.gb28181Server = gb28181.NewGB28181Server(gb28181Conf, adapter)
		sm.gb28181Server.Start()
		// 若独立配置文件中定义了上级平台的订阅关系，则在 gb28181Server 启动后进行初始化。
		// 仅对已启用的上级平台注入订阅，避免因上级 enable=false 导致 "upstream not found" 告警。
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
				Log.Warnf("init gb28181 upstream sub failed. upstream=%s stream=%s channel=%s err=%+v",
					sub.UpstreamID, sub.StreamName, sub.ChannelID, err)
			}
		}
		Log.Infof("gb28181 server started . upsteam_enable=%v", sm.config.Gb28181Config.UpstreamEnable)
	}

	if sm.config.PprofConfig.Enable {
		sm.pprofServer = &http.Server{Addr: sm.config.PprofConfig.Addr, Handler: nil}
	}

	if sm.option.Authentication == nil {
		sm.option.Authentication = NewSimpleAuthCtx(sm.config.SimpleAuthConfig)
	}

	return sm
}

// ----- implement ILalServer interface --------------------------------------------------------------------------------

func (sm *ServerManager) RunLoop() error {
	// TODO(chef): 作为阻塞函数，外部只能获取失败或结束的信息，没法获取到启动成功的信息

	sm.nhOnServerStart(sm.StatLalInfo())

	if sm.pprofServer != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					Log.Errorf("pprof server panic recovered: %+v", r)
				}
			}()
			//Log.Warn("start fgprof.")
			//http.DefaultServeMux.Handle("/debug/fgprof", fgprof.Handler())
			Log.Infof("start web pprof listen. addr=%s", sm.config.PprofConfig.Addr)
			if err := sm.pprofServer.ListenAndServe(); err != nil {
				// http.ErrServerClosed 是正常关闭，不需要记录错误
				if err != http.ErrServerClosed {
					Log.Errorf("pprof server error: %+v", err)
				} else {
					Log.Debug("pprof server closed")
				}
			}
		}()
	}

	go base.RunSignalHandler(func() {
		sm.Dispose()
	})

	var addMux = func(config CommonHttpServerConfig, handler base.Handler, name string) error {
		if config.Enable {
			err := sm.httpServerManager.AddListen(
				base.LocalAddrCtx{Addr: config.HttpListenAddr},
				config.UrlPattern,
				handler,
			)
			if err != nil {
				Log.Errorf("add http listen for %s failed. addr=%s, pattern=%s, err=%+v", name, config.HttpListenAddr, config.UrlPattern, err)
				return err
			}
			Log.Infof("add http listen for %s. addr=%s, pattern=%s", name, config.HttpListenAddr, config.UrlPattern)
		}
		if config.EnableHttps {
			err := sm.httpServerManager.AddListen(
				base.LocalAddrCtx{IsHttps: true, Addr: config.HttpsListenAddr, CertFile: config.HttpsCertFile, KeyFile: config.HttpsKeyFile},
				config.UrlPattern,
				handler,
			)
			if err != nil {
				Log.Errorf("add https listen for %s failed. addr=%s, pattern=%s, err=%+v", name, config.HttpsListenAddr, config.UrlPattern, err)
			} else {
				Log.Infof("add https listen for %s. addr=%s, pattern=%s", name, config.HttpsListenAddr, config.UrlPattern)
			}
		}
		return nil
	}

	if err := addMux(sm.config.HttpflvConfig.CommonHttpServerConfig, sm.httpServerHandler.ServeSubSession, "httpflv"); err != nil {
		return err
	}
	if err := addMux(sm.config.HttptsConfig.CommonHttpServerConfig, sm.httpServerHandler.ServeSubSession, "httpts"); err != nil {
		return err
	}
	if err := addMux(sm.config.HlsConfig.CommonHttpServerConfig, sm.serveHls, "hls"); err != nil {
		return err
	}

	if sm.httpServerManager != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					Log.Errorf("http server manager panic recovered: %+v", r)
				}
			}()
			if err := sm.httpServerManager.RunLoop(); err != nil {
				Log.Errorf("http server manager error: %+v", err)
			}
		}()
	}

	if sm.rtmpServer != nil {
		if err := sm.rtmpServer.Listen(); err != nil {
			return fmt.Errorf("rtmp server listen failed: %w", err)
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					Log.Errorf("rtmp server panic recovered: %+v", r)
				}
			}()
			if err := sm.rtmpServer.RunLoop(); err != nil {
				Log.Errorf("rtmp server error: %+v", err)
			}
		}()
	}

	if sm.rtmpsServer != nil {
		err := sm.rtmpsServer.ListenWithTLS(sm.config.RtmpConfig.RtmpsCertFile, sm.config.RtmpConfig.RtmpsKeyFile)
		// rtmps启动失败影响降级：当rtmps启动时我们并不返回错误，保证不因为rtmps影响其他服务
		if err == nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						Log.Errorf("rtmps server panic recovered: %+v", r)
					}
				}()
				if errRun := sm.rtmpsServer.RunLoop(); errRun != nil {
					Log.Errorf("rtmps server error: %+v", errRun)
				}
			}()
		} else {
			Log.Warnf("rtmps server listen failed (non-fatal): %+v", err)
		}
	}

	if sm.rtspServer != nil {
		if err := sm.rtspServer.Listen(); err != nil {
			return fmt.Errorf("rtsp server listen failed: %w", err)
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					Log.Errorf("rtsp server panic recovered: %+v", r)
				}
			}()
			if err := sm.rtspServer.RunLoop(); err != nil {
				Log.Errorf("rtsp server error: %+v", err)
			}
		}()
	}

	if sm.rtspsServer != nil {
		err := sm.rtspsServer.ListenWithTLS(sm.config.RtspConfig.RtspsCertFile, sm.config.RtspConfig.RtspsKeyFile)
		// rtsps启动失败影响降级：当rtsps启动时我们并不返回错误，保证不因为rtsps影响其他服务
		if err == nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						Log.Errorf("rtsps server panic recovered: %+v", r)
					}
				}()
				if errRun := sm.rtspsServer.RunLoop(); errRun != nil {
					Log.Errorf("rtsps server error: %+v", errRun)
				}
			}()
		} else {
			Log.Warnf("rtsps server listen failed (non-fatal): %+v", err)
		}
	}

	if sm.wsrtspServer != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					Log.Errorf("wsrtsp server panic recovered: %+v", r)
				}
			}()
			err := sm.wsrtspServer.Listen()
			if err != nil {
				Log.Errorf("wsrtsp server listen error: %+v", err)
			}
		}()
	}

	if sm.httpApiServer != nil {
		if err := sm.httpApiServer.Listen(); err != nil {
			return fmt.Errorf("http api server listen failed: %w", err)
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					Log.Errorf("http api server panic recovered: %+v", r)
				}
			}()
			if err := sm.httpApiServer.RunLoop(); err != nil {
				Log.Errorf("http api server error: %+v", err)
			}
		}()
	}

	// gb28181 (lalmax) 已在 Start() 中启动 SIP，无需在此 Listen/RunLoop

	uis := uint32(sm.config.HttpNotifyConfig.UpdateIntervalSec)
	var updateInfo base.UpdateInfo
	// 首次启动时构建一份 StatAllGroup 缓存，供 HTTP-API 与 Notify 复用。
	updateInfo.Groups = sm.StatAllGroup()
	sm.nhOnUpdate(updateInfo)

	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	var tickCount uint32
	for {
		select {
		case <-sm.exitChan:
			return nil
		case <-t.C:
			tickCount++

			sm.mutex.Lock()

			// 关闭空闲的group
			sm.groupManager.Iterate(func(group *Group) bool {
				if group.IsInactive() {
					Log.Infof("erase inactive group. [%s]", group.UniqueKey)
					group.Dispose()
					return false
				}

				group.Tick(tickCount)
				return true
			})

			// 每秒刷新一次 StatAllGroup 缓存，供 HTTP-API 使用。
			var sgs []base.StatGroup
			sm.groupManager.Iterate(func(group *Group) bool {
				sgs = append(sgs, group.GetStat(math.MaxInt32))
				return true
			})
			sm.lastStatAllGroup = sgs
			sm.lastStatAllGroupAt = time.Now()

			// 定时打印一些group相关的debug日志
			if sm.config.DebugConfig.LogGroupIntervalSec > 0 &&
				tickCount%uint32(sm.config.DebugConfig.LogGroupIntervalSec) == 0 {
				groupNum := sm.groupManager.Len()
				Log.Debugf("DEBUG_GROUP_LOG: group size=%d", groupNum)
				if sm.config.DebugConfig.LogGroupMaxGroupNum > 0 {
					var loggedGroupCount int
					sm.groupManager.Iterate(func(group *Group) bool {
						loggedGroupCount++
						if loggedGroupCount <= sm.config.DebugConfig.LogGroupMaxGroupNum {
							Log.Debugf("DEBUG_GROUP_LOG: %d %s", loggedGroupCount, group.StringifyDebugStats(sm.config.DebugConfig.LogGroupMaxSubNumPerGroup))
						}
						return true
					})
				}
			}

			sm.mutex.Unlock()

			// 定时通过http notify发送group相关的信息
			if uis != 0 && (tickCount%uis) == 0 {
				updateInfo.Groups = sm.StatAllGroup()
				sm.nhOnUpdate(updateInfo)
			}
		}
	}

	// never reach here
}

func (sm *ServerManager) Dispose() {
	Log.Debug("dispose server manager.")

	// 清理通知处理线程池
	if sm.notifyHandlerThread != nil {
		// taskpool.Pool 通常有 Stop 或 Close 方法，但为了兼容性，这里先检查
		// 如果 taskpool 提供了 Stop 方法，应该在这里调用
		// 注意：taskpool 的具体实现可能不同，需要根据实际情况调整
		if stopFunc, ok := interface{}(sm.notifyHandlerThread).(interface{ Stop() }); ok {
			stopFunc.Stop()
		}
	}

	// 清理所有服务器实例
	if sm.rtmpServer != nil {
		sm.rtmpServer.Dispose()
		sm.rtmpServer = nil
	}

	if sm.rtmpsServer != nil {
		sm.rtmpsServer.Dispose()
		sm.rtmpsServer = nil
	}

	if sm.rtspServer != nil {
		sm.rtspServer.Dispose()
		sm.rtspServer = nil
	}

	if sm.rtspsServer != nil {
		sm.rtspsServer.Dispose()
		sm.rtspsServer = nil
	}

	if sm.gb28181Server != nil {
		sm.gb28181Server.Dispose()
		sm.gb28181Server = nil
	}

	if sm.httpServerManager != nil {
		sm.httpServerManager.Dispose()
		sm.httpServerManager = nil
	}

	// 关闭 pprof 服务器
	if sm.pprofServer != nil {
		// 使用 Shutdown 进行优雅关闭，如果失败则使用 Close 强制关闭
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sm.pprofServer.Shutdown(ctx); err != nil {
			Log.Warnf("pprof server shutdown failed, force close: %+v", err)
			// Shutdown 失败时，使用 Close 强制关闭
			if closeErr := sm.pprofServer.Close(); closeErr != nil {
				Log.Warnf("pprof server force close failed: %+v", closeErr)
			}
		}
		sm.pprofServer = nil
	}

	//if sm.hlsServer != nil {
	//	sm.hlsServer.Dispose()
	//}

	// 清理所有 group，使用 defer 确保 mutex 解锁
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	sm.groupManager.Iterate(func(group *Group) bool {
		if group != nil {
			group.Dispose()
		}
		return true
	})

	// 发送退出信号，使用非阻塞方式避免死锁
	select {
	case sm.exitChan <- struct{}{}:
		// 成功发送
	default:
		// channel 已满或已关闭，记录警告但不阻塞
		Log.Warn("exit channel is full or closed, skip sending exit signal")
	}

	Log.Debug("server manager disposed successfully")
}

// ---------------------------------------------------------------------------------------------------------------------

func (sm *ServerManager) AddCustomizePubSession(streamName string) (ICustomizePubSessionContext, error) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getOrCreateGroup("", streamName)
	return group.AddCustomizePubSession(streamName)
}

func (sm *ServerManager) DelCustomizePubSession(sessionCtx ICustomizePubSessionContext) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup("", sessionCtx.StreamName())
	if group == nil {
		return
	}
	group.DelCustomizePubSession(sessionCtx)
}

func (sm *ServerManager) RequestUpstreamRtpFeed(streamName string, feedFn func(rawRtp []byte)) (cancel func(), err error) {
	if streamName == "" {
		return nil, fmt.Errorf("streamName is required")
	}
	group := sm.GetGroup("", streamName)
	if group == nil {
		return nil, fmt.Errorf("stream not found: %s", streamName)
	}
	// 基于 Group 的 AvPacket 订阅，将视频帧打成 PS 并封装为 PS/RTP，喂给上层的 feedFn。
	subID := fmt.Sprintf("upstream_rtp_%s_%d", streamName, time.Now().UnixNano())

	psMuxer := mpegps.NewPsMuxer()
	var videoSid uint8
	var inited bool
	var started bool
	var seq uint16
	psMuxer.OnPacket = func(ps []byte, pts90 uint64) {
		if len(ps) == 0 {
			return
		}
		// 使用 PS 包的 PTS(90k 时基) 作为 RTP 时间戳，保持与实际帧时间对齐。
		ts := uint32(pts90 & 0xffffffff)
		const maxPayload = 1300
		for off := 0; off < len(ps); {
			size := len(ps) - off
			if size > maxPayload {
				size = maxPayload
			}
			end := off + size
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: seq,
					Timestamp:      ts,
					Marker:         end >= len(ps), // 最后一片打 marker
				},
				Payload: ps[off:end],
			}
			seq++
			raw, err := pkt.Marshal()
			if err != nil {
				return
			}
			feedFn(raw)
			off = end
		}
	}

	cancelSub := group.AddAvPacketSubscriber(subID, func(pkt base.AvPacket) {
		if !pkt.IsVideo() {
			return
		}
		if !inited {
			switch pkt.PayloadType {
			case base.AvPacketPtAvc:
				videoSid = psMuxer.AddStream(mpegps.PsStreamH264)
			case base.AvPacketPtHevc:
				videoSid = psMuxer.AddStream(mpegps.PsStreamH265)
			default:
				// 非 H264/H265 暂不支持
				return
			}
			inited = true
		}
		// 在收到首个关键帧（IDR）之前，不向上级发送任何 PS/RTP，避免对端 HLS 反复报 V not opened。
		if !started {
			isKey := false
			switch pkt.PayloadType {
			case base.AvPacketPtAvc:
				isKey = isH264Idr(pkt.Payload)
			case base.AvPacketPtHevc:
				isKey = isHevcIdr(pkt.Payload)
			}
			if !isKey {
				return
			}
			started = true
		}
		pts := uint64(pkt.Pts)
		dts := uint64(pkt.Timestamp)
		func() {
			defer func() {
				if r := recover(); r != nil {
					// mpegps 组件内部存在 panic（如越界写），这里做保护，避免单帧异常导致进程崩溃。
					Log.Warnf("upstream rtp feed ps muxer panic recovered. streamName=%s panic=%v", streamName, r)
				}
			}()
			_ = psMuxer.Write(videoSid, pkt.Payload, pts, dts)
		}()
	})

	return func() { cancelSub() }, nil
}

// isH264Idr 判断 AnnexB H264 帧中是否包含 IDR 切片。
func isH264Idr(frame []byte) bool {
	// 扫描整帧所有 NALU，只要包含 IDR slice（type=5）即可视为关键帧。
	i := 0
	n := len(frame)
	for i+4 <= n {
		// 查找 start code 0x000001 或 0x00000001
		if frame[i] == 0x00 && frame[i+1] == 0x00 {
			if frame[i+2] == 0x01 {
				i += 3
			} else if i+3 < n && frame[i+2] == 0x00 && frame[i+3] == 0x01 {
				i += 4
			} else {
				i++
				continue
			}
			// 跳过可能的填充 0x00
			for i < n && frame[i] == 0x00 {
				i++
			}
			if i >= n {
				return false
			}
			// nalu header 第一个字节
			t := avc.ParseNaluType(frame[i])
			if t == avc.NaluTypeIdrSlice {
				return true
			}
			// 继续扫描下一个 start code（当前 NALU 的内容无需逐字节跳过，因为我们在 i++ 的循环里继续找）
		}
		i++
	}
	return false
}

// isHevcIdr 判断 AnnexB H265 帧中是否包含 IDR/I 帧。
func isHevcIdr(frame []byte) bool {
	// 扫描整帧所有 NALU，只要包含 IRAP/BLA/CRA 等 VCL 即视为关键帧起点。
	i := 0
	n := len(frame)
	for i+5 <= n {
		if frame[i] == 0x00 && frame[i+1] == 0x00 {
			if frame[i+2] == 0x01 {
				i += 3
			} else if i+3 < n && frame[i+2] == 0x00 && frame[i+3] == 0x01 {
				i += 4
			} else {
				i++
				continue
			}
			for i < n && frame[i] == 0x00 {
				i++
			}
			if i >= n {
				return false
			}
			naluType := hevc.ParseNaluType(frame[i])
			if naluType >= hevc.NaluTypeSliceBlaWlp && naluType <= hevc.NaluTypeSliceRsvIrapVcl23 {
				return true
			}
			// 继续扫描后续 NALU
		}
		i++
	}
	return false
}

func (sm *ServerManager) WithOnHookSession(onHookSession func(uniqueKey string, streamName string) ICustomizeHookSessionContext) {
	sm.onHookSession = onHookSession
}

// ----- implement rtmp.IServerObserver interface -----------------------------------------------------------------------

func (sm *ServerManager) OnRtmpConnect(session *rtmp.ServerSession, opa rtmp.ObjectPairArray) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var info base.RtmpConnectInfo
	info.SessionId = session.UniqueKey()
	info.RemoteAddr = session.GetStat().RemoteAddr
	info.App, _ = opa.FindString("app")
	info.FlashVer, _ = opa.FindString("flashVer")
	info.TcUrl, _ = opa.FindString("tcUrl")
	sm.nhOnRtmpConnect(info)
}

func (sm *ServerManager) OnNewRtmpPubSession(session *rtmp.ServerSession) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	info := base.Session2PubStartInfo(session)

	// 先做simple auth鉴权
	if err := sm.option.Authentication.OnPubStart(info); err != nil {
		return err
	}

	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	if err := group.AddRtmpPubSession(session); err != nil {
		return err
	}

	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()

	sm.nhOnPubStart(info)
	return nil
}

func (sm *ServerManager) OnDelRtmpPubSession(session *rtmp.ServerSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelRtmpPubSession(session)

	info := base.Session2PubStopInfo(session)
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	sm.nhOnPubStop(info)
}

func (sm *ServerManager) OnNewRtmpSubSession(session *rtmp.ServerSession) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	info := base.Session2SubStartInfo(session)

	if err := sm.option.Authentication.OnSubStart(info); err != nil {
		return err
	}

	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	group.AddRtmpSubSession(session)

	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()

	sm.nhOnSubStart(info)
	return nil
}

func (sm *ServerManager) OnDelRtmpSubSession(session *rtmp.ServerSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelRtmpSubSession(session)

	info := base.Session2SubStopInfo(session)
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	sm.nhOnSubStop(info)
}

// ----- implement IHttpServerHandlerObserver interface -----------------------------------------------------------------

func (sm *ServerManager) OnNewHttpflvSubSession(session *httpflv.SubSession) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	info := base.Session2SubStartInfo(session)

	if err := sm.option.Authentication.OnSubStart(info); err != nil {
		return err
	}

	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	group.AddHttpflvSubSession(session)

	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()

	sm.nhOnSubStart(info)
	return nil
}

func (sm *ServerManager) OnDelHttpflvSubSession(session *httpflv.SubSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelHttpflvSubSession(session)

	info := base.Session2SubStopInfo(session)
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	sm.nhOnSubStop(info)
}

func (sm *ServerManager) OnNewHttptsSubSession(session *httpts.SubSession) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	info := base.Session2SubStartInfo(session)

	if err := sm.option.Authentication.OnSubStart(info); err != nil {
		return err
	}

	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	group.AddHttptsSubSession(session)

	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()

	sm.nhOnSubStart(info)

	return nil
}

func (sm *ServerManager) OnDelHttptsSubSession(session *httpts.SubSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelHttptsSubSession(session)

	info := base.Session2SubStopInfo(session)
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	sm.nhOnSubStop(info)
}

// ----- implement rtsp.IServerObserver interface -----------------------------------------------------------------------

func (sm *ServerManager) OnNewRtspSessionConnect(session *rtsp.ServerCommandSession) {
	// TODO chef: impl me
}

func (sm *ServerManager) OnDelRtspSession(session *rtsp.ServerCommandSession) {
	// TODO chef: impl me
}

func (sm *ServerManager) OnNewRtspPubSession(session *rtsp.PubSession) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	info := base.Session2PubStartInfo(session)

	if err := sm.option.Authentication.OnPubStart(info); err != nil {
		return err
	}

	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	if err := group.AddRtspPubSession(session); err != nil {
		return err
	}

	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()

	sm.nhOnPubStart(info)
	return nil
}

func (sm *ServerManager) OnDelRtspPubSession(session *rtsp.PubSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelRtspPubSession(session)

	info := base.Session2PubStopInfo(session)
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	sm.nhOnPubStop(info)
}

func (sm *ServerManager) OnNewRtspSubSessionDescribe(session *rtsp.SubSession) (ok bool, sdp []byte) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	info := base.Session2SubStartInfo(session)

	if err := sm.option.Authentication.OnSubStart(info); err != nil {
		return false, nil
	}

	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	ok, sdp = group.HandleNewRtspSubSessionDescribe(session)
	if !ok {
		return
	}

	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()

	sm.nhOnSubStart(info)
	return
}

func (sm *ServerManager) OnNewRtspSubSessionPlay(session *rtsp.SubSession) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	group.HandleNewRtspSubSessionPlay(session)
	return nil
}

func (sm *ServerManager) OnDelRtspSubSession(session *rtsp.SubSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelRtspSubSession(session)

	info := base.Session2SubStopInfo(session)
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	sm.nhOnSubStop(info)
}

func (sm *ServerManager) OnNewHlsSubSession(session *hls.SubSession) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	info := base.Session2SubStartInfo(session)

	if err := sm.option.Authentication.OnSubStart(info); err != nil {
		return err
	}

	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	group.AddHlsSubSession(session)

	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()

	sm.nhOnSubStart(info)

	return nil
}

func (sm *ServerManager) OnDelHlsSubSession(session *hls.SubSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelHlsSubSession(session)

	info := base.Session2SubStopInfo(session)
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	sm.nhOnSubStop(info)

	// 当最后一个 HLS 订阅者离开，且当前没有任何输入/输出会话时，
	// 立即停止 HLS Muxer，并触发 HLS 目录清理（包括内存模式下的片段释放），以降低内存占用。
	if !group.HasInSession() && !group.HasOutSession() && !group.HasHlsSubSession() {
		group.stopHlsIfNeeded()
	}
}

// ----- implement IGroupCreator interface -----------------------------------------------------------------------------

func (sm *ServerManager) CreateGroup(appName string, streamName string) *Group {
	var config *Config
	if sm.option.ModConfigGroupCreator != nil {
		cloneConfig := *sm.config
		sm.option.ModConfigGroupCreator(appName, streamName, &cloneConfig)
		config = &cloneConfig
	} else {
		config = sm.config
	}
	option := GroupOption{
		onHookSession: sm.onHookSession,
	}
	return NewGroup(appName, streamName, config, option, sm)
}

// ----- implement IGroupObserver interface -----------------------------------------------------------------------------

func (sm *ServerManager) CleanupHlsIfNeeded(appName string, streamName string, path string) {
	if sm.config.HlsConfig.Enable &&
		(sm.config.HlsConfig.CleanupMode == hls.CleanupModeInTheEnd || sm.config.HlsConfig.CleanupMode == hls.CleanupModeAsap) {
		defertaskthread.Go(
			sm.config.HlsConfig.FragmentDurationMs*(sm.config.HlsConfig.FragmentNum+sm.config.HlsConfig.DeleteThreshold),
			func(param ...interface{}) {
				an := param[0].(string)
				sn := param[1].(string)
				outPath := param[2].(string)

				if g := sm.GetGroup(an, sn); g != nil {
					if g.IsHlsMuxerAlive() {
						Log.Warnf("cancel cleanup hls file path since hls muxer still alive. streamName=%s", sn)
						return
					}
				}

				Log.Infof("cleanup hls file path. streamName=%s, path=%s", sn, outPath)
				if err := hls.RemoveAll(outPath); err != nil {
					Log.Warnf("cleanup hls file path error. path=%s, err=%+v", outPath, err)
				}
			},
			appName,
			streamName,
			path,
		)
	}
}

func (sm *ServerManager) OnRelayPullStart(info base.PullStartInfo) {
	sm.nhOnRelayPullStart(info)
}

func (sm *ServerManager) OnRelayPullStop(info base.PullStopInfo) {
	sm.nhOnRelayPullStop(info)
}

func (sm *ServerManager) OnHlsMakeTs(info base.HlsMakeTsInfo) {
	sm.nhOnHlsMakeTs(info)
}

// ---------------------------------------------------------------------------------------------------------------------

func (sm *ServerManager) Config() *Config {
	return sm.config
}

// UpdateSnapshot 实现 IGroupObserver：将关键帧写入全局截图缓存。
func (sm *ServerManager) UpdateSnapshot(streamName string, pkt base.AvPacket) {
	if sm.snapshotStore == nil {
		return
	}
	sm.snapshotStore.Update(streamName, pkt)
}

// GetSnapshotFrame 提供给 HTTP API，按 streamName 获取最新关键帧。
func (sm *ServerManager) GetSnapshotFrame(streamName string) (*SnapshotFrame, bool) {
	if sm.snapshotStore == nil {
		return nil, false
	}
	return sm.snapshotStore.Get(streamName)
}

func (sm *ServerManager) GetGroup(appName string, streamName string) *Group {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	return sm.getGroup(appName, streamName)
}

// ----- private method ------------------------------------------------------------------------------------------------

// 注意，函数内部不加锁，由调用方保证加锁进入
func (sm *ServerManager) getOrCreateGroup(appName string, streamName string) *Group {
	g, createFlag := sm.groupManager.GetOrCreateGroup(appName, streamName)
	if createFlag {
		go g.RunLoop()
	}
	return g
}

func (sm *ServerManager) getGroup(appName string, streamName string) *Group {
	return sm.groupManager.GetGroup(appName, streamName)
}

func (sm *ServerManager) serveHls(writer http.ResponseWriter, req *http.Request) {
	urlCtx, err := base.ParseUrl(base.ParseHttpRequest(req), 80)
	if err != nil {
		Log.Errorf("parse url. err=%+v", err)
		return
	}

	if urlCtx.GetFileType() == "m3u8" {
		// TODO(chef): [refactor] 需要整理，这里使用 hls.PathStrategy 不太好 202207
		streamName := hls.PathStrategy.GetRequestInfo(urlCtx, sm.config.HlsConfig.OutPath).StreamName
		if err = sm.option.Authentication.OnHls(streamName, urlCtx.RawQuery); err != nil {
			Log.Errorf("simple auth failed. err=%+v", err)
			return
		}
	}

	remoteIp, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		Log.Warnf("SplitHostPort failed. addr=%s, err=%+v", req.RemoteAddr, err)
		return
	}

	if sm.ipBlacklist.Has(remoteIp) {
		//Log.Warnf("found %s in ip blacklist, so do not serve this request.", remoteIp)

		sm.hlsServerHandler.CloseSubSessionIfExist(req)

		writer.WriteHeader(http.StatusNotFound)
		return
	}

	sm.hlsServerHandler.ServeHTTP(writer, req)
}

// gb28181ConfigFromLogic 将 logic 的 Gb28181Config 转换为 gb28181.GB28181Config（lalmax 格式）
func gb28181ConfigFromLogic(c Gb28181Config) gb28181.GB28181Config {
	rtpMin := c.SipRtpPortMin
	rtpMax := c.SipRtpPortMax
	if rtpMin == 0 {
		rtpMin = c.RtpPortMin
	}
	if rtpMax == 0 {
		rtpMax = c.RtpPortMax
	}
	if rtpMin == 0 {
		rtpMin = 30000
	}
	if rtpMax == 0 {
		rtpMax = 60000
	}
	inc := uint16(rtpMax - rtpMin)
	if inc > 3000 {
		inc = 3000
	}
	sipPort := uint16(c.LocalSipPort)
	if sipPort == 0 {
		sipPort = 5060
	}
	realm := c.LocalSipDomain
	if realm == "" {
		realm = c.LocalSipId
	}
	if len(realm) > 10 {
		realm = realm[:10]
	}
	// 视频参数：优先使用嵌套 video，否则用平铺字段（flexInt 已支持 JSON 数字或字符串）
	videoCodec, videoWidth, videoHeight := c.VideoCodec, int(c.VideoWidth), int(c.VideoHeight)
	videoBitrate, videoFramerate := int(c.VideoBitrate), int(c.VideoFramerate)
	videoProfile, videoLevel := c.VideoProfile, c.VideoLevel
	if c.Video != nil {
		if c.Video.Codec != "" {
			videoCodec = c.Video.Codec
		}
		if c.Video.Width > 0 {
			videoWidth = int(c.Video.Width)
		}
		if c.Video.Height > 0 {
			videoHeight = int(c.Video.Height)
		}
		if c.Video.Bitrate > 0 {
			videoBitrate = int(c.Video.Bitrate)
		}
		if c.Video.Framerate > 0 {
			videoFramerate = int(c.Video.Framerate)
		}
		if c.Video.Profile != "" {
			videoProfile = c.Video.Profile
		}
		if c.Video.Level != "" {
			videoLevel = c.Video.Level
		}
	}
	cfg := gb28181.GB28181Config{
		Enable:     true,
		ListenAddr: "0.0.0.0",
		SipIP:      c.LocalSipIp,
		SipPort:    sipPort,
		UpstreamSipPort: func(v int) uint16 {
			if v <= 0 {
				return 5061
			}
			return uint16(v)
		}(c.UpstreamSipPort),
		Serial:                   c.LocalSipId,
		Realm:                    realm,
		Username:                 c.Username,
		Password:                 c.Password,
		KeepaliveInterval:        60,
		QuickLogin:               true,
		AllowNonStandardDeviceId: c.AllowNonStandardDeviceId,
		MediaConfig: gb28181.GB28181MediaConfig{
			MediaIp:               c.LocalSipIp,
			ListenPort:            uint16(rtpMin),
			MultiPortMaxIncrement: inc,
		},
		VideoCodec:            videoCodec,
		VideoWidth:            videoWidth,
		VideoHeight:           videoHeight,
		VideoBitrate:          videoBitrate,
		VideoFramerate:        videoFramerate,
		VideoProfile:          videoProfile,
		VideoLevel:            videoLevel,
		AutoRetryOnDisconnect: c.AutoRetryOnDisconnect,
		RetryMaxCount:         defaultGb28181RetryMaxCount(c.AutoRetryOnDisconnect, c.RetryMaxCount),
		RetryFirstDelayMs:     retryFirstDelayMs(c.RetryFirstDelayMs),
		RetryMaxDelayMs:       retryMaxDelayMs(c.RetryMaxDelayMs),
		CatalogQueryInterval:  c.CatalogQueryInterval,
		UpstreamEnable:        c.UpstreamEnable,
	}

	// 映射上级平台配置（级联）
	if len(c.Upstreams) > 0 {
		cfg.Upstreams = make([]gb28181.GB28181UpstreamConfig, 0, len(c.Upstreams))
		for _, u := range c.Upstreams {
			up := gb28181.GB28181UpstreamConfig{
				ID:               u.ID,
				Enable:           u.Enable,
				SipID:            u.SipID,
				Realm:            u.Realm,
				SipIP:            u.SipIP,
				SipPort:          uint16(u.SipPort),
				LocalDeviceID:    u.LocalDeviceID,
				Username:         u.Username,
				Password:         u.Password,
				RegisterValidity: u.RegisterValidity,
				KeepaliveInterval: func(v int) int {
					if v <= 0 {
						return 60
					}
					return v
				}(u.KeepaliveInterval),
				MediaIP:   u.MediaIP,
				MediaPort: uint16(u.MediaPort),
				Comment:   u.Comment,
			}
			cfg.Upstreams = append(cfg.Upstreams, up)
		}
	}

	return cfg
}

func defaultGb28181RetryMaxCount(autoRetry bool, v int) int {
	if autoRetry && v == 0 {
		return 3
	}
	return v
}
func retryFirstDelayMs(v int) int {
	if v <= 0 {
		return 3000
	}
	return v
}
func retryMaxDelayMs(v int) int {
	if v <= 0 {
		return 60000
	}
	return v
}
