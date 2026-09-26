package gateway

// 定期对照上游 /models 列表（由定时任务调度），找出两类情况，在总览「需要关注」和模型库里提示：
//   - 已启用、但上游列表里已经找不到的模型（可能已下架）
//   - 已确认价格、但与上游列表声明的价格相差超过 1% 的模型（目前只有 OpenRouter 这类会在列表里带价格）
// 有的上游列表并不完整、价格也可能是促销价，所以只提示、不自动禁用或改价，由管理员决定；
// 列表拉取失败的供应商跳过——拉不到列表不等于模型下架。

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

type missingModels struct {
	mu    sync.Mutex
	since map[string]int64  // 模型 ID → 首次发现不在上游列表的时刻
	drift map[string]Object // 模型 ID → 本地与上游价格
}

// upstreamPrice 取上游列表声明的输入 / 输出价格（$/M）；没声明或是动态价格（负数）时 ok=false
func upstreamPrice(item Object) (in, out float64, ok bool) {
	pr := obj(item["pricing"])
	if pr["prompt"] == nil || pr["completion"] == nil {
		return 0, 0, false
	}
	in, out = price(pr, "prompt"), price(pr, "completion")
	return in, out, in >= 0 && out >= 0
}

// priceDiffers 判断两个价格相差是否超过 1%
func priceDiffers(a, b float64) bool {
	return math.Abs(a-b) > 0.01*math.Max(a, b)
}

// finding 是一条可推送的发现；key 用于推送去重，推送成功后才记为已推送
type finding struct{ key, text string }

// checkUpstreamModels 刷新两份清单，返回当前全部发现（是否推送由调用方按去重键决定）
func (a *App) checkUpstreamModels(ctx context.Context) []finding {
	c := a.Store.Config()
	a.missing.mu.Lock()
	prev, prevDrift := a.missing.since, a.missing.drift
	a.missing.mu.Unlock()
	next, drift, found := map[string]int64{}, map[string]Object{}, []finding{}
	for _, p := range c.Providers {
		if !p.Enabled || p.Kind == "mock" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		items, err := a.upstreamModels(cctx, p)
		cancel()
		ours := []Model{}
		for _, m := range c.Models {
			if m.ProviderID == p.ID && m.Enabled {
				ours = append(ours, m)
			}
		}
		if err != nil {
			// 拉不到列表时沿用上次的结论，不新增也不清除
			slog.Warn("upstream model list check skipped", "provider", p.ID, "err", err)
			for _, m := range ours {
				if t, ok := prev[m.ID]; ok {
					next[m.ID] = t
				}
				if d, ok := prevDrift[m.ID]; ok {
					drift[m.ID] = d
				}
			}
			continue
		}
		listed := map[string]Object{}
		for _, v := range items {
			listed[str(obj(v), "id")] = obj(v)
		}
		for _, m := range ours {
			item, ok := listed[m.Upstream]
			if !ok {
				next[m.ID] = cmp.Or(prev[m.ID], now())
				found = append(found, finding{"missing:" + m.ID, fmt.Sprintf("%s（%s）在上游模型列表中找不到，可能已下架", m.ID, p.Name)})
				continue
			}
			in, out, ok := upstreamPrice(item)
			if !m.PricingSet || !ok || (!priceDiffers(m.InputPrice, in) && !priceDiffers(m.OutputPrice, out)) {
				continue
			}
			d := Object{"id": m.ID, "provider_id": p.ID, "input": m.InputPrice, "output": m.OutputPrice, "upstream_input": in, "upstream_output": out}
			drift[m.ID] = d
			found = append(found, finding{fmt.Sprintf("drift:%s:%g:%g", m.ID, in, out),
				fmt.Sprintf("%s 的上游价格为 $%g / $%g 每百万 Token，本地为 $%g / $%g", m.ID, in, out, m.InputPrice, m.OutputPrice)})
		}
	}
	a.missing.mu.Lock()
	a.missing.since, a.missing.drift = next, drift
	a.missing.mu.Unlock()
	return found
}

// missingList 返回当前不在上游列表里的模型：[{id, provider_id, since}]
func (a *App) missingList() []any {
	c := a.Store.Config()
	a.missing.mu.Lock()
	defer a.missing.mu.Unlock()
	out := []any{}
	for id, t := range a.missing.since {
		if m, ok := c.model(id); ok && m.Enabled {
			out = append(out, Object{"id": id, "provider_id": m.ProviderID, "since": t})
		}
	}
	return out
}

// driftList 返回本地价格与上游声明不一致的模型；本地价格已改成一致的立即不再列出
func (a *App) driftList() []any {
	c := a.Store.Config()
	a.missing.mu.Lock()
	defer a.missing.mu.Unlock()
	out := []any{}
	for id, d := range a.missing.drift {
		m, ok := c.model(id)
		if ok && m.Enabled && m.PricingSet && (priceDiffers(m.InputPrice, num(d, "upstream_input")) || priceDiffers(m.OutputPrice, num(d, "upstream_output"))) {
			out = append(out, Object{"id": id, "provider_id": m.ProviderID, "input": m.InputPrice, "output": m.OutputPrice,
				"upstream_input": d["upstream_input"], "upstream_output": d["upstream_output"]})
		}
	}
	return out
}
