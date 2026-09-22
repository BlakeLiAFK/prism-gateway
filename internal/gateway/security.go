package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func validateBaseURL(raw string, allowPrivate bool) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("Base URL 必须是无账号、无查询参数的绝对 URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("Provider 仅支持 http/https")
	}
	if u.Scheme == "http" && !allowPrivate {
		return errors.New("HTTP 上游必须显式勾选允许私有网络；公网推荐 HTTPS")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !allowPrivate && privateIP(ip) {
		return errors.New("私有地址需要显式授权")
	}
	return nil
}
func privateIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.ParseIP("169.254.169.254"))
}
func clientFor(p Provider) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{MaxIdleConns: 20, MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: time.Duration(p.TimeoutSec) * time.Second, DisableCompression: true, ForceAttemptHTTP2: true}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil {
			return nil, errors.New("上游 DNS 解析失败")
		}
		if len(ips) == 0 {
			return nil, errors.New("上游 DNS 无地址")
		}
		for _, v := range ips {
			if !p.AllowPrivate && privateIP(v.IP) {
				return nil, errors.New("上游 DNS 指向未授权私有地址")
			}
		}
		var last error
		for _, v := range ips {
			c, e := dialer.DialContext(ctx, network, net.JoinHostPort(v.IP.String(), port))
			if e == nil {
				return c, nil
			}
			last = e
		}
		return nil, last
	}
	return &http.Client{Transport: tr, Timeout: time.Duration(p.TimeoutSec) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// requestIsSecure 判断客户端到网关这一段是不是 HTTPS。
// 网关通常部署在反向代理之后，此时 r.TLS 为 nil，仅凭它判断会让
// HTTPS 部署下的会话 Cookie 丢掉 Secure 标志。
// 只有请求来自回环地址时才采信 X-Forwarded-Proto：公网直连的客户端
// 可以随意伪造这个头，不能作数。
func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return false
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func sameOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, e := url.Parse(o)
	return e == nil && u.Host == r.Host && (u.Scheme == "http" || u.Scheme == "https")
}
func secureEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}
func requestHeaders(dst *http.Request, src *http.Request, p Provider, protocol, session string) {
	dst.Header.Set("Content-Type", "application/json")
	dst.Header.Set("Accept", "application/json, text/event-stream")
	dst.Header.Set("User-Agent", "Prism-Gateway/"+Version)
	if p.Secret != "" {
		if p.Auth == "x-api-key" || (p.Auth == "auto" && protocol == "messages") {
			dst.Header.Set("x-api-key", p.Secret)
		} else if p.Auth != "none" {
			dst.Header.Set("Authorization", "Bearer "+p.Secret)
		}
	}
	if protocol == "messages" {
		v := src.Header.Get("anthropic-version")
		if v == "" {
			v = "2023-06-01"
		}
		dst.Header.Set("anthropic-version", v)
		if b := src.Header.Get("anthropic-beta"); b != "" {
			dst.Header.Set("anthropic-beta", b)
		}
	}
	for _, h := range []string{"x-opencode-session", "x-session-id", "x-claude-code-session-id", "session_id", "x-codex-session-id"} {
		if v := src.Header.Get(h); v != "" && len(v) <= 512 {
			dst.Header.Set(h, v)
		}
	}
	if dst.Header.Get("x-opencode-session") == "" && session != "" {
		dst.Header.Set("x-opencode-session", session)
	}
}

// client 是一次调用的来源元数据：只有 IP 与 User-Agent，不含任何请求内容。
type client struct {
	IP    string
	Agent string
}

// clientOf 取调用方地址。网关部署在同机反代（Caddy）后面，所以只有当直连地址
// 是回环时才采信 X-Forwarded-For 的第一跳；公网直连时伪造的 XFF 一律不认，
// 这样不必再维护一份可信代理名单。
func clientOf(r *http.Request) client {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if p := net.ParseIP(ip); p != nil && p.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if net.ParseIP(first) != nil {
				ip = first
			}
		}
	}
	agent := r.Header.Get("User-Agent")
	if len(agent) > 200 {
		agent = agent[:200]
	}
	return client{IP: ip, Agent: agent}
}
