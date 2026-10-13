package gateway

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestModelsDevPriceUsesExactProviderAndModel(t *testing.T) {
	catalog := Object{
		"openai": modelsDevTestProvider("https://api.openai.com/v1", Object{
			"same": Object{"cost": Object{"input": 1.25, "output": 10.0, "cache_read": 0.0}},
		}),
		"anthropic": modelsDevTestProvider("https://api.anthropic.com/v1", Object{
			"same": Object{"cost": Object{"input": 3.0, "output": 15.0}},
		}),
	}

	got, ok := modelsDevPrice(catalog, Provider{Kind: "openai", BaseURL: "https://API.OPENAI.COM/v1/"}, "same")
	if !ok || got.input != 1.25 || got.output != 10 || got.cache == nil || *got.cache != 0 || got.write != nil {
		t.Fatalf("价格单位或可选零值错误: %#v, ok=%v", got, ok)
	}
	if got.source != "models.dev:openai" {
		t.Fatalf("source=%q", got.source)
	}
	for _, tc := range []struct {
		name     string
		provider Provider
		model    string
	}{
		{"模型 ID 不猜测", Provider{Kind: "openai", BaseURL: "https://api.openai.com/v1"}, "Same"},
		{"供应商隔离", Provider{Kind: "deepseek", BaseURL: "https://api.deepseek.com"}, "same"},
		{"代理不按 kind 收原厂价", Provider{Kind: "openai", BaseURL: "https://proxy.example/v1"}, "same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := modelsDevPrice(catalog, tc.provider, tc.model); ok {
				t.Fatal("不应匹配")
			}
		})
	}
}

func TestModelsDevPriceRejectsInvalidCosts(t *testing.T) {
	valid := Object{"input": 0.0, "output": 2.0}
	if _, ok := modelsDevCost(valid); !ok {
		t.Fatal("合法零价格应接受")
	}
	cases := map[string]Object{
		"缺 input":         {"output": 2.0},
		"null input":      {"input": nil, "output": 2.0},
		"负数":              {"input": -1.0, "output": 2.0},
		"NaN":             {"input": math.NaN(), "output": 2.0},
		"价格超限":            {"input": 1_000_001.0, "output": 2.0},
		"非法可选价格":          {"input": 1.0, "output": 2.0, "cache_read": "0.1"},
		"独立 reasoning 价格": {"input": 1.0, "output": 2.0, "reasoning": 3.0},
	}
	for name, cost := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := modelsDevCost(cost); ok {
				t.Fatal("非法价格不应被采用")
			}
		})
	}
	if _, ok := modelsDevCost(Object{"input": 1.0, "output": 2.0, "reasoning": 2.0}); !ok {
		t.Fatal("与 output 相同的 reasoning 价格应接受")
	}
	got, ok := modelsDevCost(Object{"input": 1.0, "output": 2.0, "cache_read": nil, "cache_write": nil})
	if !ok || got.cache != nil || got.write != nil {
		t.Fatal("缺失或 null 的缓存价格必须保留未知")
	}
}

func TestModelsDevPriceTiers(t *testing.T) {
	cost := Object{"input": 1.0, "output": 2.0, "tiers": []any{
		Object{"tier": Object{"type": "context", "size": 200000}, "input": 3.0, "output": 4.0},
		Object{"tier": Object{"type": "context", "size": 100000}, "input": 2.0, "output": 3.0, "cache_read": 0.2, "reasoning": 3.0},
	}}
	got, ok := modelsDevCost(cost)
	if !ok || len(got.tiers) != 2 || got.tiers[0].MinInput != 100000 || got.tiers[1].MinInput != 200000 {
		t.Fatalf("tiers 未排序或解析失败: %#v, ok=%v", got.tiers, ok)
	}
	if got.tiers[0].CachePrice == nil || *got.tiers[0].CachePrice != 0.2 || got.tiers[1].CachePrice != nil {
		t.Fatalf("tier 可选价格错误: %#v", got.tiers)
	}
	if empty, ok := modelsDevCost(Object{"input": 1.0, "output": 2.0, "tiers": []any{}}); !ok || empty.tiers != nil {
		t.Fatal("空 tiers 应归一化为 nil")
	}

	badTier := func(selector Object) Object {
		return Object{"input": 1.0, "output": 2.0, "tiers": []any{
			Object{"tier": selector, "input": 2.0, "output": 3.0},
		}}
	}
	cases := map[string]Object{
		"不支持的类型":    badTier(Object{"type": "region", "size": 100}),
		"非整数阈值":     badTier(Object{"type": "context", "size": 100.5}),
		"阈值溢出":      badTier(Object{"type": "context", "size": json.Number("9223372036854775808")}),
		"阈值超过模型上限":  badTier(Object{"type": "context", "size": 10_000_001}),
		"tiers 非数组": {"input": 1.0, "output": 2.0, "tiers": Object{}},
		"重复阈值": {"input": 1.0, "output": 2.0, "tiers": []any{
			Object{"tier": Object{"type": "context", "size": 100}, "input": 2.0, "output": 3.0},
			Object{"tier": Object{"type": "context", "size": 100}, "input": 3.0, "output": 4.0},
		}},
	}
	cases["tier 负价格"] = Object{"input": 1.0, "output": 2.0, "tiers": []any{
		Object{"tier": Object{"type": "context", "size": 100}, "input": -2.0, "output": 3.0},
	}}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := modelsDevCost(value); ok {
				t.Fatal("非法 tier 不应被忽略")
			}
		})
	}
}

