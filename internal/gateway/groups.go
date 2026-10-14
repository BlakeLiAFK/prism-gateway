package gateway

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// 路由由若干「组」按顺序组成：请求先在第 1 组按该组的策略选候选，第 1 组全部不可用或都接不了
// 这次请求时才轮到第 2 组，依此类推。每组有自己的策略与会话亲和。

const (
	maxGroups = 5
	// groupPenalty 让后一组的所有候选排在前一组之后：比任何策略分、亲和分与不健康扣分的组合都大
	groupPenalty = 1e9
	// defaultAffinity 是路由与组都没设置亲和时的兜底，与新建路由「主」组一直以来的默认一致
	defaultAffinity = true
)

type Group struct {
	Name     string `json:"name"`
	Strategy string `json:"strategy"`
	// Affinity 由客户端提交时是三态：nil = 继承路由的 AffinityDefault，true / false = 本组显式设置。
	// normalize 会把它解析成确定值并写回非 nil，此后运行时（读绑定 / 亲和加分 / 写绑定）
	// 直接读它，不再重复做默认值判断。已解析的配置序列化后始终输出明确 true / false。
	Affinity   *bool       `json:"affinity"`
	Candidates []Candidate `json:"candidates"`
}

// all 把各组候选按组序展平：统计、监控、免费模型等只关心「路由用到了哪些模型」的地方使用
func (r Route) all() []Candidate {
	var out []Candidate
	for _, g := range r.Groups {
		out = append(out, g.Candidates...)
	}
	return out
}

// primaryStrategy 是第 1 组的策略，监控与实时面板只有一行位置展示它
func (r Route) primaryStrategy() string {
	if len(r.Groups) == 0 {
		return ""
	}
	return r.Groups[0].Strategy
}

func boolPtr(v bool) *bool { return &v }

func cloneGroups(gs []Group) []Group {
	if gs == nil {
		return nil
	}
	out := make([]Group, len(gs))
	for i, g := range gs {
		out[i] = g
		if g.Affinity != nil {
			out[i].Affinity = boolPtr(*g.Affinity)
		}
		out[i].Candidates = slices.Clone(g.Candidates)
		if out[i].Candidates == nil {
			out[i].Candidates = []Candidate{}
		}
	}
	return out
}

// normalize 把旧版单组字段（以及 1.36.0 的 fallback 标记）转成 groups，并把亲和解析成确定值。
// load、Store.Change（含 config.import）、route.save 都走这里，保证内存里的路由只有一种形态。
func (r *Route) normalize() error {
	affinity := defaultAffinity
	if r.AffinityDefault != nil {
		affinity = *r.AffinityDefault
	}
	// 没有 groups 且没给 AffinityDefault，说明这是旧形态的路由：那时顶层 affinity 字段一直存在，
	// 布尔零值 false 同样是有意义的「显式关闭」，没有「未设置」这一态，所以取旧字段的原值
	if len(r.Groups) == 0 && r.AffinityDefault == nil {
		affinity = r.Affinity
	}
	if len(r.Groups) > 0 && (r.Strategy != "" || r.Affinity || len(r.Candidates) > 0) {
		return errors.New("路由不能同时带 groups 与旧版的 strategy / affinity / candidates")
	}
	if len(r.Groups) == 0 {
		primary, fallback := splitFallback(r.Candidates)
		r.Groups = []Group{{Name: "主", Strategy: cmp.Or(r.Strategy, "priority"), Affinity: boolPtr(affinity), Candidates: primary}}
		if len(fallback) > 0 {
			r.Groups = append(r.Groups, Group{Name: "兜底", Strategy: "priority", Affinity: boolPtr(true), Candidates: fallback})
		}
	}
	// 旧字段已经被读进第 1 组，之后不再输出
	r.Strategy, r.Affinity, r.Candidates = "", false, nil
	// 存确定的默认值：组保留 nil（继承）时，这里就是它们解析出的值
	r.AffinityDefault = boolPtr(affinity)
	for i := range r.Groups {
		g := &r.Groups[i]
		g.Name = cmp.Or(strings.TrimSpace(g.Name), fmt.Sprintf("第 %d 组", i+1))
		// 组不写回解析值：nil 表示「继承路由默认值」，把这个状态持久化下来，
		// 之后拨动路由开关才能一次影响所有没单独设置的组（groupAffinity 在运行时解析）
		if g.Candidates == nil {
			g.Candidates = []Candidate{}
		}
		for _, c := range g.Candidates {
			if c.Fallback {
				return errors.New("fallback 标记已被分组取代，请把兜底候选放进后面的组")
			}
		}
	}
	return nil
}

func splitFallback(cs []Candidate) (primary, fallback []Candidate) {
	primary, fallback = []Candidate{}, []Candidate{}
	for _, c := range cs {
		if c.Fallback {
			c.Fallback = false
			fallback = append(fallback, c)
		} else {
			primary = append(primary, c)
		}
	}
	return
}

func (r Route) validateGroups(c Config) error {
	if len(r.Groups) < 1 || len(r.Groups) > maxGroups {
		return fmt.Errorf("路由的组数必须介于 1 和 %d", maxGroups)
	}
	seen := map[string]bool{}
	for _, g := range r.Groups {
		if !routeStrategies[g.Strategy] {
			return errors.New("路由策略仅支持 priority / balanced / weighted / latency / cost / least_busy")
		}
		if len([]rune(g.Name)) > 40 {
			return errors.New("组名最长 40 个字符")
		}
		for _, x := range g.Candidates {
			if _, ok := c.model(x.ModelID); !ok || seen[x.ModelID] {
				return errors.New("路由模型不存在，或同一模型出现在多个组 / 重复")
			}
			seen[x.ModelID] = true
			if x.Weight < 1 || x.Weight > 1000 {
				return errors.New("路由权重必须介于 1 和 1000")
			}
		}
	}
	return nil
}

