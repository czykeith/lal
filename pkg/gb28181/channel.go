package gb28181

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/q191201771/naza/pkg/nazaatomic"

	"github.com/ghettovoice/gosip/sip"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/gb28181/mediaserver"
)

type Channel struct {
	device *Device // 所属设备
	//status  atomic.Int32 // 通道状态,0:空闲,1:正在invite,2:正在播放
	GpsTime time.Time // gps时间
	number  uint16
	ackReq  sip.Request

	observer IMediaOpObserver
	playInfo *PlayInfo

	ChannelInfo
	conf GB28181Config
}

// Channel 通道
type ChannelInfo struct {
	ChannelId    string        `xml:"DeviceID"`     // 设备id
	ParentId     string        `xml:"ParentID"`     //父目录Id
	Name         string        `xml:"Name"`         //设备名称
	Manufacturer string        `xml:"Manufacturer"` //制造厂商
	Model        string        `xml:"Model"`        //型号
	Owner        string        `xml:"Owner"`        //设备归属
	CivilCode    string        `xml:"CivilCode"`    //行政区划编码
	Address      string        `xml:"Address"`      //地址
	Port         int           `xml:"Port"`         //端口
	Parental     int           `xml:"Parental"`     //存在子设备，这里表明有子目录存在 1代表有子目录，0表示没有
	SafetyWay    int           `xml:"SafetyWay"`    //信令安全模式（可选）缺省为 0；0：不采用；2：S/MIME 签名方式；3：S/MIME	加密签名同时采用方式；4：数字摘要方式
	RegisterWay  int           `xml:"RegisterWay"`  //标准的认证注册模式
	Secrecy      int           `xml:"Secrecy"`      //0 表示不涉密
	Status       ChannelStatus `xml:"Status"`       // 状态  on 在线 off离线
	Longitude    string        `xml:"Longitude"`    // 经度
	Latitude     string        `xml:"Latitude"`     // 纬度
	StreamName   string        `xml:"-"`
	serial       string
	mediaserver.MediaInfo
	sn nazaatomic.Uint32
}

type ChannelStatus string

const (
	ChannelOnStatus  = "ON"
	ChannelOffStatus = "OFF"
)

func (channel *Channel) WithMediaServer(observer IMediaOpObserver) {
	channel.observer = observer
}

func (channel *Channel) TryAutoInvite(opt *InviteOptions, streamName string, playInfo *PlayInfo) {
	if channel.CanInvite(streamName) {
		go channel.Invite(opt, streamName, playInfo, nil)
	}
}

func (channel *Channel) CanInvite(streamName string) bool {
	if len(channel.ChannelId) != 20 || channel.Status == ChannelOffStatus {
		base.Log.Info("return false,  channel.DeviceID:", len(channel.ChannelId), " channel.Status:", channel.Status)
		return false
	}
	if channel.Parental != 0 {
		return false
	}

	if channel.MediaInfo.IsInvite {
		return false
	}

	// 11～13位是设备类型编码
	typeID := channel.ChannelId[10:13]
	if typeID == "132" || typeID == "131" {
		return true
	}

	base.Log.Info("return false")

	return false
}

