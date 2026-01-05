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
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SIP消息类型
const (
	SipMethodRegister = "REGISTER"
	SipMethodInvite   = "INVITE"
	SipMethodAck      = "ACK"
	SipMethodBye      = "BYE"
	SipMethodMessage  = "MESSAGE"
	SipMethodCancel   = "CANCEL"
)

// SIP消息结构
type SipMessage struct {
	Method     string            // 请求方法（REGISTER, INVITE等）
	RequestUri string            // 请求URI
	StatusCode int               // 响应状态码（0表示请求）
	StatusText string            // 响应状态文本
	Headers    map[string]string // 消息头
	Body       string            // 消息体
	Raw        string            // 原始消息
}

// 解析SIP消息
func ParseSipMessage(data []byte) (*SipMessage, error) {
	msg := &SipMessage{
		Headers: make(map[string]string),
		Raw:     string(data),
	}

	lines := strings.Split(string(data), "\r\n")
	if len(lines) == 1 {
		lines = strings.Split(string(data), "\n")
	}

	// 解析起始行
	startLine := lines[0]
	if strings.HasPrefix(startLine, "SIP/") {
		// 响应消息
		parts := strings.SplitN(startLine, " ", 3)
		if len(parts) >= 3 {
			msg.StatusCode, _ = strconv.Atoi(parts[1])
			msg.StatusText = parts[2]
		}
	} else {
		// 请求消息
		parts := strings.SplitN(startLine, " ", 3)
		if len(parts) >= 3 {
			msg.Method = parts[0]
			msg.RequestUri = parts[1]
		}
	}

	// 解析消息头
	bodyStart := -1
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			bodyStart = i + 1
			break
		}

		// 处理多行头（以空格或tab开头）
		if i > 1 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			// 续行
			lastKey := ""
			for k := range msg.Headers {
				lastKey = k
				break
			}
			if lastKey != "" {
				msg.Headers[lastKey] += " " + strings.TrimSpace(line)
			}
			continue
		}

		idx := strings.Index(line, ":")
		if idx > 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			msg.Headers[strings.ToLower(key)] = value
		}
	}

	// 解析消息体
	if bodyStart > 0 && bodyStart < len(lines) {
		msg.Body = strings.Join(lines[bodyStart:], "\r\n")
	}

	return msg, nil
}

// 生成SIP请求消息
func BuildSipRequest(method, requestUri string, headers map[string]string, body string) string {
	var sb strings.Builder

	// 起始行
	sb.WriteString(fmt.Sprintf("%s %s SIP/2.0\r\n", method, requestUri))

	// 消息头
	for k, v := range headers {
		sb.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}

	// 消息体
	if body != "" {
		sb.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(body)))
		sb.WriteString("\r\n")
		sb.WriteString(body)
	} else {
		sb.WriteString("\r\n")
	}

	return sb.String()
}

// 生成SIP响应消息
func BuildSipResponse(statusCode int, statusText string, headers map[string]string, body string) string {
	var sb strings.Builder

	// 起始行
	sb.WriteString(fmt.Sprintf("SIP/2.0 %d %s\r\n", statusCode, statusText))

	// 消息头
	for k, v := range headers {
		sb.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}

	// 消息体
	if body != "" {
		sb.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(body)))
		sb.WriteString("\r\n")
		sb.WriteString(body)
	} else {
		sb.WriteString("\r\n")
	}

	return sb.String()
}

// 从From/To头中提取设备ID
func ExtractDeviceId(fromHeader string) string {
	// From: <sip:34020000001320000001@192.168.1.100:5060>;tag=123456
	re := regexp.MustCompile(`sip:(\d+)@`)
	matches := re.FindStringSubmatch(fromHeader)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// 从Contact头中提取IP和端口
func ExtractContactAddr(contactHeader string) (string, int) {
	// Contact: <sip:34020000001320000001@192.168.1.100:5060>
	re := regexp.MustCompile(`@([\d.]+):(\d+)`)
	matches := re.FindStringSubmatch(contactHeader)
	if len(matches) >= 3 {
		port, _ := strconv.Atoi(matches[2])
		return matches[1], port
	}
	return "", 0
}

// 生成Call-ID
func GenerateCallId() string {
	return fmt.Sprintf("%d@%d", time.Now().UnixNano(), time.Now().Unix())
}

// 生成Tag
func GenerateTag() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// 生成Branch
func GenerateBranch() string {
	return fmt.Sprintf("z9hG4bK%d", time.Now().UnixNano())
}
