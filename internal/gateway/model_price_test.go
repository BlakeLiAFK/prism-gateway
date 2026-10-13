package gateway

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestTieredCostAtThreshold(t *testing.T) {
	cache := 1.0
	m := pricedModel("m", "p", "m", 1, 2, .5, .75)
	m.PriceTiers = []PriceTier{{MinInput: 100, InputPrice: 3, OutputPrice: 6, CachePrice: &cache}, {MinInput: 200, InputPrice: 4, OutputPrice: 8}}
	for _, tc := range []struct{ input, want int64 }{{99, 97000}, {100, 283000}, {101, 286000}, {200, 768000}} {
		if got := cost(m, Usage{Input: tc.input, Output: 2, Cache: 10, Write: 4}); got != tc.want {
			t.Errorf("input=%d got=%d want=%d", tc.input, got, tc.want)
		}
	}
	m.PricingSet = false
	if cost(m, Usage{Input: 200, Output: 2}) != 0 {
		t.Fatal("未知计价不能计算费用")
	}
}

func TestPriceTierSnapshotAndValidation(t *testing.T) {
	h := newHarness(t)
	cache, write := .5, .75
	m := pricedModel("m", "p_test", "m", 1, 2, .5, .75)
	m.PriceTiers = []PriceTier{{MinInput: 100, InputPrice: 3, OutputPrice: 6, CachePrice: &cache, WritePrice: &write}}
	h.configure(t, "http://127.0.0.1:1", m)
	before := raw(h.s.Config())
	_, err := h.s.Change(h.s.Config().Version, "test", "", func(c *Config) error {
		*c.Models[0].PriceTiers[0].CachePrice = 99
		*c.Models[0].PriceTiers[0].WritePrice = 99
		return errors.New("rollback")
	})
	if err == nil || raw(h.s.Config()) != before {
		t.Fatal("回滚不能修改已发布分档指针")
	}
	for _, tiers := range [][]PriceTier{
		{{MinInput: 0}}, {{MinInput: 10000001}}, {{MinInput: 100}, {MinInput: 100}}, {{MinInput: 100, InputPrice: -1}}, {{MinInput: 100, OutputPrice: math.Inf(1)}},
	} {
		if validatePriceTiers(tiers) == nil {
			t.Errorf("非法分档被接受: %+v", tiers)
		}
	}
}

func TestModelSavePreservesAndClearsAutomaticPrices(t *testing.T) {
	h := newHarness(t)
	m := pricedModel("m", "p_test", "m", 1, 2, .5, .75)
	m.PriceSource, m.PriceUpdatedAt = "models.dev:openai", 123
	m.PriceTiers = []PriceTier{{MinInput: 100, InputPrice: 3, OutputPrice: 6}}
	h.configure(t, "http://127.0.0.1:1", m)
	save := func(fields Object, version int64) int {
		w, _ := h.rpc(t, "model.save", Object{"id": "m", "version": version, "model": fields}, h.token)
		return w.Code
	}
	before := raw(h.s.Config())
	if save(Object{"price_tiers": []PriceTier{{MinInput: 100, InputPrice: 99}}}, 0) != 409 || raw(h.s.Config()) != before {
		t.Fatal("失败的解码不能修改快照")
	}
	if save(Object{"name": "renamed", "price_source": "forged", "price_tiers": []PriceTier{{MinInput: 100, InputPrice: 99}}}, h.s.Config().Version) != 200 {
		t.Fatal("保存名称失败")
	}
	got, _ := h.s.Config().model("m")
	if got.PriceSource != m.PriceSource || got.PriceUpdatedAt != 123 || len(got.PriceTiers) != 1 || got.PriceTiers[0].InputPrice != 3 {
		t.Fatalf("普通编辑应保留只读价格元数据: %+v", got)
	}
	if save(Object{"input_price": 4}, h.s.Config().Version) != 200 {
		t.Fatal("手动改价失败")
	}
	got, _ = h.s.Config().model("m")
	if got.PriceSource != "" || got.PriceTiers != nil || got.PriceUpdatedAt <= 123 || got.InputPrice != 4 {
		t.Fatalf("手动改价应清除自动分档: %+v", got)
	}
}

func TestFreeModelClearsPaidTiers(t *testing.T) {
	m := pricedModel("m", "p", "m", 1, 2, 3, 4)
	m.PriceSource = "models.dev:openrouter"
	m.PriceUpdatedAt = 123
	m.PriceTiers = []PriceTier{{MinInput: 10, InputPrice: 3, OutputPrice: 6}}
	enableFree(&m, scheduleConfig{})
	if len(m.PriceTiers) != 0 || m.PriceSource != "" || m.PriceUpdatedAt <= 123 || cost(m, Usage{Input: 100, Output: 10}) != 0 {
		t.Fatalf("免费模型仍带付费分档: %+v", m)
	}
}

func TestModelSaveUnknownPriceHasNoUpdateTime(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("existing", "chat"))
	m := modelFixture("new", "chat")
	m.ProviderID, m.PricingSet = "p_test", false
	w, _ := h.rpc(t, "model.save", Object{"version": h.s.Config().Version, "model": m}, h.token)
	requireStatus(t, w, 200)
	got, _ := h.s.Config().model("new")
	if got.PriceUpdatedAt != 0 || got.PriceSource != "" || got.PricingSet {
		t.Fatalf("未知价格不应虚构更新时间: %+v", got)
	}
}

func TestCostStrategyUsesInputTier(t *testing.T) {
	h := newHarness(t)
	strategyFixture(t, h, "cost")
	h.change(t, func(c *Config) {
		c.Models[1].PriceTiers = []PriceTier{{MinInput: 100, InputPrice: 20, OutputPrice: 30}}
	})
	for _, tc := range []struct{ text, first string }{{"hi", "b"}, {strings.Repeat("x", 600), "a"}} {
		request := Object{"model": "r", "messages": []any{Object{"role": "user", "content": tc.text}}}
		selected, _, err := h.a.Engine.selections(h.s.Config(), request, "chat", "")
		if err != nil || len(selected) == 0 || selected[0].Model.ID != tc.first {
			t.Fatalf("分档成本未参与排序: %v %v", selected, err)
		}
	}
	if h.s.Config().Models[1].InputPrice != .3 {
		t.Fatal("排序不能改变模型的基础价格")
	}
}