// Invite 发送Invite报文 invites a channel to play
// 注意里面的锁保证不同时发送invite报文，该锁由channel持有
/***
f字段： f = v/编码格式/分辨率/帧率/码率类型/码率大小a/编码格式/码率大小/采样率
各项具体含义：
    v：后续参数为视频的参数；各参数间以 “/”分割；
编码格式：十进制整数字符串表示
1 –MPEG-4 2 –H.264 3 – SVAC 4 –3GP
    分辨率：十进制整数字符串表示
1 – QCIF 2 – CIF 3 – 4CIF 4 – D1 5 –720P 6 –1080P/I
帧率：十进制整数字符串表示 0～99
码率类型：十进制整数字符串表示
1 – 固定码率（CBR）     2 – 可变码率（VBR）
码率大小：十进制整数字符串表示 0～100000（如 1表示1kbps）
    a：后续参数为音频的参数；各参数间以 “/”分割；
编码格式：十进制整数字符串表示
1 – G.711    2 – G.723.1     3 – G.729      4 – G.722.1
码率大小：十进制整数字符串
音频编码码率： 1 — 5.3 kbps （注：G.723.1中使用）
   2 — 6.3 kbps （注：G.723.1中使用）
   3 — 8 kbps （注：G.729中使用）
   4 — 16 kbps （注：G.722.1中使用）
   5 — 24 kbps （注：G.722.1中使用）
   6 — 32 kbps （注：G.722.1中使用）
   7 — 48 kbps （注：G.722.1中使用）
   8 — 64 kbps（注：G.711中使用）
采样率：十进制整数字符串表示
	1 — 8 kHz（注：G.711/ G.723.1/ G.729中使用）
	2—14 kHz（注：G.722.1中使用）
	3—16 kHz（注：G.722.1中使用）
	4—32 kHz（注：G.722.1中使用）
	注1：字符串说明
本节中使用的“十进制整数字符串”的含义为“0”～“4294967296” 之间的十进制数字字符串。
注2：参数分割标识
各参数间以“/”分割，参数间的分割符“/”不能省略；
若两个分割符 “/”间的某参数为空时（即两个分割符 “/”直接将相连时）表示无该参数值；
注3：f字段说明
使用f字段时，应保证视频和音频参数的结构完整性，即在任何时候，f字段的结构都应是完整的结构：
f = v/编码格式/分辨率/帧率/码率类型/码率大小a/编码格式/码率大小/采样率
若只有视频时，音频中的各参数项可以不填写，但应保持 “a///”的结构:
f = v/编码格式/分辨率/帧率/码率类型/码率大小a///
若只有音频时也类似处理，视频中的各参数项可以不填写，但应保持 “v/”的结构：
f = v/a/编码格式/码率大小/采样率
f字段中视、音频参数段之间不需空格分割。
可使用f字段中的分辨率参数标识同一设备不同分辨率的码流。
*/

