package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type syncedModelPrice struct {
	input, output float64
	cache, write  *float64
}

func (a *App) taskPriceSync(scheduleConfig) (string, error) {
	cfg := a.Store.Config()
	feeds := map[string]map[string]syncedModelPrice{}
	providers := []string{}
	for _, p := range cfg.Providers {
		if !p.Enabled || !isOpenRouter(p) {
			continue
		}
		ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
		items, err := a.upstreamModels(ctx, p)
		cancel()
		if err != nil {
			label := p.Name
			if label == "" {
				label = p.ID
			}
			return "", fmt.Errorf("同步 OpenRouter 价格失败（%s）：%w", label, err)
		}
		feeds[p.ID] = collectSyncedPrices(items)
		providers = append(providers, p.ID)
	}
	if len(providers) == 0 {
		return "", fail("NOT_FOUND", "没有启用的 OpenRouter 供应商", 400)
	}
	return a.applySyncedPrices(cfg.Version, providers, feeds)
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
		p := syncedModelPrice{input: input, output: output}
		if v, ok := strictPrice(pricing, "input_cache_read"); ok {
			p.cache = &v
		}
		if v, ok := strictPrice(pricing, "input_cache_write"); ok {
			p.write = &v
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
			before := [5]any{m.InputPrice, m.OutputPrice, m.CachePrice, m.WritePrice, m.PricingSet}
			m.InputPrice, m.OutputPrice, m.PricingSet = price.input, price.output, true
			if price.cache != nil {
				m.CachePrice = *price.cache
			}
			if price.write != nil {
				m.WritePrice = *price.write
			}
			after := [5]any{m.InputPrice, m.OutputPrice, m.CachePrice, m.WritePrice, m.PricingSet}
			if before != after {
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
	return fmt.Sprintf("OpenRouter 价格已同步：%d 个模型（%s）", len(changed), strings.Join(changed, "、")), nil
}
