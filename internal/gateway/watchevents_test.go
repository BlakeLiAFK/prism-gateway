package gateway

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// findEvent 在回看缓冲里找第一条满足条件的事件
func findEvent(h *harness, kind, contains string) (watchEvent, bool) {
	for _, ev := range h.a.Engine.watch.recent(watchRing) {
		if ev.Kind == kind && strings.Contains(ev.Text, contains) {
			return ev, true
		}
	}
	return watchEvent{}, false
}

func requireEvent(t *testing.T, h *harness, kind, contains string) watchEvent {
	t.Helper()
	ev, ok := findEvent(h, kind, contains)
	if !ok {
		t.Fatalf("没有找到 %s 事件 %q；现有: %v", kind, contains, h.a.Engine.watch.recent(20))
	}
	return ev
}

func TestWatchEventForCompletedRequest(t *testing.T) {
	h := newHarness(t)
	up, _ := historyUpstream(t)
	h.configure(t, up.URL, modelFixture("m", "chat"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "m", Weight: 10}}}}
	})
	requireStatus(t, h.generate(t, "messages", requestFixture("messages", "auto")), 200)
	ev := requireEvent(t, h, "OK", "auto -> m")
	// 完整格式：路由 -> 模型  耗时  输出量  原生或转换
	if !regexp.MustCompile(`^auto -> m  \S+  \d+ tok  converted$`).MatchString(ev.Text) {
		t.Errorf("完成事件格式不对: %q", ev.Text)
	}
	// 直接按模型调用时不写「路由 -> 模型」
	requireStatus(t, h.generate(t, "chat", requestFixture("chat", "m")), 200)
	direct, ok := findEvent(h, "OK", "native")
	if !ok || strings.Contains(direct.Text, "->") {
		t.Errorf("直接调用事件不对: %v", direct)
	}
	req, errs := h.a.Engine.watch.series()
	if req[watchMinutes-1]+req[watchMinutes-2] != 2 || errs[watchMinutes-1]+errs[watchMinutes-2] != 0 {
		t.Errorf("趋势统计不对: %v %v", req[len(req)-3:], errs[len(errs)-3:])
	}
}

func TestWatchEventForFailedRequestAndCooldown(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(429)
	}))
	t.Cleanup(up.Close)
	h.configure(t, up.URL, modelFixture("m", "chat"))
	h.generate(t, "chat", requestFixture("chat", "m"))
	fail := requireEvent(t, h, "FAIL", "UPSTREAM_429")
	if !strings.Contains(fail.Text, "m  UPSTREAM_429") {
		t.Errorf("失败事件格式: %q", fail.Text)
	}
	cool := requireEvent(t, h, "COOL", "m")
	if !strings.Contains(cool.Text, "cooldown 45.0s") {
		t.Errorf("冷却事件应带时长: %q", cool.Text)
	}
	_, errs := h.a.Engine.watch.series()
	if errs[watchMinutes-1]+errs[watchMinutes-2] != 1 {
		t.Errorf("失败应计入趋势: %v", errs[len(errs)-3:])
	}
}

func TestWatchEventForRejectedRequestIsSanitized(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", dropModel("b", "chat", false))
	body := requestFixture("messages", "b")
	body["messages"] = []any{Object{"role": "user", "content": []any{Object{"type": "evil\x1b[31mRED\x1b]0;pwned\x07"}}}}
	requireStatus(t, h.generate(t, "messages", body), 400)
	ev := requireEvent(t, h, "REJ", "NO_COMPATIBLE_MODEL")
	if strings.ContainsAny(ev.Text, "\x1b\x07") || !strings.Contains(ev.Text, "RED") {
		t.Errorf("拒绝事件必须清洗客户端字符串，且保留可见内容: %q", ev.Text)
	}
	if !strings.HasPrefix(ev.Text, "b  ") {
		t.Errorf("拒绝事件应以请求的模型开头: %q", ev.Text)
	}
}

func TestWatchEventForAlertAndJob(t *testing.T) {
	h := newHarness(t)
	// 推送通道没启用时告警也要出现在监控里：它本来就是「发生了什么」
	h.a.alert("route", "k", "路由 auto 的全部候选都不可用\n第二行")
	ev := requireEvent(t, h, "ALERT", "auto")
	if strings.Contains(ev.Text, "\n") || !strings.Contains(ev.Text, "route") {
		t.Errorf("告警事件格式: %q", ev.Text)
	}
	h.a.recordJob("schedule.report", taskRun{At: now(), Status: "succeeded", Result: "日报已推送"})
	job := requireEvent(t, h, "JOB", "schedule.report")
	if !strings.Contains(job.Text, "succeeded") {
		t.Errorf("任务事件应带结果状态: %q", job.Text)
	}
}

// 配置变更与其他管理操作都写审计表，监控直接按审计表推送，不必逐个操作接线
func TestWatchEventForAdminActions(t *testing.T) {
	old := watchAuditEvery
	watchAuditEvery = 20 * time.Millisecond
	t.Cleanup(func() { watchAuditEvery = old })
	h := newHarness(t)
	sub, _, err := h.a.Engine.watch.subscribe("t") // 没有人在看时不轮询，所以先登记一个观察者
	if err != nil {
		t.Fatal(err)
	}
	defer h.a.Engine.watch.unsubscribe(sub)
	time.Sleep(60 * time.Millisecond)
	h.createKey(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("m", "chat"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, a := findEvent(h, "ADMIN", "apikey.create")
		_, b := findEvent(h, "ADMIN", "cfg v")
		if a && b {
			if ev := requireEvent(t, h, "ADMIN", "apikey.create"); strings.Contains(ev.Text, "prism_sk_") {
				t.Fatalf("事件里不应出现密钥: %q", ev.Text)
			}
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("管理操作没有出现在监控里: %v", h.a.Engine.watch.recent(20))
}

// 没有人在看的时候不能白白查库，也不能把之前积压的审计记录在有人连上时一股脑推出来
func TestWatchAuditPollingSkipsWhenNobodyWatches(t *testing.T) {
	old := watchAuditEvery
	watchAuditEvery = 20 * time.Millisecond
	t.Cleanup(func() { watchAuditEvery = old })
	h := newHarness(t)
	h.createKey(t)
	time.Sleep(150 * time.Millisecond)
	sub, _, err := h.a.Engine.watch.subscribe("t")
	if err != nil {
		t.Fatal(err)
	}
	defer h.a.Engine.watch.unsubscribe(sub)
	time.Sleep(150 * time.Millisecond)
	if _, ok := findEvent(h, "ADMIN", "apikey.create"); ok {
		t.Fatal("连接之前发生的审计记录不应被补推")
	}
}
