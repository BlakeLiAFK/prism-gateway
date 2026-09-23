package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
)

type App struct {
	Store      *Store
	Engine     *Engine
	UI         http.Handler
	Listen     *Listener
	Started    int64
	Context    context.Context
	loginMu    sync.Mutex
	logins     map[string][]int64
	workers    sync.WaitGroup
	usageMu    sync.Mutex
	usageCache map[string]usageCacheEntry
}

func NewApp(ctx context.Context, s *Store, ui http.Handler) *App {
	a := &App{Store: s, Engine: NewEngine(s), UI: ui, Started: now(), Context: ctx, logins: map[string][]int64{}, usageCache: map[string]usageCacheEntry{}}
	go a.Engine.Prune(ctx)
	return a
}
func (a *App) Wait() { a.workers.Wait(); a.Engine.Close() }
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/api.json" {
		a.management(w, r)
		return
	}
	if r.URL.Path == "/healthz" {
		a.healthz(w, r)
		return
	}
	if r.URL.Path == "/metrics" {
		a.metrics(w, r)
		return
	}
	p := "chat"
	if strings.HasPrefix(r.URL.Path, "/anthropic/v1/") {
		p = "messages"
	}
	if strings.HasPrefix(r.URL.Path, "/typesafe/") {
		p = "systemone"
	}
	if strings.HasPrefix(r.URL.Path, "/openai/") || strings.HasPrefix(r.URL.Path, "/anthropic/") || strings.HasPrefix(r.URL.Path, "/typesafe/") {
		key, err := a.Engine.Authenticate(r)
		if err != nil {
			status, code, msg := errorParts(err)
			protocolError(w, p, status, code, msg, randomID("req_"))
			return
		}
		switch {
		case r.Method == "GET" && (r.URL.Path == "/openai/v1/models" || strings.HasPrefix(r.URL.Path, "/openai/v1/models/") || r.URL.Path == "/anthropic/v1/models" || strings.HasPrefix(r.URL.Path, "/anthropic/v1/models/")):
			a.Engine.Models(w, r, p, key)
		case r.Method == "POST" && r.URL.Path == "/openai/v1/chat/completions":
			a.Engine.Handle(w, r, "chat", key)
		case r.Method == "POST" && r.URL.Path == "/openai/v1/responses":
			a.Engine.Handle(w, r, "responses", key)
		case r.Method == "POST" && r.URL.Path == "/anthropic/v1/messages":
			a.Engine.Handle(w, r, "messages", key)
		case r.Method == "POST" && r.URL.Path == "/anthropic/v1/messages/count_tokens":
			a.Engine.CountTokens(w, r, key)
		case r.Method == "POST" && r.URL.Path == "/typesafe/v1/systemone":
			a.Engine.Handle(w, r, "systemone", key)
		default:
			protocolError(w, p, 404, "ENDPOINT_NOT_SUPPORTED", "此端点不在本版本兼容范围内", randomID("req_"))
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/v1") || strings.HasPrefix(r.URL.Path, "/admin") || strings.HasPrefix(r.URL.Path, "/gateway") {
		http.NotFound(w, r)
		return
	}
	if a.UI == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	a.UI.ServeHTTP(w, r)
}

