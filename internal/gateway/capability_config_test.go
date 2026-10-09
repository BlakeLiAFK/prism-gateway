package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestProtocolAddressesAndSnapshot(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Errorf("协议地址错误: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	m := modelFixture("m", "chat")
	m.Protocols = []string{"chat", "messages"}
	h.configure(t, "http://127.0.0.1:1", m)
	h.change(t, func(c *Config) {
		c.Providers[0].ProtocolURLs = map[string]string{"messages": up.URL + "/anthropic/v1"}
	})
	requireStatus(t, h.generate(t, "messages", requestFixture("messages", "m")), 200)
	before := raw(h.s.Config())
	// 版本冲突必须不改变已发布快照，JSON 解码也不能提前改动共享 map/slice。
	w, _ := h.rpc(t, "provider.save", Object{"id": "p_test", "version": 0, "provider": Object{"protocol_urls": Object{"messages": "https://other.example/v1"}}}, h.token)
	if w.Code < 400 || raw(h.s.Config()) != before {
		t.Fatal("失败的 provider.save 改动了配置快照")
	}
	w, _ = h.rpc(t, "model.save", Object{"id": "m", "version": 0, "model": Object{"supported_protocols": []string{"chat", "responses"}}}, h.token)
	if w.Code < 400 || raw(h.s.Config()) != before {
		t.Fatal("失败的 model.save 改动了配置快照")
	}
	w, _ = h.rpc(t, "provider.save", Object{"id": "p_test", "version": h.s.Config().Version, "provider": Object{"protocol_urls": Object{}}}, h.token)
	requireStatus(t, w, 200)
	if len(h.s.Config().Providers[0].ProtocolURLs) != 0 {
		t.Fatal("清空协议地址无效")
	}
}

func TestSyncedProtocolsRefreshPreservesSettings(t *testing.T) {
	h := newHarness(t)
	m := modelFixture("existing", "chat")
	m.Tools, m.Vision, m.Name, m.DropReasoning, m.DropDocuments = false, false, "人工名称", true, true
	m.Protocols = []string{"chat"}
	locked := m
	locked.ID, locked.Upstream, locked.ProtocolsLocked = "locked", "locked", true
	h.configure(t, "http://127.0.0.1:1", m, locked)
	p := h.s.Config().Providers[0]
	p.Kind = "commandcode"
	items := []any{}
	for _, id := range []string{m.Upstream, locked.Upstream, "new"} {
		items = append(items, Object{"id": id, "supported_endpoints": []any{"/chat/completions", "/responses"}})
	}
	_, err := h.a.applySyncedModels(p, h.s.Config().Version, items)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := h.s.Config().model(m.ID)
	if !slices.Equal(got.Protocols, []string{"chat", "responses"}) || got.Name != m.Name || got.Tools || got.Vision || got.ToolsUnknown || got.VisionUnknown || !got.DropReasoning || !got.DropDocuments {
		t.Fatalf("同步覆盖了人工配置: %+v", got)
	}
	got, _ = h.s.Config().model("locked")
	if !slices.Equal(got.Protocols, []string{"chat"}) {
		t.Fatal("锁定协议被覆盖")
	}
	got, _ = h.s.Config().model("new")
	if !got.ToolsUnknown || !got.VisionUnknown {
		t.Fatal("缺失的能力声明不应变成支持或不支持")
	}
}

func TestUnknownCapabilitiesAndProtocols(t *testing.T) {
	h := newHarness(t)
	m := modelFixture("m", "messages")
	m.Tools, m.Vision = false, false
	m.ToolsUnknown, m.VisionUnknown = true, true
	h.configure(t, "http://127.0.0.1:1", m)
	body := toolImageRequest()
	body["tools"] = []any{Object{"name": "read_file", "input_schema": Object{"type": "object"}}}
	selected, _, err := h.a.Engine.selections(h.s.Config(), body, "messages", "")
	if err != nil || len(selected) != 1 {
		t.Fatalf("未知能力应交上游裁决: %v", err)
	}
	h.change(t, func(c *Config) { c.Models[0].Protocols = []string{} })
	selected, _, err = h.a.Engine.selections(h.s.Config(), body, "messages", "")
	if err != nil || len(selected) != 0 {
		t.Fatal("显式空协议不应变为默认 chat/messages")
	}
	h.change(t, func(c *Config) { c.Models[0].Protocols = nil })
	w, _ := h.rpc(t, "model.save", Object{"id": "m", "version": h.s.Config().Version, "model": Object{"tools": false, "vision": false}}, h.token)
	requireStatus(t, w, 200)
	got, _ := h.s.Config().model("m")
	if got.ToolsUnknown || got.VisionUnknown {
		t.Fatal("人工确认未清除未知状态")
	}
	selected, _, _ = h.a.Engine.selections(h.s.Config(), body, "messages", "")
	if len(selected) != 0 {
		t.Fatal("人工禁止的能力不能自动放行")
	}
}

func TestProtocolConfigurationValidation(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "https://example.com/v1", modelFixture("m", "chat"))
	for _, protocols := range [][]string{{"messages"}, {"chat", "chat"}, {"chat", "unknown"}, {"chat", "systemone"}} {
		c := cloneConfig(h.s.Config())
		c.Models[0].Protocols = protocols
		if c.Validate() == nil {
			t.Fatalf("非法协议应被拒绝: %v", protocols)
		}
	}
	for _, address := range []string{"ftp://example.com", "https://user:pass@example.com", "https://127.0.0.1/v1"} {
		c := cloneConfig(h.s.Config())
		c.Providers[0].AllowPrivate = false
		c.Providers[0].ProtocolURLs = map[string]string{"messages": address}
		if c.Validate() == nil {
			t.Fatal("非法协议地址应被拒绝")
		}
	}
}

