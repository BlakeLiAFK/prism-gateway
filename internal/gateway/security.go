package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

// clientFor 建上游客户端。供应商超时只管到拿到响应头为止；响应体（尤其是持续几分钟的流式输出）
// 不设总时长，改为连续 idle 没有数据才断开：还在输出就不打断，上游卡死也能及时释放并发名额
func clientFor(p Provider, idle time.Duration) *http.Client {
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
	return &http.Client{Transport: idleTransport{tr, idle}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// idleTransport 给响应体加空闲计时：每读到数据就重置，超时则取消这次请求
type idleTransport struct {
	base http.RoundTripper
	idle time.Duration
}

func (t idleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(r.Context())
	res, err := t.base.RoundTrip(r.WithContext(ctx))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	b := &idleBody{rc: res.Body, cancel: cancel, idle: t.idle}
	b.timer = time.AfterFunc(t.idle, func() { cancel(errStreamIdle) })
	res.Body = b
	return res, nil
}

var errStreamIdle = errors.New("上游持续无数据，已按空闲超时断开")

type idleBody struct {
	rc     io.ReadCloser
	timer  *time.Timer
	cancel context.CancelCauseFunc
	idle   time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel(nil)
	return err
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
	// OpenRouter 按 x-session-id 把同一会话固定到同一家上游厂商（闲置 10 分钟失效），缓存才能持续命中
	if dst.Header.Get("x-session-id") == "" && session != "" {
		dst.Header.Set("x-session-id", session)
	}
}

// client 是一次调用的来源元数据：只有 IP 与 User-Agent，不含任何请求内容。
type client struct {
	IP    string
	Agent string
	// Role 是 Claude Code 声明的请求角色，例如 subagent:Explore；其他客户端为空
	Role string
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
	return client{IP: ip, Agent: agent, Role: agentRole(r.Header)}
}

// agentRole 由 Claude Code 的提示头组成「类别:子代理类型」。类别与类型只在客户端
// 开了 CLAUDE_CODE_GATEWAY_HINT_HEADERS 时才发；子代理始终带 agent-id，据此至少标出 subagent。
func agentRole(h http.Header) string {
	class, kind := h.Get("x-claude-code-request-class"), h.Get("x-claude-code-agent-type")
	if class == "" && h.Get("x-claude-code-agent-id") != "" {
		class = "subagent"
	}
	role := class
	if kind != "" {
		role += ":" + kind
	}
	if len(role) > 64 {
		role = role[:64]
	}
	return role
}

func (e *Engine) Authenticate(r *http.Request) (Principal, error) {
	token := bearer(r)
	if token == "" {
		token = r.Header.Get("x-api-key")
	}
	if len(token) < 20 {
		return Principal{}, fail("UNAUTHORIZED", "需要网关 API Key，不是上游 Key", 401)
	}
	k, err := e.lookupKey(digest(token))
	if err != nil {
		return Principal{}, err
	}
	if k.p.Policy.expired() {
		return Principal{}, fail("KEY_EXPIRED", "网关 API Key 已过期", 401)
	}
	// last_used 只用于界面展示，每把 Key 最多每分钟落库一次
	e.keyMu.Lock()
	t := now()
	write := t-k.written >= lastUsedEvery
	if write {
		k.written = t
	}
	e.keyMu.Unlock()
	if write {
		if err := e.store.DB.Exec("UPDATE api_keys SET last_used=? WHERE id=?", t, k.p.ID); err != nil {
			slog.Error("api key last_used update failed", "key_id", k.p.ID, "err", err)
		}
	}
	return k.p, nil
}

// cachedKey 已通过校验的网关 Key；written 是上次写 last_used 的时刻
type cachedKey struct {
	p       Principal
	written int64
}

const lastUsedEvery = 60000

// keyRefreshEvery：快照里查不到某个 Key 时，快照足够新就直接判无效，不查库；
// 超过这个时间才重载一次。无效 Key 因此最多每 5 秒引发一次查库，
// 同时兼顾别的进程刚创建的 Key（最多晚 5 秒生效；本进程内的创建与吊销会立即失效快照）。
const keyRefreshEvery = 5000

// peekKey 只看内存快照。decided 为 false 表示快照缺失或过期，需要重载才能下结论。
func (e *Engine) peekKey(d string) (k *cachedKey, decided bool) {
	e.keyMu.Lock()
	defer e.keyMu.Unlock()
	if e.keysAt == 0 {
		return nil, false
	}
	if k := e.keys[d]; k != nil {
		return k, true
	}
	return nil, now()-e.keysAt < keyRefreshEvery
}

// lookupKey 按 digest 找到启用中的网关 Key。命中快照或快照够新时不碰数据库，
// 所以随机字符串的无效 Key 刷不到数据库上。
func (e *Engine) lookupKey(d string) (*cachedKey, error) {
	k, decided := e.peekKey(d)
	if !decided {
		// 同一时刻只让一个协程重载，其余等它做完再看快照
		e.loadMu.Lock()
		defer e.loadMu.Unlock()
		if k, decided = e.peekKey(d); !decided {
			var err error
			if k, err = e.reloadKeys(d); err != nil {
				return nil, err
			}
		}
	}
	if k == nil {
		return nil, fail("UNAUTHORIZED", "网关 API Key 无效或已撤销", 401)
	}
	return k, nil
}

// reloadKeys 载入全部启用的 Key 作为新快照，返回其中 d 对应的那把（没有则为 nil）。
// 查库不持 keyMu：数据库忙时，快照里已有的 Key 不能跟着排队。
// 载入期间 forgetKeys 推进了 keyGen（例如刚吊销了 Key），这份结果可能是吊销前读到的，
// 只用于本次请求，不作为快照。
func (e *Engine) reloadKeys(d string) (*cachedKey, error) {
	e.keyMu.Lock()
	gen := e.keyGen
	e.keyMu.Unlock()
	if e.onKeyLoad != nil {
		e.onKeyLoad()
	}
	rows, err := e.store.DB.Query("SELECT id,digest,allowed," + keyPolicyColumns + " FROM api_keys WHERE enabled=1")
	if err != nil {
		return nil, err
	}
	fresh := make(map[string]*cachedKey, len(rows))
	for _, row := range rows {
		k := &cachedKey{p: Principal{ID: row.String("id"), Policy: keyPolicy{ExpiresAt: row.Int("expires_at"), LimitDay: row.Float("limit_day"), LimitMonth: row.Float("limit_month"), RPM: int(row.Int("rpm"))}}}
		if err := json.Unmarshal([]byte(row.String("allowed")), &k.p.Allowed); err != nil {
			// 一条损坏的记录只让这把 Key 不可用，不能连累其他 Key
			slog.Error("api key record unreadable", "key_id", row.String("id"), "err", err)
			continue
		}
		fresh[row.String("digest")] = k
	}
	e.keyMu.Lock()
	defer e.keyMu.Unlock()
	if gen == e.keyGen {
		// 沿用上次写 last_used 的时刻，重载不会让每把 Key 多写一次库
		for dig, k := range fresh {
			if old := e.keys[dig]; old != nil {
				k.written = old.written
			}
		}
		e.keys = fresh
		e.keysAt = now()
	}
	return fresh[d], nil
}

// forgetKeys 在 api_keys 写库后调用，下一次请求重新查库
func (e *Engine) forgetKeys() {
	e.keyMu.Lock()
	e.keys = map[string]*cachedKey{}
	e.keysAt = 0
	e.keyGen++
	e.keyMu.Unlock()
}
