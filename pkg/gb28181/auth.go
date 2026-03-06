// Copyright 2024, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package gb28181

import (
	"crypto/md5"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Authorization Digest 认证信息
type Authorization struct {
	Username  string
	Realm     string
	Nonce     string
	URI       string
	Response  string
	Algorithm string
	Cnonce    string
	Qop       string
	Nc        string
}

// ParseAuthorization 从 Authorization 头中解析认证信息
func ParseAuthorization(authHeader string) *Authorization {
	if authHeader == "" {
		return nil
	}

	auth := &Authorization{
		Algorithm: "MD5", // 默认算法
	}

	// 移除 "Digest " 前缀
	authHeader = strings.TrimPrefix(authHeader, "Digest ")
	authHeader = strings.TrimSpace(authHeader)

	// 解析各个字段
	re := regexp.MustCompile(`(\w+)="?([^",\s]+)"?`)
	matches := re.FindAllStringSubmatch(authHeader, -1)

	for _, match := range matches {
		if len(match) >= 3 {
			key := strings.ToLower(match[1])
			value := match[2]

			switch key {
			case "username":
				auth.Username = value
			case "realm":
				auth.Realm = value
			case "nonce":
				auth.Nonce = value
			case "uri":
				auth.URI = value
			case "response":
				auth.Response = value
			case "algorithm":
				auth.Algorithm = value
			case "cnonce":
				auth.Cnonce = value
			case "qop":
				auth.Qop = value
			case "nc":
				auth.Nc = value
			}
		}
	}

	return auth
}

// Verify 验证 Digest 认证响应
func (a *Authorization) Verify(username, password, method, uri string) bool {
	if a == nil {
		return false
	}

	// 计算 HA1 = MD5(username:realm:password)
	ha1 := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", username, a.Realm, password))))

	// 计算 HA2 = MD5(method:uri)
	ha2 := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s", method, uri))))

	// 计算 Response
	var response string
	if a.Qop != "" {
		// 如果使用 qop，Response = MD5(HA1:nonce:nc:cnonce:qop:HA2)
		response = fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s:%s:%s:%s", ha1, a.Nonce, a.Nc, a.Cnonce, a.Qop, ha2))))
	} else {
		// 如果没有 qop，Response = MD5(HA1:nonce:HA2)
		response = fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", ha1, a.Nonce, ha2))))
	}

	return strings.EqualFold(response, a.Response)
}

// BuildWWWAuthenticate 构建 WWW-Authenticate 响应头（401 挑战）
func BuildWWWAuthenticate(realm, nonce string) string {
	return fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, realm, nonce)
}

// GenerateNonce 生成随机 nonce（简化版本，实际应该使用更安全的随机数生成）
func GenerateNonce() string {
	// 使用时间戳和随机数生成 nonce
	// 实际应用中应该使用更安全的随机数生成器
	return fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%d", time.Now().UnixNano()))))
}
