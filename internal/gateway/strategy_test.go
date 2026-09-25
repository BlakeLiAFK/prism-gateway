package gateway

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// strategyFixture 建两个不同厂商的供应商和三个 chat 模型：a、b 在 p_x，c 在 p_y
func strategyFixture(t *testing.T, h *harness, strategy string) {
	t.Helper()
	h.change(t, func(c *Config) {
		c.Providers = []Provider{
			{ID: "p_x", Name: "x", Kind: "openai", BaseURL: "https://x.example/v1", Auth: "auto", Enabled: true, TimeoutSec: 30},
			{ID: "p_y", Name: "y", Kind: "deepseek", BaseURL: "https://y.example/v1", Auth: "auto", Enabled: true, TimeoutSec: 30},
		}
		a, b, cc := modelFixture("a", "chat"), modelFixture("b", "chat"), modelFixture("c", "chat")
		a.ProviderID, b.ProviderID, cc.ProviderID = "p_x", "p_x", "p_y"
		a.InputPrice, a.OutputPrice, a.PricingSet = 3, 15, true
		b.InputPrice, b.OutputPrice, b.PricingSet = 0.3, 1.2, true
		c.Models = []Model{a, b, cc}
		c.Routes = []Route{{ID: "r", Name: "r", Strategy: strategy, Enabled: true,
			Candidates: []Candidate{{"a", 30}, {"b", 20}, {"c", 10}}}}
	})
}

func rankedIDs(t *testing.T, h *harness) string {
	t.Helper()
	w, out := h.rpc(t, "route.test", Object{"id": "r"}, h.token)
	requireStatus(t, w, 200)
	var ids []string
	for _, x := range arr(obj(out["data"])["ranked"]) {
		ids = append(ids, str(obj(x), "model_id"))
	}
	return strings.Join(ids, ",")
}

func TestStrategyOrdering(t *testing.T) {
	cases := []struct {
		strategy string
		setup    func(e *Engine)
		want     string
	}{
		{"priority", nil, "a,b,c"},
		// 未确认计价的 c 排最后：价格未知不等于免费
		{"cost", nil, "b,a,c"},
		// 没有样本的 c 先试；其余按响应头耗时升序
		{"latency", func(e *Engine) { e.state("a").TTFB = 900; e.state("b").TTFB = 200 }, "c,b,a"},
		{"least_busy", func(e *Engine) { e.state("a").Active = 4; e.state("b").Active = 2 }, "c,b,a"},
		// 冷却中与已满的候选沉底，其余保持优先级顺序
		{"priority", func(e *Engine) { e.state("a").Cooldown = now() + 60000 }, "b,c,a"},
		{"priority", func(e *Engine) { e.state("b").Active = 8 }, "a,c,b"},
	}
	for _, tc := range cases {
		h := newHarness(t)
		strategyFixture(t, h, tc.strategy)
		if tc.setup != nil {
			tc.setup(h.a.Engine)
		}
		if got := rankedIDs(t, h); got != tc.want {
			t.Errorf("%s: 排序 %s，期望 %s", tc.strategy, got, tc.want)
		}
	}
}

// 首选概率应正比于权重：a:b:c = 30:20:10
func TestWeightedFollowsWeights(t *testing.T) {
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{
			{ID: "p_x", Name: "x", Kind: "openai", BaseURL: "https://x.example/v1", Auth: "auto", Enabled: true, TimeoutSec: 30},
			{ID: "p_y", Name: "y", Kind: "deepseek", BaseURL: "https://y.example/v1", Auth: "auto", Enabled: true, TimeoutSec: 30},
			{ID: "p_z", Name: "z", Kind: "zai", BaseURL: "https://z.example/v1", Auth: "auto", Enabled: true, TimeoutSec: 30},
		}
		a, b, cc := modelFixture("a", "chat"), modelFixture("b", "chat"), modelFixture("c", "chat")
		a.ProviderID, b.ProviderID, cc.ProviderID = "p_x", "p_y", "p_z"
		c.Models = []Model{a, b, cc}
		c.Routes = []Route{{ID: "r", Name: "r", Strategy: "weighted", Enabled: true,
			Candidates: []Candidate{{"a", 30}, {"b", 20}, {"c", 10}}}}
	})
	const n = 3000
	first := map[string]int{}
	for i := 0; i < n; i++ {
		first[strings.Split(rankedIDs(t, h), ",")[0]]++
	}
	for id, want := range map[string]float64{"a": 0.5, "b": 1.0 / 3, "c": 1.0 / 6} {
		if got := float64(first[id]) / n; math.Abs(got-want) > 0.04 {
			t.Errorf("%s 首选占比 %.3f，期望约 %.3f", id, got, want)
		}
	}
}

// 真实转发成功后应留下响应头耗时样本，并出现在运行状态里
func TestUpstreamTTFBRecorded(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("m", "chat"))
	requireStatus(t, h.generate(t, "chat", requestFixture("chat", "m")), 200)
	h.a.Engine.mu.Lock()
	first := h.a.Engine.state("m").TTFB
	h.a.Engine.mu.Unlock()
	if first <= 0 {
		t.Fatalf("应记录响应头耗时，实得 %v", first)
	}
	if _, ok := obj(obj(h.a.Engine.Health()["models"])["m"])["ttfb_ms"]; !ok {
		t.Fatal("运行状态应包含 ttfb_ms")
	}
	// 滑动平均：新样本占 30%
	h.a.Engine.state("m").TTFB = 1000
	h.a.Engine.recordTTFB("m", 1)
	if got := h.a.Engine.state("m").TTFB; got != 700.3 {
		t.Fatalf("滑动平均应为 700.3，实得 %v", got)
	}
}
