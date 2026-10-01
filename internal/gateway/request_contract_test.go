package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCrossProtocolStructuredOutputPreserved(t *testing.T) {
	schema := Object{
		"type":                 "object",
		"properties":           Object{"ok": Object{"type": "boolean"}},
		"required":             []any{"ok"},
		"additionalProperties": false,
	}
	tests := []struct {
		name     string
		from     string
		to       string
		decorate func(Object)
		assert   func(*testing.T, Object)
	}{
		{
			name: "messages-to-chat", from: "messages", to: "chat",
			decorate: func(o Object) {
				o["output_config"] = Object{"effort": "high", "format": Object{"type": "json_schema", "schema": schema}}
			},
			assert: func(t *testing.T, got Object) {
				format := obj(got["response_format"])
				js := obj(format["json_schema"])
				if str(format, "type") != "json_schema" || !boolean(js, "strict") || raw(js["schema"]) != raw(schema) {
					t.Fatalf("Chat 结构化输出不完整: %s", raw(format))
				}
			},
		},
		{
			name: "chat-to-responses", from: "chat", to: "responses",
			decorate: func(o Object) {
				o["response_format"] = Object{"type": "json_schema", "json_schema": Object{"name": "answer", "strict": true, "schema": schema}}
			},
			assert: func(t *testing.T, got Object) {
				format := obj(obj(got["text"])["format"])
				if str(format, "type") != "json_schema" || str(format, "name") != "answer" || !boolean(format, "strict") || raw(format["schema"]) != raw(schema) {
					t.Fatalf("Responses 结构化输出不完整: %s", raw(format))
				}
			},
		},
		{
			name: "responses-to-messages", from: "responses", to: "messages",
			decorate: func(o Object) {
				o["text"] = Object{"format": Object{"type": "json_schema", "name": "answer", "strict": true, "schema": schema}}
			},
			assert: func(t *testing.T, got Object) {
				format := obj(obj(got["output_config"])["format"])
				if str(format, "type") != "json_schema" || raw(format["schema"]) != raw(schema) {
					t.Fatalf("Messages 结构化输出不完整: %s", raw(format))
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			received := make(chan Object, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got Object
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				received <- got
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(completionObject(Completion{Blocks: []Block{{Kind: "text", Text: `{"ok":true}`}}}, tc.to, "m", "fixture"))
			}))
			defer up.Close()
			h.configure(t, up.URL, modelFixture("m", tc.to))
			body := requestFixture(tc.from, "m")
			tc.decorate(body)
			w := h.generate(t, tc.from, body)
			requireStatus(t, w, 200)
			tc.assert(t, <-received)
			if tc.from == "messages" && !strings.Contains(w.Header().Get("X-Prism-Ignored"), "output_config.effort") {
				t.Fatalf("未披露忽略 effort: %q", w.Header().Get("X-Prism-Ignored"))
			}
		})
	}
}

func TestMessagesStructuredOutputRejectsNonStrictSource(t *testing.T) {
	c, err := decodeCanonical(Object{
		"model": "m",
		"input": "hello",
		"text":  Object{"format": Object{"type": "json_schema", "name": "answer", "strict": false, "schema": Object{"type": "object"}}},
	}, "responses")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encodeCanonical(c, "messages", "m"); err == nil {
		t.Fatal("非严格结构化输出不能伪装成 Messages 的严格约束")
	}
}

func TestMessagesStructuredOutputRejectsDescription(t *testing.T) {
	c, err := decodeCanonical(Object{
		"model": "m",
		"input": "hello",
		"text": Object{"format": Object{
			"type": "json_schema", "name": "answer", "description": "Generate only verified data",
			"strict": true, "schema": Object{"type": "object"},
		}},
	}, "responses")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encodeCanonical(c, "messages", "m"); err == nil {
		t.Fatal("生成提示 description 不能静默丢失")
	}
}

func TestToolStrictPreservedAcrossProtocols(t *testing.T) {
	strict := true
	c := Canonical{
		Messages: []Message{{Role: "user", Blocks: []Block{{Kind: "text", Text: "hello"}}}},
		Tools:    []Tool{{Name: "lookup", Schema: Object{"type": "object"}, Strict: &strict}},
		Choice:   "auto",
	}
	for _, protocol := range []string{"chat", "messages", "responses"} {
		o, err := encodeCanonical(c, protocol, "m")
		if err != nil {
			t.Fatal(err)
		}
		tool := obj(arr(o["tools"])[0])
		if protocol == "chat" {
			tool = obj(tool["function"])
		}
		if !boolean(tool, "strict") {
			t.Fatalf("%s 丢失 tools.strict: %s", protocol, raw(o["tools"]))
		}
		decoded, err := decodeCanonical(o, protocol)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Tools[0].Strict == nil || !*decoded.Tools[0].Strict {
			t.Fatalf("%s 未读回 tools.strict", protocol)
		}
	}
}

func TestOnlyNoOpContextManagementCanBeIgnoredAcrossProtocols(t *testing.T) {
	o := requestFixture("messages", "m")
	o["context_management"] = Object{"edits": []any{Object{"type": "clear_thinking_20251015"}}}
	c, err := decodeCanonical(o, "messages")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(c.Ignored, ","), "context_management.edits.clear_thinking_20251015") {
		t.Fatalf("未披露 no-op context edit: %v", c.Ignored)
	}
	o["context_management"] = Object{"edits": []any{Object{"type": "clear_tool_uses_20250919"}}}
	if _, err := decodeCanonical(o, "messages"); err == nil {
		t.Fatal("会改变工具历史的 context edit 不能跨协议忽略")
	}
}

