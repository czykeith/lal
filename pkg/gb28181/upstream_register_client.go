package gb28181

import (
	"crypto/md5"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/q191201771/lal/pkg/base"
)

// doUpstreamRegisterOnce 使用自定义 UDP 客户端向上级平台发送一次 REGISTER。
// - sConf: 本地 gb28181 全局配置（用于 realm 等默认值）
// - uConf: 上级平台配置
// - realm/nonce: 若已从上一次 401 中解析出，则用于构造 Authorization；否则可为空
// - withAuth: 是否在本次请求中携带 Authorization 头
// 返回值:
// - ok: 是否注册成功（status=200）
// - respRealm/respNonce: 当 status=401 时从 WWW-Authenticate 中解析出的 realm/nonce（可为空）
// - err: 其他错误
func doUpstreamRegisterOnce(sConf *GB28181Config, uConf *GB28181UpstreamConfig, realm, nonce string, withAuth bool) (ok bool, respRealm, respNonce string, err error) {
	deviceID := uConf.LocalDeviceID
	if deviceID == "" {
		deviceID = sConf.Serial
	}
	if realm == "" {
		if uConf.Realm != "" {
			realm = uConf.Realm
		} else {
			realm = sConf.Realm
		}
	}

	remote := net.JoinHostPort(uConf.SipIP, strconv.Itoa(int(uConf.SipPort)))
	raddr, err := net.ResolveUDPAddr("udp", remote)
	if err != nil {
		return
	}
	// 将本地绑定端口设置为 UpstreamSipPort（避免与设备侧 5060 冲突）
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: int(sConf.UpstreamSipPort)}
	conn, err := net.DialUDP("udp", laddr, raddr)
	if err != nil {
		return
	}
	defer conn.Close()

	raw := buildUpstreamRawRegister(sConf, uConf, deviceID, realm, nonce, withAuth)
	base.Log.Debugf("gb28181 upstream raw REGISTER (withAuth=%v) >>>\n%s", withAuth, raw)

	if _, err = conn.Write([]byte(raw)); err != nil {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return
	}

	resp := string(buf[:n])
	base.Log.Debugf("gb28181 upstream raw REGISTER response <<<\n%s", resp)

	statusCode := 0
	if _, e := fmt.Sscanf(resp, "SIP/2.0 %d", &statusCode); e != nil {
		err = fmt.Errorf("parse status code failed: %v", e)
		return
	}
	switch statusCode {
	case http.StatusOK:
		ok = true
		return
	case http.StatusUnauthorized:
		respRealm, respNonce = parseWwwAuthenticate(resp)
		return
	default:
		err = fmt.Errorf("unexpected status code: %d", statusCode)
		return
	}
}

// buildUpstreamRawRegister 构造一条原始 REGISTER 报文（SIP 文本）。
func buildUpstreamRawRegister(sConf *GB28181Config, uConf *GB28181UpstreamConfig, deviceID, realm, nonce string, withAuth bool) string {
	callID := RandNumString(10)
	// CSeq 过大可能导致部分服务端解析异常，这里固定为 1，符合 SIP 要求的“正整数”即可。
	cseq := "1"
	branch := RandNumString(8)
	tag := RandNumString(9)

	uri := fmt.Sprintf("sip:%s@%s", deviceID, realm)

	var b strings.Builder
	fmt.Fprintf(&b, "REGISTER %s SIP/2.0\r\n", uri)
	// Via/From/To/Contact 使用 upstream_sip_port，避免与设备侧 sip_port 混用。
	upPort := sConf.UpstreamSipPort
	if upPort == 0 {
		upPort = 5061
	}
	// 优先使用上级配置中的 media_ip（上级可达的本机 IP），避免多网卡/NAT 场景下填错导致回包异常。
	localIP := uConf.MediaIP
	if localIP == "" {
		localIP = sConf.MediaConfig.MediaIp
	}
	fmt.Fprintf(&b, "Via: SIP/2.0/UDP %s:%d;branch=z9hG4bK%s\r\n", localIP, upPort, branch)
	fmt.Fprintf(&b, "From: <sip:%s@%s>;tag=%s\r\n", deviceID, realm, tag)
	fmt.Fprintf(&b, "To: <sip:%s@%s>\r\n", deviceID, realm)
	fmt.Fprintf(&b, "Call-ID: %s\r\n", callID)
	fmt.Fprintf(&b, "CSeq: %s REGISTER\r\n", cseq)
	fmt.Fprintf(&b, "Max-Forwards: 70\r\n")
	fmt.Fprintf(&b, "User-Agent: keith_lalserver\r\n")
	fmt.Fprintf(&b, "Contact: <sip:%s@%s:%d>\r\n", deviceID, localIP, upPort)
	expires := uConf.RegisterValidity
	if expires <= 0 {
		expires = 3600
	}
	fmt.Fprintf(&b, "Expires: %d\r\n", expires)

	if withAuth {
		username := uConf.Username
		if username == "" {
			username = deviceID
		}
		response := calcDigestResponse(username, uConf.Password, realm, nonce, "REGISTER", uri)
		fmt.Fprintf(&b, "Authorization: Digest username=\"%s\",realm=\"%s\",nonce=\"%s\",uri=\"%s\",response=\"%s\",algorithm=MD5\r\n",
			username, realm, nonce, uri, response)
	}

	fmt.Fprintf(&b, "Content-Length: 0\r\n\r\n")
	return b.String()
}

// calcDigestResponse 按 RFC2617 计算 Digest response。
func calcDigestResponse(username, password, realm, nonce, method, uri string) string {
	s1 := fmt.Sprintf("%s:%s:%s", username, realm, password)
	r1 := fmt.Sprintf("%x", md5.Sum([]byte(s1)))
	s2 := fmt.Sprintf("%s:%s", method, uri)
	r2 := fmt.Sprintf("%x", md5.Sum([]byte(s2)))
	s3 := fmt.Sprintf("%s:%s:%s", r1, nonce, r2)
	return fmt.Sprintf("%x", md5.Sum([]byte(s3)))
}

// parseWwwAuthenticate 从 401 原始响应中解析 realm/nonce。
func parseWwwAuthenticate(resp string) (realm, nonce string) {
	lines := strings.Split(resp, "\r\n")
	for _, l := range lines {
		lower := strings.ToLower(l)
		if strings.HasPrefix(lower, "www-authenticate:") {
			// 示例：WWW-Authenticate: Digest realm="3402000000",algorithm=MD5,nonce="xxxx"
			//
			// 注意：realm/nonce 的值可能包含大小写敏感内容（尤其是 nonce）。
			// 这里只对匹配做 ToLower，但切片取值必须从原始字符串 l 中截取，避免把值转成小写导致鉴权失败。
			if idx := strings.Index(lower, `realm="`); idx >= 0 {
				start := idx + len(`realm="`)
				if end := strings.Index(lower[start:], `"`); end > 0 {
					realm = l[start : start+end]
				}
			}
			if idx := strings.Index(lower, `nonce="`); idx >= 0 {
				start := idx + len(`nonce="`)
				if end := strings.Index(lower[start:], `"`); end > 0 {
					nonce = l[start : start+end]
				}
			}
			break
		}
	}
	return
}