// Invite 发起 INVITE。sdpConf 可选：非 nil 时用其生成 SDP 的 f=/fmtp，否则用 channel.conf（保证 API 调用时传入服务器配置可使视频参数生效）。
func (channel *Channel) Invite(opt *InviteOptions, streamName string, playInfo *PlayInfo, sdpConf *GB28181Config) (code int, err error) {
	d := channel.device
	sdpCfg := channel.conf
	if sdpConf != nil {
		sdpCfg = *sdpConf
	}
	// 实时点播 s=Play；历史回放 s=Playback（国标要求）
	s := "Play"
	if opt != nil && !opt.IsLive() {
		s = "Playback"
	}

	//然后按顺序生成，一个channel最大999 方便排查问题,也能保证唯一性
	channel.number++
	if channel.number > 999 {
		channel.number = 1
	}
	if len(channel.serial) == 0 {
		channel.serial = RandNumString(6)
	}
	opt.CreateSSRC(channel.serial, channel.number)

	var mediaserver *mediaserver.GB28181MediaServer
	if channel.observer != nil {
		mediaserver = channel.observer.OnStartMediaServer(playInfo.NetWork, playInfo.SinglePort, channel.device.ID, channel.ChannelId)
	}
	if mediaserver == nil {
		return http.StatusNotFound, err
	}

	protocol := ""
	if playInfo.NetWork == "tcp" {
		opt.MediaPort = mediaserver.GetListenerPort()
		protocol = "TCP/"
	} else {
		opt.MediaPort = mediaserver.GetListenerPort()
	}

	// 提前写入 MediaInfo（避免设备在 200 OK 后立刻推流导致 SSRC 校验竞态）
	// 注意：GB28181Server.CheckSsrc/GetMediaInfoByKey 会要求 IsInvite=true 才视为有效。
	channel.playInfo = playInfo
	channel.MediaInfo.IsInvite = true
	channel.MediaInfo.Ssrc = opt.SSRC
	channel.MediaInfo.StreamName = streamName
	channel.MediaInfo.SinglePort = playInfo.SinglePort
	channel.MediaInfo.DumpFileName = playInfo.DumpFileName
	channel.MediaInfo.MediaKey = fmt.Sprintf("%s%d", playInfo.NetWork, mediaserver.GetListenerPort())

	sdpInfo := []string{
		"v=0",
		fmt.Sprintf("o=%s 0 0 IN IP4 %s", channel.ChannelId, d.mediaIP),
		"s=" + s,
	}
	// 回放时必须有 u= 字段（通道ID:回放类型，0=回放 3=下载）
	if opt != nil && !opt.IsLive() {
		sdpInfo = append(sdpInfo, fmt.Sprintf("u=%s:0", channel.ChannelId))
	}
	sdpInfo = append(sdpInfo,
		"c=IN IP4 "+d.mediaIP,
		opt.String(),
		fmt.Sprintf("m=video %d %sRTP/AVP 96", opt.MediaPort, protocol),
		"a=recvonly",
		"a=rtpmap:96 PS/90000",
		"y="+opt.ssrc,
	)

	// 视频参数（GB28181 常用：f= 行；部分设备也会接受 fmtp 扩展）
	// f= v/编码格式/分辨率/帧率/码率类型/码率大小 a///
	//
	// 编码格式（常见映射）：1=H264，4=H265；分辨率：1=QCIF，2=CIF，3=4CIF，4=720P，5=1080P；也可用 WxH。
	if f := buildGb28181SdpFLine(sdpCfg); f != "" {
		sdpInfo = append(sdpInfo, "f="+f)
	}
	if fmtp := buildGb28181SdpFmtpLine(sdpCfg); fmtp != "" {
		sdpInfo = append(sdpInfo, "a=fmtp:96 "+fmtp)
	}

	// 码流索引（常用扩展，海康等设备支持，0=主，1=子，2=第三...）
	// 说明：
	// - 海康常见写法：Subject 为 "通道ID:索引,平台ID:索引"（示例：3402..:0,1001:0）
	// - SDP 扩展常见写法：a=streamprofile:<index> / a=streamid:<index>
	streamIndex := playInfo.StreamIndex
	if streamIndex < 0 {
		streamIndex = 0
	}
	sdpInfo = append(sdpInfo,
		fmt.Sprintf("a=streamprofile:%d", streamIndex),
		fmt.Sprintf("a=streamid:%d", streamIndex),
	)

	if playInfo.NetWork == "tcp" {
		sdpInfo = append(sdpInfo, "a=setup:passive", "a=connection:new")
	}
	// 回放倍速（国标常见 a=scale 或 downloadspeed；部分设备用 a=scale:1.0）
	if opt != nil && !opt.IsLive() && opt.Scale > 0 {
		sdpInfo = append(sdpInfo, fmt.Sprintf("a=scale:%.2f", opt.Scale))
	}

	invite := channel.CreateRequst(sip.INVITE, channel.conf)
	contentType := sip.ContentType("application/sdp")
	invite.AppendHeader(&contentType)

	body := strings.Join(sdpInfo, "\r\n") + "\r\n"
	contentLength := sip.ContentLength(len(body))
	invite.AppendHeader(&contentLength)

	invite.SetBody(body, true)

	// Subject：通道ID:索引,平台ID:索引
	// 说明：
	// - 主流 stream_index=0 时为 ...:0,...:0（与常见示例一致）
	// - 子码流等其它索引用 stream_index 直接表示，便于部分实现按 Subject 选择码流。
	subject := sip.GenericHeader{
		HeaderName: "Subject", Contents: fmt.Sprintf("%s:%d,%s:%d", channel.ChannelId, streamIndex, channel.conf.Serial, streamIndex),
	}
	invite.AppendHeader(&subject)

	base.Log.Info("GB28181 INVITE >>>", "\n", invite.String())

	inviteRes, err := d.SipRequestForResponse(invite)
	if err != nil {
		base.Log.Error("invite failed, err:", err, " invite msg:", invite.String())

		//jay 在media端口监听成功后，但是sip发送失败时
		if channel.observer != nil {
			if err = channel.observer.OnStopMediaServer(playInfo.NetWork, playInfo.SinglePort, channel.device.ID, channel.ChannelId, ""); err != nil {
				base.Log.Errorf("gb28181 MediaServer stop err:%s", err.Error())
			}
		}
		channel.MediaInfo.Clear()
		channel.playInfo = nil

		return http.StatusInternalServerError, err
	}
	code = int(inviteRes.StatusCode())
	if code == http.StatusOK {
		ds := strings.Split(inviteRes.Body(), "\r\n")
		for _, l := range ds {
			if ls := strings.Split(l, "="); len(ls) > 1 {
				if ls[0] == "y" && len(ls[1]) > 0 {
					yv := strings.TrimSpace(ls[1])
					if _ssrc, err := strconv.ParseInt(yv, 10, 0); err == nil {
						// 部分设备返回 y=0000000000 或 y=0，但 RTP 实际 SSRC 非 0。
						// 这里仅在 y>0 时覆盖，否则保留请求侧生成的 SSRC。
						if _ssrc > 0 {
							opt.SSRC = uint32(_ssrc)
						}
					} else {
						base.Log.Error("parse invite response y failed, err:", err)
					}
				}
				if ls[0] == "m" && len(ls[1]) > 0 {
					netinfo := strings.Split(ls[1], " ")
					if strings.ToUpper(netinfo[2]) == "TCP/RTP/AVP" {
						base.Log.Info("Device support tcp")
					} else {
						base.Log.Info("Device not support tcp")
					}
				}
			}
		}
		// 这里以最终 SSRC 覆盖（通常等于请求侧生成的 SSRC；若设备返回 y>0 则以设备返回为准）
		channel.MediaInfo.Ssrc = opt.SSRC

		ackReq := sip.NewAckRequest("", invite, inviteRes, "", nil)
		//保存一下播放信息
		channel.ackReq = ackReq
		channel.playInfo = playInfo

		err = channel.device.sipSvr.Send(ackReq)
	} else {

		if channel.observer != nil {
			if err = channel.observer.OnStopMediaServer(playInfo.NetWork, playInfo.SinglePort, channel.device.ID, channel.ChannelId, ""); err != nil {
				base.Log.Errorf("gb28181 MediaServer stop err:%s", err.Error())
			}
		}
		channel.MediaInfo.Clear()
		channel.playInfo = nil

	}
	return
}

