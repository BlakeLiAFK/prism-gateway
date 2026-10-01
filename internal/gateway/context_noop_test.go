package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepSeekNoopContextPreservesNativeRequest(t *testing.T) {
	for _, context := range []string{`{}`, `{"edits":[]}`, `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`} {
		t.Run(context, func(t *testing.T) {
			h := newHarness(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got Object
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
					return
				}
				if got["context_management"] != nil || str(obj(got["thinking"]), "type") != "disabled" {
					t.Errorf("错误的原生请求适配: %s", raw(got))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer up.Close()
			h.configure(t, up.URL, modelFixture("m", "messages"))
			h.change(t, func(c *Config) { c.Providers[0].Kind = "deepseek" })
			var body Object
			if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"disabled"},"context_management":`+context+`}`), &body); err != nil {
				t.Fatal(err)
			}
			w := h.generate(t, "messages", body)
			requireStatus(t, w, 200)
			if !strings.Contains(w.Header().Get("X-Prism-Ignored"), "context_management") {
				t.Fatal("未披露供应商空操作适配")
			}
		})
	}
}

func TestDeepSeekContextWithHistoryStillRejected(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("m", "messages"))
	h.change(t, func(c *Config) { c.Providers[0].Kind = "deepseek" })
	body := requestFixture("messages", "m")
	body["messages"] = []any{Object{"role": "assistant", "content": []any{Object{"type": "thinking", "thinking": "retained", "signature": "opaque"}}}, Object{"role": "user", "content": "continue"}}
	body["context_management"] = Object{"edits": []any{Object{"type": "clear_thinking_20251015", "keep": Object{"type": "thinking_turns", "value": 1}}}}
	w := h.generate(t, "messages", body)
	requireStatus(t, w, 400)
}

func TestContextEmptyAndMalformedEdits(t *testing.T) {
	for _, edits := range []any{nil, []any{}, "", Object{}, true, []any{nil}} {
		body := requestFixture("messages", "m")
		body["context_management"] = Object{"edits": edits}
		_, err := decodeCanonical(body, "messages")
		valid := edits == nil
		if a, ok := edits.([]any); ok && len(a) == 0 {
			valid = true
		}
		if (err == nil) != valid {
			t.Errorf("edits=%#v: %v", edits, err)
		}
	}
}
