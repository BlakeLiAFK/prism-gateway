package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func priceSyncProvider(t *testing.T, body string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func pricedModel(id, provider, upstream string, input, output, cache, write float64) Model {
	m := modelFixture(id, "chat")
	m.ProviderID, m.Upstream = provider, upstream
	m.InputPrice, m.OutputPrice, m.CachePrice, m.WritePrice = input, output, cache, write
	m.PricingSet = true
	return m
}

func TestPriceSyncAllProvidersLocksAndMissingCache(t *testing.T) {
	var firstCalls, secondCalls, disabledCalls atomic.Int32
	first := priceSyncProvider(t, `{"data":[
		{"id":"vendor/a","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000001"}},
		{"id":"vendor/free","pricing":{"prompt":"0","completion":"0"}},
		{"id":"vendor/locked","pricing":{"prompt":"0.000009","completion":"0.000009"}},
		{"id":"vendor/negative","pricing":{"prompt":"-0.1","completion":"0.000002"}},
		{"id":"vendor/bad","pricing":{"prompt":"0.000001","completion":"unknown"}}
	]}`, &firstCalls)
	second := priceSyncProvider(t, `{"data":[{"id":"vendor/b","pricing":{"prompt":0.000003,"completion":0.000004,"input_cache_write":0.0000005}}]}`, &secondCalls)
	disabled := priceSyncProvider(t, `{"data":[{"id":"vendor/off","pricing":{"prompt":"0.1","completion":"0.1"}}]}`, &disabledCalls)
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{
			{ID: "p_one", Name: "OpenRouter One", BaseURL: first.URL + "/openrouter.ai/api/v1", Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
			{ID: "p_two", Name: "OpenRouter Two", BaseURL: second.URL + "/openrouter.ai/api/v1", Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
			{ID: "p_off", Name: "Disabled", BaseURL: disabled.URL + "/openrouter.ai/api/v1", Auth: "none", Enabled: false, AllowPrivate: true, TimeoutSec: 30},
		}
		locked := pricedModel("locked", "p_one", "vendor/locked", 7, 8, 9, 10)
		locked.PriceLocked = true
		free := pricedModel("free", "p_one", "vendor/free", 0, 0, 0, 0)
		free.PricingSet = false
		c.Models = []Model{
			pricedModel("a", "p_one", "vendor/a", 10, 20, 30, 40),
			pricedModel("b", "p_two", "vendor/b", 10, 20, 30, 40),
			free,
			locked,
			pricedModel("negative", "p_one", "vendor/negative", 11, 12, 13, 14),
			pricedModel("bad", "p_one", "vendor/bad", 15, 16, 17, 18),
			pricedModel("off", "p_off", "vendor/off", 19, 20, 21, 22),
		}
	})

	result, err := h.a.taskPriceSync(scheduleConfig{})
	if err != nil || !strings.Contains(result, "3 个模型") || firstCalls.Load() != 1 || secondCalls.Load() != 1 || disabledCalls.Load() != 0 {
		t.Fatalf("同步结果不正确: result=%q err=%v calls=%d/%d/%d", result, err, firstCalls.Load(), secondCalls.Load(), disabledCalls.Load())
	}
	a, _ := h.s.Config().model("a")
	if !a.PricingSet || a.InputPrice != 1 || a.OutputPrice != 2 || a.CachePrice != .1 || a.WritePrice != 40 {
		t.Errorf("A 价格不正确，缺失写缓存价应保留: %+v", a)
	}
	b, _ := h.s.Config().model("b")
	if b.InputPrice != 3 || b.OutputPrice != 4 || b.CachePrice != 30 || b.WritePrice != .5 {
		t.Errorf("B 价格不正确，缺失读缓存价应保留: %+v", b)
	}
	freeModel, _ := h.s.Config().model("free")
	if !freeModel.PricingSet || freeModel.InputPrice != 0 || freeModel.OutputPrice != 0 {
		t.Errorf("显式同步应确认上游零价格: %+v", freeModel)
	}
	for id, want := range map[string][4]float64{
		"locked":   {7, 8, 9, 10},
		"negative": {11, 12, 13, 14},
		"bad":      {15, 16, 17, 18},
		"off":      {19, 20, 21, 22},
	} {
		m, _ := h.s.Config().model(id)
		got := [4]float64{m.InputPrice, m.OutputPrice, m.CachePrice, m.WritePrice}
		if got != want {
			t.Errorf("%s 不应被修改: got=%v want=%v", id, got, want)
		}
	}
	rows, err := h.s.DB.Query("SELECT COUNT(*) n FROM audit_logs WHERE action='schedule.price_sync'")
	if err != nil || rows[0].Int("n") != 1 {
		t.Fatalf("应记录一次价格同步审计: rows=%v err=%v", rows, err)
	}

	version := h.s.Config().Version
	if result, err = h.a.taskPriceSync(scheduleConfig{}); err != nil || result != "" || h.s.Config().Version != version {
		t.Fatalf("无变化不应产生新版本: result=%q err=%v v%d→v%d", result, err, version, h.s.Config().Version)
	}
}

func TestPriceSyncFetchFailureIsAtomic(t *testing.T) {
	var calls atomic.Int32
	good := priceSyncProvider(t, `{"data":[{"id":"vendor/a","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`, &calls)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(bad.Close)
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{
			{ID: "p_good", Name: "Good", BaseURL: good.URL + "/openrouter.ai/api/v1", Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
			{ID: "p_bad", Name: "Bad", BaseURL: bad.URL + "/openrouter.ai/api/v1", Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
		}
		c.Models = []Model{pricedModel("a", "p_good", "vendor/a", 10, 20, 30, 40)}
	})
	version := h.s.Config().Version
	result, err := h.a.taskPriceSync(scheduleConfig{})
	if err == nil || !strings.Contains(err.Error(), "Bad") || result != "" {
		t.Fatalf("应返回明确的供应商错误: result=%q err=%v", result, err)
	}
	m, _ := h.s.Config().model("a")
	if m.InputPrice != 10 || h.s.Config().Version != version {
		t.Fatalf("抓取失败不应部分更新: %+v v%d→v%d", m, version, h.s.Config().Version)
	}
}

func TestPriceSyncRejectsConcurrentConfigChange(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"data":[{"id":"vendor/a","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`)
	}))
	t.Cleanup(server.Close)
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p_or", Name: "OpenRouter", BaseURL: server.URL + "/openrouter.ai/api/v1", Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		c.Models = []Model{pricedModel("a", "p_or", "vendor/a", 10, 20, 30, 40)}
	})
	type outcome struct {
		result string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := h.a.taskPriceSync(scheduleConfig{})
		done <- outcome{result, err}
	}()
	<-entered
	h.change(t, func(c *Config) { c.Settings.AppName = "concurrent change" })
	close(release)
	got := <-done
	var apiErr *APIError
	if got.result != "" || !errors.As(got.err, &apiErr) || apiErr.Code != "VERSION_CONFLICT" {
		t.Fatalf("并发更新应返回版本冲突: result=%q err=%v", got.result, got.err)
	}
	m, _ := h.s.Config().model("a")
	if m.InputPrice != 10 {
		t.Fatalf("版本冲突不应修改价格: %+v", m)
	}
}

