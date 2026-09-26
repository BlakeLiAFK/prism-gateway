package gateway

// 路由与候选的实时面板：把引擎内存里的运行时状态和 requests/sessions 表里的
// 近窗统计合成一份，管理界面不必连着跑四五个接口才能画出一行路由。

import (
	"math"
	"sync"
)

func init() {
	consoleActions["route.live"] = func(a *App, _ string, _ Object) (any, error) { return a.routeLive() }
}

// liveTTL：面板是持续刷新的，五秒一次的缓存已经能省掉大部分聚合查询，数字又不会看着卡住
const liveTTL = 5000

type liveEntry struct {
	at  int64
	val Object
}

var (
	liveMu    sync.Mutex
	liveCache = map[*App]liveEntry{}
)

// routeLive 返回 {now, global, models, routes}，按 App 缓存五秒。
// 缓存是包级的：App 上没有可用字段，而一个进程只有一个 App，
// 测试里每个 harness 又是新实例，按指针区分就不会互相命中。
func (a *App) routeLive() (any, error) {
	liveMu.Lock()
	defer liveMu.Unlock()
	t := now()
	if c, ok := liveCache[a]; ok && t-c.at < liveTTL {
		return c.val, nil
	}
	for k, c := range liveCache {
		if t-c.at >= liveTTL {
			delete(liveCache, k)
		}
	}
	v, err := a.computeRouteLive(t)
	if err != nil {
		return nil, err
	}
	liveCache[a] = liveEntry{t, v}
	return v, nil
}

// liveState 是某个模型此刻的内存快照。锁内复制出来，之后不再碰 Engine
type liveState struct {
	active   int
	rpm      int
	cooldown int64
	ttfb     int64
	limits   Object
	rejects  map[string]int
	stream   float64
}

// liveAgg 是 requests 表按模型或按请求名汇总出来的近窗计数
type liveAgg struct {
	requests, success, rpm int64
	tokens, dur            int64
}

// snapshot 一次加锁取走全局并发与给定模型的运行时状态。
// rpm 与引擎自己的限流判定同口径：Recent 里近 60 秒的条数。
func (e *Engine) snapshot(ids []string, t int64) (int, map[string]liveState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]liveState, len(ids))
	for _, id := range ids {
		s := liveState{}
		if h := e.states[id]; h != nil {
			s.active, s.cooldown, s.limits = h.Active, h.Cooldown, h.Limits
			s.ttfb = int64(math.Round(h.TTFB))
			s.stream = e.streamedTokS(id, t)
			s.rejects = map[string]int{}
			for _, r := range h.Rejects {
				if r.At > t-5*60000 {
					s.rejects[r.Code]++
				}
			}
			for _, ts := range h.Recent {
				if ts > t-60000 {
					s.rpm++
				}
			}
		}
		out[id] = s
	}
	return e.global, out
}

