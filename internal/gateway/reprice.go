package gateway

// 历史请求补录花费：模型在确认计价之前产生的请求，上游已经报了真实 tokens，
// 只是当时没有单价。这里按当前单价补算。用量未知（reserved_*）、被拒绝与演示请求不动：
// 用量不知道就是不知道，不能当成 0，更不能编一个数。

import (
	"fmt"
	"math"

	"prism-gateway/internal/sqlite"
)

// repricedMode 标记事后按补录时单价算出的花费，与请求当时算的区分开
const repricedMode = "reported_tokens_repriced"

// reprice 补录历史花费。dry_run 只统计不写入；model_id 为空时处理全部模型。
// 返回 {count, cost, models:[{model_id,count,cost}], unpriced_models:[...]}
func (a *App) reprice(p Object) (any, error) {
	c := a.Store.Config()
	model, dry := str(p, "model_id"), boolean(p, "dry_run")
	rows, err := a.Store.DB.Query(`SELECT id, model_id, input_tokens, output_tokens, cache_tokens, write_tokens
		FROM requests WHERE cost_known=0 AND usage_mode='reported_tokens' AND is_demo=0 AND (?='' OR model_id=?)`, model, model)
	if err != nil {
		return nil, err
	}
	type item struct {
		id   string
		cost int64
	}
	items := []item{}
	perModel, skipped := Object{}, Object{}
	for _, r := range rows {
		m, ok := c.model(r.String("model_id"))
		if !ok || !m.PricingSet {
			skipped[r.String("model_id")] = true
			continue
		}
		v := cost(m, Usage{Input: r.Int("input_tokens"), Output: r.Int("output_tokens"), Cache: r.Int("cache_tokens"), Write: r.Int("write_tokens")})
		items = append(items, item{r.String("id"), v})
		s := obj(perModel[m.ID])
		if s == nil {
			s = Object{"model_id": m.ID, "count": int64(0), "cost_nano": int64(0)}
			perModel[m.ID] = s
		}
		s["count"] = s["count"].(int64) + 1
		s["cost_nano"] = s["cost_nano"].(int64) + v
	}
	total := int64(0)
	models := []any{}
	for _, id := range sortedKeys(perModel) {
		s := obj(perModel[id])
		total += s["cost_nano"].(int64)
		models = append(models, Object{"model_id": id, "count": s["count"], "cost": dollars(s["cost_nano"].(int64))})
	}
	out := Object{"count": len(items), "cost": math.Round(dollars(total)*1e6) / 1e6, "models": models, "unpriced_models": sortedKeys(skipped), "dry_run": dry}
	if dry || len(items) == 0 {
		return out, nil
	}
	// 条件里再带一次 cost_known=0，并发的第二次补录不会重复计价
	err = a.Store.DB.Transaction(func(t *sqlite.Tx) error {
		for _, it := range items {
			if err := t.Exec("UPDATE requests SET cost_nano=?, cost_known=1, usage_mode=? WHERE id=? AND cost_known=0", it.cost, repricedMode, it.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	a.statsMu.Lock()
	a.statsCache = nil
	a.statsMu.Unlock()
	target := model
	if target == "" {
		target = "all"
	}
	a.audit("request.reprice", fmt.Sprintf("%s:%d:$%.4f", target, len(items), dollars(total)))
	return out, nil
}