// splitLegacy 把 route.save 里的旧版单组字段从补丁中拆出来。请求同时带 groups 时不拆：
// 交给 normalize 判成 400，而不是悄悄选一边。
func splitLegacy(patch Object) (rest, legacy Object) {
	rest, legacy = Object{}, Object{}
	_, hasGroups := patch["groups"]
	for k, v := range patch {
		switch k {
		case "strategy", "affinity", "candidates":
			if !hasGroups {
				legacy[k] = v
				continue
			}
		}
		rest[k] = v
	}
	return
}

// applyLegacy 把旧客户端（旧脚本、暂停开关、curl）发来的单组字段落到路由的第 1 组上。
// 路由已经有多组时拒绝：旧字段没法表达「改哪一组」，吞掉它就是静默丢弃写入。
func (r *Route) applyLegacy(legacy Object) error {
	if len(legacy) == 0 {
		return nil
	}
	if len(r.Groups) > 1 {
		return fail("INVALID_PARAMS", "此路由已分组，请用 groups 修改，不能再用 strategy / affinity / candidates", 400)
	}
	var in struct {
		Strategy   *string      `json:"strategy"`
		Affinity   *bool        `json:"affinity"`
		Candidates *[]Candidate `json:"candidates"`
	}
	if er := decode(legacy, &in); er != nil {
		return fail("INVALID_PARAMS", er.Error(), 400)
	}
	if len(r.Groups) == 0 {
		r.Groups = []Group{{Name: "主", Strategy: "priority", Affinity: boolPtr(defaultAffinity), Candidates: []Candidate{}}}
	}
	g := &r.Groups[0]
	if in.Strategy != nil {
		g.Strategy = *in.Strategy
	}
	if in.Affinity != nil {
		// 旧字段落到第 1 组的同时也定下路由默认值：旧客户端不会分「路由默认」与「本组设置」，
		// 只有这样，只发 affinity 的旧调用（含本组的暂停开关）才会在 normalize 里保持原意
		g.Affinity = boolPtr(*in.Affinity)
		r.AffinityDefault = boolPtr(*in.Affinity)
	}
	if in.Candidates != nil {
		primary, fallback := splitFallback(*in.Candidates)
		g.Candidates = primary
		if len(fallback) > 0 {
			r.Groups = append(r.Groups[:1], Group{Name: "兜底", Strategy: "priority", Affinity: boolPtr(true), Candidates: fallback})
		}
	}
	return nil
}

// sessionKey 是会话在第 n 组的亲和键。第 1 组沿用原来的键，升级前的会话绑定不受影响
func sessionKey(session string, group int) string {
	if group == 0 {
		return session
	}
	return fmt.Sprintf("%s:g%d", session, group)
}

// groupPlan 是一次请求里某一组的候选与该组读到的会话绑定。
// Affinity 是 normalize 解析后的确定值，一路透传到具体模型，下游不再做默认值判断。
type groupPlan struct {
	Name       string
	Strategy   string
	Affinity   bool
	Candidates []Candidate
	Pinned     string
}

// expandGroups 把请求的模型名展开成按序的组：路由展开成它的各组并读出每组的会话绑定；
// 直接按模型 ID 调用是一个不记亲和的虚拟组。
func (e *Engine) expandGroups(c Config, resolved string, o Object, session string) (plans []groupPlan, route Route, routeOK bool, err error) {
	route, routeOK = c.route(resolved)
	if routeOK {
		if !route.Enabled {
			return nil, route, true, fail("MODEL_DISABLED", "路由已禁用", 404)
		}
		if o["previous_response_id"] != nil || o["conversation"] != nil {
			return nil, route, true, unsupported("有服务端会话状态的请求必须指定固定模型，不能使用自动路由")
		}
		for _, g := range route.Groups {
			plans = append(plans, groupPlan{g.Name, g.Strategy, route.affinity(g), g.Candidates, ""})
		}
		e.readPins(c, plans, session)
		return plans, route, true, nil
	}
	if _, ok := c.model(resolved); ok {
		return []groupPlan{{Strategy: "priority", Candidates: []Candidate{{ModelID: resolved, Weight: 1}}}}, route, false, nil
	}
	return nil, route, false, fail("MODEL_NOT_FOUND", "模型或路由不存在", 404)
}

// affinity 取本组解析后的亲和值：组显式设置就用它，nil（继承）时用路由的默认值。
// 这是全链路唯一的默认值判断点，解析后的布尔值一路透传到具体模型，下游直接读。
func (r Route) affinity(g Group) bool {
	if g.Affinity != nil {
		return *g.Affinity
	}
	if r.AffinityDefault != nil {
		return *r.AffinityDefault
	}
	return defaultAffinity
}

// readPins 一次查询读出所有开了亲和的组的会话绑定
func (e *Engine) readPins(c Config, plans []groupPlan, session string) {
	if session == "" {
		return
	}
	keys, args := map[string]int{}, []any{}
	for i, p := range plans {
		if p.Affinity {
			k := sessionKey(session, i)
			keys[k] = i
			args = append(args, k)
		}
	}
	if len(args) == 0 {
		return
	}
	q := "SELECT id, model_id FROM sessions WHERE updated_at>? AND id IN (?" + strings.Repeat(",?", len(args)-1) + ")"
	rows, er := e.store.DB.Query(q, append([]any{now() - int64(c.Settings.SessionTTLHours)*3600000}, args...)...)
	if er != nil {
		slog.Warn("session affinity lookup failed", "err", er)
		return
	}
	for _, r := range rows {
		plans[keys[r.String("id")]].Pinned = r.String("model_id")
	}
}
