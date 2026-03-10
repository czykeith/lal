package gb28181

import "encoding/xml"

const PlaybackControl = "PlaybackControl"

// CDataText 用于在 XML 中输出 <![CDATA[...]]>，避免内容（如换行）被转义成实体（&#xA; 等）。
//
// 注意：encoding/xml 的 `,cdata` 仅能用于 `xml:",cdata"`（无元素名），不能写成 `xml:"Info,cdata"`。
type CDataText struct {
	Text string
}

func (c CDataText) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	// start.Name 通常已经是 Info（来自字段 tag `xml:"Info"`）
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.Directive([]byte("[CDATA[" + c.Text + "]]"))); err != nil {
		return err
	}
	return e.EncodeToken(xml.EndElement{Name: start.Name})
}

// MessagePlaybackControl GB28181 回放控制消息体（常用于 SIP INFO）。
//
// 典型用法：<Info> 内携带类 RTSP 的 PLAY/PAUSE 等控制文本，例如：
// PLAY RTSP/1.0\r\nCSeq: 3\r\nScale: 2.0\r\n\r\n
type MessagePlaybackControl struct {
	XMLName  xml.Name  `xml:"Control"`
	CmdType  string    `xml:"CmdType"`
	SN       int       `xml:"SN"`
	DeviceID string    `xml:"DeviceID"`
	Info     CDataText `xml:"Info"`
}
