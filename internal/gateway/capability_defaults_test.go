package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSyncedCapabilityDefaults(t *testing.T) {
	for _, src := range []Object{
		{},
		{"supported_parameters": []any{}, "input_modalities": []any{}},
		{"supported_parameters": []any{"temperature"}, "architecture": Object{"input_modalities": []any{"text"}}},
	} {
		m := newSyncedModel("m", "p_test", "m", "m", "messages", src)
		if !m.ToolsUnknown || !m.VisionUnknown {
			t.Fatalf("未声明的能力应为未知: %+v", m)
		}
		verifyDefaultCapabilities(t, m, false, 400)
	}
	m := newSyncedModel("m", "p_test", "m", "m", "messages", Object{
		"supported_parameters": []any{"tools"}, "input_modalities": []any{"text", "image"},
	})
	if !m.Tools || !m.Vision || m.ToolsUnknown || m.VisionUnknown {
		t.Fatalf("明确支持的能力应保留: %+v", m)
	}
}

func TestAdminCapabilityDefaults(t *testing.T) {
	for _, status := range []int{200, 400} {
		verifyDefaultCapabilities(t, Model{}, true, status)
	}
}

func verifyDefaultCapabilities(t *testing.T, m Model, viaAdmin bool, status int) {
	t.Helper()
	h := newHarness(t)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body Object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(arr(body["tools"])) != 1 || !requestHasImage(body, "messages") {
			t.Error("工具或图像未送达上游")
		}
		w.Header().Set("Content-Type", "application/json")
		if status == 200 {
			_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"upstream capability rejected"}}`)
	}))
	defer up.Close()
	h.configure(t, up.URL)
	if !viaAdmin {
		h.change(t, func(c *Config) { c.Models = []Model{m} })
	}
	if viaAdmin {
		w, _ := h.rpc(t, "model.save", Object{"version": h.s.Config().Version, "model": Object{
			"id": "m", "name": "m", "provider_id": "p_test", "upstream": "m", "protocol": "messages",
		}}, h.token)
		requireStatus(t, w, 200)
	}
	got, _ := h.s.Config().model("m")
	if !got.ToolsUnknown || !got.VisionUnknown {
		t.Fatal("新增能力默认值未保存")
	}
	body := (reasonRequest{tools: true, image: true}).build("m")
	requireStatus(t, h.generate(t, "messages", body), status)
	if calls.Load() != 1 {
		t.Fatalf("应由上游判断，实际调用次数 %d", calls.Load())
	}
	w, _ := h.rpc(t, "model.save", Object{"id": "m", "version": h.s.Config().Version,
		"model": Object{"tools": false, "vision": false}}, h.token)
	requireStatus(t, w, 200)
	requireStatus(t, h.generate(t, "messages", body), 400)
	if calls.Load() != 1 {
		t.Fatal("人工禁止后不应调用上游")
	}
}