func buildGb28181SdpFLine(conf GB28181Config) string {
	// video fields
	codec := strings.ToUpper(strings.TrimSpace(conf.VideoCodec))
	var vEnc string
	switch codec {
	case "", "H264":
		// 默认 H264
		vEnc = "1"
	case "H265", "HEVC":
		vEnc = "4"
	default:
		// 未知则不强制声明
		vEnc = ""
	}

	// resolution mapping
	var vRes string
	w, h := conf.VideoWidth, conf.VideoHeight
	if w > 0 && h > 0 {
		switch {
		case w == 176 && h == 144:
			vRes = "1" // QCIF
		case w == 352 && h == 288:
			vRes = "2" // CIF
		case (w == 704 && h == 576) || (w == 720 && h == 576):
			vRes = "3" // 4CIF/D1
		case w == 1280 && h == 720:
			vRes = "4" // 720P
		case w == 1920 && h == 1080:
			vRes = "5" // 1080P
		default:
			// 2022 扩展：支持 WxH
			vRes = fmt.Sprintf("%dx%d", w, h)
		}
	}

	var vFps string
	if conf.VideoFramerate > 0 {
		vFps = fmt.Sprintf("%d", conf.VideoFramerate)
	}

	// bitrate: type + size(kbps)
	var vBrType string
	var vBrSize string
	if conf.VideoBitrate > 0 {
		vBrType = "1"
		vBrSize = fmt.Sprintf("%d", conf.VideoBitrate)
	}

	// no fields -> return empty
	if vEnc == "" && vRes == "" && vFps == "" && vBrType == "" && vBrSize == "" {
		return ""
	}

	// keep full structure with empty fields allowed
	// v/<enc>/<res>/<fps>/<brType>/<brSize>a///
	return fmt.Sprintf("v/%s/%s/%s/%s/%sa///", vEnc, vRes, vFps, vBrType, vBrSize)
}

