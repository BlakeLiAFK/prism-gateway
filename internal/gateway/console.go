package gateway

// 管理后台的观测类动作：给界面提供“现在为什么这样路由”的实时依据。
// admin.go 的分发器在未命中时回落到这里。

var consoleActions = map[string]func(a *App, id string, p Object) (any, error){
	"route.stats": func(a *App, _ string, p Object) (any, error) { return a.routeStats(p) },
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
		FROM requests WHERE started_at >= ? GROUP BY requested_model, model_id`, now()-int64(minutes)*60000)
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
	return Object{"minutes": minutes, "share": share, "runtime": a.Engine.Health(), "now": now()}, nil
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
