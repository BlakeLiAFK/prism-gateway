package gateway

import (
	"strings"

	"prism-gateway/internal/sqlite"
)

// 管理后台的观测类动作：给界面提供“现在为什么这样路由”的实时依据。
// admin.go 的分发器在未命中时回落到这里。

var consoleActions = map[string]func(a *App, id string, p Object) (any, error){
	"route.stats":     func(a *App, _ string, p Object) (any, error) { return a.routeStats(p) },
	"model.stats":     func(a *App, _ string, p Object) (any, error) { return a.modelStats(p) },
	"model.usage":     func(a *App, id string, p Object) (any, error) { return a.modelUsage(id, p) },
	"request.reprice": func(a *App, _ string, p Object) (any, error) { return a.reprice(p) },
}

// routeStats 返回运行时状态与最近若干分钟各路由的实际落点。
// share 形如 {路由ID: {模型ID: {"attempts": n, "success": n, "affinity": n}}}，affinity 是命中会话亲和的次数，
// 按客户端请求名（requested_model）归属，别名解析到路由的也算在请求名下。
func (a *App) routeStats(p Object) (any, error) {
	minutes := clamp(int(num(p, "minutes")), 1, 1440)
	if p["minutes"] == nil {
		minutes = 60
	}
	rows, err := a.Store.DB.Query(`SELECT requested_model, model_id, COUNT(*) attempts,
		SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) success,
		SUM(CASE WHEN reason LIKE '%affinity=true%' THEN 1 ELSE 0 END) affinity
		FROM requests WHERE started_at >= ? AND model_id!='' GROUP BY requested_model, model_id`, now()-int64(minutes)*60000)
	if err != nil {
		return nil, err
	}
	share := Object{}
	for _, r := range rows {
		route := r.String("requested_model")
		m := obj(share[route])
		if m == nil {
			m = Object{}
			share[route] = m
		}
		m[r.String("model_id")] = Object{"attempts": r.Int("attempts"), "success": r.Int("success"), "affinity": r.Int("affinity")}
	}
	rows, err = a.Store.DB.Query(`SELECT u.requested_model, `+cacheColumns+`
		FROM usage_hourly u LEFT JOIN cache_hourly c USING(hour,key_id,requested_model,model_id,provider_id)
		WHERE u.hour>=? GROUP BY u.requested_model`, (now()-int64(minutes)*60000)/3600000)
	if err != nil {
		return nil, err
	}
	cache := Object{}
	for _, r := range rows {
		cache[r.String("requested_model")] = cacheStat(r)
	}
	return Object{"minutes": minutes, "share": share, "cache": cache, "runtime": a.Engine.Health(), "now": now()}, nil
}

// deleteSessions 解除会话亲和：给 id 删单个；给 model_id 删绑定到该模型的全部会话
func (a *App) deleteSessions(id, model string) (any, error) {
	if id == "" && model == "" {
		return nil, fail("INVALID_PARAMS", "需要 id 或 model_id", 400)
	}
	q, arg, target := "DELETE FROM sessions WHERE id=?", id, id
	if id == "" {
		q, arg, target = "DELETE FROM sessions WHERE model_id=?", model, "model:"+model
	}
	n, err := a.Store.DB.Query("SELECT COUNT(*) n FROM sessions WHERE "+q[len("DELETE FROM sessions WHERE "):], arg)
	if err != nil {
		return nil, err
	}
	if err = a.Store.DB.Exec(q, arg); err != nil {
		return nil, err
	}
	a.audit("session.delete", target)
	return Object{"id": id, "model_id": model, "unbound": n[0].Int("n")}, nil
}

type sessionUsage struct {
	models                               []any
	requests, cost, known, unknown, demo int64
}

func (a *App) sessionList(ttl int64) (out []sqlite.Row, err error) {
	err = a.Store.DB.Transaction(func(tx *sqlite.Tx) error {
		out, err = tx.Query("SELECT *, updated_at+? expires_at FROM sessions WHERE updated_at>? ORDER BY updated_at DESC LIMIT 200", ttl, now()-ttl)
		if err != nil || len(out) == 0 {
			return err
		}
		ids, seen := []string{}, map[string]bool{}
		for _, row := range out {
			id, _, _ := strings.Cut(row.String("id"), ":")
			if !seen[id] {
				ids, seen[id] = append(ids, id), true
			}
		}
		usage, err := sessionUsageByID(tx, ids)
		if err != nil {
			return err
		}
		for _, row := range out {
			id, _, _ := strings.Cut(row.String("id"), ":")
			u := usage[id]
			row["models"], row["usage_requests"], row["cost_nano"] = u.models, u.requests, u.cost
			row["cost_known_requests"], row["cost_unknown_requests"], row["demo_requests"] = u.known, u.unknown, u.demo
		}
		return nil
	})
	return out, err
}

func sessionUsageByID(tx *sqlite.Tx, ids []string) (map[string]*sessionUsage, error) {
	usage, args := map[string]*sessionUsage{}, make([]any, len(ids))
	for i, id := range ids {
		usage[id], args[i] = &sessionUsage{models: []any{}}, id
	}
	known := "(" + knownRequestCost + " OR usage_mode='rejected')"
	q := `SELECT session_id,model_id,provider_id,COUNT(*) requests,
		SUM(status='success') success_requests,
		SUM(CASE WHEN is_demo=0 AND usage_mode!='rejected' AND ` + knownRequestCost + ` THEN cost_nano ELSE 0 END) cost_nano,
		SUM(CASE WHEN is_demo=0 AND ` + known + ` THEN 1 ELSE 0 END) cost_known_requests,
		SUM(CASE WHEN is_demo=0 AND usage_mode NOT IN ('rejected','incompatible') AND NOT ` + knownRequestCost + ` THEN 1 ELSE 0 END) cost_unknown_requests,
		SUM(is_demo=1) demo_requests FROM requests WHERE session_id IN (?` + strings.Repeat(",?", len(ids)-1) + `)
		GROUP BY session_id,model_id,provider_id ORDER BY session_id,requests DESC,model_id,provider_id`
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		u := usage[row.String("session_id")]
		u.requests += row.Int("requests")
		u.cost += row.Int("cost_nano")
		u.known += row.Int("cost_known_requests")
		u.unknown += row.Int("cost_unknown_requests")
		u.demo += row.Int("demo_requests")
		if row.String("model_id") != "" {
			u.models = append(u.models, Object{"model_id": row.String("model_id"), "provider_id": row.String("provider_id"), "requests": row.Int("requests"), "success_requests": row.Int("success_requests")})
		}
	}
	return usage, nil
}
