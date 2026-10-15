package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// alertEvent 是 OnAlert 收到的一次告警
type alertEvent struct{ event, key, text string }

// captureAlerts 把引擎的告警收进通道。告警在独立 goroutine 里发出，所以只能带超时地等
func captureAlerts(h *harness) chan alertEvent {
	ch := make(chan alertEvent, 8)
	h.a.Engine.OnAlert = func(event, key, text string) { ch <- alertEvent{event, key, text} }
	return ch
}

func waitAlert(t *testing.T, ch chan alertEvent) alertEvent {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("应当收到告警，但等待超时")
	}
	return alertEvent{}
}

func requireNoAlert(t *testing.T, ch chan alertEvent) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("不应告警: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// aliasRoute 配置路由 auto（只有 b 一个候选）和两个指向它的别名，另有一个指向模型 b 的别名
func aliasRoute(t *testing.T, h *harness, url string, models ...Model) {
	t.Helper()
	h.configure(t, url, models...)
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: models[0].ID, Weight: 10}}}}
		c.Aliases = []Alias{
			{ID: "claude-sonnet", Target: "auto", Enabled: true},
			{ID: "claude-haiku", Target: "auto", Enabled: true},
			{ID: "gpt-direct", Target: models[0].ID, Enabled: true},
		}
	})
}

// generateZDR 带 x-cmd-zdr 请求头走真实入口；h.generate 不能设置请求头
func generateZDR(h *harness, p string, o Object, zdr string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", pathFor(p), strings.NewReader(raw(o)))
	r.Header.Set("X-Prism-Session", "test-conversation")
	r.Header.Set("x-cmd-zdr", zdr)
	w := httptest.NewRecorder()
	h.a.Engine.Handle(w, r, p, Principal{ID: "test-key"})
	return w
}

// 客户端通常经别名访问路由：告警必须认出别名背后的路由，并写明是哪个别名，限频键按路由归并
func TestIncompatibleAlertResolvesAlias(t *testing.T) {
	h := newHarness(t)
	alerts := captureAlerts(h)
	aliasRoute(t, h, "http://127.0.0.1:1", dropModel("b", "chat", false))
	for _, alias := range []string{"claude-sonnet", "claude-haiku"} {
		requireStatus(t, h.generate(t, "messages", reasoningHistoryRequest(alias)), 400)
		got := waitAlert(t, alerts)
		if got.event != "route" || got.key != "route-incompatible:auto" {
			t.Fatalf("别名 %s 的限频键应按解析后的路由归并: %+v", alias, got)
		}
		if !strings.Contains(got.text, "路由：auto") || !strings.Contains(got.text, "请求入口："+alias) || !strings.Contains(got.text, "内容类型 thinking") || !strings.Contains(got.text, "候选供应商 / 模型") || !strings.Contains(got.text, "Test upstream") {
			t.Fatalf("告警应写明路由与别名: %s", got.text)
		}
	}
	// 请求记录里保留客户端实际发来的名字，按别名才搜得到
	// 两条记录可能落在同一毫秒，按写入顺序而不是时间戳排
	rows, err := h.s.DB.Query("SELECT requested_model FROM requests ORDER BY rowid")
	if err != nil || len(rows) != 2 || rows[0].String("requested_model") != "claude-sonnet" || rows[1].String("requested_model") != "claude-haiku" {
		t.Fatalf("请求记录应保留别名: %v %v", rows, err)
	}
}

// 别名指向的是模型而不是路由时，与直接按模型调用一样不告警，只留记录
func TestIncompatibleAliasToModelDoesNotAlert(t *testing.T) {
	h := newHarness(t)
	alerts := captureAlerts(h)
	aliasRoute(t, h, "http://127.0.0.1:1", dropModel("b", "chat", false))
	requireStatus(t, h.generate(t, "messages", reasoningHistoryRequest("gpt-direct")), 400)
	if rows, err := h.s.DB.Query("SELECT * FROM requests"); err != nil || len(rows) != 1 {
		t.Fatalf("仍应留下请求记录: %d %v", len(rows), err)
	}
	requireNoAlert(t, alerts)
}

// Handle 末尾的「全部候选都不可用」告警与上面是同一个问题：经别名到达的路由也要告警
func TestExhaustedRouteAlertResolvesAlias(t *testing.T) {
	h := newHarness(t)
	alerts := captureAlerts(h)
	m := modelFixture("m", "chat")
	aliasRoute(t, h, "http://127.0.0.1:1", m)
	// 并发占满：候选合格，但没有一个能通过准入
	h.a.Engine.mu.Lock()
	h.a.Engine.state("m").Active = m.Concurrency
	h.a.Engine.mu.Unlock()
	requireStatus(t, h.generate(t, "chat", requestFixture("chat", "claude-sonnet")), 429)
	got := waitAlert(t, alerts)
	if got.event != "route" || got.key != "route:auto" {
		t.Fatalf("限频键应使用解析后的路由: %+v", got)
	}
	if !strings.Contains(got.text, "路由 auto 的全部候选都不可用") || !strings.Contains(got.text, "claude-sonnet") {
		t.Fatalf("告警应写明路由与别名: %s", got.text)
	}
	// 直接用路由 ID 调用时不带别名说明
	requireStatus(t, h.generate(t, "chat", requestFixture("chat", "auto")), 429)
	if got = waitAlert(t, alerts); got.key != "route:auto" || strings.Contains(got.text, "别名") {
		t.Fatalf("直接调用路由不应提到别名: %+v", got)
	}
}

