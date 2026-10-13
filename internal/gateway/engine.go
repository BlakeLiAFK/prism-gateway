package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"
)

type Principal struct {
	ID      string
	Allowed []string
	Policy  keyPolicy
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
	CoolWhy    string // 这次冷却的原因，准入被拒时写进给客户端的错误信息
	LastStatus int
	LatencyMS  int64
	TTFB       float64 // 上游 2xx 响应头耗时的滑动平均，latency 策略的依据
	Limits     Object  // 上游最近一次声明的限额，原样留存
	LimitsAt   int64
	Rejects    []reject // 近 5 分钟的准入拒绝
	Fails      int      // 连续失败次数，成功即清零
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
	keysAt   int64  // 快照载入时刻，0 表示需要重载
	keyGen   uint64 // forgetKeys 每次加一，用来丢弃吊销前读到的查库结果
	loadMu   sync.Mutex
	// onKeyLoad 仅供测试：快照重载读库前调用
	onKeyLoad func()
	keyHits   map[string][]int64 // 每把 Key 近 60 秒的请求时刻，用于 Key 级 RPM
	streamed  map[string]*streamWindow
	spend     map[string]keySpendEntry
	// budgetLocks 按模型串行「预算检查 + 预留写入」，值是 *sync.Mutex
	budgetLocks sync.Map
	// OnRateLimited 在上游返回 429 后调用，供 App 去查额度窗口
	OnRateLimited func(Provider)
	// OnAlert 推送告警（事件类别、限频键、正文），未设置时忽略
	OnAlert func(event, key, text string)
	// watch 是终端监控的事件中心，引擎各处发生的事都经它广播
	watch *watchHub
}

func NewEngine(s *Store) *Engine {
	return &Engine{store: s, states: map[string]*health{}, clients: map[string]*http.Client{}, keys: map[string]*cachedKey{}, keyHits: map[string][]int64{}, spend: map[string]keySpendEntry{}, watch: newWatchHub()}
}
func (e *Engine) client(p Provider) *http.Client {
	e.clientMu.Lock()
	defer e.clientMu.Unlock()
	idle := e.store.Config().Settings.StreamIdle()
	key := p.ID + "|" + p.BaseURL + fmt.Sprint(p.TimeoutSec, p.AllowPrivate, idle)
	if c := e.clients[key]; c != nil {
		return c
	}
	c := clientFor(p, idle)
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
	Pinned            string
	ToolImagesAdapted bool
	// Dropped 列出发往上游的副本里没有送达的请求历史内容（reasoning、documents），响应必须披露
	Dropped []string
	// Group 是候选所在的组序（从 0 起），亲和键与兜底告警按它区分；RouteID 与 GroupName 仅路由请求才有
	Group     int
	GroupName string
	RouteID   string
}

