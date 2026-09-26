package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// webhookSink 接收推送，并把通道配置为 webhook
func webhookSink(t *testing.T, h *harness) func() []string {
	t.Helper()
	var mu sync.Mutex
	got := []string{}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
	}))
	t.Cleanup(hook.Close)
	w, _ := h.rpc(t, "alert.save", Object{"enabled": true, "kind": "webhook", "secret": hook.URL, "events": Object{}}, h.token)
	requireStatus(t, w, 200)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, got...)
	}
}

func runScheduled(t *testing.T, h *harness, task string) Object {
	t.Helper()
	w, out := h.rpc(t, "schedule.run", Object{"task": task}, h.token)
	requireStatus(t, w, 200)
	return obj(out["data"])
}

// 默认值：备份与日报关闭；参数越界被拒；保存后回读一致
func TestScheduleConfig(t *testing.T) {
	h := newHarness(t)
	_, out := h.rpc(t, "schedule.get", Object{}, h.token)
	d := obj(out["data"])
	c := obj(d["config"])
	if c["backup_enabled"] != false || c["report_enabled"] != false || c["quota_enabled"] != true || num(c, "utc_offset") != 8 || d["channel"] != false {
		t.Fatalf("默认配置不对: %v", d)
	}
	for _, bad := range []Object{{"report_hour": 24}, {"backup_keep": 0}, {"quota_percent": 101}, {"report_hour": "9"}} {
		if w, _ := h.rpc(t, "schedule.save", bad, h.token); w.Code != 400 {
			t.Fatalf("%v 应被拒绝: %d", bad, w.Code)
		}
	}
	w, out := h.rpc(t, "schedule.save", Object{"report_enabled": true, "report_hour": 7}, h.token)
	requireStatus(t, w, 200)
	if c := obj(obj(out["data"])["config"]); c["report_enabled"] != true || num(c, "report_hour") != 7 || num(c, "backup_keep") != 7 {
		t.Fatalf("保存后应只覆盖提交的字段: %v", c)
	}
	if w, _ := h.rpc(t, "schedule.run", Object{"task": "nope"}, h.token); w.Code != 400 {
		t.Fatalf("未知任务应被拒绝: %d", w.Code)
	}
}

// 每日任务：过了设定时刻且今天没跑过才触发；按设定时区划分「今天」
func TestDailyDue(t *testing.T) {
	c := defaultSchedule // UTC+8
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	cases := []struct {
		last, now string
		want      bool
	}{
		{"1970-01-01T00:00:00Z", "2026-09-26T00:30:00Z", false}, // 本地 08:30，未到 9 点
		{"1970-01-01T00:00:00Z", "2026-09-26T01:00:00Z", true},  // 本地 09:00
		{"2026-09-26T01:00:00Z", "2026-09-26T10:00:00Z", false}, // 本地同一天已跑过
		{"2026-09-26T01:00:00Z", "2026-09-27T01:30:00Z", true},  // 次日
		{"2026-09-25T16:30:00Z", "2026-09-26T01:00:00Z", false}, // UTC 看是前一天，本地已是 26 日 00:30
	}
	for _, x := range cases {
		if got := dailyDue(c, c.ReportHour, at(x.last), at(x.now)); got != x.want {
			t.Errorf("last=%s now=%s: got %v", x.last, x.now, got)
		}
	}
}

// 自动备份只轮转自己的文件，手动备份保留
func TestScheduleBackupRotation(t *testing.T) {
	h := newHarness(t)
	requireStatus(t, func() *httptest.ResponseRecorder { w, _ := h.rpc(t, "backup.create", Object{}, h.token); return w }(), 200)
	h.rpc(t, "schedule.save", Object{"backup_keep": 2}, h.token)
	for i := 0; i < 3; i++ {
		if r := runScheduled(t, h, "backup"); str(r, "status") != "succeeded" {
			t.Fatalf("备份失败: %v", r)
		}
		time.Sleep(5 * time.Millisecond)
	}
	entries, _ := os.ReadDir(h.s.backupDir())
	auto, manual := 0, 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gateway-auto-") {
			auto++
		} else {
			manual++
		}
	}
	if auto != 2 || manual != 1 {
		t.Fatalf("应保留 2 份自动备份与 1 份手动备份: auto=%d manual=%d", auto, manual)
	}
	// 有动作的运行写入任务列表
	_, out := h.rpc(t, "job.list", Object{}, h.token)
	if n := len(arr(out["data"])); n != 3 {
		t.Fatalf("任务列表应有 3 条备份记录: %d", n)
	}
}

