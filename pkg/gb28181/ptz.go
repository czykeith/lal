// Copyright 2024, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package gb28181

import (
	"fmt"
	"net"
)

// PTZCommand PTZ控制命令类型
type PTZCommand string

const (
	PTZUp          PTZCommand = "Up"          // 上
	PTZDown        PTZCommand = "Down"        // 下
	PTZLeft        PTZCommand = "Left"        // 左
	PTZRight       PTZCommand = "Right"       // 右
	PTZUpLeft      PTZCommand = "UpLeft"      // 左上
	PTZUpRight     PTZCommand = "UpRight"     // 右上
	PTZDownLeft    PTZCommand = "DownLeft"    // 左下
	PTZDownRight   PTZCommand = "DownRight"   // 右下
	PTZZoomIn      PTZCommand = "ZoomIn"      // 放大
	PTZZoomOut     PTZCommand = "ZoomOut"     // 缩小
	PTZFocusNear   PTZCommand = "FocusNear"   // 聚焦+
	PTZFocusFar    PTZCommand = "FocusFar"    // 聚焦-
	PTZIrisOpen    PTZCommand = "IrisOpen"    // 光圈+
	PTZIrisClose   PTZCommand = "IrisClose"   // 光圈-
	PTZStop        PTZCommand = "Stop"        // 停止
	PTZSetPreset   PTZCommand = "SetPreset"   // 设置预置位
	PTZCallPreset  PTZCommand = "CallPreset"  // 调用预置位
	PTZDelPreset   PTZCommand = "DelPreset"   // 删除预置位
	PTZStartCruise PTZCommand = "StartCruise" // 开始巡航
	PTZStopCruise  PTZCommand = "StopCruise"  // 停止巡航
)

// PTZControl PTZ控制参数
type PTZControl struct {
	DeviceId  string
	ChannelId string
	Command   PTZCommand
	Speed     int // 速度 1-8，默认5
	Preset    int // 预置位编号（用于预置位相关命令）
}

// ControlPTZ 发送PTZ控制命令
func (s *Gb28181Server) ControlPTZ(control PTZControl) error {
	s.mutex.RLock()
	device, exists := s.devices[control.DeviceId]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("device not found: %s", control.DeviceId)
	}

	// 确定通道ID
	channelId := control.ChannelId
	if channelId == "" {
		channelId = control.DeviceId
	}

	// 设置默认速度
	speed := control.Speed
	if speed <= 0 || speed > 8 {
		speed = 5
	}

	// 构建PTZ控制XML
	var xmlBody string
	switch control.Command {
	case PTZSetPreset, PTZCallPreset, PTZDelPreset:
		// 预置位相关命令
		if control.Preset <= 0 {
			return fmt.Errorf("preset number required for command: %s", control.Command)
		}
		xmlBody = buildPresetXML(control.Command, channelId, control.Preset)
	case PTZStartCruise, PTZStopCruise:
		// 巡航相关命令
		if control.Preset <= 0 {
			return fmt.Errorf("cruise route number required for command: %s", control.Command)
		}
		xmlBody = buildCruiseXML(control.Command, channelId, control.Preset)
	default:
		// 方向控制命令
		xmlBody = buildPTZControlXML(control.Command, channelId, speed)
	}

	// 构建MESSAGE请求（使用域格式，符合GB28181标准）
	deviceDomain := getDeviceDomain(control.DeviceId)
	requestUri := fmt.Sprintf("sip:%s@%s", channelId, deviceDomain)
	from := fmt.Sprintf("<sip:%s@%s>", s.config.LocalSipId, s.config.LocalSipDomain)
	to := fmt.Sprintf("<sip:%s@%s>", channelId, deviceDomain)

	branch := GenerateBranch()
	callId := GenerateCallId()
	headers := map[string]string{
		"Via":            fmt.Sprintf("SIP/2.0/UDP %s:%d;branch=%s;rport", s.config.LocalSipIp, s.config.LocalSipPort, branch),
		"From":           from + ";tag=" + GenerateTag(),
		"To":             to,
		"Call-ID":        callId,
		"CSeq":           "1 MESSAGE",
		"Max-Forwards":   "70",
		"Content-Type":   "Application/MANSCDP+xml",
		"Content-Length": fmt.Sprintf("%d", len(xmlBody)),
	}

	request := BuildSipRequest(SipMethodMessage, requestUri, headers, xmlBody)

	// 发送请求
	addr := &net.UDPAddr{
		IP:   net.ParseIP(device.Ip),
		Port: device.Port,
	}
	s.sendRaw(request, addr)

	Log.Infof("send PTZ control. device_id=%s, channel_id=%s, command=%s, speed=%d, preset=%d",
		control.DeviceId, channelId, control.Command, speed, control.Preset)

	return nil
}

// buildPTZControlXML 构建PTZ控制XML
func buildPTZControlXML(cmd PTZCommand, channelId string, speed int) string {
	// 根据命令类型构建PTZCmd
	var ptzCmd string
	switch cmd {
	case PTZUp:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed)
	case PTZDown:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x10)
	case PTZLeft:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x04)
	case PTZRight:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x08)
	case PTZUpLeft:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x14)
	case PTZUpRight:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x1C)
	case PTZDownLeft:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x10|0x04)
	case PTZDownRight:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x10|0x08)
	case PTZZoomIn:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x20)
	case PTZZoomOut:
		ptzCmd = fmt.Sprintf("A50F01%02XFF", speed|0x40)
	case PTZFocusNear:
		ptzCmd = "A50F0103FF"
	case PTZFocusFar:
		ptzCmd = "A50F0104FF"
	case PTZIrisOpen:
		ptzCmd = "A50F0105FF"
	case PTZIrisClose:
		ptzCmd = "A50F0106FF"
	case PTZStop:
		ptzCmd = "A50F0100FF"
	default:
		ptzCmd = "A50F0100FF" // 默认停止
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Control>
<CmdType>DeviceControl</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
<PTZCmd>%s</PTZCmd>
</Control>`, GenerateSN(), channelId, ptzCmd)
}

// buildPresetXML 构建预置位XML
func buildPresetXML(cmd PTZCommand, channelId string, preset int) string {
	var cmdType string
	switch cmd {
	case PTZSetPreset:
		cmdType = "Preset"
	case PTZCallPreset:
		cmdType = "Preset"
	case PTZDelPreset:
		cmdType = "Preset"
	default:
		cmdType = "Preset"
	}

	// 构建PTZCmd（预置位命令格式：A50F01XXFF，XX为预置位编号）
	ptzCmd := fmt.Sprintf("A50F01%02XFF", preset)
	if cmd == PTZSetPreset {
		ptzCmd = fmt.Sprintf("A50F01%02XFF", preset|0x80) // 设置预置位
	} else if cmd == PTZDelPreset {
		ptzCmd = fmt.Sprintf("A50F01%02XFF", preset|0x90) // 删除预置位
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Control>
<CmdType>%s</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
<PTZCmd>%s</PTZCmd>
</Control>`, cmdType, GenerateSN(), channelId, ptzCmd)
}

// buildCruiseXML 构建巡航XML
func buildCruiseXML(cmd PTZCommand, channelId string, route int) string {
	// 巡航命令格式：A50F01XXFF，XX为巡航路线编号
	ptzCmd := fmt.Sprintf("A50F01%02XFF", route)
	if cmd == PTZStopCruise {
		ptzCmd = "A50F0100FF" // 停止巡航
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Control>
<CmdType>Cruise</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
<PTZCmd>%s</PTZCmd>
</Control>`, GenerateSN(), channelId, ptzCmd)
}
