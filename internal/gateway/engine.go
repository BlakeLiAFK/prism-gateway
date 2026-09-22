package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Principal struct {
	ID      string
	Allowed []string
}

func (p Principal) allows(model string) bool {
	if len(p.Allowed) == 0 {
		return true
	}
	for _, s := range p.Allowed {
		if s == model {
			return true
		}
	}
	return false
}

type health struct {
	Active     int
	Recent     []int64
	Cooldown   int64
	LastStatus int
	LatencyMS  int64
	Limits     Object // 上游最近一次声明的限额，原样留存
	LimitsAt   int64
}
type Engine struct {
	store    *Store
	mu       sync.Mutex
	states   map[string]*health
	global   int
	clientMu sync.Mutex
	clients  map[string]*http.Client
}

func NewEngine(s *Store) *Engine {
	return &Engine{store: s, states: map[string]*health{}, clients: map[string]*http.Client{}}
}
func (e *Engine) client(p Provider) *http.Client {
	e.clientMu.Lock()
	defer e.clientMu.Unlock()
	key := p.ID + "|" + p.BaseURL + fmt.Sprint(p.TimeoutSec, p.AllowPrivate)
	if c := e.clients[key]; c != nil {
		return c
	}
	c := clientFor(p)
	e.clients[key] = c
	return c
}
func (e *Engine) Close() {
	e.clientMu.Lock()
	defer e.clientMu.Unlock()
	for _, c := range e.clients {
		c.CloseIdleConnections()
	}
}
func (e *Engine) state(id string) *health {
	h := e.states[id]
	if h == nil {
		h = &health{}
		e.states[id] = h
	}
	return h
}
func (e *Engine) Health() Object {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := Object{}
	for k, v := range e.states {
		m[k] = Object{"active": v.Active, "cooldown_until": v.Cooldown, "last_status": v.LastStatus, "latency_ms": v.LatencyMS,
			"limits": v.Limits, "limits_at": v.LimitsAt}
	}
	return Object{"active": e.global, "models": m}
}
func (e *Engine) quota(model string) (Object, error) {
	r, err := e.store.DB.Query(`SELECT COALESCE(SUM(CASE WHEN started_at>=? THEN cost_nano ELSE 0 END),0) h,COALESCE(SUM(CASE WHEN started_at>=? THEN cost_nano ELSE 0 END),0) w,COALESCE(SUM(cost_nano),0) m FROM requests WHERE model_id=? AND started_at>=?`, now()-5*3600000, now()-7*86400000, model, now()-30*86400000)
	if err != nil {
		return nil, err
	}
	return Object{"used_5h": dollars(r[0].Int("h")), "used_7d": dollars(r[0].Int("w")), "used_30d": dollars(r[0].Int("m"))}, nil
}
func (e *Engine) Quotas() ([]any, error) {
	out := []any{}
	for _, m := range e.store.Config().Models {
		q, err := e.quota(m.ID)
		if err != nil {
			return nil, err
		}
		q["id"] = m.ID
		q["name"] = m.Name
		q["limit_5h"] = m.Limit5h
		q["limit_7d"] = m.Limit7d
		q["limit_30d"] = m.Limit30d
		q["mode"] = "local_rolling_estimate"
		q["pricing_set"] = m.PricingSet
		out = append(out, q)
	}
	return out, nil
}
func cost(m Model, u Usage) int64 {
	if !m.PricingSet {
		return 0
	}
	uncached := max(int64(0), u.Input-u.Cache-u.Write)
	v := (float64(uncached)*m.InputPrice + float64(u.Output)*m.OutputPrice + float64(u.Cache)*m.CachePrice + float64(u.Write)*m.WritePrice) * 1000
	return int64(math.Ceil(v))
}
func estimateInput(body Object) int64 { return int64((len(raw(body))+2)/3 + 16) }
func reserveCost(m Model, o Object) int64 {
	n := int(num(o, "max_tokens"))
	if v := int(num(o, "max_completion_tokens")); v > 0 {
		n = v
	}
	if v := int(num(o, "max_output_tokens")); v > 0 {
		n = v
	}
	if n < 1 {
		n = min(m.MaxOutput, 4096)
	}
	return cost(m, Usage{Input: estimateInput(o), Output: int64(n)})
}

type selection struct {
	Model    Model
	Provider Provider
	Body     Object
	Score    float64
	Reason   string
	Cross    bool
	// Ignored 是被接受但没有传给上游的请求字段，会在 X-Prism-Ignored 里告知调用方
	Ignored []string
}

