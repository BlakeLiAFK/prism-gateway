package gateway

// 模型与供应商的用量统计。数据来自 requests 表，花费是按管理员填写的单价估算，
// 不是上游账单：只算已结束的非演示请求，价格未确认的请求单独计数而不是当作 0。

import "prism-gateway/internal/sqlite"

const usageColumns = `COUNT(*) requests,
	COALESCE(SUM(status='success'),0) success,
	COALESCE(SUM(status IN ('error','unknown')),0) errors,
	COALESCE(SUM(input_tokens),0) input_tokens,
	COALESCE(SUM(output_tokens),0) output_tokens,
	COALESCE(SUM(cache_tokens),0) cache_tokens,
	COALESCE(SUM(CASE WHEN status!='running' AND is_demo=0 THEN cost_nano ELSE 0 END),0) cost_nano,
	COALESCE(SUM(status='success' AND cost_known=0),0) unpriced,
	COALESCE(AVG(CASE WHEN status='success' THEN duration_ms END),0) latency_ms,
	COALESCE(MAX(started_at),0) last_used`

var usageInts = []string{"requests", "success", "errors", "input_tokens", "output_tokens", "cache_tokens", "unpriced", "last_used"}

// usageStat 把一行聚合结果转成接口返回的对象
func usageStat(r sqlite.Row) Object {
	o := Object{"cost": dollars(r.Int("cost_nano")), "latency_ms": int64(r.Float("latency_ms"))}
	for _, k := range usageInts {
		o[k] = r.Int(k)
	}
	return o
}

// addUsage 把 b 累加进 a，用于由模型汇总出供应商
func addUsage(a, b Object) {
	for _, k := range usageInts {
		if k == "last_used" {
			a[k] = max(int64(num(a, k)), int64(num(b, k)))
			continue
		}
		a[k] = int64(num(a, k)) + int64(num(b, k))
	}
	a["cost"] = num(a, "cost") + num(b, "cost")
}

type statsCacheEntry struct {
	at  int64
	val Object
}

// statsTTL：统计差几十秒无妨，而 1 核小机器上聚合几万行要几百毫秒，每次翻页都重算不划算
const statsTTL = 30000

// modelStats 返回最近 days 天按模型与供应商的用量：{days, models:{id:用量}, providers:{id:用量}}，
// 按天数缓存 30 秒。供应商的平均耗时按成功请求数加权。
func (a *App) modelStats(p Object) (any, error) {
	days := clamp(int(num(p, "days")), 1, 90)
	if p["days"] == nil {
		days = 7
	}
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if c, ok := a.statsCache[days]; ok && now()-c.at < statsTTL {
		return c.val, nil
	}
	v, err := a.computeModelStats(days)
	if err != nil {
		return nil, err
	}
	if a.statsCache == nil {
		a.statsCache = map[int]statsCacheEntry{}
	}
	a.statsCache[days] = statsCacheEntry{now(), v}
	return v, nil
}

func (a *App) computeModelStats(days int) (Object, error) {
	rows, err := a.Store.DB.Query(`SELECT model_id, provider_id, `+usageColumns+`
		FROM requests WHERE started_at >= ? GROUP BY model_id, provider_id`, now()-int64(days)*86400000)
	if err != nil {
		return nil, err
	}
	models, providers := Object{}, Object{}
	for _, r := range rows {
		u := usageStat(r)
		if old := obj(models[r.String("model_id")]); old != nil {
			addUsage(old, u)
		} else {
			models[r.String("model_id")] = u
		}
		pid := r.String("provider_id")
		pu := obj(providers[pid])
		if pu == nil {
			pu = Object{"latency_sum": 0.0}
			providers[pid] = pu
		}
		addUsage(pu, u)
		pu["latency_sum"] = num(pu, "latency_sum") + num(u, "latency_ms")*num(u, "success")
	}
	for _, v := range providers {
		pu := obj(v)
		if s := num(pu, "success"); s > 0 {
			pu["latency_ms"] = int64(num(pu, "latency_sum") / s)
		}
		delete(pu, "latency_sum")
	}
	return Object{"days": days, "models": models, "providers": providers}, nil
}

// modelUsage 返回单个模型最近 days 天（默认 30）的每日用量与按客户端角色的拆分。
// tz_offset_min 与浏览器 Date.getTimezoneOffset 相同（UTC 减本地，单位分钟），用于按本地日期分桶。
func (a *App) modelUsage(id string, p Object) (any, error) {
	if id == "" {
		return nil, fail("INVALID_PARAMS", "需要模型 id", 400)
	}
	days := clamp(int(num(p, "days")), 1, 90)
	if p["days"] == nil {
		days = 30
	}
	offset := int64(clamp(int(num(p, "tz_offset_min")), -840, 840)) * 60000
	const day = int64(86400000)
	// 本地「今天」零点对应的 UTC 毫秒，往前推 days-1 天作为起点
	start := ((now()-offset)/day-int64(days-1))*day + offset
	rows, err := a.Store.DB.Query(`SELECT (started_at-?)/? d, `+usageColumns+`
		FROM requests WHERE model_id=? AND started_at>=? GROUP BY d`, offset, day, id, start)
	if err != nil {
		return nil, err
	}
	byDay := map[int64]Object{}
	for _, r := range rows {
		byDay[r.Int("d")] = usageStat(r)
	}
	daily := make([]any, 0, days)
	for i := 0; i < days; i++ {
		t := start + int64(i)*day
		u := byDay[(t-offset)/day]
		if u == nil {
			u = Object{"requests": 0, "success": 0, "input_tokens": 0, "output_tokens": 0, "cost": 0.0}
		}
		u["time"] = t
		daily = append(daily, u)
	}
	roles, err := a.Store.DB.Query(`SELECT agent_role role, COUNT(*) requests,
		COALESCE(SUM(input_tokens+output_tokens),0) tokens
		FROM requests WHERE model_id=? AND started_at>=? GROUP BY agent_role ORDER BY requests DESC`, id, start)
	if err != nil {
		return nil, err
	}
	return Object{"id": id, "days": days, "daily": daily, "roles": roles}, nil
}
