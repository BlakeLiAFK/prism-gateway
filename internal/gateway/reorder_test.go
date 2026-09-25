package gateway

import (
	"strings"
	"testing"
)

func providerOrder(c Config) string {
	var ids []string
	for _, p := range c.Providers {
		if strings.HasPrefix(p.ID, "ps_") {
			ids = append(ids, p.ID)
		}
	}
	return strings.Join(ids, ",")
}

// 供应商顺序与路由同理：整体写入、编辑后不丢、重启后仍在
func TestProviderReorderPersists(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"ps_a", "ps_b", "ps_c"} {
		w, out := h.rpc(t, "provider.save", Object{"version": h.a.Store.Config().Version, "id": id,
			"provider": Object{"id": id, "name": id, "base_url": "https://example.com/v1", "api_key": "k"}}, h.token)
		if w.Code != 200 {
			t.Fatalf("创建供应商失败: %v", out)
		}
	}
	w, out := h.rpc(t, "provider.reorder", Object{"version": h.a.Store.Config().Version, "ids": []any{"ps_c", "ps_a", "ps_b"}}, h.token)
	if w.Code != 200 {
		t.Fatalf("重排失败: %v", out)
	}
	if got := providerOrder(h.a.Store.Config()); got != "ps_c,ps_a,ps_b" {
		t.Fatalf("顺序未按请求写入: %s", got)
	}
	// 编辑不带 sort 时沿用原值，不能让供应商跳回最前
	h.rpc(t, "provider.save", Object{"version": h.a.Store.Config().Version, "id": "ps_b",
		"provider": Object{"name": "renamed"}}, h.token)
	if got := providerOrder(h.a.Store.Config()); got != "ps_c,ps_a,ps_b" {
		t.Fatalf("编辑后顺序改变: %s", got)
	}
	w, _ = h.rpc(t, "provider.reorder", Object{"version": h.a.Store.Config().Version, "ids": []any{}}, h.token)
	if w.Code != 400 {
		t.Fatalf("空 ids 应被拒绝，实得 %d", w.Code)
	}
	h.a.Store.DB.Close()
	s, e := OpenStore(strings.TrimSuffix(h.s.KeyPath, ".key"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	if got := providerOrder(s.Config()); got != "ps_c,ps_a,ps_b" {
		t.Fatalf("重启后顺序丢失: %s", got)
	}
}
