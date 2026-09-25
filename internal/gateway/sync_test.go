package gateway

import "testing"

// 各家上游对上下文与输出上限的字段名不统一，常见写法都要认
func TestSyncedModelReadsCommonFields(t *testing.T) {
	m := newSyncedModel("m", "p", "m", "m", "chat", Object{"context_window": float64(200000), "max_output_tokens": float64(8000)})
	if m.Context != 200000 || m.MaxOutput != 8000 {
		t.Fatalf("字段未识别: context=%d max_output=%d", m.Context, m.MaxOutput)
	}
	m = newSyncedModel("m", "p", "m", "m", "chat", Object{"max_model_len": float64(32768)})
	if m.Context != 32768 || m.MaxOutput != 32768 {
		t.Fatalf("vLLM 字段未识别或输出上限超过上下文: %d/%d", m.Context, m.MaxOutput)
	}
}

// 路由保存时显式要求启用候选：同一事务里生效；不要求时不动模型开关
func TestRouteSaveEnablesCandidatesOnRequest(t *testing.T) {
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p_x", Name: "x", Kind: "openai", BaseURL: "https://x.example/v1", Auth: "auto", Enabled: true, TimeoutSec: 30}}
		a, b := modelFixture("a", "chat"), modelFixture("b", "chat")
		a.ProviderID, b.ProviderID = "p_x", "p_x"
		a.Enabled, b.Enabled = false, false
		c.Models = []Model{a, b}
	})
	save := func(enable bool, ids ...string) {
		cs := []any{}
		for _, id := range ids {
			cs = append(cs, Object{"model_id": id, "weight": 10})
		}
		w, out := h.rpc(t, "route.save", Object{"version": h.a.Store.Config().Version, "id": "r", "enable_models": enable,
			"route": Object{"id": "r", "name": "r", "strategy": "priority", "enabled": true, "candidates": cs}}, h.token)
		if w.Code != 200 {
			t.Fatalf("保存路由失败: %v", out)
		}
	}
	enabled := func(id string) bool { m, _ := h.a.Store.Config().model(id); return m.Enabled }
	save(false, "a")
	if enabled("a") {
		t.Fatal("未要求启用时不应改动模型开关")
	}
	save(true, "a")
	if !enabled("a") || enabled("b") {
		t.Fatalf("只应启用候选里的模型: a=%v b=%v", enabled("a"), enabled("b"))
	}
}