func TestZDRRejectsAmbiguousHeaders(t *testing.T) {
	for _, values := range [][]string{{"0", "1"}, {"1", "0"}, {""}, {"true"}, {"1,0"}} {
		_, err := constrainSelections(nil, http.Header{"X-Cmd-Zdr": values})
		if err == nil {
			t.Fatalf("不应接受歧义隐私头: %v", values)
		}
	}
}

func TestZDRClearsIneligibleAffinity(t *testing.T) {
	selections := []selection{
		{Model: Model{ID: "or"}, Provider: Provider{Kind: "openrouter"}, Pinned: "or"},
		{Model: Model{ID: "cc"}, Provider: Provider{Kind: "commandcode"}, Pinned: "or"},
	}
	out, err := constrainSelections(selections, http.Header{"X-Cmd-Zdr": {"1"}})
	if err != nil || len(out) != 1 || out[0].Pinned != "" {
		t.Fatalf("不能等待已被 ZDR 排除的亲和模型: %+v %v", out, err)
	}
}

func TestMalformedCapabilityDeclarationsStayUnknown(t *testing.T) {
	for _, value := range []any{nil, "image", true, Object{}} {
		src := Object{"supported_endpoints": value, "supported_parameters": value, "input_modalities": value}
		if syncedProtocols(Provider{Kind: "commandcode"}, src) != nil {
			t.Fatalf("错误类型不应清空协议: %#v", value)
		}
		m := newSyncedModel("m", "p", "m", "m", "chat", src)
		if !m.ToolsUnknown || !m.VisionUnknown {
			t.Fatalf("错误声明不应变为确认不支持: %#v", value)
		}
	}
	src := Object{"supported_endpoints": []any{}, "supported_parameters": []any{}, "input_modalities": []any{}}
	if protocols := syncedProtocols(Provider{Kind: "commandcode"}, src); protocols == nil || len(protocols) != 0 {
		t.Fatal("真实空数组应保留为空声明")
	}
	m := newSyncedModel("m", "p", "m", "m", "chat", src)
	if !m.ToolsUnknown || !m.VisionUnknown || m.Tools || m.Vision {
		t.Fatal("空能力清单应交给上游判断")
	}
}

func TestLegacyCustomOpenRouterKeepsProviderCapabilities(t *testing.T) {
	p := Provider{Kind: "custom", BaseURL: "https://openrouter.ai/api/v1", Auth: "auto", Secret: "test-secret"}
	if !slices.Equal(syncedProtocols(p, Object{}), []string{"chat", "messages", "responses"}) {
		t.Fatal("既有自定义 OpenRouter 供应商未获得多协议能力")
	}
	req, _ := http.NewRequest("POST", p.BaseURL+"/messages", nil)
	requestHeaders(req, &http.Request{Header: http.Header{}}, p, "messages", "")
	if req.Header.Get("Authorization") != "Bearer test-secret" || req.Header.Get("x-api-key") != "" {
		t.Fatal("自定义 OpenRouter 应继续使用 Bearer")
	}
}
