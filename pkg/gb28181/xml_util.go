// Copyright 2024, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package gb28181

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// BuildCatalogQueryXML 构建目录查询XML
// 注意：Catalog 查询时，DeviceID 应该填写要查询的设备ID（通常是设备自身）
func BuildCatalogQueryXML(deviceId string, sn int) string {
	// GB28181 标准格式：Catalog 查询时 DeviceID 填写要查询的设备ID
	// 如果查询设备自身的通道列表，DeviceID 填写设备ID
	// 如果查询子设备的通道列表，DeviceID 填写子设备ID
	// 注意：某些设备要求 XML 格式紧凑（无换行），且不需要 encoding 属性
	return fmt.Sprintf(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>%d</SN><DeviceID>%s</DeviceID></Query>`, sn, deviceId)
}

// BuildDeviceInfoQueryXML 构建设备信息查询XML
func BuildDeviceInfoQueryXML(deviceId string, sn int) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Query>
<CmdType>DeviceInfo</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
</Query>`, sn, deviceId)
}

// BuildDeviceStatusQueryXML 构建设备状态查询XML
func BuildDeviceStatusQueryXML(deviceId string, sn int) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Query>
<CmdType>DeviceStatus</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
</Query>`, sn, deviceId)
}

// BuildAlarmResponseXML 构建告警响应XML
func BuildAlarmResponseXML(deviceId, cmdType, sn string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
<CmdType>%s</CmdType>
<SN>%s</SN>
<DeviceID>%s</DeviceID>
<Result>OK</Result>
</Response>`, cmdType, sn, deviceId)
}

// BuildDeviceInfoResponseXML 构建设备信息响应XML
func BuildDeviceInfoResponseXML(deviceId, deviceName, manufacturer, model, firmware string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
<CmdType>DeviceInfo</CmdType>
<DeviceID>%s</DeviceID>
<DeviceName>%s</DeviceName>
<Manufacturer>%s</Manufacturer>
<Model>%s</Model>
<Firmware>%s</Firmware>
</Response>`, deviceId, deviceName, manufacturer, model, firmware)
}

// DecodeGbkToUtf8 将GBK编码的XML转换为UTF-8
func DecodeGbkToUtf8(data []byte) ([]byte, error) {
	// 检查是否是GBK/GB2312编码
	xmlStr := string(data)
	if !strings.Contains(xmlStr, "encoding=\"GB2312\"") &&
		!strings.Contains(xmlStr, "encoding='GB2312'") &&
		!strings.Contains(xmlStr, "encoding=\"GBK\"") &&
		!strings.Contains(xmlStr, "encoding='GBK'") {
		// 不是GBK编码，直接返回
		return data, nil
	}

	// 转换GBK到UTF-8
	reader := transform.NewReader(bytes.NewReader(data), simplifiedchinese.GBK.NewDecoder())
	utf8Bytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("convert GBK to UTF-8 failed: %w", err)
	}

	// 替换XML声明中的编码为UTF-8
	utf8Str := string(utf8Bytes)
	utf8Str = strings.ReplaceAll(utf8Str, "encoding=\"GB2312\"", "encoding=\"UTF-8\"")
	utf8Str = strings.ReplaceAll(utf8Str, "encoding='GB2312'", "encoding='UTF-8'")
	utf8Str = strings.ReplaceAll(utf8Str, "encoding=\"GBK\"", "encoding=\"UTF-8\"")
	utf8Str = strings.ReplaceAll(utf8Str, "encoding='GBK'", "encoding='UTF-8'")

	return []byte(utf8Str), nil
}

// EncodeUtf8ToGbk 将UTF-8编码的XML转换为GBK（用于发送给设备）
func EncodeUtf8ToGbk(data []byte) ([]byte, error) {
	// 转换UTF-8到GBK
	reader := transform.NewReader(bytes.NewReader(data), simplifiedchinese.GBK.NewEncoder())
	gbkBytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("convert UTF-8 to GBK failed: %w", err)
	}

	// 替换XML声明中的编码为GBK
	gbkStr := string(gbkBytes)
	gbkStr = strings.ReplaceAll(gbkStr, "encoding=\"UTF-8\"", "encoding=\"GBK\"")
	gbkStr = strings.ReplaceAll(gbkStr, "encoding='UTF-8'", "encoding='GBK'")

	return []byte(gbkStr), nil
}

// ParseXMLResponse 解析XML响应（自动处理编码转换）
func ParseXMLResponse(data []byte, v interface{}) error {
	// 先转换为UTF-8
	utf8Data, err := DecodeGbkToUtf8(data)
	if err != nil {
		return err
	}

	// 解析XML
	decoder := xml.NewDecoder(bytes.NewReader(utf8Data))
	return decoder.Decode(v)
}

// GenerateSN 生成序列号（使用时间戳）
func GenerateSN() int {
	return int(time.Now().Unix())
}
