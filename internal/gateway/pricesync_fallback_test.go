package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func priceSyncCatalog(t *testing.T, h *harness, catalog Object, calls *atomic.Int32) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("目录请求泄露供应商凭证")
		}
		if calls != nil {
			calls.Add(1)
		}
		io.WriteString(w, raw(catalog))
	}))
	t.Cleanup(server.Close)
	h.a.pricingClient = modelsDevTestClient(t, server, nil)
}

func TestUpstreamPricesRejectInvalidCacheRates(t *testing.T) {
	for _, field := range []string{"input_cache_read", "input_cache_write"} {
		pricing := Object{"prompt": "0.000001", "completion": "0.000002", field: "invalid"}
		if len(collectSyncedPrices([]any{Object{"id": "m", "pricing": pricing}})) != 0 {
			t.Fatalf("非法 %s 不能作为完整上游价格", field)
		}
	}
}

func TestPriceSyncPerModelFallbackAndStableVersion(t *testing.T) {
	var upstreamCalls, catalogCalls atomic.Int32
	upstream := priceSyncProvider(t, `{"data":[{"id":"native","pricing":{"prompt":"0.000001","completion":"0.000002"}},{"id":"fallback"},{"id":"invalid","pricing":{"prompt":"-1","completion":"0"}}]}`, &upstreamCalls)
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p", Name: "Custom", Kind: "custom", BaseURL: upstream.URL, Auth: "bearer", Secret: "test-secret", Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		c.Models = []Model{pricedModel("a", "p", "native", 10, 20, 30, 40), pricedModel("b", "p", "fallback", 10, 20, 30, 40), pricedModel("c", "p", "invalid", 10, 20, 30, 40)}
	})
	priceSyncCatalog(t, h, Object{"catalog": modelsDevTestProvider(upstream.URL, Object{
		"native":   Object{"cost": Object{"input": 9, "output": 9}},
		"fallback": Object{"cost": Object{"input": 3, "output": 4, "tiers": []any{Object{"tier": Object{"type": "context", "size": 100}, "input": 6, "output": 8}}}},
		"invalid":  Object{"cost": Object{"input": 0, "output": 0}},
	})}, &catalogCalls)
	result, err := h.a.taskPriceSync(scheduleConfig{})
	if err != nil || !strings.Contains(result, "3 个模型") || catalogCalls.Load() != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("result=%q err=%v calls=%d/%d", result, err, upstreamCalls.Load(), catalogCalls.Load())
	}
	native, _ := h.s.Config().model("a")
	fallback, _ := h.s.Config().model("b")
	zero, _ := h.s.Config().model("c")
	if native.InputPrice != 1 || native.PriceSource != "upstream" || fallback.InputPrice != 3 || fallback.PriceSource != "models.dev:catalog" || len(fallback.PriceTiers) != 1 || zero.InputPrice != 0 || !zero.PricingSet || fallback.PriceUpdatedAt == 0 {
		t.Fatalf("价格来源或分档不正确: %+v %+v %+v", native, fallback, zero)
	}
	version, updated := h.s.Config().Version, fallback.PriceUpdatedAt
	result, err = h.a.taskPriceSync(scheduleConfig{})
	fallback, _ = h.s.Config().model("b")
	if err != nil || result != "" || h.s.Config().Version != version || fallback.PriceUpdatedAt != updated {
		t.Fatalf("无变化应保留版本和更新时间: %q %v", result, err)
	}
}

func TestPriceSyncUpstreamFailureFallsBack(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) }))
	t.Cleanup(upstream.Close)
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p", Name: "Custom", Kind: "custom", BaseURL: upstream.URL, Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		c.Models = []Model{pricedModel("m", "p", "model", 10, 20, 30, 40)}
	})
	priceSyncCatalog(t, h, Object{"custom-feed": modelsDevTestProvider(upstream.URL, Object{"model": Object{"cost": Object{"input": 2, "output": 5}}})}, nil)
	if result, err := h.a.taskPriceSync(scheduleConfig{}); err != nil || !strings.Contains(result, "上游模型接口失败") {
		t.Fatalf("应报告上游失败后的兜底: %q %v", result, err)
	}
	m, _ := h.s.Config().model("m")
	if m.InputPrice != 2 || m.PriceSource != "models.dev:custom-feed" {
		t.Fatalf("上游失败未兜底: %+v", m)
	}
}

func TestPriceSyncCatalogFailurePreservesUnknown(t *testing.T) {
	var calls atomic.Int32
	upstream := priceSyncProvider(t, `{"data":[{"id":"model"}]}`, &calls)
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p", Name: "Custom", Kind: "custom", BaseURL: upstream.URL, Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		m := pricedModel("m", "p", "model", 10, 20, 30, 40)
		m.PricingSet = false
		c.Models = []Model{m}
	})
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502); io.WriteString(w, "secret-body") }))
	t.Cleanup(failed.Close)
	h.a.pricingClient = modelsDevTestClient(t, failed, nil)
	before := raw(h.s.Config())
	result, err := h.a.taskPriceSync(scheduleConfig{})
	if err == nil || !strings.Contains(err.Error(), "models.dev 不可用") || strings.Contains(err.Error(), "secret-body") || result != "" || before != raw(h.s.Config()) {
		t.Fatalf("全失败应保留未知计价且报告安全错误: %q %v", result, err)
	}
}

func TestPriceSyncSkipsCatalogWhenUpstreamComplete(t *testing.T) {
	var upstreamCalls, catalogCalls atomic.Int32
	upstream := priceSyncProvider(t, `{"data":[{"id":"m","pricing":{"prompt":"0","completion":"0"}}]}`, &upstreamCalls)
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p", Name: "Custom", Kind: "custom", BaseURL: upstream.URL, Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		c.Models = []Model{pricedModel("m", "p", "m", 1, 2, 3, 4)}
	})
	priceSyncCatalog(t, h, Object{}, &catalogCalls)
	if _, err := h.a.taskPriceSync(scheduleConfig{}); err != nil {
		t.Fatal(err)
	}
	if catalogCalls.Load() != 0 {
		t.Fatal("上游完整时不应读取目录")
	}
	h.change(t, func(c *Config) { c.Models[0].PriceLocked = true })
	if result, err := h.a.taskPriceSync(scheduleConfig{}); err != nil || result != "" || upstreamCalls.Load() != 1 || catalogCalls.Load() != 0 {
		t.Fatalf("全锁定时不应请求网络: %q %v", result, err)
	}
}