func (e *Engine) selections(c Config, o Object, p, session string) ([]selection, []any, error) {
	resolved, _ := c.resolveAlias(str(o, "model"))
	plans, route, routeOK, err := e.expandGroups(c, resolved, o, session)
	if err != nil {
		return nil, nil, err
	}
	out := []selection{}
	reasons := []any{}
	for gi, g := range plans {
		for i, cm := range g.Candidates {
			m, ok := c.model(cm.ModelID)
			if !ok {
				continue
			}
			if m.supportsProtocol(p) {
				m.Protocol = p
			}
			pr, ok := c.provider(m.ProviderID)
			if base := pr.ProtocolURLs[m.Protocol]; base != "" {
				pr.BaseURL = base
			}
			why := ""
			if m.Protocols != nil && len(m.Protocols) == 0 {
				why = "原生协议尚未确认，请同步上游声明或在模型设置中指定"
			}
			if boolean(obj(o["provider"]), "zdr") && !isOpenRouter(pr) {
				why = "provider.zdr 只能交给 OpenRouter 执行，不能切换到其他供应商"
			}
			if len(arr(o["tools"])) > 0 && !m.Tools && !m.ToolsUnknown {
				why = "tools_unsupported"
			}
			// state 是调用方自定义的 JSON，可能恰好含 image_url 这类键；
			// System One 只吃文本，不该被图像检测误伤。
			if m.Protocol != "systemone" && requestHasImage(o, p) && !m.Vision && !m.VisionUnknown {
				why = "vision_unsupported"
			}
			// 停用与暂停是管理员自己的操作，必须排在最后覆盖前面所有原因：
			// recordIncompatible 靠这两个原因判断「不必告警」，被请求内容相关的原因盖掉就会误报；
			// 两者并存时报暂停，因为它是路由里更具体的一层
			if !ok || !m.Enabled || !pr.Enabled {
				why = "disabled"
			}
			// 暂停的候选直接淘汰，不进入 out：亲和加分与 Pinned 等待都以「仍是合格候选」为前提，随之失效
			if cm.Disabled {
				why = "candidate_paused"
			}
			var body Object
			cross := p != m.Protocol
			ignored := []string{}
			toolImagesAdapted := false
			dropped := []string{}
			// System One 返回类型化决策，对话协议返回消息序列，两边没有共同语义。
			// 允许转换只能靠编造文本或字符串化 JSON，都会静默改变调用方拿到的东西。
			if why == "" && cross && (p == "systemone" || m.Protocol == "systemone") {
				why = "System One 与对话协议之间没有等价语义，不能转换；请直接调用该协议的模型"
			}
			if why == "" && cross {
				src := o
				// 历史里的推理内容没有跨协议的等价表达，显式开启后只从这个候选的副本里去掉
				if m.DropReasoning {
					var did bool
					if src, did = dropHistoryReasoning(src, p); did {
						dropped = append(dropped, "reasoning")
					}
				}
				if m.DropDocuments {
					var did bool
					if src, did = dropHistoryDocuments(src, p); did {
						dropped = append(dropped, "documents")
					}
				}
				// 仅显式允许的推理输出偏好可忽略；禁用思考或新增控制不能改成目标默认值。
				if m.DropReasoning && canDropThinking(o["thinking"]) {
					base := src
					src = Object{}
					for k, v := range base {
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
					toolImagesAdapted = m.Protocol == "chat" && hasToolResultImages(ir)
					body, err = encodeCanonical(ir, m.Protocol, m.Upstream)
					if err != nil {
						why = err.Error()
					}
				}
			} else if why == "" {
				// 原生透传要改 model、输出上限和思考预算，所以每个候选各持一份副本；
				// 被淘汰的候选和跨协议候选（body 由 encodeCanonical 生成）都不需要
				body = cloneJSON(o).(Object)
				body["model"] = m.Upstream
			}
			if why == "" {
				ignored = append(ignored, normalizeProviderContext(pr, m.Protocol, body)...)
				why = providerRequestError(pr, m.Protocol, body)
			}
			if why == "" && m.Protocol != "systemone" {
				field := "max_tokens"
				if m.Protocol == "responses" {
					field = "max_output_tokens"
				}
				if m.Protocol == "chat" && body["max_completion_tokens"] != nil {
					field = "max_completion_tokens"
				}
				// 客户端没写输出上限时交给上游默认（GLM 65536、DeepSeek 8K/64K 等，都比网关自定的大）；
				// 只有 Messages 协议必填，补模型上限，与 LiteLLM、Vercel AI SDK、Claude Code 的做法一致
				if body[field] == nil && m.Protocol == "messages" {
					body[field] = m.MaxOutput
				}
				// 超过模型上限时降到上限，而不是淘汰候选：Claude Code 固定要 64000，配置偏小的模型不该因此整个不可用
				if body[field] != nil && num(body, field) > float64(m.MaxOutput) {
					body[field] = m.MaxOutput
					why = fitThinking(body, m.MaxOutput)
				}
			}
			if why == "" {
				pressure, err := e.pressure(m)
				if err != nil {
					return nil, nil, err
				}
				l := e.load(m)
				priced := m
				if g.Strategy == "cost" && len(m.PriceTiers) > 0 {
					priced = modelPriceAt(m, estimateInput(body))
				}
				score := baseScore(g.Strategy, i, cm, priced, pressure, l)
				if g.Pinned == m.ID {
					score += affinityBonus
				}
				if !cross {
					score += nativeBonus(g.Strategy)
				}
				if l.Cooling || l.Full {
					score -= unhealthyPenalty
				}
				// 后一组整体排在前一组之后：前一组健康时轮不到它，前一组全部失败或淘汰时才接手
				score -= float64(gi) * groupPenalty
				sel := selection{Model: m, Provider: pr, Body: body, Score: score, Cross: cross, Ignored: ignored, ToolImagesAdapted: toolImagesAdapted, Dropped: dropped, Group: gi}
				sel.Reason = fmt.Sprintf("%s; native=%t; affinity=%t; pressure=%.3f; cooling=%t; full=%t; ttfb=%.0f", g.Strategy, !cross, g.Pinned == m.ID, pressure, l.Cooling, l.Full, l.TTFB)
				if routeOK {
					sel.RouteID, sel.GroupName = route.ID, g.Name
					if len(plans) > 1 {
						sel.Reason += "; group=" + g.Name
					}
				}
				out = append(out, sel)
			}
			reasons = append(reasons, Object{"model_id": m.ID, "eligible": why == "", "reason": why, "native": !cross, "group": g.Name})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	// 每组各自的会话绑定：只有绑定的模型仍是本组的合格候选时才生效
	for i := range out {
		pin := plans[out[i].Group].Pinned
		if pin != "" && slices.ContainsFunc(out, func(s selection) bool { return s.Group == out[i].Group && s.Model.ID == pin }) {
			out[i].Pinned = pin
		}
	}
	return out, reasons, nil
}

// failureCooldown 是上游 5xx、网络错误或流首事件即错误时给候选的短冷却。网关不替客户端重放，
// 但客户端（Claude Code、OpenAI SDK）会按自己的规则重试；冷却让那次重试落到别的候选上。
const failureCooldown = 30 * time.Second

func (e *Engine) cooldownFor(id string, d time.Duration) {
	e.cooldownBecause(id, d, "")
}

func (e *Engine) cooldownBecause(id string, d time.Duration, why string) {
	e.mu.Lock()
	h := e.state(id)
	h.Cooldown = now() + int64(min(d, 24*time.Hour)/time.Millisecond)
	h.CoolWhy = why
	e.mu.Unlock()
	e.noteCooldown(id, d)
}

// retryable 是「上游暂时忙，换个候选重试是安全的」这一类状态码。
// 529 是 TypeSafe 的过载码，语义与 503 相同。
func retryable(status int) bool {
	return status == 429 || status == 503 || status == 529
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
		{"jobs", "DELETE FROM jobs WHERE updated_at<? AND status NOT IN ('queued','running')", now() - 30*86400000},
		{"sessions", "DELETE FROM sessions WHERE updated_at<?", now() - int64(s.SessionTTLHours)*3600000},
		// 过期或撤销满一天的监控令牌没有保留价值
		{"watch_tokens", "DELETE FROM watch_tokens WHERE (CASE WHEN revoked_at>0 THEN revoked_at ELSE expires_at END)<?", now() - 86400000},
		// 汇总表很小，保留比请求明细长，历史用量不随明细一起清掉
		{"usage_hourly", "DELETE FROM usage_hourly WHERE hour<?", (now() - 400*86400000) / 3600000},
		{"cache_hourly", "DELETE FROM cache_hourly WHERE hour<?", (now() - 400*86400000) / 3600000},
	} {
		if err := e.store.DB.Exec(job.query, job.cutoff); err != nil {
			slog.Error("retention cleanup failed", "table", job.name, "err", err)
		}
	}
}
