package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

type syncedModelPrice struct {
	input, output float64
	cache, write  *float64
	source        string
	tiers         []PriceTier
}

func (a *App) taskPriceSync(scheduleConfig) (string, error) {
	cfg := a.Store.Config()
	feeds, providers, warnings := a.upstreamPriceFeeds(cfg)
	missing, matched := a.fillCatalogPrices(cfg, feeds)
	if matched == 0 {
		if len(missing) == 0 {
			return "", nil
		}
		return "", fmt.Errorf("未获取有效价格，已保留原价：%s", strings.Join(missing, "、"))
	}
	result, err := a.applySyncedPrices(cfg.Version, providers, feeds)
	if err != nil {
		return "", err
	}
	if len(missing) > 0 {
		warnings = append(warnings, "已保留未匹配模型原价："+strings.Join(missing, "、"))
	}
	if len(warnings) > 0 {
		if result != "" {
			warnings = append([]string{result}, warnings...)
		}
		result = strings.Join(warnings, "；")
	}
	return result, nil
}

func (a *App) upstreamPriceFeeds(cfg Config) (map[string]map[string]syncedModelPrice, []string, []string) {
	feeds := map[string]map[string]syncedModelPrice{}
	var providers, warnings []string
	for _, p := range cfg.Providers {
		if !p.Enabled || p.Kind == "mock" || !hasUnlockedPriceModel(cfg, p.ID) {
			continue
		}
		ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
		items, err := a.upstreamModels(ctx, p)
		cancel()
		if err != nil {
			items = nil
			warnings = append(warnings, p.ID+" 上游模型接口失败，已尝试 models.dev 兜底")
		}
		feeds[p.ID] = collectSyncedPrices(items)
		providers = append(providers, p.ID)
	}
	return feeds, providers, warnings
}

func hasUnlockedPriceModel(cfg Config, provider string) bool {
	for _, m := range cfg.Models {
		if m.ProviderID == provider && !m.PriceLocked {
			return true
		}
	}
	return false
}

func (a *App) fillCatalogPrices(cfg Config, feeds map[string]map[string]syncedModelPrice) ([]string, int) {
	var catalog Object
	var missing []string
	loaded, matched := false, 0
	for _, m := range cfg.Models {
		feed, enabled := feeds[m.ProviderID]
		if !enabled || m.PriceLocked {
			continue
		}
		if _, exists := feed[m.Upstream]; exists {
			matched++
			continue
		}
		if !loaded {
			ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
			client := a.pricingClient
			if client == nil {
				client = clientFor(Provider{TimeoutSec: 30}, 30*time.Second)
				defer client.CloseIdleConnections()
			}
			catalog, _ = loadModelsDev(ctx, client)
			cancel()
			loaded = true
		}
		p, _ := cfg.provider(m.ProviderID)
		if price, ok := modelsDevPrice(catalog, p, m.Upstream); ok {
			feed[m.Upstream] = price
			matched++
		} else {
			reason := "无有效匹配"
			if catalog == nil {
				reason = "models.dev 不可用"
			}
			missing = append(missing, m.ID+"（"+reason+"）")
		}
	}
	sort.Strings(missing)
	return missing, matched
}

func collectSyncedPrices(items []any) map[string]syncedModelPrice {
	out := map[string]syncedModelPrice{}
	for _, raw := range items {
		item := obj(raw)
		id, pricing := str(item, "id"), obj(item["pricing"])
		input, inputOK := strictPrice(pricing, "prompt")
		output, outputOK := strictPrice(pricing, "completion")
		if id == "" || !inputOK || !outputOK {
			continue
		}
		p := syncedModelPrice{input: input, output: output, source: "upstream"}
		invalid := false
		if v, ok := strictPrice(pricing, "input_cache_read"); ok {
			p.cache = &v
		} else if pricing["input_cache_read"] != nil {
			invalid = true
		}
		if v, ok := strictPrice(pricing, "input_cache_write"); ok {
			p.write = &v
		} else if pricing["input_cache_write"] != nil {
			invalid = true
		}
		if invalid {
			continue
		}
		out[id] = p
	}
	return out
}

func strictPrice(pricing Object, key string) (float64, bool) {
	raw, exists := pricing[key]
	if !exists || raw == nil {
		return 0, false
	}
	var value float64
	switch v := raw.(type) {
	case float64:
		value = v
	case string:
		var err error
		value, err = strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	value *= 1e6
	if value < 0 || value > 1e6 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return roundPrice(value), true
}

func (a *App) applySyncedPrices(version int64, providers []string, feeds map[string]map[string]syncedModelPrice) (string, error) {
	changed := []string{}
	_, err := a.Store.Change(version, "schedule.price_sync", strings.Join(providers, ","), func(c *Config) error {
		for i := range c.Models {
			m := &c.Models[i]
			price, ok := feeds[m.ProviderID][m.Upstream]
			if !ok || m.PriceLocked {
				continue
			}
			if applyModelPrice(m, price) {
				changed = append(changed, m.ID)
			}
		}
		if len(changed) == 0 {
			return errNoChange
		}
		return nil
	})
	if errors.Is(err, errNoChange) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sort.Strings(changed)
	return fmt.Sprintf("价格已同步：%d 个模型（%s）", len(changed), strings.Join(changed, "、")), nil
}

func applyModelPrice(m *Model, price syncedModelPrice) bool {
	before := *m
	m.InputPrice, m.OutputPrice, m.PricingSet = price.input, price.output, true
	if price.cache != nil {
		m.CachePrice = *price.cache
	}
	if price.write != nil {
		m.WritePrice = *price.write
	}
	m.PriceSource, m.PriceTiers = price.source, clonePriceTiers(price.tiers)
	changed := before.InputPrice != m.InputPrice || before.OutputPrice != m.OutputPrice ||
		before.CachePrice != m.CachePrice || before.WritePrice != m.WritePrice ||
		before.PricingSet != m.PricingSet || before.PriceSource != m.PriceSource ||
		!reflect.DeepEqual(before.PriceTiers, m.PriceTiers)
	if changed {
		m.PriceUpdatedAt = now()
	}
	return changed
}