func (a *App) computeRouteLive(t int64) (Object, error) {
	c := a.Store.Config()
	// 只列被路由引用过的模型：孤立的模型没有候选关系，在面板上没有位置
	used, seen := []string{}, map[string]bool{}
	for _, rt := range c.Routes {
		for _, cm := range rt.Candidates {
			if _, ok := c.model(cm.ModelID); ok && !seen[cm.ModelID] {
				seen[cm.ModelID] = true
				used = append(used, cm.ModelID)
			}
		}
	}
	// 三条 GROUP BY 覆盖全部数据。5 分钟窗口打底，60 秒的那几列用 CASE 在同一次
	// 扫描里顺带算出来，不必按模型逐个查，也不必为了两个窗口扫两遍。
	mRows, err := a.Store.DB.Query(`SELECT model_id,COUNT(*) requests,
		COALESCE(SUM(status='success'),0) success,
		COALESCE(SUM(CASE WHEN status='success' THEN output_tokens ELSE 0 END),0) tokens,
		COALESCE(SUM(CASE WHEN status='success' THEN duration_ms ELSE 0 END),0) dur,
		COALESCE(SUM(CASE WHEN started_at>=? THEN 1 ELSE 0 END),0) rpm
		FROM requests WHERE started_at>=? GROUP BY model_id`, t-60000, t-5*60000)
	if err != nil {
		return nil, err
	}
	// 路由侧按客户端请求名（requested_model）归属，别名解析到路由的也算在请求名下
	rRows, err := a.Store.DB.Query(`SELECT requested_model,COUNT(*) requests,
		COALESCE(SUM(status='success'),0) success,
		COALESCE(SUM(CASE WHEN started_at>=? AND status='success' THEN output_tokens ELSE 0 END),0) tokens,
		COALESCE(SUM(CASE WHEN started_at>=? THEN 1 ELSE 0 END),0) rpm
		FROM requests WHERE started_at>=? GROUP BY requested_model`, t-60000, t-60000, t-5*60000)
	if err != nil {
		return nil, err
	}
	sRows, err := a.Store.DB.Query(`SELECT model_id,COUNT(*) n FROM sessions
		WHERE updated_at>? GROUP BY model_id`, t-int64(c.Settings.SessionTTLHours)*3600000)
	if err != nil {
		return nil, err
	}
	byModel, byRoute := map[string]liveAgg{}, map[string]liveAgg{}
	for _, r := range mRows {
		byModel[r.String("model_id")] = liveAgg{requests: r.Int("requests"), success: r.Int("success"), rpm: r.Int("rpm"), tokens: r.Int("tokens"), dur: r.Int("dur")}
	}
	for _, r := range rRows {
		byRoute[r.String("requested_model")] = liveAgg{requests: r.Int("requests"), success: r.Int("success"), rpm: r.Int("rpm"), tokens: r.Int("tokens")}
	}
	sessions := map[string]int64{}
	for _, r := range sRows {
		sessions[r.String("model_id")] = r.Int("n")
	}
	global, states := a.Engine.snapshot(used, t)

	models := Object{}
	for _, id := range used {
		m, _ := c.model(id)
		st, g := states[id], byModel[id]
		models[id] = Object{
			"active": st.active, "concurrency": m.Concurrency, "rpm": st.rpm, "rpm_limit": m.RPM,
			"ttfb_ms": st.ttfb, "cooldown_until": st.cooldown, "limits": st.limits,
			"sessions": sessions[id], "requests_5m": g.requests, "success_5m": g.success,
			"tok_s": perSecond(g), "rejected_5m": st.rejects, "live_tok_s": math.Round(st.stream*10) / 10,
		}
	}

	routes := Object{}
	for _, rt := range c.Routes {
		var active, capacity, sess int64
		var weighted, weights, live float64
		seen := map[string]bool{}
		for _, cm := range rt.Candidates {
			m, ok := c.model(cm.ModelID)
			if !ok || seen[cm.ModelID] {
				continue
			}
			seen[cm.ModelID] = true
			st, w := states[cm.ModelID], float64(byModel[cm.ModelID].requests)
			active += int64(st.active)
			live += st.stream
			capacity += int64(m.Concurrency)
			sess += sessions[cm.ModelID]
			// 候选的响应头耗时按各自近 5 分钟的请求数加权
			weighted += float64(st.ttfb) * w
			weights += w
		}
		ttfb := int64(0)
		if weights > 0 {
			ttfb = int64(math.Round(weighted / weights))
		}
		g := byRoute[rt.ID]
		// 60 秒窗口的成功输出 token 摊到整分钟；没有成功请求就是 0
		tok := 0.0
		if g.tokens > 0 {
			tok = float64(g.tokens) / 60
		}
		routes[rt.ID] = Object{
			"active": active, "capacity": capacity, "rpm": g.rpm, "tok_s": tok,
			"requests_5m": g.requests, "success_5m": g.success,
			"sessions": sess, "ttfb_ms": ttfb, "live_tok_s": math.Round(live*10) / 10,
		}
	}
	return Object{
		"now":    t,
		"global": Object{"active": global, "limit": c.Settings.GlobalConcurrency},
		"models": models, "routes": routes,
	}, nil
}

// perSecond 是单请求速度：成功请求的输出 token 总和除以它们的耗时总和。
// 除以耗时而不是请求数，一条慢的长回答不该和一个短的算成同一个速度。
// 没有成功样本时给 0，避免拿 0 除出 NaN 显示到界面上。
func perSecond(g liveAgg) float64 {
	if g.tokens <= 0 || g.dur <= 0 {
		return 0
	}
	return math.Round(float64(g.tokens)*1000/float64(g.dur)*100) / 100
}
