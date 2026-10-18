package gateway

import (
	"errors"
	"math"
	"slices"
)

func validatePriceTiers(tiers []PriceTier) error {
	var previous int64
	for _, tier := range tiers {
		if tier.MinInput <= previous || tier.MinInput > 10000000 {
			return errors.New("价格分档输入阈值必须递增且不超过 10000000")
		}
		previous = tier.MinInput
		prices := []float64{tier.InputPrice, tier.OutputPrice}
		for _, v := range []*float64{tier.CachePrice, tier.WritePrice} {
			if v != nil {
				prices = append(prices, *v)
			}
		}
		for _, v := range prices {
			if v < 0 || v > 1000000 || math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("分档价格必须是有限非负数且不超过 1000000")
			}
		}
	}
	return nil
}

func clonePriceTiers(tiers []PriceTier) []PriceTier {
	out := slices.Clone(tiers)
	for i, tier := range out {
		if tier.CachePrice != nil {
			value := *tier.CachePrice
			out[i].CachePrice = &value
		}
		if tier.WritePrice != nil {
			value := *tier.WritePrice
			out[i].WritePrice = &value
		}
	}
	return out
}

func modelPriceAt(m Model, input int64) Model {
	for i := len(m.PriceTiers) - 1; i >= 0; i-- {
		tier := m.PriceTiers[i]
		if input < tier.MinInput {
			continue
		}
		m.InputPrice, m.OutputPrice = tier.InputPrice, tier.OutputPrice
		if tier.CachePrice != nil {
			m.CachePrice = *tier.CachePrice
		}
		if tier.WritePrice != nil {
			m.WritePrice = *tier.WritePrice
		}
		break
	}
	return m
}

func prepareModelPricing(m *Model, c Config, id string) {
	old, exists := c.model(id)
	m.PriceSource, m.PriceUpdatedAt, m.PriceTiers = old.PriceSource, old.PriceUpdatedAt, clonePriceTiers(old.PriceTiers)
	if !exists || m.ProviderID != old.ProviderID || m.Upstream != old.Upstream ||
		m.InputPrice != old.InputPrice || m.OutputPrice != old.OutputPrice ||
		m.CachePrice != old.CachePrice || m.WritePrice != old.WritePrice || m.PricingSet != old.PricingSet {
		m.PriceSource, m.PriceTiers, m.PriceUpdatedAt = "", nil, 0
		if exists || m.PricingSet || m.InputPrice != 0 || m.OutputPrice != 0 || m.CachePrice != 0 || m.WritePrice != 0 {
			m.PriceUpdatedAt = now()
		}
	}
}

func (a *App) saveModel(c Config, version int64, id string, input any) (any, error) {
	v := Model{ID: id, Enabled: true, Protocol: "chat", ToolsUnknown: true, VisionUnknown: true, Context: 128000, MaxOutput: 4096}
	if old, ok := c.model(id); ok {
		v = old
		v.PriceTiers = clonePriceTiers(old.PriceTiers)
	}
	if er := decode(input, &v); er != nil {
		return nil, fail("INVALID_PARAMS", er.Error(), 400)
	}
	confirmCapabilities(&v, obj(input))
	prepareModelPricing(&v, c, id)
	if v.ID == "" {
		v.ID = randomID("m_")
	}
	return a.Store.Change(version, "model.save", v.ID, func(c *Config) error {
		for i, x := range c.Models {
			if x.ID == v.ID {
				c.Models[i] = v
				return nil
			}
		}
		c.Models = append(c.Models, v)
		return nil
	})
}