func (e *Engine) selections(c Config, o Object, p, session string) ([]selection, []any, error) {
	requested := str(o, "model")
	resolved := requested
	for _, a := range c.Aliases {
		if a.ID == resolved && a.Enabled {
			resolved = a.Target
			break
		}
	}
	route, routeOK := c.route(resolved)
	var candidates []Candidate
	if routeOK {
		if !route.Enabled {
			return nil, nil, fail("MODEL_DISABLED", "路由已禁用", 404)
		}
		candidates = route.Candidates
	} else if _, ok := c.model(resolved); ok {
		candidates = []Candidate{{resolved, 1}}
	} else {
		return nil, nil, fail("MODEL_NOT_FOUND", "模型或路由不存在", 404)
	}
	if routeOK && (o["previous_response_id"] != nil || o["conversation"] != nil) {
		return nil, nil, unsupported("有服务端会话状态的请求必须指定固定模型，不能使用自动路由")
	}
	pinned := ""
	if session != "" && routeOK && route.Affinity {
		r, er := e.store.DB.Query("SELECT model_id FROM sessions WHERE id=? AND updated_at>?", session, now()-int64(c.Settings.SessionTTLHours)*3600000)
		if er == nil && len(r) > 0 {
			pinned = r[0].String("model_id")
		}
	}
	out := []selection{}
	reasons := []any{}
	for i, cm := range candidates {
		m, ok := c.model(cm.ModelID)
		if !ok {
			continue
		}
		pr, ok := c.provider(m.ProviderID)
		why := ""
		if !ok || !m.Enabled || !pr.Enabled {
			why = "disabled"
		}
		if len(arr(o["tools"])) > 0 && !m.Tools {
			why = "tools_unsupported"
		}
		// state 是调用方自定义的 JSON，可能恰好含 image_url 这类键；
		// System One 只吃文本，不该被图像检测误伤。
		if m.Protocol != "systemone" && containsImage(o) && !m.Vision {
			why = "vision_unsupported"
		}
		var body Object
		json.Unmarshal([]byte(raw(o)), &body)
		cross := p != m.Protocol
		ignored := []string{}
		// System One 返回类型化决策，对话协议返回消息序列，两边没有共同语义。
		// 允许转换只能靠编造文本或字符串化 JSON，都会静默改变调用方拿到的东西。
		if why == "" && cross && (p == "systemone" || m.Protocol == "systemone") {
			why = "System One 与对话协议之间没有等价语义，不能转换；请直接调用该协议的模型"
		}
		if why == "" && cross {
			src := o
			// 已经接受丢弃推理内容的模型，请求侧的思考开关也就没有意义了：
			// 去掉它而不是让整个请求失败。丢弃同样会被告知调用方。
			if m.DropReasoning && o["thinking"] != nil {
				src = Object{}
				for k, v := range o {
					src[k] = v
				}
				delete(src, "thinking")
				ignored = append(ignored, "thinking")
			}
			ir, err := decodeCanonical(src, p)
			if err != nil {
				why = err.Error()
			} else {
				ignored = append(ignored, ir.Ignored...)
				body, err = encodeCanonical(ir, m.Protocol, m.Upstream)
				if err != nil {
					why = err.Error()
				}
			}
		} else {
			body["model"] = m.Upstream
		}
		if why == "" && m.Protocol != "systemone" {
			field := "max_tokens"
			if m.Protocol == "responses" {
				field = "max_output_tokens"
			}
			if m.Protocol == "chat" && body["max_completion_tokens"] != nil {
				field = "max_completion_tokens"
			}
			if body[field] == nil {
				body[field] = min(m.MaxOutput, 4096)
			}
			if num(body, field) > float64(m.MaxOutput) {
				why = "请求输出上限超过模型配置"
			}
		}
		if why == "" {
			q, err := e.quota(m.ID)
			if err != nil {
				return nil, nil, err
			}
			pressure := 0.0
			for _, v := range []struct {
				key   string
				limit float64
			}{{"used_5h", m.Limit5h}, {"used_7d", m.Limit7d}, {"used_30d", m.Limit30d}} {
				if v.limit > 0 {
					pressure = max(pressure, num(q, v.key)/v.limit)
				}
			}
			score := 1000 - float64(i)*10
			if route.Strategy == "balanced" {
				score = 1000 - pressure*500 + float64(cm.Weight)
			}
			if pinned == m.ID {
				score += 10000
			}
			if !cross {
				score += 2
			}
			out = append(out, selection{m, pr, body, score, fmt.Sprintf("%s; native=%t; affinity=%t; pressure=%.3f", route.Strategy, !cross, pinned == m.ID, pressure), cross, ignored})
		}
		reasons = append(reasons, Object{"model_id": m.ID, "eligible": why == "", "reason": why, "native": !cross})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, reasons, nil
}
func containsImage(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if t := str(x, "type"); t == "image" || t == "input_image" || t == "image_url" {
			return true
		}
		for _, v := range x {
			if containsImage(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if containsImage(v) {
				return true
			}
		}
	}
	return false
}
func (e *Engine) admit(s selection, key Principal, reqID, requested, p, session string, from client) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.state(s.Model.ID)
	t := now()
	if h.Cooldown > t {
		return "", fail("UPSTREAM_COOLDOWN", "上游限流冷却中", 429)
	}
	if e.global >= e.store.Config().Settings.GlobalConcurrency || h.Active >= s.Model.Concurrency {
		return "", fail("CONCURRENCY_LIMIT", "并发已满，请稍后重试", 429)
	}
	recent := h.Recent[:0]
	for _, ts := range h.Recent {
		if ts > t-60000 {
			recent = append(recent, ts)
		}
	}
	h.Recent = recent
	if s.Model.RPM > 0 && len(h.Recent) >= s.Model.RPM {
		return "", fail("RPM_LIMIT", "本地 RPM 限额已满", 429)
	}
	q, err := e.quota(s.Model.ID)
	if err != nil {
		return "", err
	}
	reserve := reserveCost(s.Model, s.Body)
	for _, v := range []struct {
		k string
		n float64
	}{{"used_5h", s.Model.Limit5h}, {"used_7d", s.Model.Limit7d}, {"used_30d", s.Model.Limit30d}} {
		if v.n > 0 && nano(num(q, v.k))+reserve > nano(v.n) {
			return "", fail("LOCAL_QUOTA_LIMIT", "本地滚动预算不足（包含本次预留）；不是上游官方余额", 429)
		}
	}
	id := randomID("att_")
	err = e.store.DB.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,cost_nano,cost_known,reason,is_demo,client_ip,user_agent) VALUES (?,?,?,?,?,?,?,?,?,'running',?,?,?,?,?,?,?)`, id, reqID, key.ID, requested, s.Model.ID, s.Provider.ID, p, s.Model.Protocol, session, t, reserve, s.Model.PricingSet, s.Reason, s.Provider.Kind == "mock", from.IP, from.Agent)
	if err != nil {
		return "", err
	}
	h.Active++
	e.global++
	h.Recent = append(h.Recent, t)
	return id, nil
}
func (e *Engine) finish(s selection, id, session, keyID string, u Usage, status int, err error, start int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.state(s.Model.ID)
	h.Active = max(0, h.Active-1)
	e.global = max(0, e.global-1)
	h.LastStatus = status
	h.LatencyMS = now() - start
	state := "success"
	code := ""
	mode := "reported_tokens"
	value := cost(s.Model, u)
	known := u.Known && s.Model.PricingSet
	if err != nil || status >= 400 {
		state = "error"
		code = fmt.Sprintf("UPSTREAM_%d", status)
		// 带自有错误码的失败（空回答、跨协议不兼容）记它自己的码：
		// 记成 UPSTREAM_502 会把「上游坏了」和「上游好好的但内容用不了」混为一谈
		var ae *APIError
		if errors.As(err, &ae) && ae.Code != "" {
			code = ae.Code
		}
		if status == 499 {
			code = "CLIENT_CANCELED"
		}
	}
	if !u.Known {
		mode = "unknown"
		known = false
	}
	// 上游回了 200 又给了可信用量，就按实际用量记账——空回答同样烧掉了推理 token，
	// 那是真花出去的钱，退回预留值或抹成零都不对。
	billed := status == 200 && u.Known
	if (state == "error" || !u.Known) && !billed {
		if status == 429 || status == 503 || (status >= 400 && status < 500 && status != 499) {
			value = 0
			mode = "rejected"
		} else {
			mode = "reserved_unknown"
			state = "unknown"
			value = reserveCost(s.Model, s.Body)
			known = false
		}
	}
	if err := e.store.DB.Exec(`UPDATE requests SET status=?,http_status=?,duration_ms=?,input_tokens=?,output_tokens=?,cache_tokens=?,write_tokens=?,cost_nano=?,cost_known=?,usage_mode=?,error_code=? WHERE id=?`, state, status, now()-start, u.Input, u.Output, u.Cache, u.Write, value, known, mode, code, id); err != nil {
		slog.Error("request accounting write failed", "request_id", id, "model", s.Model.ID, "err", err)
	}
	if session != "" && state == "success" {
		// 冲突分支里的列要用本表限定。曾经误写成 requests.requests（另一张表），
		// 导致整条语句编译失败、会话亲和记录一条都没写进去，而错误被丢弃因此无人察觉。
		if e := e.store.DB.Exec(`INSERT INTO sessions VALUES (?,?,?,?,?,1) ON CONFLICT(id) DO UPDATE SET model_id=excluded.model_id,provider_id=excluded.provider_id,updated_at=excluded.updated_at,requests=sessions.requests+1`, session, keyID, s.Model.ID, s.Provider.ID, now()); e != nil {
			slog.Error("session affinity write failed", "request_id", id, "err", e)
		}
	}
}
func (e *Engine) cooldown(id, retry string) {
	d := 30 * time.Second
	if n, er := strconv.Atoi(retry); er == nil && n > 0 {
		d = time.Duration(n) * time.Second
	} else if t, er := http.ParseTime(retry); er == nil && t.After(time.Now()) {
		d = time.Until(t)
	}
	e.cooldownFor(id, d)
}

func (e *Engine) cooldownFor(id string, d time.Duration) {
	e.mu.Lock()
	e.state(id).Cooldown = now() + int64(min(d, 24*time.Hour)/time.Millisecond)
	e.mu.Unlock()
}

// retryable 是「上游暂时忙，换个候选重试是安全的」这一类状态码。
// 529 是 TypeSafe 的过载码，语义与 503 相同。
func retryable(status int) bool {
	return status == 429 || status == 503 || status == 529
}

// gatewayFailure 判断上游自己的网关层报错。只在「上游确实回了一个 HTTP 响应」
// 时才成立——这种响应到不了模型，用量为零，换候选是安全的。
// 网关自己标成 502 的那些情况（连不上、SSE 类型不对、响应读不懂）不走这里：
// 请求可能已经被部分处理，重放并不安全。
func gatewayFailure(upstreamStatus int) bool {
	return upstreamStatus == 502 || upstreamStatus == 504
}

// exhausted 是「这个上游对本次请求确定性不可用」：额度用尽（402）、
// 套餐不含该模型或凭证权限不足（403）、模型在上游已经不存在（404，
// 上游下架某个模型变体就会这样）。同样该换候选，但和 429 不是一回事——
// 429 过一会儿就好了，这类问题要等人去充值、升级套餐或改配置，
// 所以冷却时间长得多。
func exhausted(status int) bool {
	return status == 402 || status == 403 || status == 404
}

// exhaustedCooldown 决定耗尽类故障的冷却时长。
// 取 15 分钟：短到额度恢复后不至于把候选闲置太久，
// 长到不会每来一个请求就先去撞一次已经没额度的上游。
const exhaustedCooldown = 15 * time.Minute

func pathFor(p string) string {
	switch p {
	case "chat":
		return "/chat/completions"
	case "responses":
		return "/responses"
	case "systemone":
		return "/systemone"
	default:
		return "/messages"
	}
}
func protocolError(w http.ResponseWriter, p string, status int, code, message, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", id)
	if status == 429 {
		w.Header().Set("Retry-After", "30")
	}
	w.WriteHeader(status)
	if p == "messages" {
		typ := "invalid_request_error"
		if status == 401 {
			typ = "authentication_error"
		} else if status == 429 {
			typ = "rate_limit_error"
		} else if status >= 500 {
			typ = "api_error"
		}
		json.NewEncoder(w).Encode(Object{"type": "error", "error": Object{"type": typ, "message": message}, "request_id": id})
	} else {
		json.NewEncoder(w).Encode(Object{"error": Object{"type": code, "code": code, "message": message, "param": nil}, "request_id": id})
	}
}
func errorParts(err error) (int, string, string) {
	var a *APIError
	if errors.As(err, &a) {
		return a.Status, a.Code, a.Message
	}
	return 500, "INTERNAL_ERROR", "内部操作失败，请检查服务端日志"
}
func (e *Engine) Handle(w http.ResponseWriter, r *http.Request, p string, key Principal) {
	from := clientOf(r)
	id := randomID("req_")
	w.Header().Set("X-Request-ID", id)
	c := e.store.Config()
	r.Body = http.MaxBytesReader(w, r.Body, int64(c.Settings.MaxBodyMB)<<20)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		protocolError(w, p, 413, "BODY_TOO_LARGE", "请求体过大", id)
		return
	}
	var o Object
	if err = json.Unmarshal(b, &o); err != nil || o == nil {
		protocolError(w, p, 400, "INVALID_JSON", "无效的 JSON 对象", id)
		return
	}
	requested := str(o, "model")
	if requested == "" {
		protocolError(w, p, 400, "MODEL_REQUIRED", "必须指定 model", id)
		return
	}
	if !key.allows(requested) {
		protocolError(w, p, 403, "MODEL_FORBIDDEN", "此 API Key 无权调用该模型或路由", id)
		return
	}
	rawSession := ""
	for _, h := range []string{"x-prism-session", "x-opencode-session", "x-claude-code-session-id", "x-codex-session-id", "session_id", "x-session-id"} {
		if v := r.Header.Get(h); v != "" && len(v) <= 512 {
			rawSession = v
			break
		}
	}
	if rawSession == "" {
		rawSession = randomID("conv_")
	}
	session := "ses_" + digest(key.ID + ":" + rawSession)[:32]
	w.Header().Set("X-Prism-Session", rawSession)
	w.Header().Set("X-Prism-Config-Version", fmt.Sprint(c.Version))
	if p == "systemone" {
		if er := validateSystemOne(o); er != nil {
			protocolError(w, p, 400, "INVALID_REQUEST", er.Error(), id)
			return
		}
	}
	selections, whys, err := e.selections(c, o, p, session)
	if err != nil {
		status, code, msg := errorParts(err)
		protocolError(w, p, status, code, msg, id)
		return
	}
	if len(selections) == 0 {
		// 每个候选为什么被排除是算出来了的，不告诉调用方等于让人去猜
		detail := ""
		for _, v := range whys {
			e := obj(v)
			if r := str(e, "reason"); r != "" {
				if detail != "" {
					detail += "；"
				}
				detail += str(e, "model_id") + ": " + r
			}
		}
		if detail == "" {
			detail = "候选池为空或全部被禁用"
		}
		protocolError(w, p, 400, "NO_COMPATIBLE_MODEL", "没有兼容此请求的候选模型（"+detail+"）", id)
		return
	}
	// Requests with server-managed state or built-in tools must never be replayed.
	safeFallback := o["previous_response_id"] == nil && o["conversation"] == nil && !boolean(o, "background")
	for _, t := range arr(o["tools"]) {
		tt := str(obj(t), "type")
		if tt != "" && tt != "function" {
			safeFallback = false
		}
	}
	lastErr := fail("NO_CAPACITY", "所有候选模型当前均不可用", 429)
	for _, s := range selections {
		att, er := e.admit(s, key, id, requested, p, session, from)
		if er != nil {
			lastErr = er
			continue
		}
		start := now()
		u := Usage{}
		status := 200
		upstreamStatus := 0
		var runErr error
		func() {
			defer func() { e.finish(s, att, session, key.ID, u, status, runErr, start) }()
			w.Header().Set("X-Prism-Model", s.Model.ID)
			w.Header().Set("X-Prism-Provider", s.Provider.ID)
			w.Header().Set("X-Prism-Upstream-Model", s.Model.Upstream)
			mode := "native"
			if s.Cross {
				mode = "converted"
			}
			w.Header().Set("X-Prism-Protocol-Mode", mode)
			if len(s.Ignored) > 0 {
				// 这些请求字段被接受但没有传给上游。忽略可以，不说不行。
				w.Header().Set("X-Prism-Ignored", strings.Join(s.Ignored, ","))
			}
			if s.Provider.Kind == "mock" && p == "systemone" {
				w.Header().Set("X-Prism-Demo", "true")
				res := demoSystemOne(o, s.Model.Upstream)
				extractUsage(obj(res["usage"]), "systemone", &u)
				w.Header().Set("Content-Type", "application/json")
				runErr = json.NewEncoder(w).Encode(res)
				return
			}
			if s.Provider.Kind == "mock" {
				w.Header().Set("X-Prism-Demo", "true")
				co := demoCompletion(o, p)
				u = co.Usage
				if boolean(o, "stream") {
					w.Header().Set("Content-Type", "text/event-stream")
					em := newEmitter(w, p, s.Model.ID, id)
					em.usage = u
					for i, b := range co.Blocks {
						if er = em.start(i, b); er != nil {
							runErr = er
							break
						}
						text := b.Text
						if b.Kind == "tool" {
							text = b.Arguments
						}
						runes := []rune(text)
						for n := 0; n < len(runes); n += 8 {
							select {
							case <-r.Context().Done():
								runErr = r.Context().Err()
								status = 499
								return
							default:
							}
							if er = em.delta(i, string(runes[n:min(n+8, len(runes))])); er != nil {
								runErr = er
								status = 499
								return
							}
							time.Sleep(8 * time.Millisecond)
						}
					}
					runErr = em.finish()
				} else {
					w.Header().Set("Content-Type", "application/json")
					runErr = json.NewEncoder(w).Encode(completionObject(co, p, s.Model.ID, id))
				}
				return
			}
			target := strings.TrimRight(s.Provider.BaseURL, "/") + pathFor(s.Model.Protocol)
			req, er := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewBufferString(raw(s.Body)))
			if er != nil {
				runErr = er
				status = 502
				return
			}
			requestHeaders(req, r, s.Provider, s.Model.Protocol, rawSession)
			res, er := e.client(s.Provider).Do(req)
			if er != nil {
				runErr = er
				status = 502
				if r.Context().Err() != nil {
					status = 499
				}
				return
			}
			defer res.Body.Close()
			status = res.StatusCode
			upstreamStatus = res.StatusCode
			if status < 200 || status >= 300 {
				io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
				runErr = fmt.Errorf("upstream status %d", status)
				// 被拒的响应往往才带着 Retry-After 和剩余额度，这里同样要留存
				e.recordLimits(s.Model.ID, res.Header)
				if retryable(status) {
					e.cooldown(s.Model.ID, res.Header.Get("Retry-After"))
				} else if exhausted(status) {
					e.cooldownFor(s.Model.ID, exhaustedCooldown)
				}
				return
			}
			e.recordLimits(s.Model.ID, res.Header)
			for _, h := range []string{"retry-after", "anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-tokens-remaining", "x-ratelimit-remaining-requests", "x-ratelimit-remaining-tokens"} {
				if v := res.Header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
			if boolean(o, "stream") {
				if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
					status = 502
					runErr = errors.New("expected SSE content type")
					return
				}
				if s.Cross && s.Model.DropReasoning {
					// 流式没法等到发现推理内容再补响应头，所以开关开启时先声明：
					// 本次跨协议响应会丢弃推理内容（如果上游返回了的话）
					w.Header().Set("X-Prism-Dropped", "reasoning")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache, no-transform")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(200)
				if s.Cross {
					runErr = convertedStream(w, res.Body, s.Model.Protocol, p, s.Model.ID, id, &u, s.Model.DropReasoning)
				} else {
					runErr = nativeStream(w, res.Body, p, &u)
				}
				if runErr != nil {
					status = 502
					if r.Context().Err() != nil {
						status = 499
					} else {
						streamError(w, p, id)
					}
				}
				return
			}
			data, er := io.ReadAll(io.LimitReader(res.Body, (16<<20)+1))
			if er != nil || len(data) > 16<<20 {
				status = 502
				runErr = errors.New("invalid or oversized upstream body")
				return
			}
			var ro Object
			if er = json.Unmarshal(data, &ro); er != nil || ro == nil {
				status = 502
				runErr = errors.New("invalid upstream JSON")
				return
			}
			if ro["error"] != nil {
				status = 502
				runErr = errors.New("upstream error object")
				return
			}
			extractUsage(obj(ro["usage"]), s.Model.Protocol, &u)
			if !hasVisibleOutput(ro, s.Model.Protocol) {
				// 上游 200 了却没给出任何可用内容。静默把空回答转出去，调用方只会
				// 以为模型答不上来；实际是这个候选本次不可用，该换下一个。
				if truncatedStop(ro, s.Model.Protocol) {
					runErr = fail("EMPTY_OUTPUT_TRUNCATED", "上游在触及输出上限前没有产出正文（推理模型常见：输出预算被思考占满）", 502)
				} else {
					runErr = fail("EMPTY_OUTPUT", "上游返回了空内容", 502)
				}
				return
			}
			if s.Cross {
				co, er := decodeCompletion(ro, s.Model.Protocol)
				if er != nil && s.Model.DropReasoning && stripReasoning(ro, s.Model.Protocol) {
					// 管理员为这个模型显式接受了丢弃推理内容。剥离后重试，
					// 并在响应头标注——丢弃可以被接受，静默不行。
					if co, er = decodeCompletion(ro, s.Model.Protocol); er == nil {
						w.Header().Set("X-Prism-Dropped", "reasoning")
					}
				}
				if er != nil {
					// 上游是通的，也确实返回了内容；问题出在响应里有跨协议表达不了的
					// 部分（典型是带签名的推理）。必须如实说明，否则调用方只会去
					// 排查根本没出问题的上游。
					status = 502
					runErr = fail("UPSTREAM_INCOMPATIBLE",
						"上游响应包含无法跨协议转换的内容（"+er.Error()+"）；请对该模型改用其原生协议调用",
						502)
					return
				}
				data = []byte(raw(completionObject(co, p, s.Model.ID, id)))
			}
			w.Header().Set("Content-Type", "application/json")
			_, runErr = w.Write(data)
			if runErr != nil {
				status = 499
			}
		}()
		if runErr == nil {
			return
		}
		if boolean(o, "stream") && w.Header().Get("Content-Type") == "text/event-stream" {
			return
		}
		if retryable(status) && safeFallback {
			lastErr = fail("UPSTREAM_RATE_LIMIT", "上游拒绝请求，已尝试可用候选", 429)
			continue
		}
		if exhausted(status) && safeFallback {
			lastErr = fail("UPSTREAM_EXHAUSTED", "上游额度或权限不可用，已尝试可用候选", 402)
			continue
		}
		if gatewayFailure(upstreamStatus) && safeFallback {
			lastErr = fail("UPSTREAM_GATEWAY_ERROR", "上游网关错误，请求未到达模型，已尝试可用候选", 502)
			continue
		}
		if emptyOutput(runErr) && safeFallback {
			// 换一个候选才有意义：同一个模型再来一次大概率还是同样的结果
			lastErr = fail("EMPTY_OUTPUT", "候选模型没有返回任何可用内容，已尝试可用候选", 502)
			continue
		}
		if status == 499 {
			return
		}
		outStatus := status
		if outStatus < 400 || outStatus >= 500 {
			outStatus = 502
		}
		// 网关自己构造的错误带着准确的原因，不要被笼统的「上游请求失败」盖掉。
		// 上游返回的错误正文仍然不转发。
		var detailed *APIError
		if errors.As(runErr, &detailed) {
			protocolError(w, p, detailed.Status, detailed.Code, detailed.Message, id)
			return
		}
		protocolError(w, p, outStatus, "UPSTREAM_ERROR", fmt.Sprintf("上游请求失败（HTTP %d）；请求未被自动重放。", status), id)
		return
	}
	status, code, msg := errorParts(lastErr)
	protocolError(w, p, status, code, msg, id)
}
func demoCompletion(o Object, p string) Completion {
	input := estimateInput(o)
	text := "这是 Prism Gateway 的本地演示响应，不是真实 AI 推理，也不会调用任何云端服务。\n\n连接、鉴权、协议适配、路由选择和请求统计链路均已运行。添加真实 Provider 与 API Key 后即可处理实际代码任务。"
	if p == "responses" {
		text += "\n\n当前入口：OpenAI Responses。"
	} else if p == "messages" {
		text += "\n\n当前入口：Anthropic Messages。"
	} else {
		text += "\n\n当前入口：OpenAI Chat Completions。"
	}
	return Completion{Blocks: []Block{{Kind: "text", Text: text}}, Usage: Usage{Input: input, Output: int64(len([]rune(text))), Known: true}, Stop: "stop"}
}
func (e *Engine) CountTokens(w http.ResponseWriter, r *http.Request, key Principal) {
	id := randomID("req_")
	var o Object
	r.Body = http.MaxBytesReader(w, r.Body, int64(e.store.Config().Settings.MaxBodyMB)<<20)
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		protocolError(w, "messages", 400, "INVALID_JSON", "JSON 无效", id)
		return
	}
	if !key.allows(str(o, "model")) {
		protocolError(w, "messages", 403, "MODEL_FORBIDDEN", "模型未授权", id)
		return
	}
	c := e.store.Config()
	cp := Object{}
	for k, v := range o {
		cp[k] = v
	}
	cp["max_tokens"] = 1
	sel, _, err := e.selections(c, cp, "messages", "")
	if err != nil || len(sel) == 0 {
		protocolError(w, "messages", 400, "NO_COMPATIBLE_MODEL", "没有兼容模型", id)
		return
	}
	s := sel[0]
	if s.Model.NativeCount && s.Model.Protocol == "messages" && s.Provider.Kind != "mock" {
		body := Object{}
		for k, v := range o {
			body[k] = v
		}
		body["model"] = s.Model.Upstream
		req, _ := http.NewRequestWithContext(r.Context(), "POST", strings.TrimRight(s.Provider.BaseURL, "/")+"/messages/count_tokens", bytes.NewBufferString(raw(body)))
		requestHeaders(req, r, s.Provider, "messages", "")
		res, err := e.client(s.Provider).Do(req)
		if err != nil {
			protocolError(w, "messages", 502, "UPSTREAM_ERROR", "计数上游连接失败", id)
			return
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err == nil && res.StatusCode == 200 && json.Valid(data) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Prism-Token-Count-Mode", "provider")
			w.Write(data)
			return
		}
		protocolError(w, "messages", 502, "UPSTREAM_ERROR", "上游计数失败；没有静默替换为估算值", id)
		return
	}
	if !c.Settings.AllowEstimatedCount {
		protocolError(w, "messages", 501, "TOKEN_COUNT_UNSUPPORTED", "此模型没有原生计数接口；本地估算已关闭", id)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Prism-Token-Count-Mode", "estimated")
	json.NewEncoder(w).Encode(Object{"input_tokens": estimateInput(o)})
}
func (e *Engine) Authenticate(r *http.Request) (Principal, error) {
	token := bearer(r)
	if token == "" {
		token = r.Header.Get("x-api-key")
	}
	if len(token) < 20 {
		return Principal{}, fail("UNAUTHORIZED", "需要网关 API Key，不是上游 Key", 401)
	}
	rows, err := e.store.DB.Query("SELECT id,allowed FROM api_keys WHERE digest=? AND enabled=1", digest(token))
	if err != nil {
		return Principal{}, err
	}
	if len(rows) == 0 {
		return Principal{}, fail("UNAUTHORIZED", "网关 API Key 无效或已撤销", 401)
	}
	p := Principal{ID: rows[0].String("id")}
	if err = json.Unmarshal([]byte(rows[0].String("allowed")), &p.Allowed); err != nil {
		return Principal{}, err
	}
	if err := e.store.DB.Exec("UPDATE api_keys SET last_used=? WHERE id=?", now(), p.ID); err != nil {
		slog.Error("api key last_used update failed", "key_id", p.ID, "err", err)
	}
	return p, nil
}
func (e *Engine) Models(w http.ResponseWriter, r *http.Request, p string, key Principal) {
	c := e.store.Config()
	ids := []string{}
	for _, m := range c.Models {
		// System One 模型不是对话模型，列在这里只会让客户端拿去发对话请求，
		// 然后收到一个本可以避免的 NO_COMPATIBLE_MODEL。
		if m.Protocol == "systemone" {
			continue
		}
		pr, _ := c.provider(m.ProviderID)
		if m.Enabled && pr.Enabled && key.allows(m.ID) {
			ids = append(ids, m.ID)
		}
	}
	for _, rt := range c.Routes {
		if rt.Enabled && key.allows(rt.ID) {
			ids = append(ids, rt.ID)
		}
	}
	for _, a := range c.Aliases {
		if a.Enabled && key.allows(a.ID) {
			ids = append(ids, a.ID)
		}
	}
	sort.Strings(ids)
	prefix := "/openai/v1/models"
	if p == "messages" {
		prefix = "/anthropic/v1/models"
	}
	specific := strings.TrimPrefix(r.URL.Path, prefix+"/")
	isSpecific := r.URL.Path != prefix
	list := []any{}
	for _, id := range ids {
		if isSpecific && specific != id {
			continue
		}
		if p == "messages" {
			list = append(list, Object{"id": id, "type": "model", "display_name": id, "created_at": "2026-09-19T00:00:00Z"})
		} else {
			list = append(list, Object{"id": id, "object": "model", "created": 0, "owned_by": "prism-gateway"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if isSpecific {
		if len(list) == 0 {
			protocolError(w, p, 404, "MODEL_NOT_FOUND", "模型不存在或未授权", randomID("req_"))
			return
		}
		json.NewEncoder(w).Encode(list[0])
		return
	}
	if p == "messages" {
		var first, last any
		if len(list) > 0 {
			first = obj(list[0])["id"]
			last = obj(list[len(list)-1])["id"]
		}
		json.NewEncoder(w).Encode(Object{"data": list, "has_more": false, "first_id": first, "last_id": last})
	} else {
		json.NewEncoder(w).Encode(Object{"object": "list", "data": list})
	}
}

// Only metadata is returned; prompts, generated text, and credentials are not stored.
// Prune 按保留策略清理过期数据。启动时先跑一次：只有 ticker 的话，
// 运行不满一个周期就重启的实例永远不会清理，积压会一直留着。
func (e *Engine) Prune(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	e.pruneOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.pruneOnce()
		}
	}
}

func (e *Engine) pruneOnce() {
	s := e.store.Config().Settings
	for _, job := range []struct {
		name, query string
		cutoff      int64
	}{
		{"requests", "DELETE FROM requests WHERE started_at<?", now() - int64(s.RetentionDays)*86400000},
		{"admin_sessions", "DELETE FROM admin_sessions WHERE expires_at<?", now()},
		{"sessions", "DELETE FROM sessions WHERE updated_at<?", now() - int64(s.SessionTTLHours)*3600000},
	} {
		if err := e.store.DB.Exec(job.query, job.cutoff); err != nil {
			slog.Error("retention cleanup failed", "table", job.name, "err", err)
		}
	}
}

// emptyOutput 判断这次失败是不是「上游通了但没给出正文」。
// 只在非流式路径产生：流式一旦开始写就不能回退换候选了。
func emptyOutput(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Code == "EMPTY_OUTPUT" || ae.Code == "EMPTY_OUTPUT_TRUNCATED")
}

// recordLimits 留存上游这次声明的限额，并在它明说额度已经归零时
// 直接冷却到重置时刻——不必再拿下一个请求去撞一次 429。
func (e *Engine) recordLimits(model string, h http.Header) {
	raw := limitHeaders(h)
	if len(raw) == 0 {
		return
	}
	e.mu.Lock()
	st := e.state(model)
	st.Limits = raw
	st.LimitsAt = now()
	if until := exhaustedUntil(raw); until > st.Cooldown {
		st.Cooldown = min(until, now()+int64(24*time.Hour/time.Millisecond))
	}
	e.mu.Unlock()
}
