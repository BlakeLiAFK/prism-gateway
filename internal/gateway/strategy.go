package gateway

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"
)

// 路由策略只决定候选的基础分（约 0~1000，越高越先尝试）。
// 资格筛选、会话亲和、健康降级在 selections 里统一叠加。
var routeStrategies = map[string]bool{"priority": true, "balanced": true, "weighted": true, "latency": true, "cost": true, "least_busy": true}

const (
	// 亲和加分要压过任何基础分，会话才能稳定留在同一模型上
	affinityBonus = 10000
	// 冷却中或已满的候选沉到最后：试了也是本地直接拒绝，只会多一次无效尝试。
	// 不剔除，全部不可用时仍按原逻辑逐个尝试并给出准确的拒绝原因。
	unhealthyPenalty = 20000
	// 响应头耗时的平滑系数，新样本占 30%
	ttfbAlpha = 0.3
)

// load 是某个模型此刻的运行时负载快照
type load struct {
	Cooling bool
	Full    bool
	Busy    float64 // 当前并发 / 并发上限
	TTFB    float64 // 上游响应头耗时的滑动平均，0 表示还没有样本
}

func (e *Engine) load(m Model) load {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.states[m.ID]
	if h == nil {
		return load{}
	}
	t := now()
	rpm := 0
	for _, ts := range h.Recent {
		if ts > t-60000 {
			rpm++
		}
	}
	return load{
		Cooling: h.Cooldown > t,
		Full:    h.Active >= m.Concurrency || (m.RPM > 0 && rpm >= m.RPM),
		Busy:    float64(h.Active) / float64(max(m.Concurrency, 1)),
		TTFB:    h.TTFB,
	}
}

// recordTTFB 记录上游 2xx 响应头到达的耗时。
// ponytail: 非流式请求的响应头要等整段生成完，会把长回答算慢；按流式/非流式分开统计时再拆。
func (e *Engine) recordTTFB(id string, ms int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.state(id)
	// 0 表示没有样本，亚毫秒的响应按 1ms 记，避免被当成未测过
	ms = max(ms, 1)
	if h.TTFB == 0 {
		h.TTFB = float64(ms)
		return
	}
	h.TTFB = h.TTFB*(1-ttfbAlpha) + float64(ms)*ttfbAlpha
}

// baseScore 计算策略分；i 是候选在路由中的位置，同分时按它保持稳定顺序
func baseScore(strategy string, i int, cm Candidate, m Model, pressure float64, l load) float64 {
	tie := float64(i) * 0.01
	switch strategy {
	case "balanced":
		return 1000 - pressure*500 + float64(cm.Weight)
	case "weighted":
		// Efraimidis-Spirakis 加权随机排列：key = u^(1/w)。
		// 首选概率正比于权重，失败后的备选顺序也按剩余权重抽取。
		return 1000 * math.Pow(rand.Float64(), 1/float64(max(cm.Weight, 1)))
	case "latency":
		// 没有样本的候选排最前，先试一次拿到数据
		return 1000 - min(l.TTFB, 99000)/100 - tie
	case "cost":
		// 未确认计价的排最后：价格未知不等于免费
		if !m.PricingSet {
			return -tie
		}
		return 1000 - min(m.InputPrice+m.OutputPrice, 999) - tie
	case "least_busy":
		return 1000 - l.Busy*500 - tie
	}
	return 1000 - float64(i)*10
}

// nativeBonus 让无需协议转换的候选略占优。老策略沿用 +2；
// 新策略只作同分裁决，否则会扭曲权重比例与价格排序。
func nativeBonus(strategy string) float64 {
	if strategy == "priority" || strategy == "balanced" {
		return 2
	}
	return 0.001
}

// hasBudget 模型是否设了任一本地滚动预算；都没设时不必查用量
func hasBudget(m Model) bool { return m.Limit5h > 0 || m.Limit7d > 0 || m.Limit30d > 0 }

// pressure 返回预算使用率最高的窗口占比，没设预算时为 0 且不查库
func (e *Engine) pressure(m Model) (float64, error) {
	if !hasBudget(m) {
		return 0, nil
	}
	q, err := e.quota(m.ID)
	if err != nil {
		return 0, err
	}
	p := 0.0
	for _, v := range []struct {
		key   string
		limit float64
	}{{"used_5h", m.Limit5h}, {"used_7d", m.Limit7d}, {"used_30d", m.Limit30d}} {
		if v.limit > 0 {
			p = max(p, num(q, v.key)/v.limit)
		}
	}
	return p, nil
}