// 额度预警：达到阈值推一次，同一窗口同一重置周期不重复；未配置通道时记为失败
func TestScheduleQuotaWarning(t *testing.T) {
	up := ccUpstream(t, now()+int64(50*time.Hour/time.Millisecond))
	defer up.Close()
	h := newHarness(t)
	h.change(t, func(c *Config) { c.Providers = append(c.Providers, ccProvider(up.URL)) })
	if r := runScheduled(t, h, "quota"); str(r, "status") != "skipped" || !strings.Contains(str(r, "result"), "推送通道未启用") {
		t.Fatalf("未配置通道应记为未推送: %v", r)
	}
	if _, out := h.rpc(t, "job.list", Object{}, h.token); len(arr(out["data"])) != 0 {
		t.Fatalf("未推送不应写入任务列表: %v", out["data"])
	}
	msgs := webhookSink(t, h)
	runScheduled(t, h, "quota")
	runScheduled(t, h, "quota")
	got := msgs()
	// 本周 35/35 超过 80%，5 小时 2/14 未超过
	if len(got) != 1 || !strings.Contains(got[0], "本周窗口已用 100%") || strings.Contains(got[0], "5 小时") {
		t.Fatalf("应只推送一次本周窗口预警: %v", got)
	}
	_, out := h.rpc(t, "schedule.get", Object{}, h.token)
	if r := obj(obj(obj(out["data"])["runs"])["quota"]); str(r, "status") != "succeeded" || str(r, "result") != "" {
		t.Fatalf("去重后的运行应无事可做: %v", r)
	}
}

// 日报：按设定时区统计昨天，含成功率、花费、常用模型与失败最多的模型
func TestDailyReport(t *testing.T) {
	h := newHarness(t)
	c := defaultSchedule
	nowAt := time.Date(2026, 9, 26, 9, 0, 0, 0, c.zone())
	y := time.Date(2026, 9, 25, 12, 0, 0, 0, c.zone()).UnixMilli()
	insertUsage(t, h, "a", "p", "success", "", y, 2e9, true, false)
	insertUsage(t, h, "a", "p", "success", "", y, 1e9, true, false)
	insertUsage(t, h, "b", "p", "error", "", y, 0, true, false)
	insertUsage(t, h, "a", "p", "success", "", nowAt.UnixMilli(), 5e9, true, false) // 今天的不计入
	text, err := h.a.dailyReport(c, nowAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"日报 2026-09-25（UTC+8）", "请求 3 次，成功率 66.7%，失败 1 次", "估算花费 $3.00", "常用模型：a 2 次 · b 1 次", "花费最多的 Key：k $3.00", "失败最多：b 1 次"} {
		if !strings.Contains(text, want) {
			t.Errorf("日报缺少「%s」:\n%s", want, text)
		}
	}
	if text, _ = h.a.dailyReport(c, nowAt.AddDate(0, 0, 5)); !strings.Contains(text, "昨日没有请求") {
		t.Errorf("没有请求时应说明: %s", text)
	}
}

// Key 到期提醒：提前期内的 Key 提醒一次；已过期或期外的不提醒
func TestScheduleKeyExpiry(t *testing.T) {
	h := newHarness(t)
	for name, at := range map[string]int64{"soon": now() + 86400000, "later": now() + 10*86400000, "gone": now() - 1000} {
		requireStatus(t, func() *httptest.ResponseRecorder {
			w, _ := h.rpc(t, "apikey.create", Object{"name": name, "allowed": []any{}, "expires_at": at}, h.token)
			return w
		}(), 200)
	}
	msgs := webhookSink(t, h)
	runScheduled(t, h, "expiry")
	runScheduled(t, h, "expiry")
	got := msgs()
	if len(got) != 1 || !strings.Contains(got[0], "「soon」") || strings.Contains(got[0], "later") || strings.Contains(got[0], "gone") {
		t.Fatalf("应只提醒一次 soon: %v", got)
	}
}

