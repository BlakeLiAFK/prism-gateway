package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestSpendAnomalyScheduleHandler(t *testing.T) {
	h := newHarness(t)
	msgs := webhookSink(t, h)
	c := defaultSchedule
	local := time.Now().In(c.zone())
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.zone())
	for day := 1; day <= 7; day++ {
		insertUsage(t, h, "m", "p", "success", "", today.AddDate(0, 0, -day).Add(time.Hour).UnixMilli(), 1e9, true, false)
	}
	insertUsage(t, h, "m", "p", "success", "", time.Now().Add(-time.Minute).UnixMilli(), 3e9, true, false)
	insertUsage(t, h, "m", "p", "success", "", time.Now().Add(-time.Minute).UnixMilli(), 0, false, false)
	insertUsage(t, h, "m", "p", "success", "", today.AddDate(0, 0, -1).Add(time.Hour).UnixMilli(), 0, false, false)
	if err := h.s.DB.Exec("DELETE FROM requests"); err != nil {
		t.Fatal(err)
	}

	w, out := h.rpc(t, "schedule.save", Object{"spend_enabled": true, "spend_multiple": 2.5, "spend_minimum": 2}, h.token)
	requireStatus(t, w, 200)
	if c := obj(obj(out["data"])["config"]); c["spend_enabled"] != true || num(c, "spend_multiple") != 2.5 || num(c, "spend_minimum") != 2 {
		t.Fatalf("花费异常配置未持久化: %v", c)
	}
	if w, _ = h.rpc(t, "schedule.save", Object{"spend_multiple": 0}, h.token); w.Code != 400 {
		t.Fatalf("非法倍数应被拒绝: %d", w.Code)
	}

	first := runScheduled(t, h, "spend")
	second := runScheduled(t, h, "spend")
	if str(first, "status") != "succeeded" || str(second, "result") != "" {
		t.Fatalf("花费异常运行结果不对: first=%v second=%v", first, second)
	}
	got := msgs()
	if len(got) != 1 || !strings.Contains(got[0], "今日已记录花费 $3.00") || !strings.Contains(got[0], "前 7 个完整日均值 $1.00") || !strings.Contains(got[0], "今日 1 次、前 7 日 1 次成功请求价格未确认") || !strings.Contains(got[0], "可能含本地估算与异常预留") {
		t.Fatalf("花费异常通知不对: %v", got)
	}
}
