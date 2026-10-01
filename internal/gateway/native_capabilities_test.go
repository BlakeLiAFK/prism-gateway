package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeMultimodalAndReasoningRequests(t *testing.T) {
	cases := []struct{ name, kind, protocol, body, reply string }{
		{"openrouter-media", "openrouter", "chat", `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERi0="}},{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}},{"type":"video_url","video_url":{"url":"https://example.com/clip.mp4"}}]}],"plugins":[{"id":"file-parser","pdf":{"engine":"native"}}],"provider":{"zdr":true,"require_parameters":true}}`, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`},
		{"openrouter-document", "openrouter", "messages", `{"model":"m","max_tokens":128,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="},"citations":{"enabled":true}}]}],"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, `{"type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
		{"deepseek-tool-history", "deepseek", "chat", `{"model":"m","messages":[{"role":"user","content":"read"},{"role":"assistant","content":null,"reasoning_content":"retained reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"done"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}],"thinking":{"type":"enabled"}}`, `{"choices":[{"message":{"role":"assistant","reasoning_content":"final reasoning","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`},
		{"responses-encrypted-history", "openrouter", "responses", `{"model":"m","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque-signature","summary":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}],"include":["reasoning.encrypted_content"],"tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}],"parallel_tool_calls":false}`, `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var got Object
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("x-api-key") != "" {
					t.Error("OpenRouter/DeepSeek 应使用供应商 Bearer 鉴权")
				}
				if r.URL.Path != pathFor(tc.protocol) {
					t.Errorf("错误原生端点: %s", r.URL.Path)
				}
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer up.Close()
			m := modelFixture("m", "chat")
			m.Protocols = []string{"chat", "messages", "responses"}
			h.configure(t, up.URL, m)
			h.change(t, func(c *Config) { c.Providers[0].Kind = tc.kind })
			var body Object
			_ = json.Unmarshal([]byte(tc.body), &body)
			w := h.generate(t, tc.protocol, body)
			requireStatus(t, w, 200)
			body["model"] = m.Upstream
			if raw(got) != raw(body) || w.Body.String() != tc.reply {
				t.Fatalf("原生请求或响应被改写: %s %s", raw(got), w.Body)
			}
		})
	}
}

func TestDeepSeekIgnoredContractsAreRejectedBeforeSending(t *testing.T) {
	for _, body := range []Object{
		{"model": "m", "input": "x", "previous_response_id": "r1"},
		{"model": "m", "input": "x", "store": true},
		{"model": "m", "input": "x", "background": true},
		{"model": "m", "input": "x", "conversation": "c1"},
		{"model": "m", "input": "x", "parallel_tool_calls": false},
		{"model": "m", "input": "x", "tools": []any{Object{"type": "web_search"}}},
		{"model": "m", "messages": []any{}, "output_config": Object{"format": Object{"type": "json_schema"}}},
		{"model": "m", "messages": []any{}, "context_management": Object{"edits": []any{}}},
	} {
		h := newHarness(t)
		m := modelFixture("m", "chat")
		m.Protocols = []string{"chat", "messages", "responses"}
		h.configure(t, "http://127.0.0.1:1", m)
		h.change(t, func(c *Config) { c.Providers[0].Kind = "deepseek" })
		protocol := "responses"
		if body["messages"] != nil {
			protocol = "messages"
		}
		w := h.generate(t, protocol, body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "DeepSeek") {
			t.Fatalf("不能让上游静默忽略硬约束: %d %s", w.Code, w.Body)
		}
	}
}

func TestZDRNeverFallsBackToOtherProvider(t *testing.T) {
	h := newHarness(t)
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		hits++
		if r.Header.Get("x-cmd-zdr") != "1" {
			t.Error("ZDR 约束丢失")
		}
		w.WriteHeader(429)
	}))
	defer up.Close()
	a, b := modelFixture("cc", "chat"), modelFixture("or", "chat")
	b.ProviderID = "other"
	h.configure(t, up.URL, a)
	h.change(t, func(c *Config) {
		c.Providers[0].Kind = "commandcode"
		p := c.Providers[0]
		p.ID, p.Kind = "other", "openrouter"
		c.Providers = append(c.Providers, p)
		c.Models = append(c.Models, b)
		c.Routes = []Route{{ID: "r", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "cc", Weight: 1}, {ModelID: "or", Weight: 1}}}}
	})
	r := httptest.NewRequest("POST", pathFor("chat"), strings.NewReader(raw(requestFixture("chat", "r"))))
	r.Header.Set("x-cmd-zdr", "1")
	w := httptest.NewRecorder()
	h.a.Engine.Handle(w, r, "chat", Principal{ID: "k"})
	if w.Code != 429 || hits != 1 {
		t.Fatalf("ZDR 请求不能切到其他供应商: status=%d hits=%d", w.Code, hits)
	}
}
