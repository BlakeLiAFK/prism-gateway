package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeCodeContextKeep(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, keep := range []string{`"all"`, `{"type":"all"}`, `{"type":"thinking_turns","value":2}`} {
			t.Run(protocol+keep, func(t *testing.T) {
				h := newHarness(t)
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var got Object
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
						return
					}
					if (got["context_management"] != nil) != (protocol == "messages") {
						t.Error("上下文策略必须原生保留或跨协议披露")
					}
					if protocol == "messages" && raw(got["context_management"]) != `{"edits":[{"keep":`+keep+`,"type":"clear_thinking_20251015"}]}` {
						t.Error("原生 keep 被改写")
					}
					w.Header().Set("Content-Type", "application/json")
					reply := `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
					if protocol == "responses" {
						reply = `{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
					}
					if protocol == "messages" {
						reply = `{"type":"message","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
					}
					_, _ = io.WriteString(w, reply)
				}))
				defer up.Close()
				h.configure(t, up.URL, modelFixture("m", protocol))
				var body Object
				if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":`+keep+`}]}}`), &body); err != nil {
					t.Fatal(err)
				}
				w := h.generate(t, "messages", body)
				requireStatus(t, w, 200)
				if protocol != "messages" && !strings.Contains(w.Header().Get("X-Prism-Ignored"), "clear_thinking_20251015") {
					t.Fatal("跨协议未披露 no-op")
				}
			})
		}
	}
}

func TestContextKeepRejectsInvalidValuesAndThinking(t *testing.T) {
	for _, keep := range []any{"none", 2, Object{}, Object{"type": "all", "value": 1}, Object{"type": "thinking_turns", "value": 0}, Object{"type": "thinking_turns", "value": -1}, Object{"type": "thinking_turns", "value": 1.5}, Object{"type": "thinking_turns", "value": "2"}, Object{"type": "thinking_turns", "value": 2, "unknown": true}} {
		body := requestFixture("messages", "m")
		body["context_management"] = Object{"edits": []any{Object{"type": "clear_thinking_20251015", "keep": keep}}}
		if _, err := decodeCanonical(body, "messages"); err == nil {
			t.Errorf("非法 keep 被接受: %v", keep)
		}
	}
	for _, kind := range []string{"thinking", "redacted_thinking"} {
		body := requestFixture("messages", "m")
		body["context_management"] = Object{"edits": []any{Object{"type": "clear_thinking_20251015", "keep": "all"}}}
		body["messages"] = []any{Object{"role": "assistant", "content": []any{Object{"type": kind, "thinking": "private", "signature": "opaque", "data": "opaque"}}}}
		if _, err := decodeCanonical(body, "messages"); err == nil {
			t.Fatalf("%s 不得随 no-op 丢失", kind)
		}
	}
}