// 候选本来就是空的时候，x-cmd-zdr 不是原因：客户端要看到真正的淘汰原因，后台要有记录
func TestZDRWithNoEligibleCandidateReportsRealReason(t *testing.T) {
	h := newHarness(t)
	aliasRoute(t, h, "http://127.0.0.1:1", dropModel("b", "chat", false))
	w := generateZDR(h, "messages", reasoningHistoryRequest("auto"), "1")
	requireStatus(t, w, 400)
	if !strings.Contains(w.Body.String(), "没有兼容此请求的候选模型") || !strings.Contains(w.Body.String(), "内容类型 thinking") {
		t.Fatalf("应当给出真实的淘汰原因而不是 ZDR 错误: %s", w.Body)
	}
	rows, err := h.s.DB.Query("SELECT * FROM requests")
	if err != nil || len(rows) != 1 || rows[0].String("error_code") != "NO_COMPATIBLE_MODEL" || rows[0].String("usage_mode") != "incompatible" {
		t.Fatalf("应当留下 NO_COMPATIBLE_MODEL 记录: %v %v", rows, err)
	}
}

// 候选被 x-cmd-zdr 筛光：客户端响应保持不变，记录里要能看到这次 400
func TestZDRConstraintRemovingEveryCandidateIsRecorded(t *testing.T) {
	h := newHarness(t)
	alerts := captureAlerts(h)
	// u 的原生协议未确认，淘汰原因是「可处理」的那一类。没有它，whys 里只剩合格候选，
	// 不告警是自然结果，下面的 requireNoAlert 就测不到 recordIncompatible 里对错误码的判断
	unconfirmed := modelFixture("u", "chat")
	unconfirmed.Protocols = []string{}
	aliasRoute(t, h, "http://127.0.0.1:1", modelFixture("b", "chat"), unconfirmed)
	h.change(t, func(c *Config) {
		c.Routes[0].Groups[0].Candidates = []Candidate{{ModelID: "b", Weight: 10}, {ModelID: "u", Weight: 10}}
	})
	w := generateZDR(h, "chat", requestFixture("chat", "auto"), "1")
	requireStatus(t, w, 400)
	const msg = "没有可执行 x-cmd-zdr 零数据保留约束的候选供应商"
	if !strings.Contains(w.Body.String(), `"code":"ZDR_UNSUPPORTED"`) || !strings.Contains(w.Body.String(), msg) {
		t.Fatalf("客户端响应应保持 ZDR_UNSUPPORTED: %s", w.Body)
	}
	rows, err := h.s.DB.Query("SELECT * FROM requests")
	if err != nil || len(rows) != 1 {
		t.Fatalf("应当留下一条请求记录: %d %v", len(rows), err)
	}
	r := rows[0]
	if r.String("status") != "error" || r.Int("http_status") != 400 || r.String("error_code") != "ZDR_UNSUPPORTED" ||
		r.String("reason") != msg || r.String("requested_model") != "auto" || r.String("model_id") != "" ||
		r.String("usage_mode") != "incompatible" || r.String("protocol") != "chat" {
		t.Fatalf("请求记录内容不对: %v", r)
	}
	if r.String("parent_id") != w.Header().Get("X-Request-ID") {
		t.Fatal("记录应能按响应里的请求 ID 查到")
	}
	// 这是单个请求自带的约束，不是路由配置出了问题
	requireNoAlert(t, alerts)
	// 没有到达任何模型，不进用量汇总
	if usage, err := h.s.DB.Query("SELECT * FROM usage_hourly"); err != nil || len(usage) != 0 {
		t.Fatalf("不应计入用量汇总: %d %v", len(usage), err)
	}
}

// 失败率任务只统计到达过模型的请求。直接按模型调用、候选全部暂停这类刻意不告警的 400
// 不能反过来推高失败率告警
func TestFailureRateIgnoresIncompatibleRecords(t *testing.T) {
	h := newHarness(t)
	msgs := webhookSink(t, h)
	h.configure(t, "http://127.0.0.1:1", dropModel("b", "chat", false))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "b", Weight: 10, Disabled: true}}}}
	})
	for _, model := range []string{"b", "b", "auto", "auto"} {
		requireStatus(t, h.generate(t, "messages", reasoningHistoryRequest(model)), 400)
	}
	at := now() - 60000
	// 记录由真实入口写入；挪进统计窗口，避免和任务运行时刻落在同一毫秒
	if err := h.s.DB.Exec("UPDATE requests SET started_at=? WHERE usage_mode='incompatible'", at); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		insertUsage(t, h, "m", "p", "success", "", at, 0, true, false)
	}
	w, _ := h.rpc(t, "schedule.save", Object{"failure_enabled": true, "failure_minutes": 15, "failure_min_samples": 5, "failure_percent": 30}, h.token)
	requireStatus(t, w, 200)

	if run := runScheduled(t, h, "failure"); str(run, "result") != "" || len(msgs()) != 0 {
		t.Fatalf("只有未到达模型的 400 时不应触发失败率告警: %v %v", run, msgs())
	}
	// 对照：真实的上游失败仍然计入，且分母里没有那四条记录
	for i := 0; i < 4; i++ {
		insertUsage(t, h, "m", "p", "error", "", at, 0, true, false)
	}
	runScheduled(t, h, "failure")
	if got := msgs(); len(got) != 1 || !strings.Contains(got[0], "失败：4 / 10 次（40.0%）") {
		t.Fatalf("真实失败应按 4/10 计入: %v", got)
	}
}
