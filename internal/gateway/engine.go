package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
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
	TTFB       float64 // 上游 2xx 响应头耗时的滑动平均，latency 策略的依据
	Limits     Object  // 上游最近一次声明的限额，原样留存
	LimitsAt   int64
}
type Engine struct {
	store    *Store
	mu       sync.Mutex
	states   map[string]*health
	global   int
	clientMu sync.Mutex
	clients  map[string]*http.Client
	keyMu    sync.Mutex
	keys     map[string]*cachedKey
	// OnRateLimited 在上游返回 429 后调用，供 App 去查额度窗口
	OnRateLimited func(Provider)
}

func NewEngine(s *Store) *Engine {
	return &Engine{store: s, states: map[string]*health{}, clients: map[string]*http.Client{}, keys: map[string]*cachedKey{}}
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
		m[k] = Object{"active": v.Active, "cooldown_until": v.Cooldown, "last_status": v.LastStatus, "latency_ms": v.LatencyMS, "ttfb_ms": math.Round(v.TTFB),
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
	// Pinned 是会话原绑定的模型，仅当它在本次请求中仍是合格候选时非空
	Pinned string
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
			pressure, err := e.pressure(m)
			if err != nil {
				return nil, nil, err
			}
			l := e.load(m)
			score := baseScore(route.Strategy, i, cm, m, pressure, l)
			if pinned == m.ID {
				score += affinityBonus
			}
			if !cross {
				score += nativeBonus(route.Strategy)
			}
			if l.Cooling || l.Full {
				score -= unhealthyPenalty
			}
			out = append(out, selection{m, pr, body, score, fmt.Sprintf("%s; native=%t; affinity=%t; pressure=%.3f; cooling=%t; full=%t; ttfb=%.0f", route.Strategy, !cross, pinned == m.ID, pressure, l.Cooling, l.Full, l.TTFB), cross, ignored, ""})
		}
		reasons = append(reasons, Object{"model_id": m.ID, "eligible": why == "", "reason": why, "native": !cross})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if slices.ContainsFunc(out, func(s selection) bool { return s.Model.ID == pinned }) {
		for i := range out {
			out[i].Pinned = pinned
		}
	}
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
	reserve := reserveCost(s.Model, s.Body)
	if hasBudget(s.Model) {
		q, err := e.quota(s.Model.ID)
		if err != nil {
			return "", err
		}
		for _, v := range []struct {
			k string
			n float64
		}{{"used_5h", s.Model.Limit5h}, {"used_7d", s.Model.Limit7d}, {"used_30d", s.Model.Limit30d}} {
			if v.n > 0 && nano(num(q, v.k))+reserve > nano(v.n) {
				return "", fail("LOCAL_QUOTA_LIMIT", "本地滚动预算不足（包含本次预留）；不是上游官方余额", 429)
			}
		}
	}
	id := randomID("att_")
	err := e.store.DB.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,cost_nano,cost_known,reason,is_demo,client_ip,user_agent,agent_role) VALUES (?,?,?,?,?,?,?,?,?,'running',?,?,?,?,?,?,?,?)`, id, reqID, key.ID, requested, s.Model.ID, s.Provider.ID, p, s.Model.Protocol, session, t, reserve, s.Model.PricingSet, s.Reason, s.Provider.Kind == "mock", from.IP, from.Agent, from.Role)
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
		q := `INSERT INTO sessions VALUES (?,?,?,?,?,1) ON CONFLICT(id) DO UPDATE SET model_id=excluded.model_id,provider_id=excluded.provider_id,updated_at=excluded.updated_at,requests=sessions.requests+1`
		if e.keepPin(s) {
			// 原模型只是临时故障：保留绑定、只续期，冷却结束后回到原模型
			q = `INSERT INTO sessions VALUES (?,?,?,?,?,1) ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at,requests=sessions.requests+1`
		}
		if e := e.store.DB.Exec(q, session, keyID, s.Model.ID, s.Provider.ID, now()); e != nil {
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
