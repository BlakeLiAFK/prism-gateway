package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
)

const modelsDevURL = "https://models.dev/api.json"
const modelsDevMaxBytes = 32 << 20

type PriceTier struct {
	MinInput    int64    `json:"min_input_tokens"`
	InputPrice  float64  `json:"input_price"`
	OutputPrice float64  `json:"output_price"`
	CachePrice  *float64 `json:"cache_price,omitempty"`
	WritePrice  *float64 `json:"write_price,omitempty"`
}

func loadModelsDev(ctx context.Context, client *http.Client) (Object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 models.dev 请求：%w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取 models.dev：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev 返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 models.dev 响应：%w", err)
	}
	if len(data) > modelsDevMaxBytes || resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {
		return nil, fmt.Errorf("models.dev 响应不完整或超过 32 MiB")
	}
	return decodeModelsDev(data)
}

func decodeModelsDev(data []byte) (Object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var catalog Object
	if err := dec.Decode(&catalog); err != nil || catalog == nil {
		return nil, fmt.Errorf("models.dev JSON 无效")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("models.dev JSON 含有多余内容")
	}
	return catalog, nil
}

func modelsDevPrice(catalog Object, p Provider, upstream string) (syncedModelPrice, bool) {
	key, ok := modelsDevProviderKey(catalog, p)
	if !ok || upstream == "" {
		return syncedModelPrice{}, false
	}
	provider, ok := catalog[key].(map[string]any)
	if !ok {
		return syncedModelPrice{}, false
	}
	models, ok := provider["models"].(map[string]any)
	if !ok {
		return syncedModelPrice{}, false
	}
	model, ok := models[upstream].(map[string]any)
	if !ok {
		return syncedModelPrice{}, false
	}
	price, ok := modelsDevCost(obj(model["cost"]))
	if !ok {
		return syncedModelPrice{}, false
	}
	price.source = "models.dev:" + key
	return price, true
}

func modelsDevCost(cost Object) (syncedModelPrice, bool) {
	input, ok := modelsDevNumber(cost["input"])
	if !ok {
		return syncedModelPrice{}, false
	}
	output, ok := modelsDevNumber(cost["output"])
	if !ok || !modelsDevReasoning(cost, output) {
		return syncedModelPrice{}, false
	}
	cache, ok := modelsDevOptionalNumber(cost, "cache_read")
	if !ok {
		return syncedModelPrice{}, false
	}
	write, ok := modelsDevOptionalNumber(cost, "cache_write")
	if !ok {
		return syncedModelPrice{}, false
	}
	tiers, ok := modelsDevTiers(cost["tiers"])
	if !ok {
		return syncedModelPrice{}, false
	}
	return syncedModelPrice{input: input, output: output, cache: cache, write: write, tiers: tiers}, true
}

func modelsDevReasoning(cost Object, output float64) bool {
	raw, exists := cost["reasoning"]
	if !exists || raw == nil {
		return true
	}
	value, ok := modelsDevNumber(raw)
	return ok && value == output
}

func modelsDevTiers(raw any) ([]PriceTier, bool) {
	if raw == nil {
		return nil, true
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	if len(items) == 0 {
		return nil, true
	}
	tiers := make([]PriceTier, 0, len(items))
	seen := make(map[int64]bool, len(items))
	for _, item := range items {
		tier, ok := modelsDevTier(obj(item))
		if !ok || seen[tier.MinInput] {
			return nil, false
		}
		seen[tier.MinInput] = true
		tiers = append(tiers, tier)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].MinInput < tiers[j].MinInput })
	return tiers, true
}

func modelsDevTier(item Object) (PriceTier, bool) {
	selector := obj(item["tier"])
	if str(selector, "type") != "context" {
		return PriceTier{}, false
	}
	minInput, ok := modelsDevInteger(selector["size"])
	if !ok || minInput < 1 || minInput > 10_000_000 {
		return PriceTier{}, false
	}
	input, ok := modelsDevNumber(item["input"])
	if !ok {
		return PriceTier{}, false
	}
	output, ok := modelsDevNumber(item["output"])
	if !ok || !modelsDevReasoning(item, output) {
		return PriceTier{}, false
	}
	cache, ok := modelsDevOptionalNumber(item, "cache_read")
	if !ok {
		return PriceTier{}, false
	}
	write, ok := modelsDevOptionalNumber(item, "cache_write")
	return PriceTier{MinInput: minInput, InputPrice: input, OutputPrice: output, CachePrice: cache, WritePrice: write}, ok
}

func modelsDevNumber(raw any) (float64, bool) {
	var value float64
	switch v := raw.(type) {
	case json.Number:
		var err error
		value, err = strconv.ParseFloat(string(v), 64)
		if err != nil {
			return 0, false
		}
	case float64:
		value = v
	case int:
		value = float64(v)
	case int64:
		value = float64(v)
	default:
		return 0, false
	}
	return value, value >= 0 && value <= 1_000_000 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func modelsDevOptionalNumber(o Object, key string) (*float64, bool) {
	raw, exists := o[key]
	if !exists || raw == nil {
		return nil, true
	}
	value, ok := modelsDevNumber(raw)
	if !ok {
		return nil, false
	}
	return &value, true
}

func modelsDevInteger(raw any) (int64, bool) {
	switch v := raw.(type) {
	case json.Number:
		n, err := strconv.ParseInt(string(v), 10, 64)
		return n, err == nil
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		if v < math.MinInt64 || v >= math.MaxInt64 || math.Trunc(v) != v {
			return 0, false
		}
		return int64(v), true
	default:
		return 0, false
	}
}

func modelsDevProviderKey(catalog Object, p Provider) (string, bool) {
	endpoint, ok := normalizeCatalogEndpoint(p.BaseURL)
	if !ok {
		return "", false
	}
	if p.Kind == "custom" {
		return customModelsDevProvider(catalog, endpoint)
	}
	known := map[string]map[string]string{
		"openai":     {"api.openai.com/v1": "openai"},
		"anthropic":  {"api.anthropic.com/v1": "anthropic"},
		"openrouter": {"openrouter.ai/api/v1": "openrouter"},
		"deepseek":   {"api.deepseek.com/": "deepseek", "api.deepseek.com/v1": "deepseek"},
		"zai": {
			"api.z.ai/api/paas/v4":        "zai",
			"api.z.ai/api/coding/paas/v4": "zai-coding-plan",
			"api.z.ai/api/anthropic":      "zai-coding-plan",
			"api.z.ai/api/anthropic/v1":   "zai-coding-plan",
		},
		"opencode": {"opencode.ai/zen/v1": "opencode", "opencode.ai/zen/go/v1": "opencode-go"},
	}
	key, ok := known[p.Kind][endpoint]
	if !ok {
		return "", false
	}
	_, exists := catalog[key]
	return key, exists
}

func customModelsDevProvider(catalog Object, endpoint string) (string, bool) {
	match := ""
	for key, raw := range catalog {
		provider, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		api, ok := normalizeCatalogEndpoint(str(provider, "api"))
		if !ok || api != endpoint {
			continue
		}
		if match != "" {
			return "", false
		}
		match = key
	}
	return match, match != ""
}

func normalizeCatalogEndpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && !((u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80")) {
		host += ":" + port
	}
	p := path.Clean("/" + strings.TrimSpace(u.Path))
	return host + p, true
}