func TestModelsDevProviderProfilesAndCustomMatch(t *testing.T) {
	catalog := Object{
		"zai":             modelsDevTestProvider("https://api.z.ai/api/paas/v4", oneModelsDevModel(1)),
		"zai-coding-plan": modelsDevTestProvider("https://api.z.ai/api/coding/paas/v4", oneModelsDevModel(2)),
		"opencode":        modelsDevTestProvider("https://opencode.ai/zen/v1", oneModelsDevModel(3)),
		"opencode-go":     modelsDevTestProvider("https://opencode.ai/zen/go/v1", oneModelsDevModel(4)),
		"private":         modelsDevTestProvider("https://catalog.example/api/v1/", oneModelsDevModel(5)),
	}
	cases := []struct {
		provider Provider
		want     string
		input    float64
	}{
		{Provider{Kind: "zai", BaseURL: "https://api.z.ai/api/coding/paas/v4"}, "models.dev:zai-coding-plan", 2},
		{Provider{Kind: "zai", BaseURL: "https://api.z.ai/api/anthropic/v1"}, "models.dev:zai-coding-plan", 2},
		{Provider{Kind: "opencode", BaseURL: "https://opencode.ai/zen/go/v1/"}, "models.dev:opencode-go", 4},
		{Provider{Kind: "custom", BaseURL: "http://CATALOG.EXAMPLE:80/api/v1"}, "models.dev:private", 5},
	}
	for _, tc := range cases {
		got, ok := modelsDevPrice(catalog, tc.provider, "m")
		if !ok || got.source != tc.want || got.input != tc.input {
			t.Fatalf("provider=%#v got=%#v ok=%v", tc.provider, got, ok)
		}
	}
	catalog["duplicate"] = modelsDevTestProvider("https://catalog.example/api/v1", oneModelsDevModel(9))
	if _, ok := modelsDevPrice(catalog, Provider{Kind: "custom", BaseURL: "https://catalog.example/api/v1"}, "m"); ok {
		t.Fatal("多个 custom provider 匹配时不应猜测")
	}
}

func TestLoadModelsDevUsesBoundedUnauthenticatedGET(t *testing.T) {
	var original string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Errorf("请求方法或鉴权头错误: %s %#v", r.Method, r.Header)
		}
		_, _ = io.WriteString(w, `{"openai":{"models":{}}}`)
	}))
	defer server.Close()
	client := modelsDevTestClient(t, server, &original)
	catalog, err := loadModelsDev(context.Background(), client)
	if err != nil || catalog["openai"] == nil || original != modelsDevURL {
		t.Fatalf("catalog=%#v err=%v original=%q", catalog, err, original)
	}
}

func TestLoadModelsDevRejectsBadResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		length int64
		secret string
	}{
		{"非 200", http.StatusBadGateway, "secret-upstream-body", -1, "secret-upstream-body"},
		{"非法 JSON", http.StatusOK, `{`, -1, ""},
		{"尾随 JSON", http.StatusOK, `{} {}`, -1, ""},
		{"超限", http.StatusOK, strings.Repeat(" ", modelsDevMaxBytes+1), -1, ""},
		{"截断", http.StatusOK, `{}`, 50, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.length >= 0 {
					w.Header().Set("Content-Length", "50")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := loadModelsDev(context.Background(), modelsDevTestClient(t, server, nil))
			if err == nil || tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func modelsDevTestProvider(api string, models Object) Object {
	return Object{"api": api, "models": models}
}

func oneModelsDevModel(input float64) Object {
	return Object{"m": Object{"cost": Object{"input": input, "output": input + 1}}}
}

type modelsDevRoundTrip func(*http.Request) (*http.Response, error)

func (f modelsDevRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func modelsDevTestClient(t *testing.T, server *httptest.Server, original *string) *http.Client {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := server.Client().Transport
	return &http.Client{Transport: modelsDevRoundTrip(func(r *http.Request) (*http.Response, error) {
		if original != nil {
			*original = r.URL.String()
		}
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(clone)
	})}
}