func buildGb28181SdpFmtpLine(conf GB28181Config) string {
	// PS over RTP 的 fmtp 并非标准，但部分设备会解析；做 best-effort 传递。
	profile := strings.TrimSpace(conf.VideoProfile)
	level := strings.TrimSpace(conf.VideoLevel)
	if profile == "" && level == "" {
		return ""
	}
	if profile != "" && level != "" {
		return fmt.Sprintf("profile=%s;level=%s", profile, level)
	}
	if profile != "" {
		return fmt.Sprintf("profile=%s", profile)
	}
	return fmt.Sprintf("level=%s", level)
}
func (channel *Channel) GetCallId() string {
	if channel.ackReq != nil {
		if callId, ok := channel.ackReq.CallID(); ok {
			return callId.Value()
		}
	}
	return ""
}
func (channel *Channel) stopMediaServer() (err error) {
	if channel.playInfo != nil {
		if channel.observer != nil {
			if err = channel.observer.OnStopMediaServer(channel.playInfo.NetWork, channel.playInfo.SinglePort, channel.device.ID, channel.ChannelId, channel.playInfo.StreamName); err != nil {
				base.Log.Errorf("gb28181 MediaServer stop err:%s", err.Error())
			}
		}
	}
	return
}
func (channel *Channel) byeClear() (err error) {
	err = channel.stopMediaServer()
	channel.ackReq = nil
	channel.MediaInfo.Clear()
	return
}
func (channel *Channel) Bye(streamName string) (err error) {
	if channel.ackReq != nil {
		byeReq := channel.ackReq
		channel.ackReq = nil
		byeReq.SetMethod(sip.BYE)
		seq, _ := byeReq.CSeq()
		seq.SeqNo += 1

		base.Log.Info("GB28181 BYE >>>", "\n", byeReq.String())

		channel.device.sipSvr.Send(byeReq)
	}
	channel.stopMediaServer()
	// 无论是否存在 ackReq，都视为“停止动作已执行”（幂等），避免 API 层因重复 stop 或竞态导致失败。
	channel.MediaInfo.Clear()
	channel.playInfo = nil
	return err
}
func (channel *Channel) CreateRequst(Method sip.RequestMethod, conf GB28181Config) (req sip.Request) {
	d := channel.device
	d.sn++

	callId := sip.CallID(RandNumString(10))
	userAgent := sip.UserAgentHeader("LALMax")
	maxForwards := sip.MaxForwards(70) //增加max-forwards为默认值 70
	cseq := sip.CSeq{
		SeqNo:      uint32(d.sn),
		MethodName: Method,
	}
	port := sip.Port(conf.SipPort)
	serverAddr := sip.Address{
		Uri: &sip.SipUri{
			FUser: sip.String{Str: conf.Serial},
			FHost: d.sipIP,
			FPort: &port,
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: RandNumString(9)}),
	}
	//非同一域的目标地址需要使用@host
	host := conf.Realm
	if channel.ChannelId[0:9] != host {
		if channel.Port != 0 {
			deviceIp := d.NetAddr
			deviceIp = deviceIp[0:strings.LastIndex(deviceIp, ":")]
			host = fmt.Sprintf("%s:%d", deviceIp, channel.Port)
		} else {
			host = d.NetAddr
		}
	}

	channelAddr := sip.Address{
		Uri: &sip.SipUri{FUser: sip.String{Str: channel.ChannelId}, FHost: host},
	}
	req = sip.NewRequest(
		"",
		Method,
		channelAddr.Uri,
		"SIP/2.0",
		[]sip.Header{
			serverAddr.AsFromHeader(),
			channelAddr.AsToHeader(),
			&callId,
			&userAgent,
			&cseq,
			&maxForwards,
			serverAddr.AsContactHeader(),
		},
		"",
		nil,
	)

	req.SetTransport(channel.device.network)
	req.SetDestination(d.NetAddr)
	return req
}
func (channel *Channel) PtzDirection(direction *PtzDirection) error {
	ptz := Ptz{
		ZoomOut: false,
		ZoomIn:  false,
		Up:      direction.Up,
		Down:    direction.Down,
		Left:    direction.Left,
		Right:   direction.Right,
		Speed:   direction.Speed,
	}
	msgPtz := &MessagePtz{
		CmdType:  DeviceControl,
		DeviceID: direction.ChannelId,
		SN:       int(channel.sn.Add(1)),
		PTZCmd:   ptz.Pack(),
	}
	xml, err := XmlEncode(msgPtz)
	if err != nil {
		return err
	}
	return channel.sipMessage(xml)
}
func (channel *Channel) PtzZoom(zoom *PtzZoom) error {
	ptz := Ptz{
		ZoomOut: zoom.ZoomOut,
		ZoomIn:  zoom.ZoomIn,
		Speed:   zoom.Speed,
	}
	msgPtz := &MessagePtz{
		CmdType:  DeviceControl,
		DeviceID: zoom.ChannelId,
		SN:       int(channel.sn.Add(1)),
		PTZCmd:   ptz.Pack(),
	}
	xml, err := XmlEncode(msgPtz)
	if err != nil {
		return err
	}
	return channel.sipMessage(xml)
}
func (channel *Channel) PtzFi(fi *PtzFi) error {
	ptzFi := Fi{
		IrisIn:    fi.IrisIn,
		IrisOut:   fi.IrisOut,
		FocusNear: fi.FocusNear,
		FocusFar:  fi.FocusFar,
		Speed:     fi.Speed,
	}
	msgPtz := &MessagePtz{
		CmdType:  DeviceControl,
		DeviceID: fi.ChannelId,
		SN:       int(channel.sn.Add(1)),
		PTZCmd:   ptzFi.Pack(),
	}
	xml, err := XmlEncode(msgPtz)
	if err != nil {
		return err
	}
	return channel.sipMessage(xml)
}
func (channel *Channel) PtzPreset(ptzPreset *PtzPreset) error {
	cmd := byte(PresetSet)
	switch ptzPreset.Cmd {
	case PresetEditPoint:
		cmd = PresetSet
	case PresetDelPoint:
		cmd = PresetDel
	case PresetCallPoint:
		cmd = PresetCall
	default:
		return errors.New(fmt.Sprintf("ptz preset cmd error:%d", ptzPreset.Cmd))
	}
	preset := Preset{
		CMD:   cmd,
		Point: ptzPreset.Point,
	}
	msgPtz := &MessagePtz{
		CmdType:  DeviceControl,
		DeviceID: ptzPreset.ChannelId,
		SN:       int(channel.sn.Add(1)),
		PTZCmd:   preset.Pack(),
	}
	xml, err := XmlEncode(msgPtz)
	if err != nil {
		return err
	}
	return channel.sipMessage(xml)
}
func (channel *Channel) PtzStop(stop *PtzStop) error {
	ptz := Ptz{}
	msgPtz := &MessagePtz{
		CmdType:  DeviceControl,
		DeviceID: stop.ChannelId,
		SN:       int(channel.sn.Add(1)),
		PTZCmd:   ptz.Pack(),
	}
	xml, err := XmlEncode(msgPtz)
	if err != nil {
		return err
	}
	return channel.sipMessage(xml)
}
func (channel *Channel) sipMessage(xml string) error {
	d := channel.device
	msg := channel.CreateRequst(sip.MESSAGE, channel.conf)
	msg.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSCDP+xml"})
	msg.SetBody(xml, true)
	msgRes, err := d.SipRequestForResponse(msg)
	if err != nil {
		return err
	}

	code := int(msgRes.StatusCode())
	if code == http.StatusOK {
		return nil
	} else {
		return errors.New(fmt.Sprintf("sip message ptz fail,code:%d", code))
	}
}