func TestPriceSyncScheduleHandler(t *testing.T) {
	var calls atomic.Int32
	server := priceSyncProvider(t, `{"data":[{"id":"vendor/a","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`, &calls)
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p_or", Name: "OpenRouter", BaseURL: server.URL + "/openrouter.ai/api/v1", Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		c.Models = []Model{pricedModel("a", "p_or", "vendor/a", 10, 20, 30, 40)}
	})

	_, out := h.rpc(t, "schedule.get", Object{}, h.token)
	config := obj(obj(out["data"])["config"])
	if config["price_enabled"] != false || num(config, "price_hours") != 24 {
		t.Fatalf("价格同步应默认关闭且间隔 24 小时: %v", config)
	}
	w, _ := h.rpc(t, "schedule.save", Object{"price_enabled": true, "price_hours": 12}, h.token)
	requireStatus(t, w, http.StatusOK)
	w, _ = h.rpc(t, "schedule.run", Object{"task": "price"}, "")
	requireStatus(t, w, http.StatusUnauthorized)

	run := runScheduled(t, h, "price")
	if str(run, "status") != "succeeded" || !strings.Contains(str(run, "result"), "1 个模型") || calls.Load() != 1 {
		t.Fatalf("真实 schedule.run 未完成价格同步: %v calls=%d", run, calls.Load())
	}
	m, _ := h.s.Config().model("a")
	if m.InputPrice != 1 || m.OutputPrice != 2 || !m.PricingSet {
		t.Fatalf("schedule.run 未写入价格: %+v", m)
	}
}
