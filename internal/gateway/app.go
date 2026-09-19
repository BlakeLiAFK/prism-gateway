package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"prism-gateway/internal/sqlite"
	"runtime"
	"strings"
	"sync"
	"time"
)

type App struct {
	Store   *Store
	Engine  *Engine
	UI      http.Handler
	Listen  *Listener
	Started int64
	Context context.Context
	loginMu sync.Mutex
	logins  map[string][]int64
	workers sync.WaitGroup
}

func NewApp(ctx context.Context, s *Store, ui http.Handler) *App {
	a := &App{Store: s, Engine: NewEngine(s), UI: ui, Started: now(), Context: ctx, logins: map[string][]int64{}}
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
func (a *App) call(ctx context.Context, action string, p Object) (any, error) {
	c := a.Store.Config()
	version := int64(num(p, "version"))
	id := str(p, "id")
	switch action {
	case "config.get":
		return c, nil
	case "config.export":
		return Object{
			"format":          "prism.config/1",
			"gateway_version": Version,
			"exported_at":     now(),
			"config":          c,
			"note":            "不含上游 API Key、客户端 Key 与管理员令牌；导入到新实例后需重新填写供应商凭证",
		}, nil
	case "config.import":
		return a.importConfig(version, p)
	case "system.info":
		return Object{"version": Version, "go": runtime.Version(), "sqlite": sqlite.Version(), "uptime_ms": now() - a.Started, "config_version": c.Version, "storage": "SQLite", "ui": "embed.FS", "management_api": "POST /api.json", "runtime": a.Engine.Health()}, nil
	case "dashboard.get", "usage.summary", "usage.timeseries":
		return a.dashboard(p)
	case "provider.list":
		return c.Providers, nil
	case "model.list":
		return c.Models, nil
	case "route.list":
		return c.Routes, nil
	case "alias.list":
		return c.Aliases, nil
	case "settings.get":
		return c.Settings, nil
	case "quota.list":
		return a.Engine.Quotas()
	case "provider.save":
		v := Provider{ID: id, Enabled: true, Kind: "custom", Auth: "auto", TimeoutSec: 300}
		if id != "" {
			if old, ok := c.provider(id); ok {
				v = old
			}
		} else {
			v.ID = randomID("p_")
		}
		vals := obj(p["provider"])
		if vals == nil {
			return nil, fail("INVALID_PARAMS", "缺少 provider 对象", 400)
		}
		secret, changeSecret := vals["api_key"]
		valsCopy := Object{}
		for k, x := range vals {
			if k != "api_key" {
				valsCopy[k] = x
			}
		}
		if er := decode(valsCopy, &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		if changeSecret {
			v.Secret, _ = secret.(string)
		}
		v.HasKey = v.Secret != ""
		return a.Store.Change(version, action, v.ID, func(c *Config) error {
			for i, x := range c.Providers {
				if x.ID == v.ID {
					c.Providers[i] = v
					return nil
				}
			}
			c.Providers = append(c.Providers, v)
			return nil
		})
	case "model.save":
		v := Model{ID: id, Enabled: true, Protocol: "chat", Tools: true, Context: 128000, MaxOutput: 4096, Concurrency: 2}
		if old, ok := c.model(id); ok {
			v = old
		}
		if er := decode(p["model"], &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		if v.ID == "" {
			v.ID = randomID("m_")
		}
		return a.Store.Change(version, action, v.ID, func(c *Config) error {
			for i, x := range c.Models {
				if x.ID == v.ID {
					c.Models[i] = v
					return nil
				}
			}
			c.Models = append(c.Models, v)
			return nil
		})
	case "route.save":
		v := Route{ID: id, Enabled: true, Strategy: "priority", Affinity: true, Candidates: []Candidate{}}
		if old, ok := c.route(id); ok {
			v = old
		}
		if er := decode(p["route"], &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		return a.Store.Change(version, action, v.ID, func(c *Config) error {
			for i, x := range c.Routes {
				if x.ID == v.ID {
					c.Routes[i] = v
					return nil
				}
			}
			c.Routes = append(c.Routes, v)
			return nil
		})
	case "alias.save":
		v := Alias{Enabled: true}
		if er := decode(p["alias"], &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		return a.Store.Change(version, action, v.ID, func(c *Config) error {
			for i, x := range c.Aliases {
				if x.ID == v.ID {
					c.Aliases[i] = v
					return nil
				}
			}
			c.Aliases = append(c.Aliases, v)
			return nil
		})
	case "settings.update":
		v := c.Settings
		if er := decode(p["settings"], &v); er != nil {
			return nil, fail("INVALID_PARAMS", er.Error(), 400)
		}
		// 监听地址改了就先把新地址占下来；占不到就直接失败，配置保持原样，
		// 服务继续跑在旧地址上，不会因为一次手滑把自己关在门外
		// 只有管理员真的改动了这一项才切换。基准是配置里的旧值，不是当前实际地址：
		// --listen 救援覆盖期间保存其它设置，不该把服务拽回数据库里那个坏地址。
		var pending net.Listener
		if a.Listen != nil && v.Listen != "" && !sameListen(v.Listen, c.Settings.Listen) && !a.Listen.SameAddr(v.Listen) {
			ln, er := a.Listen.Bind(v.Listen)
			if er != nil {
				return nil, er
			}
			pending = ln
		}
		cfg, er := a.Store.Change(version, action, "settings", func(c *Config) error { c.Settings = v; return nil })
		if er != nil {
			if pending != nil {
				pending.Close()
			}
			return nil, er
		}
		if pending != nil {
			a.Listen.Adopt(pending)
		}
		return cfg, nil
	case "provider.delete", "model.delete", "route.delete", "alias.delete":
		return a.Store.Change(version, action, id, func(c *Config) error {
			found := false
			switch action {
			case "provider.delete":
				xs := []Provider{}
				for _, v := range c.Providers {
					if v.ID == id {
						found = true
						continue
					}
					xs = append(xs, v)
				}
				c.Providers = xs
			case "model.delete":
				xs := []Model{}
				for _, v := range c.Models {
					if v.ID == id {
						found = true
						continue
					}
					xs = append(xs, v)
				}
				c.Models = xs
			case "route.delete":
				xs := []Route{}
				for _, v := range c.Routes {
					if v.ID == id {
						found = true
						continue
					}
					xs = append(xs, v)
				}
				c.Routes = xs
			case "alias.delete":
				xs := []Alias{}
				for _, v := range c.Aliases {
					if v.ID == id {
						found = true
						continue
					}
					xs = append(xs, v)
				}
				c.Aliases = xs
			}
			if !found {
				return fail("NOT_FOUND", "对象不存在", 404)
			}
			return nil
		})
	case "route.test":
		o := obj(p["request"])
		if o == nil {
			o = Object{"model": id, "messages": []any{Object{"role": "user", "content": "Describe this code."}}}
		}
		protocol := str(p, "protocol")
		if protocol == "" {
			protocol = "chat"
		}
		ss, reasons, er := a.Engine.selections(c, o, protocol, "")
		if er != nil {
			return nil, er
		}
		ranked := []any{}
		for _, s := range ss {
			ranked = append(ranked, Object{"model_id": s.Model.ID, "score": s.Score, "reason": s.Reason, "protocol": s.Model.Protocol})
		}
		return Object{"ranked": ranked, "checks": reasons, "note": "静态协议/配置模拟，不发起模型请求。实际调用仍须通过实时额度、冷却与并发检查。"}, nil
	case "provider.test":
		pr, ok := c.provider(id)
		if !ok {
			return nil, fail("NOT_FOUND", "Provider 不存在", 404)
		}
		if pr.Kind == "mock" {
			return Object{"status": 200, "latency_ms": 0, "message": "本地演示 Provider 正常；未调用云端"}, nil
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(pr.BaseURL, "/")+"/models", nil)
		requestHeaders(req, &http.Request{Header: http.Header{}}, pr, providerProtocol(pr), "")
		t := now()
		res, er := a.Engine.client(pr).Do(req)
		if er != nil {
			return nil, fail("CONNECTION_FAILED", "上游连接失败；检查 URL、证书、私有网络权限与网络连通性", 502)
		}
		defer res.Body.Close()
		io.Copy(io.Discard, io.LimitReader(res.Body, 65536))
		return Object{"status": res.StatusCode, "latency_ms": now() - t, "message": "测试 GET /models；不是模型生成能力验证"}, nil
	case "provider.sync_models":
		return a.startSync(id)
	case "job.list":
		return a.Store.DB.Query("SELECT * FROM jobs ORDER BY created_at DESC LIMIT 100")
	case "job.get":
		r, er := a.Store.DB.Query("SELECT * FROM jobs WHERE id=?", id)
		if er != nil {
			return nil, er
		}
		if len(r) == 0 {
			return nil, fail("NOT_FOUND", "任务不存在", 404)
		}
		return r[0], nil
	case "apikey.list":
		return a.Store.DB.Query("SELECT id,name,prefix,enabled,allowed,created_at,last_used,revoked_at FROM api_keys ORDER BY created_at DESC")
	case "apikey.create":
		name := strings.TrimSpace(str(p, "name"))
		if name == "" || len(name) > 120 {
			return nil, fail("INVALID_PARAMS", "Key 名称不能为空且长度最多 120", 400)
		}
		allowed := []string{}
		for _, v := range arr(p["allowed"]) {
			s, ok := v.(string)
			if !ok {
				return nil, fail("INVALID_PARAMS", "allowed 应为字符串数组", 400)
			}
			exists := false
			for _, x := range c.Models {
				exists = exists || x.ID == s
			}
			for _, x := range c.Routes {
				exists = exists || x.ID == s
			}
			for _, x := range c.Aliases {
				exists = exists || x.ID == s
			}
			if !exists {
				return nil, fail("INVALID_PARAMS", "授权目标不存在: "+s, 400)
			}
			allowed = append(allowed, s)
		}
		key := randomID("prism_sk_")
		kid := randomID("key_")
		er := a.Store.DB.Exec("INSERT INTO api_keys(id,name,prefix,digest,enabled,allowed,created_at) VALUES (?,?,?,?,1,?,?)", kid, name, key[:17], digest(key), raw(allowed), now())
		if er != nil {
			return nil, er
		}
		a.audit(action, kid)
		return Object{"id": kid, "key": key, "warning": "完整密钥只显示这一次；请保存在密码管理器中。"}, nil
	case "apikey.revoke":
		er := a.Store.DB.Exec("UPDATE api_keys SET enabled=0,revoked_at=? WHERE id=?", now(), id)
		if er == nil {
			a.audit(action, id)
		}
		return Object{"id": id, "revoked": true}, er
	case "session.list":
		return a.Store.DB.Query("SELECT * FROM sessions ORDER BY updated_at DESC LIMIT 200")
	case "session.delete":
		er := a.Store.DB.Exec("DELETE FROM sessions WHERE id=?", id)
		if er == nil {
			a.audit(action, id)
		}
		return Object{"id": id, "unbound": true}, er
	case "request.list":
		return a.requests(p)
	case "request.get":
		r, er := a.Store.DB.Query("SELECT * FROM requests WHERE id=?", id)
		if er != nil {
			return nil, er
		}
		if len(r) == 0 {
			return nil, fail("NOT_FOUND", "请求不存在", 404)
		}
		return r[0], nil
	case "audit.list":
		return a.Store.DB.Query("SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT 200")
	case "playground.run":
		protocol := str(p, "protocol")
		if protocol != "chat" && protocol != "messages" && protocol != "responses" && protocol != "systemone" {
			return nil, fail("INVALID_PARAMS", "protocol 无效", 400)
		}
		body := Object{"model": str(p, "model")}
		if protocol == "systemone" {
			// System One 的载荷是状态加问题，没有 prompt 也没有流式开关
			st := str(p, "state")
			if len(st) > 64000 || st == "" {
				return nil, fail("INVALID_PARAMS", "请输入不超过 64KB 的状态内容", 400)
			}
			body["state"] = st
			body["questions"] = obj(p["questions"])
			if er := validateSystemOne(body); er != nil {
				return nil, fail("INVALID_PARAMS", er.Error(), 400)
			}
		} else {
			prompt := str(p, "prompt")
			if len(prompt) > 64000 || prompt == "" {
				return nil, fail("INVALID_PARAMS", "请输入不超过 64KB 的提示词", 400)
			}
			body["stream"] = false
			if protocol == "responses" {
				body["input"] = prompt
				body["max_output_tokens"] = 512
			} else {
				body["messages"] = []any{Object{"role": "user", "content": prompt}}
				body["max_tokens"] = 512
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://internal"+pathFor(protocol), bytes.NewBufferString(raw(body)))
		req.Header.Set("X-Prism-Session", str(p, "session"))
		rr := httptest.NewRecorder()
		t := now()
		a.Engine.Handle(rr, req, protocol, Principal{ID: "admin-playground"})
		var result any
		json.Unmarshal(rr.Body.Bytes(), &result)
		return Object{"status": rr.Code, "duration_ms": now() - t, "headers": rr.Header(), "response": result}, nil
	case "backup.create":
		path, size, er := a.Store.Backup()
		if er != nil {
			return nil, er
		}
		a.audit("backup.create", filepath.Base(path))
		slog.Info("配置备份已生成", "path", path, "bytes", size)
		return Object{"path": path, "bytes": size,
			"note": "快照不含 .key 主密钥；恢复上游凭证必须配套原主密钥"}, nil
	case "backup.list":
		return a.Store.Backups()
	case "demo.enable":
		return a.EnableDemo(version)
	case "batch.read":
		requests := arr(p["requests"])
		if len(requests) < 1 || len(requests) > 10 {
			return nil, fail("INVALID_PARAMS", "batch.read 允许 1–10 个只读请求", 400)
		}
		out := []any{}
		for _, v := range requests {
			v := obj(v)
			act := str(v, "action")
			switch act {
			case "config.get", "config.export", "backup.list", "system.info", "dashboard.get", "usage.summary", "quota.list", "provider.list", "model.list", "route.list", "request.list", "session.list", "job.list", "apikey.list":
				data, er := a.call(ctx, act, obj(v["params"]))
				if er != nil {
					_, code, msg := errorParts(er)
					out = append(out, Object{"ok": false, "error": Object{"code": code, "message": msg}})
				} else {
					out = append(out, Object{"ok": true, "data": data})
				}
			default:
				return nil, fail("ACTION_NOT_ALLOWED", "batch.read 中的 action 不受支持", 400)
			}
		}
		return out, nil
	default:
		return nil, fail("UNKNOWN_ACTION", "未知管理 action: "+action, 400)
	}
}

// importConfig 用导出的快照整体替换业务配置。
// 导出文件里没有凭证，所以按 provider id 保留库中已有的密钥；
// 文件带来的新供应商没有密钥，返回值会列出需要补填的 id。
// 客户端 Key、管理员令牌、请求记录和用量都不在替换范围内。
func (a *App) importConfig(version int64, p Object) (any, error) {
	src := obj(p["config"])
	if src == nil {
		return nil, fail("INVALID_PARAMS", "缺少 config 对象；请提交 config.export 的 config 字段", 400)
	}
	if f := str(p, "format"); f != "" && f != "prism.config/1" {
		return nil, fail("UNSUPPORTED_FORMAT", "不认识的导出格式："+f, 400)
	}
	var in Config
	dec := json.NewDecoder(bytes.NewReader([]byte(raw(src))))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		return nil, fail("INVALID_CONFIG", "配置结构无法解析："+e.Error(), 400)
	}
	c, err := a.Store.Change(version, "config.import", "config", func(cur *Config) error {
		kept := map[string]string{}
		for _, old := range cur.Providers {
			kept[old.ID] = old.Secret
		}
		cur.Providers = append([]Provider{}, in.Providers...)
		for i := range cur.Providers {
			cur.Providers[i].Secret = kept[cur.Providers[i].ID]
			// has_key 由库中实际凭证决定，不接受导入文件的声明
			cur.Providers[i].HasKey = cur.Providers[i].Secret != ""
		}
		cur.Models = append([]Model{}, in.Models...)
		cur.Routes = append([]Route{}, in.Routes...)
		cur.Aliases = append([]Alias{}, in.Aliases...)
		cur.Settings = in.Settings
		return nil
	})
	if err != nil {
		return nil, err
	}
	missing := []any{}
	for _, v := range c.Providers {
		if !v.HasKey && v.Auth != "none" {
			missing = append(missing, v.ID)
		}
	}
	return Object{"version": c.Version, "providers": len(c.Providers), "models": len(c.Models),
		"routes": len(c.Routes), "aliases": len(c.Aliases), "providers_missing_key": missing}, nil
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
func providerProtocol(p Provider) string {
	if p.Kind == "anthropic" {
		return "messages"
	}
	return "chat"
}
func (a *App) startSync(id string) (any, error) {
	c := a.Store.Config()
	p, ok := c.provider(id)
	if !ok {
		return nil, fail("NOT_FOUND", "Provider 不存在", 404)
	}
	if p.Kind == "mock" {
		return nil, fail("NOT_SUPPORTED", "本地演示不需要同步模型", 400)
	}
	jid := randomID("job_")
	er := a.Store.DB.Exec("INSERT INTO jobs(id,action,status,created_at,updated_at) VALUES (?,?,'queued',?,?)", jid, "provider.sync_models", now(), now())
	if er != nil {
		return nil, er
	}
	a.audit("provider.sync_models", id)
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		a.Store.DB.Exec("UPDATE jobs SET status='running',updated_at=? WHERE id=?", now(), jid)
		ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
		defer cancel()
		result, err := a.syncModels(ctx, p, c.Version)
		if err != nil {
			msg := "同步失败：上游连接、响应格式或配置版本发生变化，请检查后重试。"
			var ae *APIError
			if errors.As(err, &ae) {
				msg = ae.Message
			}
			a.Store.DB.Exec("UPDATE jobs SET status='failed',error=?,updated_at=? WHERE id=?", msg, now(), jid)
		} else {
			a.Store.DB.Exec("UPDATE jobs SET status='succeeded',result=?,updated_at=? WHERE id=?", raw(result), now(), jid)
		}
	}()
	return Object{"job_id": jid}, nil
}
func (a *App) syncModels(ctx context.Context, p Provider, version int64) (any, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	requestHeaders(req, &http.Request{Header: http.Header{}}, p, providerProtocol(p), "")
	res, err := a.Engine.client(p).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fail("UPSTREAM_ERROR", fmt.Sprintf("模型列表返回 HTTP %d", res.StatusCode), 502)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var o Object
	if err = json.Unmarshal(data, &o); err != nil {
		return nil, err
	}
	items := arr(o["data"])
	if len(items) == 0 || len(items) > 2000 {
		return nil, fail("INVALID_MODELS", "未识别到模型列表，或模型数量超过 2000；可在界面手动添加", 400)
	}
	added := 0
	_, err = a.Store.Change(version, "provider.sync_models", p.ID, func(c *Config) error {
		for _, v := range items {
			v := obj(v)
			up := str(v, "id")
			if !validID(up) {
				continue
			}
			exists := false
			for _, m := range c.Models {
				if m.ProviderID == p.ID && m.Upstream == up {
					exists = true
					break
				}
			}
			if exists {
				continue
			}
			protocol := providerProtocol(p)
			if p.Kind == "opencode" {
				protocol = openCodeProtocol(up)
			}
			name := str(v, "name")
			if name == "" {
				name = str(v, "display_name")
			}
			if name == "" {
				name = up
			}
			// 模型 ID 直接用上游原名，客户端传 model 时更直观；
			// 与已有对象重名时才退回带 provider 前缀的形式
			mid := up
			if !validID(mid) || c.nameTaken(mid) {
				mid = p.ID + "/" + up
			}
			if !validID(mid) {
				mid = "model_" + digest(p.ID + up)[:20]
			}
			c.Models = append(c.Models, newSyncedModel(mid, p.ID, up, name, protocol, v))
			added++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return Object{"added": added, "note": "新模型默认禁用。请确认原生协议、能力、上下文和价格，再手动启用；未自动抓取官方额度。"}, nil
}
func openCodeProtocol(id string) string {
	switch id {
	case "minimax-m3", "minimax-m2.7", "minimax-m2.5", "qwen3.8-max", "qwen3.8-flash", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus":
		return "messages"
	case "gpt-5.6-luna", "grok-4.6", "muse-spark-1.3-contributor", "muse-spark-1.2-contributor":
		return "responses"
	default:
		return "chat"
	}
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
	where := ` WHERE (?='' OR model_id LIKE ? OR parent_id LIKE ?) AND (?='' OR status=?) AND (?='' OR provider_id=?)`
	args := []any{q, "%" + q + "%", "%" + q + "%", status, status, provider, provider}
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