// 上游检查：价格与上游声明不一致时列入并推送一次；本地改成一致后立即不再列出
func TestSchedulePriceDrift(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"upstream-a","pricing":{"prompt":"0.000002","completion":"0.000008"}},{"id":"upstream-b","pricing":{"prompt":"-1","completion":"-1"}}]}`)
	}))
	defer up.Close()
	h := newHarness(t)
	a, b := modelFixture("a", "chat"), modelFixture("b", "chat")
	a.PricingSet, a.InputPrice, a.OutputPrice = true, 1, 8
	b.PricingSet, b.InputPrice = true, 5
	h.configure(t, up.URL, a, b)
	// 通道未启用时发现的变动，启用后下一次运行要补推
	if r := runScheduled(t, h, "upstream"); str(r, "status") != "skipped" {
		t.Fatalf("通道未启用应记为未推送: %v", r)
	}
	msgs := webhookSink(t, h)
	runScheduled(t, h, "upstream")
	runScheduled(t, h, "upstream")
	if got := msgs(); len(got) != 1 || !strings.Contains(got[0], "a 的上游价格为 $2 / $8") || strings.Contains(got[0], "b 的") {
		t.Fatalf("应只推送一次 a 的价格变动（b 是动态价格不比较）: %v", got)
	}
	_, out := h.rpc(t, "model.stats", Object{}, h.token)
	if d := arr(obj(out["data"])["price_drift"]); len(d) != 1 || str(obj(d[0]), "id") != "a" {
		t.Fatalf("模型库应附带价格变动: %v", d)
	}
	h.change(t, func(c *Config) { c.Models[0].InputPrice = 2 })
	if d := h.a.driftList(); len(d) != 0 {
		t.Fatalf("本地价格改成一致后不应再列出: %v", d)
	}
}

// 数据库维护可以正常执行
func TestScheduleMaintain(t *testing.T) {
	h := newHarness(t)
	if r := runScheduled(t, h, "maintain"); str(r, "status") != "succeeded" {
		t.Fatalf("维护失败: %v", r)
	}
}

// 纯文本 Webhook：原样发送正文，text/plain，标题独占一行、明细逐行
func TestWebhookPlainText(t *testing.T) {
	type got struct{ ctype, body string }
	ch := make(chan got, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- got{r.Header.Get("Content-Type"), string(b)}
	}))
	defer hook.Close()
	h := newHarness(t)
	w, _ := h.rpc(t, "alert.save", Object{"enabled": true, "kind": "webhook_text", "secret": hook.URL, "events": Object{}}, h.token)
	requireStatus(t, w, 200)
	if err := h.a.notify("额度预警\n第一行\n第二行"); err != nil {
		t.Fatal(err)
	}
	g := <-ch
	if g.ctype != "text/plain; charset=utf-8" || g.body != "【Prism Gateway】\n额度预警\n第一行\n第二行" {
		t.Fatalf("纯文本推送不对: %q %q", g.ctype, g.body)
	}
}

// 重启后上游检查要在第一轮就跑：同一轮里先跑的任务会从库里重读状态，不能把清掉的运行时间读回来
func TestScheduleRunOnStartSurvivesReload(t *testing.T) {
	h := newHarness(t)
	h.a.sched.mu.Lock()
	h.a.loadScheduleState()
	recent := now() - 60000
	h.a.sched.state.Runs["upstream"] = taskRun{At: recent, Status: "succeeded"}
	h.a.sched.state.Runs["quota"] = taskRun{At: recent, Status: "succeeded"}
	h.a.saveScheduleState()
	h.a.sched.mu.Unlock()
	h.a.resetOnStart()
	h.a.scheduleTick(time.Now())
	st, _ := h.a.readScheduleState()
	if st.Runs["upstream"].At <= recent || st.Runs["quota"].At <= recent {
		t.Fatalf("启动后第一轮应重跑额度预警与上游检查: %v", st.Runs)
	}
}
