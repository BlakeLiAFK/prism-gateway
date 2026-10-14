package gateway

import (
	"testing"
)

// setRouteAffinity 走管理接口的根源开关，返回保存后的路由
func setRouteAffinity(t *testing.T, h *harness, id string, on bool) {
	t.Helper()
	w, out := h.rpc(t, "route.affinity", Object{"version": h.s.Config().Version, "id": id, "affinity": on}, h.token)
	if w.Code != 200 {
		t.Fatalf("route.affinity 失败: %v", out)
	}
}

// swapCandidates 把第 1 组的候选顺序倒过来：不改变亲和开关时，priority 会选到另一个模型
func swapCandidates(t *testing.T, h *harness, id string) {
	t.Helper()
	h.change(t, func(c *Config) {
		for i := range c.Routes {
			if c.Routes[i].ID != id {
				continue
			}
			cs := c.Routes[i].Groups[0].Candidates
			c.Routes[i].Groups[0].Candidates = []Candidate{cs[1], cs[0]}
		}
	})
}

// 路由级开关关掉后，已建立的会话绑定不再把上次的模型顶到最前：请求按组内策略重新排。
// 写会话记录的行为保持不变（关闭期间仍写），只在读绑定与打分两处生效。
func TestRouteAffinityOffStopsPinning(t *testing.T) {
	h, _, call := affinityHarness(t)
	setRouteAffinity(t, h, "auto", false)
	r, _ := h.s.Config().route("auto")
	if r.AffinityDefault == nil || *r.AffinityDefault {
		t.Fatalf("路由默认值应为关: %+v", r.AffinityDefault)
	}
	for i, g := range r.Groups {
		if g.Affinity != nil {
			t.Fatalf("复位后第 %d 组应回到继承（nil）: %+v", i+1, g.Affinity)
		}
	}
	if m := call(nil); m != "first" {
		t.Fatalf("priority 应先选 first，实得 %q", m)
	}
	if got := boundModels(t, h); len(got) != 1 || got[0] != "first" {
		t.Fatalf("关闭亲和后仍应写会话记录（写行为未改），实得 %v", got)
	}
	// 倒序后：亲和关着，没有 +10000 的加分，按 priority 落回新的第一个
	swapCandidates(t, h, "auto")
	if m := call(nil); m != "second" {
		t.Fatalf("关闭亲和后不应钉在原模型，实得 %q", m)
	}
}

// 开着亲和（默认）时，倒序也改不了已绑定会话的落点：亲和加分压过基础分
func TestRouteAffinityOnKeepsPinning(t *testing.T) {
	h, _, call := affinityHarness(t)
	if m := call(nil); m != "first" {
		t.Fatalf("首个请求应落在 first，实得 %q", m)
	}
	swapCandidates(t, h, "auto")
	if m := call(nil); m != "first" {
		t.Fatalf("开启亲和后应继续沿用 first，实得 %q", m)
	}
}

// 组可以用自己的 affinity 覆盖路由默认值：路由关着，这一组仍可显式打开
func TestGroupAffinityOverridesRouteDefault(t *testing.T) {
	h, _, call := affinityHarness(t)
	w, out := h.rpc(t, "route.save", Object{"version": h.s.Config().Version, "id": "auto", "route": Object{
		"affinity_default": false,
		"groups": []any{Object{"name": "主", "strategy": "priority", "affinity": true, "candidates": []any{
			Object{"model_id": "first", "weight": 10}, Object{"model_id": "second", "weight": 10}}}}}}, h.token)
	if w.Code != 200 {
		t.Fatalf("保存路由失败: %v", out)
	}
	if m := call(nil); m != "first" {
		t.Fatalf("首个请求应落在 first，实得 %q", m)
	}
	swapCandidates(t, h, "auto")
	if m := call(nil); m != "first" {
		t.Fatalf("组显式打开亲和后应继续沿用 first，实得 %q", m)
	}
	// 拨动根源开关会清掉组的显式设置，这一组的覆盖随之失效
	setRouteAffinity(t, h, "auto", false)
	r, _ := h.s.Config().route("auto")
	if r.Groups[0].Affinity != nil {
		t.Fatalf("根源开关应复位组的显式设置: %+v", r.Groups[0].Affinity)
	}
	if m := call(nil); m != "second" {
		t.Fatalf("复位后应回到按策略选择，实得 %q", m)
	}
}

