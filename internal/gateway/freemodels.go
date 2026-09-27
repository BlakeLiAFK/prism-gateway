package gateway

// 免费模型自动维护：按间隔拉取 OpenRouter 模型列表，把符合条件的免费模型写入模型库、启用并加入目标路由；
// 路由里的 OpenRouter 免费模型从上游列表消失后自动停用。OpenRouter 没有评分接口，只能按条件筛：
// 价格为 0 的 :free 变体、上下文不低于下限、（可选）声明支持 tools。新加入的排在路由末尾，按上线时间新的在前。
// 「丢弃推理内容」是单独的选项，默认关闭（AGENTS 第 15 条）；排除列表里的上游 ID 永远不动。
// 所有改动走 Store.Change，有配置版本与审计；没有改动时不产生新版本。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const freeCandidateWeight = 5

var errNoChange = errors.New("no change")

func openRouterProvider(c Config) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Enabled && strings.Contains(strings.ToLower(p.BaseURL), "openrouter.ai") {
			return p, true
		}
	}
	return Provider{}, false
}

// freeCandidate 判断上游条目是否符合自动加入条件
func freeCandidate(item Object, c scheduleConfig, exclude map[string]bool) bool {
	id, pr := str(item, "id"), obj(item["pricing"])
	if !strings.HasSuffix(id, ":free") || exclude[id] || price(pr, "prompt") != 0 || price(pr, "completion") != 0 {
		return false
	}
	if int(num(item, "context_length")) < c.FreeMinContext {
		return false
	}
	if !c.FreeRequireTools {
		return true
	}
	for _, v := range arr(item["supported_parameters"]) {
		if v == "tools" {
			return true
		}
	}
	return false
}

func (a *App) taskFreeModels(c scheduleConfig) (string, error) {
	cfg := a.Store.Config()
	p, ok := openRouterProvider(cfg)
	if !ok {
		return "", fail("NOT_FOUND", "没有启用的 OpenRouter 供应商", 400)
	}
	if _, ok = cfg.route(c.FreeRoute); !ok {
		return "", fail("NOT_FOUND", "目标路由不存在："+c.FreeRoute, 400)
	}
	ctx, cancel := context.WithTimeout(a.Context, 30*time.Second)
	defer cancel()
	items, err := a.upstreamModels(ctx, p)
	if err != nil {
		// 拉不到列表时什么都不动：拉不到不等于下架
		return "", fmt.Errorf("拉取 OpenRouter 模型列表失败：%w", err)
	}
	var ch freeChanges
	_, err = a.Store.Change(cfg.Version, "schedule.free_models", c.FreeRoute, func(nc *Config) error {
		ch = applyFreeModels(nc, p, items, c)
		if len(ch.added)+len(ch.enabled)+len(ch.disabled)+len(ch.dropped) == 0 {
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
	text := freeSummary(c.FreeRoute, ch)
	// 改动已经生效，推送失败只影响通知
	if e := a.notify(text); e != nil {
		text += "\n（推送未发出：" + e.Error() + "）"
	}
	return text, nil
}

type freeChanges struct{ added, enabled, disabled, dropped []string }

// applyFreeModels 在配置副本上完成：停用已下架的、重新启用又出现的、把新的符合条件的加进路由；
// 开着「丢弃推理内容」选项时，路由里已启用的免费模型也补上该选项（选项后开也能生效）
func applyFreeModels(nc *Config, p Provider, items []any, c scheduleConfig) (ch freeChanges) {
	exclude := map[string]bool{}
	for _, x := range strings.Split(c.FreeExclude, ",") {
		exclude[strings.TrimSpace(x)] = true
	}
	listed := map[string]Object{}
	for _, v := range items {
		if it := obj(v); strings.HasSuffix(str(it, "id"), ":free") {
			listed[str(it, "id")] = it
		}
	}
	ri := 0
	for i, r := range nc.Routes {
		if r.ID == c.FreeRoute {
			ri = i
		}
	}
	inRoute := map[string]bool{}
	for _, cd := range nc.Routes[ri].Candidates {
		inRoute[cd.ModelID] = true
	}
	for i := range nc.Models {
		m := &nc.Models[i]
		if m.ProviderID != p.ID || !strings.HasSuffix(m.Upstream, ":free") || !inRoute[m.ID] || exclude[m.Upstream] {
			continue
		}
		item, ok := listed[m.Upstream]
		switch {
		case m.Enabled && !ok:
			// 只因下架而停用；条件改严不追溯已在用的模型
			m.Enabled = false
			ch.disabled = append(ch.disabled, m.ID)
		case !m.Enabled && ok && freeCandidate(item, c, exclude):
			enableFree(m, c)
			ch.enabled = append(ch.enabled, m.ID)
		case m.Enabled && c.FreeDropReasoning && !m.DropReasoning:
			m.DropReasoning = true
			ch.dropped = append(ch.dropped, m.ID)
		}
	}
	ups := make([]string, 0, len(listed))
	for up := range listed {
		ups = append(ups, up)
	}
	sort.Slice(ups, func(i, j int) bool { return num(listed[ups[i]], "created") > num(listed[ups[j]], "created") })
	for _, up := range ups {
		item := listed[up]
		if !freeCandidate(item, c, exclude) {
			continue
		}
		idx := -1
		for i, m := range nc.Models {
			if m.ProviderID == p.ID && m.Upstream == up {
				idx = i
			}
		}
		if idx >= 0 && inRoute[nc.Models[idx].ID] {
			continue
		}
		if idx < 0 {
			nc.Models = append(nc.Models, newSyncedModel(freeModelID(nc, c.FreeRoute, p.ID, up), p.ID, up, str(item, "name"), "chat", item))
			idx = len(nc.Models) - 1
		}
		enableFree(&nc.Models[idx], c)
		id := nc.Models[idx].ID
		nc.Routes[ri].Candidates = append(nc.Routes[ri].Candidates, Candidate{ModelID: id, Weight: freeCandidateWeight})
		inRoute[id] = true
		ch.added = append(ch.added, id)
	}
	return ch
}

// enableFree 启用免费模型：价格就是 0，直接确认；丢弃推理内容只在管理员开了选项时打开，不会被关掉
func enableFree(m *Model, c scheduleConfig) {
	m.Enabled, m.PricingSet = true, true
	m.InputPrice, m.OutputPrice, m.CachePrice, m.WritePrice = 0, 0, 0, 0
	m.DropReasoning = m.DropReasoning || c.FreeDropReasoning
}

// freeModelID 取「路由-短名」，如 lite-qwen3.8-27b；重名或不合法时退回「供应商/上游」形式
func freeModelID(nc *Config, route, providerID, up string) string {
	short := strings.TrimSuffix(up[strings.LastIndex(up, "/")+1:], ":free")
	if id := route + "-" + short; validID(id) && !nc.nameTaken(id) {
		return id
	}
	if id := providerID + "/" + up; validID(id) && !nc.nameTaken(id) {
		return id
	}
	return "model_" + digest(providerID + up)[:20]
}

func freeSummary(route string, ch freeChanges) string {
	lines := []string{"免费模型自动维护 · 路由 " + route}
	for _, x := range []struct {
		label string
		ids   []string
	}{{"加入并启用", ch.added}, {"重新启用", ch.enabled}, {"停用（上游已下架）", ch.disabled}, {"开启丢弃推理内容", ch.dropped}} {
		if len(x.ids) > 0 {
			lines = append(lines, x.label+"："+strings.Join(x.ids, "、"))
		}
	}
	return strings.Join(lines, "\n")
}
