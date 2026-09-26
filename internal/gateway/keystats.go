package gateway

// 基于 usage_hourly 汇总表的统计：模型、供应商、网关 Key。窗口按整点对齐，最多多算不到一小时。

func init() {
	consoleActions["key.usage"] = func(a *App, id string, p Object) (any, error) { return a.keyUsage(id, p) }
}

// rollupColumns 与 usageStat 读取的列名一致
const rollupColumns = `SUM(requests) requests, SUM(success) success, SUM(errors) errors,
	SUM(input_tokens) input_tokens, SUM(output_tokens) output_tokens, SUM(cache_tokens) cache_tokens,
	SUM(cost_nano) cost_nano, SUM(unpriced) unpriced,
	CAST(SUM(success_ms) AS REAL)/MAX(SUM(success),1) latency_ms, MAX(last_at) last_used`

// sinceHour 返回最近 days 天起点所在的小时编号
func sinceHour(days int) int64 { return (now() - int64(days)*86400000) / 3600000 }

// keyStats 返回最近 days 天每把 Key 的用量，按 key_id 索引
func (a *App) keyStats(days int) (map[string]Object, error) {
	rows, err := a.Store.DB.Query(`SELECT key_id, `+rollupColumns+` FROM usage_hourly WHERE hour>=? GROUP BY key_id`, sinceHour(days))
	if err != nil {
		return nil, err
	}
	out := map[string]Object{}
	for _, r := range rows {
		out[r.String("key_id")] = usageStat(r)
	}
	return out, nil
}

// keyList 列出网关 Key，每把附带最近 days 天（默认 7）的用量 usage
func (a *App) keyList(p Object) (any, error) {
	days := clamp(int(num(p, "days")), 1, 90)
	if p["days"] == nil {
		days = 7
	}
	rows, err := a.Store.DB.Query("SELECT id,name,prefix,enabled,allowed,created_at,last_used,revoked_at," + keyPolicyColumns + " FROM api_keys ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	stats, err := a.keyStats(days)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		o := Object{}
		for k, v := range r {
			o[k] = v
		}
		o["usage"] = stats[r.String("id")]
		// 设了花费上限的 Key 附带当前用量，界面显示「已用 / 上限」
		if r.Float("limit_day") > 0 || r.Float("limit_month") > 0 {
			if day, month, er := a.Engine.keySpend(r.String("id")); er == nil {
				o["spend_day"], o["spend_month"] = day, month
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// keyUsage 返回单把 Key 最近 days 天（默认 30）的每日用量、常用模型与路由，以及近 7 天的来源
func (a *App) keyUsage(id string, p Object) (any, error) {
	if id == "" {
		return nil, fail("INVALID_PARAMS", "需要 Key id", 400)
	}
	days := clamp(int(num(p, "days")), 1, 90)
	if p["days"] == nil {
		days = 30
	}
	offset := int64(clamp(int(num(p, "tz_offset_min")), -840, 840)) * 60000
	const day = int64(86400000)
	start := ((now()-offset)/day-int64(days-1))*day + offset
	rows, err := a.Store.DB.Query(`SELECT (hour*3600000-?)/? d, `+rollupColumns+`
		FROM usage_hourly WHERE key_id=? AND hour>=? GROUP BY d`, offset, day, id, start/3600000)
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
	top := func(col string) ([]any, error) {
		rs, err := a.Store.DB.Query(`SELECT `+col+` id, SUM(requests) requests, SUM(input_tokens+output_tokens) tokens, SUM(cost_nano) cost_nano
			FROM usage_hourly WHERE key_id=? AND hour>=? GROUP BY `+col+` ORDER BY requests DESC LIMIT 5`, id, start/3600000)
		out := []any{}
		for _, r := range rs {
			out = append(out, Object{"id": r.String("id"), "requests": r.Int("requests"), "tokens": r.Int("tokens"), "cost": dollars(r.Int("cost_nano"))})
		}
		return out, err
	}
	models, err := top("model_id")
	if err != nil {
		return nil, err
	}
	routes, err := top("requested_model")
	if err != nil {
		return nil, err
	}
	// 来源只看近 7 天明细：走 (key_id, started_at) 索引，量很小
	sources, err := a.Store.DB.Query(`SELECT client_ip ip, user_agent agent, COUNT(*) requests, MAX(started_at) last_used
		FROM requests WHERE key_id=? AND started_at>=? GROUP BY client_ip, user_agent ORDER BY last_used DESC LIMIT 5`, id, now()-7*day)
	if err != nil {
		return nil, err
	}
	return Object{"id": id, "days": days, "daily": daily, "models": models, "routes": routes, "sources": sources}, nil
}
