package gateway

// 仪表盘的数据组装：复用路由实时面板（五秒缓存、不新增统计查询），再叠加事件中心里的趋势与事件。

import "time"

// activeNow 是此刻在途的上游请求总数
func (e *Engine) activeNow() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.global
}

func (a *App) dashView(o watchOpts, zone *time.Location, expires int64) (dashView, error) {
	live, err := a.routeLive()
	if err != nil {
		return dashView{}, err
	}
	lv := obj(live)
	models, routes := obj(lv["models"]), obj(lv["routes"])
	c := a.Store.Config()
	t := now()
	v := dashView{
		Version: Version, Zone: zone, Now: time.UnixMilli(t), Uptime: time.Duration(t-a.Started) * time.Millisecond,
		ConfigVer: c.Version, Active: int64(num(obj(lv["global"]), "active")),
		OKPct: -1, Watchers: a.Engine.watch.watchers(), W: o.width, H: o.height, Color: o.color,
		TokenLeft: time.Duration(expires-t) * time.Millisecond,
	}
	var req5, ok5 float64
	for _, m := range models {
		mv := obj(m)
		v.RPM += int64(num(mv, "rpm"))
		v.TokS += num(mv, "live_tok_s")
		req5 += num(mv, "requests_5m")
		ok5 += num(mv, "success_5m")
	}
	if req5 > 0 {
		v.OKPct = ok5 / req5 * 100
	}
	for _, rt := range c.Routes {
		if rt.Enabled {
			v.Routes = append(v.Routes, dashRouteOf(c, rt, obj(routes[rt.ID]), models, t))
		}
	}
	v.Req, v.Err = a.Engine.watch.series()
	v.Events = a.Engine.watch.recent(40)
	return v, nil
}

func dashRouteOf(c Config, rt Route, rv, models Object, t int64) dashRoute {
	r := dashRoute{ID: rt.ID, Strategy: rt.primaryStrategy(), RPM: int64(num(rv, "rpm")), Active: int64(num(rv, "active"))}
	var total float64
	for _, cm := range rt.all() {
		if !cm.Disabled {
			total += num(obj(models[cm.ModelID]), "requests_5m")
		}
	}
	for _, cm := range rt.all() {
		m, ok := c.model(cm.ModelID)
		mv := obj(models[cm.ModelID])
		d := dashCand{Name: cm.ModelID, Paused: cm.Disabled, Disabled: !ok || !m.Enabled, Requests5m: int64(num(mv, "requests_5m")),
			Active: int64(num(mv, "active")), RPM: int64(num(mv, "rpm")),
			TTFB: time.Duration(num(mv, "ttfb_ms")) * time.Millisecond, TokS: num(mv, "live_tok_s")}
		if ok && m.Name != "" {
			d.Name = m.Name
		}
		if d.TokS == 0 {
			d.TokS = num(mv, "tok_s")
		}
		if until := int64(num(mv, "cooldown_until")); until > t {
			d.Cooling = time.Duration(until-t) * time.Millisecond
		}
		if total > 0 && !d.Paused {
			d.Share = num(mv, "requests_5m") / total
		}
		r.Cands = append(r.Cands, d)
	}
	return r
}