func TestChatToolTextArrayAccepted(t *testing.T) {
	c, err := decodeCanonical(Object{"model": "m", "messages": []any{
		Object{"role": "user", "content": "read"},
		Object{"role": "tool", "tool_call_id": "call_1", "content": []any{Object{"type": "text", "text": "file content"}}},
	}}, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Messages[1].Blocks[0].Text; got != "file content" {
		t.Fatalf("工具结果=%q", got)
	}
}

func TestCrossProtocolReasoningDetailsRequiresOptIn(t *testing.T) {
	for _, drop := range []bool{false, true} {
		t.Run(map[bool]string{false: "reject", true: "drop"}[drop], func(t *testing.T) {
			h := newHarness(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"answer","reasoning_details":[{"type":"reasoning.encrypted","data":"signature"}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer up.Close()
			model := modelFixture("m", "chat")
			model.DropReasoning = drop
			h.configure(t, up.URL, model)
			w := h.generate(t, "messages", requestFixture("messages", "m"))
			if !drop {
				if w.Code == 200 {
					t.Fatalf("未开启丢弃仍成功: %s", w.Body.String())
				}
				return
			}
			requireStatus(t, w, 200)
			if w.Header().Get("X-Prism-Dropped") != "reasoning" {
				t.Fatalf("未标注推理丢弃: %q", w.Header().Get("X-Prism-Dropped"))
			}
		})
	}
}

func TestResponsesIncompleteToolCallRejected(t *testing.T) {
	tests := []string{
		`{"id":"r","status":"incomplete","output":[{"type":"function_call","status":"incomplete","call_id":"c1","name":"read_file","arguments":"{\"path\":"}]}`,
		`{"id":"r","status":"completed","output":[{"type":"function_call","status":"completed","call_id":"","name":"read_file","arguments":"{}"}]}`,
		`{"id":"r","status":"completed","output":[{"type":"function_call","status":"completed","call_id":"c1","name":"","arguments":"{}"}]}`,
		`{"id":"r","status":"completed","output":[{"type":"function_call","status":"completed","call_id":"c1","name":"read_file","arguments":"[]"}]}`,
	}
	for i, response := range tests {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			h := newHarness(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(response))
			}))
			defer up.Close()
			h.configure(t, up.URL, modelFixture("m", "responses"))
			w := h.generate(t, "messages", requestFixture("messages", "m"))
			if w.Code == 200 {
				t.Fatalf("损坏工具调用仍成功: %s", w.Body.String())
			}
		})
	}
}

func TestMessageOrderAndAutoImageDetail(t *testing.T) {
	c, err := decodeCanonical(Object{"model": "m", "messages": []any{
		Object{"role": "user", "content": "first"},
		Object{"role": "developer", "content": "later rule"},
		Object{"role": "user", "content": []any{Object{"type": "image_url", "image_url": Object{"url": "https://example.com/a.png", "detail": "auto"}}}},
	}}, "chat")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := encodeCanonical(c, "chat", "m")
	if err != nil {
		t.Fatal(err)
	}
	if got := str(obj(arr(chat["messages"])[1]), "role"); got != "developer" {
		t.Fatalf("developer 顺序或角色丢失: %s", raw(chat["messages"]))
	}
	responses, err := encodeCanonical(c, "responses", "m")
	if err != nil {
		t.Fatal(err)
	}
	if got := str(obj(arr(responses["input"])[1]), "role"); got != "developer" {
		t.Fatalf("Responses developer 顺序或角色丢失: %s", raw(responses["input"]))
	}
	if _, err := encodeCanonical(c, "messages", "m"); err == nil {
		t.Fatal("Messages 不能把中途 developer 静默前移")
	}
}

func TestNativeNonTextOutputsPassThrough(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		response string
		want     string
	}{
		{
			name: "chat-audio", protocol: "chat",
			response: `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null,"audio":{"id":"audio_1","data":"AA==","transcript":"hello"}}}]}`,
			want:     `"audio_1"`,
		},
		{
			name: "chat-images", protocol: "chat",
			response: `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null,"images":[{"type":"image_url","image_url":{"url":"https://example.com/out.png"}}]}}]}`,
			want:     `"images"`,
		},
		{
			name: "chat-refusal", protocol: "chat",
			response: `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null,"refusal":"cannot comply"}}]}`,
			want:     `"refusal"`,
		},
		{
			name: "messages-server-tool", protocol: "messages",
			response: `{"id":"m","type":"message","role":"assistant","content":[{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"docs"}}],"stop_reason":"tool_use"}`,
			want:     `"server_tool_use"`,
		},
		{
			name: "messages-thinking", protocol: "messages",
			response: `{"id":"m","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"internal","signature":"sig"}],"stop_reason":"end_turn"}`,
			want:     `"signature"`,
		},
		{
			name: "responses-custom-tool", protocol: "responses",
			response: `{"id":"r","object":"response","status":"completed","output":[{"type":"custom_tool_call","id":"ct_1","call_id":"call_1","name":"shell","input":"pwd"}]}`,
			want:     `"custom_tool_call"`,
		},
		{
			name: "responses-image", protocol: "responses",
			response: `{"id":"r","object":"response","status":"completed","output":[{"type":"image_generation_call","id":"img_1","status":"completed","result":"AA=="}]}`,
			want:     `"image_generation_call"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			}))
			defer up.Close()
			h.configure(t, up.URL, modelFixture("m", tc.protocol))
			w := h.generate(t, tc.protocol, requestFixture(tc.protocol, "m"))
			requireStatus(t, w, 200)
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("原生响应未保真: %s", w.Body.String())
			}
		})
	}
}
