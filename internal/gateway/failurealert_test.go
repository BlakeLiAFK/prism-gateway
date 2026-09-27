package gateway

import (
	"strings"
	"testing"
)

func TestFailureRateScheduleHandler(t *testing.T) {
	h := newHarness(t)
	msgs := webhookSink(t, h)
	at := now() - 60000
	for i := 0; i < 6; i++ {
		insertUsage(t, h, "m", "p", "success", "", at, 0, true, false)
	}
	for i := 0; i < 3; i++ {
		insertUsage(t, h, "m", "p", "error", "", at, 0, true, false)
	}
	insertUsage(t, h, "m", "p", "unknown", "", at, 0, false, false)
	insertUsage(t, h, "m", "p", "error", "", now()-3600000, 0, true, false)
	insertUsage(t, h, "m", "p", "error", "", at, 0, true, true)
	insertUsage(t, h, "m", "p", "running", "", at, 0, false, false)

	w, out := h.rpc(t, "schedule.save", Object{"failure_enabled": true, "failure_minutes": 15, "failure_min_samples": 10, "failure_percent": 30, "failure_cooldown_minutes": 45}, h.token)
	requireStatus(t, w, 200)
	config := obj(obj(out["data"])["config"])
	if config["failure_enabled"] != true || num(config, "failure_min_samples") != 10 || num(config, "failure_cooldown_minutes") != 45 {
		t.Fatalf("失败率配置未持久化: %v", config)
	}
	if w, _ = h.rpc(t, "schedule.save", Object{"failure_percent": 101}, h.token); w.Code != 400 {
		t.Fatalf("非法失败率应被拒绝: %d", w.Code)
	}

	first := runScheduled(t, h, "failure")
	second := runScheduled(t, h, "failure")
	if str(first, "status") != "succeeded" || str(second, "result") != "" {
		t.Fatalf("失败率任务运行结果不对: first=%v second=%v", first, second)
	}
	got := msgs()
	if len(got) != 1 || !strings.Contains(got[0], "最近 15 分钟 4/10 次失败") || !strings.Contains(got[0], "错误 3 次，状态未知 1 次") {
		t.Fatalf("失败率通知不对: %v", got)
	}
	state, err := h.a.readScheduleState()
	if err != nil || state.Warned["failure-rate"] < now()+44*60000 {
		t.Fatalf("失败率冷却未持久化: %v %v", state.Warned, err)
	}
}

func TestFailureRateDeliveryFailureDoesNotStartCooldown(t *testing.T) {
	h := newHarness(t)
	at := now() - 60000
	for i := 0; i < 5; i++ {
		insertUsage(t, h, "m", "p", "error", "", at, 0, true, false)
	}
	h.rpc(t, "schedule.save", Object{"failure_enabled": true, "failure_minutes": 15, "failure_min_samples": 5, "failure_percent": 50}, h.token)
	if run := runScheduled(t, h, "failure"); str(run, "status") != "skipped" {
		t.Fatalf("推送通道未启用时应标记未推送: %v", run)
	}
	state, err := h.a.readScheduleState()
	if err != nil || state.Warned["failure-rate"] != 0 {
		t.Fatalf("推送失败不应进入冷却: %v %v", state.Warned, err)
	}
}
