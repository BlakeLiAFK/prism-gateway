package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeclaredProtocolsPreserveNativeRequest(t *testing.T) {
	h := newHarness(t)
	var got Object
	var path string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer up.Close()
	m := modelFixture("m", "chat")
	if err := decode(Object{"supported_protocols": []string{"chat", "responses"}}, &m); err != nil {
		t.Fatal(err)
	}
	h.configure(t, up.URL, m)
	body := Object{"model": "m", "input": []any{Object{"type": "reasoning", "id": "rs_1", "encrypted_content": "opaque-signature"}}, "previous_response_id": "resp_old", "tools": []any{Object{"type": "web_search"}}, "text": Object{"format": Object{"type": "json_schema", "name": "result", "strict": true, "schema": Object{"type": "object"}}}}
	w := h.generate(t, "responses", body)
	requireStatus(t, w, 200)
	if path != "/responses" || w.Header().Get("X-Prism-Protocol-Mode") != "native" {
		t.Fatalf("应使用原生 Responses: path=%s headers=%v", path, w.Header())
	}
	delete(got, "model")
	delete(body, "model")
	if raw(got) != raw(body) {
		t.Fatalf("原生字段被改写: %s", raw(got))
	}
	if h.s.Config().Models[0].Protocol != "chat" {
		t.Fatal("请求修改了配置快照")
	}
}

func TestImageDetectionIgnoresToolArguments(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer up.Close()
	m := modelFixture("m", "messages")
	m.Vision = false
	h.configure(t, up.URL, m)
	w := h.generate(t, "messages", Object{"model": "m", "max_tokens": 32, "messages": []any{Object{"role": "assistant", "content": []any{Object{"type": "tool_use", "id": "call_1", "name": "read_file", "input": Object{"type": "image", "image_url": "logo.png"}}}}}})
	requireStatus(t, w, 200)
}

func TestZDRConstraintThroughHandler(t *testing.T) {
	for _, kind := range []string{"commandcode", "custom"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			called := false
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.Header.Get("x-cmd-zdr") != "1" {
					t.Error("零数据保留约束丢失")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
			}))
			defer up.Close()
			h.configure(t, up.URL, modelFixture("m", "chat"))
			h.change(t, func(c *Config) { c.Providers[0].Kind = kind })
			r := httptest.NewRequest("POST", pathFor("chat"), strings.NewReader(raw(requestFixture("chat", "m"))))
			r.Header.Set("x-cmd-zdr", "1")
			w := httptest.NewRecorder()
			h.a.Engine.Handle(w, r, "chat", Principal{ID: "key"})
			if kind == "commandcode" {
				requireStatus(t, w, 200)
			} else if w.Code < 400 || called {
				t.Fatalf("不能把 ZDR 请求发送给未支持供应商: code=%d called=%t", w.Code, called)
			}
		})
	}
}
