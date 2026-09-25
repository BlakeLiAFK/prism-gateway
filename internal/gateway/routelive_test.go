package gateway

import "testing"

// route.live 验收：路由与候选的实时数据契约
//
//	{now, global:{active,limit},
//	 models:{id:{active,concurrency,rpm,rpm_limit,ttfb_ms,cooldown_until,limits,sessions,requests_5m,success_5m,tok_s}},
//	 routes:{id:{active,capacity,rpm,tok_s,requests_5m,success_5m,sessions,ttfb_ms}}}
func TestRouteLive(t *testing.T) {
	h := newHarness(t)
	a, b := modelFixture("a", "chat"), modelFixture("b", "chat")
	a.Concurrency, b.Concurrency, a.RPM = 3, 5, 60
	h.configure(t, "http://127.0.0.1:1", a, b)
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "weighted", Candidates: []Candidate{{"a", 10}, {"b", 10}}}}
	})
	n := now()
	// 近 60 秒：a 成功 2 次（各 5 个输出 token，耗时 100ms）、b 失败 1 次；5 分钟前的旧记录不计入 60 秒窗口
	insertUsage(t, h, "a", "p_test", "success", "", n-10000, 0, true, false)
	insertUsage(t, h, "a", "p_test", "success", "", n-20000, 0, true, false)
	insertUsage(t, h, "b", "p_test", "error", "", n-30000, 0, true, false)
	insertUsage(t, h, "a", "p_test", "success", "", n-200000, 0, true, false)
	if err := h.s.DB.Exec("UPDATE requests SET requested_model='auto'"); err != nil {
		t.Fatal(err)
	}
	// 会话：a 两个有效、一个已过期；b 一个
	for _, s := range []struct {
		id, model string
		at        int64
	}{{"s1", "a", n}, {"s2", "a", n}, {"s3", "a", n - 48*3600000}, {"s4", "b", n}} {
		if err := h.s.DB.Exec(`INSERT INTO sessions VALUES (?,?,?,?,?,1)`, s.id, "k", s.model, "p_test", s.at); err != nil {
			t.Fatal(err)
		}
	}
	h.a.Engine.mu.Lock()
	h.a.Engine.state("a").Active, h.a.Engine.state("b").Active, h.a.Engine.global = 2, 1, 3
	h.a.Engine.state("a").Recent = []int64{n - 1000, n - 2000, n - 120000}
	h.a.Engine.state("a").TTFB = 800
	h.a.Engine.mu.Unlock()

	w, out := h.rpc(t, "route.live", Object{}, h.token)
	requireStatus(t, w, 200)
	d := obj(out["data"])
	if g := obj(d["global"]); num(g, "active") != 3 || num(g, "limit") != float64(h.s.Config().Settings.GlobalConcurrency) {
		t.Fatalf("global 不对: %v", g)
	}
	ma := obj(obj(d["models"])["a"])
	want := map[string]float64{"active": 2, "concurrency": 3, "rpm": 2, "rpm_limit": 60, "ttfb_ms": 800, "sessions": 2, "requests_5m": 3, "success_5m": 3}
	for k, v := range want {
		if num(ma, k) != v {
			t.Errorf("models.a.%s = %v，期望 %v（%v）", k, ma[k], v, ma)
		}
	}
	// 单请求速度：成功请求的输出 token 总和 / 耗时总和（秒）= 15 / 0.3 = 50
	if num(ma, "tok_s") != 50 {
		t.Errorf("models.a.tok_s = %v，期望 50", ma["tok_s"])
	}
	r := obj(obj(d["routes"])["auto"])
	wantR := map[string]float64{"active": 3, "capacity": 8, "rpm": 3, "requests_5m": 4, "success_5m": 3, "sessions": 3}
	for k, v := range wantR {
		if num(r, k) != v {
			t.Errorf("routes.auto.%s = %v，期望 %v（%v）", k, r[k], v, r)
		}
	}
	// 路由输出速度：近 60 秒成功请求的输出 token / 60 秒 = 10 / 60
	if got := num(r, "tok_s"); got < 0.16 || got > 0.17 {
		t.Errorf("routes.auto.tok_s = %v，期望约 0.1667", got)
	}
	// 5 秒内重复请求走缓存
	insertUsage(t, h, "a", "p_test", "success", "", n, 0, true, false)
	_, out = h.rpc(t, "route.live", Object{}, h.token)
	if num(obj(obj(obj(out["data"])["models"])["a"]), "requests_5m") != 3 {
		t.Error("5 秒内应返回缓存结果")
	}
}