// metrics 导出 Prometheus 文本。默认关闭；开启后仍要求管理员令牌，
// 因为指标里含模型名、调用量与费用估算。
func (a *App) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(405)
		return
	}
	if !a.Store.Config().Settings.MetricsEnabled {
		http.NotFound(w, r)
		return
	}
	if !a.Store.CheckAdmin(bearer(r)) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="prism-metrics"`)
		w.WriteHeader(401)
		return
	}
	var buf bytes.Buffer
	if err := a.writeMetrics(&buf); err != nil {
		slog.Error("metrics export failed", "err", err)
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write(buf.Bytes())
}

// healthz 是唯一免鉴权端点，供反向代理与容器编排做存活探测。
// 只暴露进程级信息，不返回配置、用量或运行时细节。
func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(405)
		return
	}
	status, ok := 200, true
	if _, err := a.Store.DB.Query("SELECT 1"); err != nil {
		status, ok = 503, false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if r.Method == "HEAD" {
		return
	}
	json.NewEncoder(w).Encode(Object{"ok": ok, "version": Version, "uptime_ms": now() - a.Started})
}

func (a *App) adminSession(r *http.Request) (string, bool) {
	if token := bearer(r); token != "" {
		return "", a.Store.CheckAdmin(token)
	}
	co, err := r.Cookie("prism_session")
	if err != nil {
		return "", false
	}
	rows, err := a.Store.DB.Query("SELECT csrf FROM admin_sessions WHERE digest=? AND expires_at>?", digest(co.Value), now())
	if err != nil || len(rows) == 0 {
		return "", false
	}
	return rows[0].String("csrf"), true
}
func (a *App) loginAllowed(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	cut := now() - 60000
	for h, v := range a.logins {
		if len(v) == 0 || v[len(v)-1] < cut {
			delete(a.logins, h)
		}
	}
	if len(a.logins) >= 4096 {
		return false
	}
	v := a.logins[host]
	good := []int64{}
	for _, t := range v {
		if t >= cut {
			good = append(good, t)
		}
	}
	if len(good) >= 10 {
		return false
	}
	a.logins[host] = append(good, now())
	return true
}
func (a *App) management(w http.ResponseWriter, r *http.Request) {
	id := randomID("rpc_")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", id)
	send := func(data any, err error) {
		if err != nil {
			status, code, msg := errorParts(err)
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(Object{"ok": false, "error": Object{"code": code, "message": msg}, "request_id": id})
		} else {
			json.NewEncoder(w).Encode(Object{"ok": true, "data": data, "request_id": id})
		}
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		send(nil, fail("METHOD_NOT_ALLOWED", "管理接口仅支持 POST /api.json", 405))
		return
	}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		send(nil, fail("CONTENT_TYPE", "需要 application/json", 415))
		return
	}
	if !sameOrigin(r) {
		send(nil, fail("ORIGIN_REJECTED", "拒绝跨站管理请求", 403))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req struct {
		Action string `json:"action"`
		Params Object `json:"params"`
	}
	if er := dec.Decode(&req); er != nil {
		send(nil, fail("INVALID_JSON", "无效请求；格式为 {action, params}", 400))
		return
	}
	var more any
	if dec.Decode(&more) != io.EOF {
		send(nil, fail("INVALID_JSON", "仅允许一个 JSON 对象", 400))
		return
	}
	if req.Params == nil {
		req.Params = Object{}
	}
	csrf, auth := a.adminSession(r)
	if req.Action == "auth.status" {
		send(Object{"authenticated": auth, "csrf": csrf, "version": Version, "app_name": a.Store.Config().Settings.AppName}, nil)
		return
	}
	if req.Action == "auth.login" {
		if !a.loginAllowed(r) {
			send(nil, fail("LOGIN_RATE_LIMIT", "登录尝试过于频繁，一分钟后再试", 429))
			return
		}
		if !a.Store.CheckAdmin(str(req.Params, "token")) {
			send(nil, fail("INVALID_CREDENTIAL", "管理员令牌无效", 401))
			return
		}
		token := randomID("session_")
		csrf = randomID("csrf_")
		err := a.Store.DB.Exec("INSERT INTO admin_sessions VALUES (?,?,?)", digest(token), csrf, now()+12*3600000)
		if err != nil {
			send(nil, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "prism_session", Value: token, Path: "/", HttpOnly: true, Secure: requestIsSecure(r), SameSite: http.SameSiteStrictMode, MaxAge: 43200})
		send(Object{"authenticated": true, "csrf": csrf}, nil)
		return
	}
	if !auth {
		send(nil, fail("UNAUTHORIZED", "请先登录管理后台", 401))
		return
	}
	if csrf != "" && !secureEqual(csrf, r.Header.Get("X-Prism-CSRF")) {
		send(nil, fail("CSRF_INVALID", "CSRF 校验失败，请刷新页面", 403))
		return
	}
	if req.Action == "auth.logout" {
		if co, er := r.Cookie("prism_session"); er == nil {
			a.Store.DB.Exec("DELETE FROM admin_sessions WHERE digest=?", digest(co.Value))
		}
		http.SetCookie(w, &http.Cookie{Name: "prism_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		send(Object{}, nil)
		return
	}
	data, err := a.call(r.Context(), req.Action, req.Params)
	if err != nil {
		var ae *APIError
		if !errors.As(err, &ae) {
			// 错误内容可能包含管理员刚提交的配置片段，默认只记可定位的元数据；
			// 详情降到 DEBUG，需要排障时在后台把日志级别切到 debug
			slog.Error("management call failed", "request_id", id, "action", req.Action)
			slog.Debug("management call detail", "request_id", id, "action", req.Action, "err", err)
		}
	}
	send(data, err)
}
func (a *App) audit(action, target string) {
	if err := a.Store.DB.Exec("INSERT INTO audit_logs VALUES (?,?,?,?,?)", randomID("audit_"), action, target, a.Store.Config().Version, now()); err != nil {
		slog.Error("audit write failed", "action", action, "target", target, "err", err)
	}
}
func (a *App) EnableDemo(version int64) (Config, error) {
	return a.Store.Change(version, "demo.enable", "demo", func(c *Config) error {
		if _, ok := c.provider("local-demo"); ok {
			return nil
		}
		c.Providers = append(c.Providers, Provider{ID: "local-demo", Name: "Local Sandbox", Kind: "mock", Auth: "none", Enabled: true, TimeoutSec: 30, Description: "本地确定性演示，不是真实模型，不产生云端费用"})
		for i, p := range []string{"chat", "messages", "responses"} {
			c.Models = append(c.Models, Model{ID: "demo-" + p, Name: []string{"Demo / Chat", "Demo / Messages", "Demo / Responses"}[i], ProviderID: "local-demo", Upstream: "demo-" + p, Protocol: p, Enabled: true, Tools: true, Context: 128000, MaxOutput: 4096, Concurrency: 2, RPM: 60, PricingSet: true})
		}
		c.Routes = append(c.Routes, Route{ID: "demo-auto", Name: "Sandbox route", Enabled: true, Strategy: "balanced", Affinity: true, Candidates: []Candidate{{"demo-chat", 30}, {"demo-messages", 20}, {"demo-responses", 10}}, Description: "仅用于本地演示，不会混入真实生产路由"})
		return nil
	})
}
func (a *App) requests(p Object) (any, error) {
	page := clamp(int(num(p, "page")), 1, 10000)
	limit := clamp(int(num(p, "page_size")), 1, 100)
	if p["page_size"] == nil {
		limit = 30
	}
	q := str(p, "q")
	status := str(p, "status")
	provider := str(p, "provider_id")
	where := ` WHERE (?='' OR model_id LIKE ? OR parent_id LIKE ? OR client_ip LIKE ?) AND (?='' OR status=?) AND (?='' OR provider_id=?)`
	args := []any{q, "%" + q + "%", "%" + q + "%", "%" + q + "%", status, status, provider, provider}
	count, err := a.Store.DB.Query("SELECT COUNT(*) n FROM requests"+where, args...)
	if err != nil {
		return nil, err
	}
	args = append(args, limit, (page-1)*limit)
	rows, err := a.Store.DB.Query("SELECT * FROM requests"+where+" ORDER BY started_at DESC LIMIT ? OFFSET ?", args...)
	return Object{"items": rows, "page": page, "page_size": limit, "total": count[0].Int("n")}, err
}
func (a *App) dashboard(p Object) (any, error) {
	hours := 24
	switch str(p, "range") {
	case "7d":
		hours = 168
	case "30d":
		hours = 720
	}
	cut := now() - int64(hours)*3600000
	rows, err := a.Store.DB.Query(`SELECT COUNT(*) requests,COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0) success,COALESCE(SUM(CASE WHEN status='error' OR status='unknown' THEN 1 ELSE 0 END),0) errors,COALESCE(SUM(input_tokens),0) input_tokens,COALESCE(SUM(output_tokens),0) output_tokens,COALESCE(SUM(cache_tokens),0) cache_tokens,COALESCE(SUM(CASE WHEN is_demo=0 THEN cost_nano ELSE 0 END),0) cost_nano,COALESCE(SUM(CASE WHEN is_demo=1 THEN 1 ELSE 0 END),0) demo_requests,COALESCE(AVG(CASE WHEN status='success' THEN duration_ms END),0) latency_ms,COALESCE(SUM(CASE WHEN usage_mode LIKE 'reserved%' THEN 1 ELSE 0 END),0) uncertain_requests FROM requests WHERE started_at>=?`, cut)
	if err != nil {
		return nil, err
	}
	bucketMS := int64(hours) * 3600000 / 24
	series := []any{}
	for i := 0; i < 24; i++ {
		lo := cut + int64(i)*bucketMS
		hi := lo + bucketMS
		r, er := a.Store.DB.Query("SELECT COUNT(*) requests,COALESCE(SUM(output_tokens),0) tokens,COALESCE(SUM(CASE WHEN status='error' OR status='unknown' THEN 1 ELSE 0 END),0) errors FROM requests WHERE started_at>=? AND started_at<?", lo, hi)
		if er != nil {
			return nil, er
		}
		series = append(series, Object{"time": lo, "requests": r[0].Int("requests"), "tokens": r[0].Int("tokens"), "errors": r[0].Int("errors")})
	}
	top, err := a.Store.DB.Query("SELECT model_id,provider_id,COUNT(*) requests,COALESCE(SUM(input_tokens+output_tokens),0) tokens,COALESCE(SUM(cost_nano),0) cost_nano,AVG(duration_ms) latency_ms FROM requests WHERE started_at>=? GROUP BY model_id,provider_id ORDER BY requests DESC LIMIT 20", cut)
	if err != nil {
		return nil, err
	}
	recent, err := a.Store.DB.Query("SELECT id,parent_id,model_id,provider_id,protocol,status,http_status,started_at,duration_ms,cost_nano,is_demo FROM requests ORDER BY started_at DESC LIMIT 6")
	if err != nil {
		return nil, err
	}
	quotas, err := a.Engine.Quotas()
	if err != nil {
		return nil, err
	}
	return Object{"summary": rows[0], "series": series, "top_models": top, "recent": recent, "quotas": quotas, "runtime": a.Engine.Health(), "config_version": a.Store.Config().Version, "range": hours, "uptime_ms": now() - a.Started}, nil
}