// pinGrace 取上游提示词缓存的常见有效期：原模型停摆超过它，缓存已冷，改绑不再有损失
const pinGrace = int64(5 * time.Minute / time.Millisecond)

// keepPin 判断故障切换成功后是否保留原绑定：原模型仍是合格候选，且冷却剩余不超过 pinGrace。
// 调用方持有 e.mu。
func (e *Engine) keepPin(s selection) bool {
	return s.Pinned != "" && s.Pinned != s.Model.ID && e.state(s.Pinned).Cooldown-now() <= pinGrace
}

// fitThinking 在输出上限被降低后收紧 Anthropic 的思考预算：budget_tokens 必须小于 max_tokens 且不低于 1024，
// 放不下时返回淘汰原因
func fitThinking(body Object, maxOut int) string {
	t := obj(body["thinking"])
	if t == nil || num(t, "budget_tokens") < float64(maxOut) {
		return ""
	}
	if maxOut <= 1024 {
		return "模型输出上限放不下思考预算"
	}
	t["budget_tokens"] = maxOut - 1
	return ""
}

// requestedMaxTokens 取客户端声明的输出上限，没写时为 0
func requestedMaxTokens(o Object) int {
	n := int(num(o, "max_tokens"))
	if v := int(num(o, "max_completion_tokens")); v > 0 {
		n = v
	}
	if v := int(num(o, "max_output_tokens")); v > 0 {
		n = v
	}
	return n
}

// reserveCost 预留本次请求的最大花费。客户端没写输出上限时由上游决定，最多到模型上限，按上限预留
func reserveCost(m Model, o Object) int64 {
	n := min(requestedMaxTokens(o), m.MaxOutput)
	if n < 1 {
		n = m.MaxOutput
	}
	return cost(m, Usage{Input: estimateInput(o), Output: int64(n)})
}

// reject 是一次准入拒绝：并发满、冷却中、RPM 满或本地预算不足
type reject struct {
	At   int64
	Code string
}

// reject 记录一次准入拒绝并丢掉 5 分钟前的旧记录。调用方持有 e.mu
func (h *health) reject(err error, t int64) {
	var ae *APIError
	if !errors.As(err, &ae) {
		return
	}
	keep := h.Rejects[:0]
	for _, r := range h.Rejects {
		if r.At > t-5*60000 {
			keep = append(keep, r)
		}
	}
	h.Rejects = append(keep, reject{t, ae.Code})
}

// recordRejected 在所有候选都没能通过准入时补一条请求记录：客户端收到了 429，请求日志里却什么都没有，排查无从下手
func (e *Engine) recordRejected(s selection, key Principal, reqID, requested, p, session string, from client, err error) {
	_, code, _ := errorParts(err)
	id := randomID("att_")
	if er := e.store.DB.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,http_status,started_at,cost_nano,cost_known,reason,error_code,usage_mode,is_demo,client_ip,user_agent,agent_role) VALUES (?,?,?,?,?,?,?,?,?,'error',429,?,0,0,?,?,'rejected',?,?,?,?)`,
		id, reqID, key.ID, requested, s.Model.ID, s.Provider.ID, p, s.Model.Protocol, session, now(), "全部候选准入被拒："+s.Reason, code, s.Provider.Kind == "mock", from.IP, from.Agent, from.Role); er != nil {
		slog.Error("rejected request record failed", "request_id", reqID, "err", er)
		return
	}
	e.rollup(id)
}

// failAlertAfter：同一模型连续失败这么多次就告警一次（客户端主动取消不算）
const failAlertAfter = 5

// trackFailure 维护连续失败计数，达到阈值时告警。调用方持有 e.mu
func (e *Engine) trackFailure(h *health, m Model, state, code string) {
	if state == "success" {
		h.Fails = 0
		return
	}
	if code == "CLIENT_CANCELED" {
		return
	}
	if h.Fails++; h.Fails == failAlertAfter && e.OnAlert != nil {
		go e.OnAlert("failures", "fail:"+m.ID, fmt.Sprintf("%s 连续 %d 次请求失败，最近一次：%s。", cmp.Or(m.Name, m.ID), failAlertAfter, code))
	}
}