// 只改某一组：带上 group 时不动路由默认值，也不影响其他组
func TestRouteAffinitySingleGroup(t *testing.T) {
	h, _, _ := affinityHarness(t)
	// 用 groups 重建：旧版单组字段会落下「第 1 组显式打开」，这里要的是两组都继承
	w, out := h.rpc(t, "route.save", Object{"version": h.s.Config().Version, "id": "auto", "route": Object{"groups": []any{
		Object{"name": "主", "strategy": "priority", "candidates": []any{Object{"model_id": "first", "weight": 10}}},
		Object{"name": "兜底", "strategy": "priority", "candidates": []any{Object{"model_id": "second", "weight": 10}}}}}}, h.token)
	if w.Code != 200 {
		t.Fatalf("保存两组路由失败: %v", out)
	}
	w, out = h.rpc(t, "route.affinity", Object{"version": h.s.Config().Version, "id": "auto", "group": 1, "affinity": false}, h.token)
	if w.Code != 200 {
		t.Fatalf("按组关闭失败: %v", out)
	}
	r, _ := h.s.Config().route("auto")
	if r.AffinityDefault == nil || !*r.AffinityDefault {
		t.Fatalf("按组修改不应动路由默认值: %+v", r.AffinityDefault)
	}
	if r.Groups[0].Affinity != nil {
		t.Fatalf("按组修改不应动第 1 组: %+v", r.Groups[0].Affinity)
	}
	if r.Groups[1].Affinity == nil || *r.Groups[1].Affinity {
		t.Fatalf("第 2 组应被显式关闭: %+v", r.Groups[1].Affinity)
	}
}

// 参数边界：group 越界、路由不存在、affinity 非布尔都要明确拒绝
func TestRouteAffinityParamValidation(t *testing.T) {
	h, _, _ := affinityHarness(t)
	base := func(extra Object) Object {
		p := Object{"version": h.s.Config().Version, "id": "auto", "affinity": true}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	if w, _ := h.rpc(t, "route.affinity", base(Object{"group": 9}), h.token); w.Code != 400 {
		t.Errorf("group 越界应 400，实得 %d", w.Code)
	}
	if w, _ := h.rpc(t, "route.affinity", Object{"version": h.s.Config().Version, "id": "nope", "affinity": true}, h.token); w.Code != 404 {
		t.Errorf("路由不存在应 404，实得 %d", w.Code)
	}
	if w, _ := h.rpc(t, "route.affinity", Object{"version": h.s.Config().Version, "id": "auto", "affinity": "yes"}, h.token); w.Code != 400 {
		t.Errorf("affinity 非布尔应 400，实得 %d", w.Code)
	}
}

// 组的「继承」状态要持久化：落库再加载后，没单独设置的组仍是 nil，
// 这样之后拨动路由开关才会一次影响它们
func TestRouteAffinityInheritancePersists(t *testing.T) {
	h, _, _ := affinityHarness(t)
	w, out := h.rpc(t, "route.save", Object{"version": h.s.Config().Version, "id": "auto", "route": Object{
		"affinity_default": false,
		"groups":           []any{Object{"name": "主", "strategy": "priority", "candidates": []any{Object{"model_id": "first", "weight": 10}}}}}}, h.token)
	if w.Code != 200 {
		t.Fatalf("保存路由失败: %v", out)
	}
	if err := h.s.load(); err != nil {
		t.Fatal(err)
	}
	r, _ := h.s.Config().route("auto")
	if r.AffinityDefault == nil || *r.AffinityDefault {
		t.Fatalf("重新加载后路由默认值应保持关: %+v", r.AffinityDefault)
	}
	if r.Groups[0].Affinity != nil {
		t.Fatalf("重新加载后继承的组应保持 nil: %+v", r.Groups[0].Affinity)
	}
	if r.affinity(r.Groups[0]) {
		t.Fatal("继承关闭的组解析后应为关")
	}
	// 未显式设置过默认值的路由（旧配置没有 affinity_default）按 true 兜底
	h.change(t, func(c *Config) {
		c.Routes[0].AffinityDefault = nil
	})
	if !h.s.Config().Routes[0].affinity(Group{}) {
		t.Fatal("都没设置时应按 true 兜底")
	}
}
