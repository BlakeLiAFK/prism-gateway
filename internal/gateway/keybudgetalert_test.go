package gateway

import (
	"strings"
	"testing"
)

func TestKeyBudgetScheduleHandler(t *testing.T) {
	h := newHarness(t)
	msgs := webhookSink(t, h)
	w, out := h.rpc(t, "apikey.create", Object{"name": "共享 Key", "allowed": []any{}, "limit_day": 10, "limit_month": 10}, h.token)
	requireStatus(t, w, 200)
	id := str(obj(out["data"]), "id")
	if err := h.s.DB.Exec(`INSERT INTO usage_hourly (hour,key_id,requested_model,model_id,provider_id,requests,success,errors,input_tokens,output_tokens,cache_tokens,cost_nano,unpriced,success_ms,last_at)
		VALUES (?,?,?,?,?,2,2,0,0,0,0,?,1,0,?)`, now()/3600000, id, "m", "m", "p", int64(8.5e9), now()); err != nil {
		t.Fatal(err)
	}
	for i, known := range []bool{true, false} {
		if err := h.s.DB.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,cost_known,usage_mode,reason)
			VALUES (?,?,?,?,?,?,'chat','chat','','success',?,?,?,'')`, randomID("r_"), "p", id, "m", "m", "p", now()-60000, known, []string{"reported_tokens", "reported_tokens"}[i]); err != nil {
			t.Fatal(err)
		}
	}

	w, out = h.rpc(t, "schedule.save", Object{"key_budget_enabled": true, "key_budget_percent": 80, "key_budget_cooldown_minutes": 120}, h.token)
	requireStatus(t, w, 200)
	config := obj(obj(out["data"])["config"])
	if config["key_budget_enabled"] != true || num(config, "key_budget_percent") != 80 || num(config, "key_budget_cooldown_minutes") != 120 {
		t.Fatalf("Key 预算预警配置未持久化: %v", config)
	}
	if w, _ = h.rpc(t, "schedule.save", Object{"key_budget_percent": 0}, h.token); w.Code != 400 {
		t.Fatalf("非法阈值应被拒绝: %d", w.Code)
	}

	first := runScheduled(t, h, "key-budget")
	second := runScheduled(t, h, "key-budget")
	if str(first, "status") != "succeeded" || str(second, "result") != "" {
		t.Fatalf("Key 预算任务运行结果不对: first=%v second=%v", first, second)
	}
	got := msgs()
	if len(got) != 1 || !strings.Contains(got[0], "共享 Key") || !strings.Contains(got[0], "近 24 小时 $8.50 / $10.00") || !strings.Contains(got[0], "近 30 天 $8.50 / $10.00") || !strings.Contains(got[0], "含 1 次本地估算，1 次费用未知") {
		t.Fatalf("Key 预算预警通知不对: %v", got)
	}
	state, err := h.a.readScheduleState()
	if err != nil {
		t.Fatal(err)
	}
	warned := 0
	for key, until := range state.Warned {
		if strings.HasPrefix(key, "key-budget:"+id+":") && until >= now()+119*60000 {
			warned++
		}
	}
	if warned != 2 {
		t.Fatalf("两个滚动窗口都应持久化冷却: %v", state.Warned)
	}
}
