package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNativePendingResponseIsNotEmptyFailure(t *testing.T) {
	for _, state := range []string{"queued", "in_progress"} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			reply := `{"id":"resp_1","object":"response","status":"` + state + `","output":[],"usage":null}`
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(reply))
			}))
			defer up.Close()
			h.configure(t, up.URL, modelFixture("m", "responses"))
			body := requestFixture("responses", "m")
			body["background"] = true
			w := h.generate(t, "responses", body)
			requireStatus(t, w, 200)
			if w.Body.String() != reply {
				t.Fatal("原生异步状态被改写")
			}
		})
	}
}

func TestTruncatedJSONToolCannotBecomeCompleted(t *testing.T) {
	for protocol, fixture := range map[string]string{
		"chat":     `{"choices":[{"finish_reason":"length","message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":"{}"}}]}}]}`,
		"messages": `{"stop_reason":"max_tokens","content":[{"type":"tool_use","id":"c1","name":"run","input":{}}]}`,
	} {
		var body Object
		if err := json.Unmarshal([]byte(fixture), &body); err != nil {
			t.Fatal(err)
		}
		if _, err := decodeCompletion(body, protocol); err == nil {
			t.Fatalf("%s 截断工具被标记完成", protocol)
		}
	}
}

func TestDropReasoningDoesNotEraseHardThinkingControls(t *testing.T) {
	for _, thinking := range []Object{{"type": "disabled"}, {"type": "adaptive", "between_tools": false}, {"type": "future"}} {
		h := newHarness(t)
		m := modelFixture("m", "chat")
		m.DropReasoning = true
		h.configure(t, "http://127.0.0.1:1", m)
		body := requestFixture("messages", "m")
		body["thinking"] = thinking
		w := h.generate(t, "messages", body)
		requireStatus(t, w, 400)
	}
}

func TestExplicitPlainTextFormatIsDefault(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		body := requestFixture(protocol, "m")
		if protocol == "chat" {
			body["response_format"] = Object{"type": "text"}
		} else {
			body["text"] = Object{"format": Object{"type": "text"}}
		}
		c, err := decodeCanonical(body, protocol)
		if err != nil || c.Format != nil {
			t.Fatalf("显式纯文本默认值 %s: %v", protocol, err)
		}
	}
}

func TestChatRequestValidationErrorCannotBeOverwritten(t *testing.T) {
	for key, value := range map[string]any{"store": true, "future_control": true, "response_format": Object{"type": "json_schema", "json_schema": Object{"name": "x"}}} {
		body := requestFixture("chat", "m")
		body[key] = value
		if _, err := decodeCanonical(body, "chat"); err == nil {
			t.Fatalf("请求字段 %s 的校验错误被后续消息解码覆盖", key)
		}
	}
}
