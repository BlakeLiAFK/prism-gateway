package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMessagesClientDefaultsCrossProtocol(t *testing.T) {
	h := newHarness(t)
	received := make(chan Object, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Object
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		received <- got
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("m", "chat"))

	body := Object{
		"model": "m", "max_tokens": 64,
		"tools": []any{Object{
			"type": "custom", "name": "read_file", "description": "Read a file",
			"input_schema": Object{"type": "object"}, "defer_loading": false,
			"allowed_callers": []any{"direct"},
		}},
		"messages": []any{
			Object{"role": "user", "content": []any{Object{"type": "text", "text": "read it", "citations": []any{}}}},
			Object{"role": "assistant", "content": []any{Object{"type": "tool_use", "id": "tool_1", "name": "read_file", "input": Object{"path": "main.go"}, "caller": Object{"type": "direct"}}}},
			Object{"role": "user", "content": []any{Object{"type": "tool_result", "tool_use_id": "tool_1", "content": "package main"}}},
			Object{"role": "assistant", "content": []any{Object{"type": "text", "text": "checked", "citations": nil}}},
			Object{"role": "user", "content": "continue"},
		},
	}
	w := h.generate(t, "messages", body)
	requireStatus(t, w, http.StatusOK)
	got := <-received
	if len(arr(got["tools"])) != 1 || len(arr(got["messages"])) != 5 {
		t.Fatalf("工具或多轮历史丢失: %s", raw(got))
	}
	toolCall := obj(arr(obj(arr(got["messages"])[1])["tool_calls"])[0])
	if str(toolCall, "id") != "tool_1" || str(obj(toolCall["function"]), "name") != "read_file" {
		t.Fatalf("工具调用关联丢失: %s", raw(toolCall))
	}
}

func TestMessagesClientDefaultsRejectNonDefaultSemantics(t *testing.T) {
	tests := map[string]string{
		"deferred tool":             `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"x","input_schema":{},"defer_loading":true}]}`,
		"server caller declaration": `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"x","input_schema":{},"allowed_callers":["direct","code_execution_20250825"]}]}`,
		"server tool caller":        `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{},"caller":{"type":"code_execution_20250825"}}]}]}`,
		"nonempty citations":        `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"text","text":"x","citations":[{"type":"char_location"}]}]}]}`,
		"nonempty annotations":      `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"url_citation"}]}]}]}`,
		"nonempty logprobs":         `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"text","text":"x","logprobs":[{"token":"x"}]}]}]}`,
		"unknown tool field":        `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"x","input_schema":{},"future_mode":false}]}`,
		"empty tool type":           `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"","name":"x","input_schema":{}}]}`,
		"non-string tool type":      `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":17,"name":"x","input_schema":{}}]}`,
		"caller with extra field":   `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{},"caller":{"type":"direct","metadata":null}}]}]}`,
		"annotations object":        `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"text","text":"x","annotations":{}}]}]}`,
		"citations empty string":    `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"text","text":"x","citations":""}]}]}`,
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			var body Object
			if err := json.Unmarshal([]byte(fixture), &body); err != nil {
				t.Fatal(err)
			}
			if _, err := decodeCanonical(body, "messages"); err == nil {
				t.Fatal("非默认语义不能被静默丢弃")
			}
		})
	}
}

func TestResponsesEmptyOutputMetadataCrossProtocol(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Object
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if len(arr(got["messages"])) != 2 {
			t.Errorf("Responses 历史丢失: %s", raw(got))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("m", "chat"))
	body := Object{
		"model": "m",
		"input": []any{
			Object{"type": "message", "role": "assistant", "content": []any{Object{"type": "output_text", "text": "prior", "annotations": []any{}, "logprobs": nil}}},
			Object{"type": "message", "role": "user", "content": []any{Object{"type": "input_text", "text": "continue"}}},
		},
	}
	requireStatus(t, h.generate(t, "responses", body), http.StatusOK)
}

func TestChatMessageAnnotationsRequireEmptyDefault(t *testing.T) {
	body := Object{
		"model": "m",
		"messages": []any{
			Object{"role": "assistant", "content": "first", "annotations": nil},
			Object{"role": "assistant", "content": "second", "annotations": []any{}},
		},
	}
	if _, err := decodeCanonical(body, "chat"); err != nil {
		t.Fatalf("空 annotations 应等同默认值: %v", err)
	}
	arr(body["messages"])[1].(Object)["annotations"] = []any{Object{"type": "url_citation"}}
	if _, err := decodeCanonical(body, "chat"); err == nil {
		t.Fatal("非空 annotations 不能被静默丢弃")
	}
}
