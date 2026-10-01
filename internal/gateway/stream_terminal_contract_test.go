package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagesStopReasonContract(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		wantLength   bool
		wantErr      bool
	}{
		{"context window", "model_context_window_exceeded", true, false},
		{"pause turn", "pause_turn", false, true},
		{"refusal", "refusal", false, true},
		{"unknown", "future_stop", false, true},
		{"stop sequence", "stop_sequence", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := decodeCompletion(Object{
				"type": "message", "stop_reason": tc.reason,
				"content": []any{Object{"type": "text", "text": "ok"}},
			}, "messages")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && (c.Stop == "length") != tc.wantLength {
				t.Fatalf("stop=%q wantLength=%v", c.Stop, tc.wantLength)
			}
		})
	}

	t.Run("stream context window is incomplete", func(t *testing.T) {
		source := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"partial"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"model_context_window_exceeded"}}

event: message_stop
data: {"type":"message_stop"}

`
		w := httptest.NewRecorder()
		if err := convertedStream(w, strings.NewReader(source), "messages", "responses", "m", "r", &Usage{}, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(w.Body.String(), `"status":"incomplete"`) {
			t.Fatalf("上下文窗口停止未标记 incomplete: %s", w.Body.String())
		}
	})

	t.Run("stream unknown stop is rejected", func(t *testing.T) {
		source := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"partial"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"}}

event: message_stop
data: {"type":"message_stop"}

`
		if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "messages", "chat", "m", "r", &Usage{}, false); err == nil {
			t.Fatal("未知停止原因被跨协议转换")
		}
	})
}

func TestChatFinishReasonContract(t *testing.T) {
	for _, reason := range []string{"content_filter", "future_reason", "function_call"} {
		t.Run(reason+" json", func(t *testing.T) {
			_, err := decodeCompletion(Object{"choices": []any{Object{
				"finish_reason": reason, "message": Object{"role": "assistant", "content": "partial"},
			}}}, "chat")
			if err == nil {
				t.Fatal("不可映射的 Chat 结束原因被接受")
			}
		})
		t.Run(reason+" stream", func(t *testing.T) {
			source := "data: " + raw(Object{"choices": []any{Object{
				"index": 0, "delta": Object{"content": "partial"}, "finish_reason": reason,
			}}}) + "\n\ndata: [DONE]\n\n"
			if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "chat", "messages", "m", "r", &Usage{}, false); err == nil {
				t.Fatal("流式 Chat 结束原因被静默改写")
			}
		})
	}
}

func TestMessagesServerToolCallerRejected(t *testing.T) {
	tool := Object{"type": "tool_use", "id": "call_1", "name": "search", "input": Object{}, "caller": Object{"type": "code_execution_20250825"}}
	if _, err := decodeCompletion(Object{"stop_reason": "tool_use", "content": []any{tool}}, "messages"); err == nil {
		t.Fatal("服务端工具调用被转换为客户端工具")
	}
	source := "event: content_block_start\ndata: " + raw(Object{
		"type": "content_block_start", "index": 0, "content_block": tool,
	}) + "\n\n"
	if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "messages", "chat", "m", "r", &Usage{}, false); err == nil {
		t.Fatal("流式服务端工具调用被转换为客户端工具")
	}
}

func TestResponsesNonTerminalStatusRejected(t *testing.T) {
	for _, status := range []string{"failed", "cancelled", "in_progress", "queued"} {
		t.Run(status, func(t *testing.T) {
			_, err := decodeCompletion(Object{
				"object": "response", "status": status,
				"output": []any{Object{"type": "message", "content": []any{Object{"type": "output_text", "text": "partial"}}}},
			}, "responses")
			if err == nil {
				t.Fatal("非终态 Responses 被编造成完成响应")
			}
		})
	}

	for _, reason := range []string{"", "content_filter", "future_reason"} {
		t.Run("incomplete "+reason, func(t *testing.T) {
			_, err := decodeCompletion(Object{
				"object": "response", "status": "incomplete",
				"incomplete_details": Object{"reason": reason},
				"output":             []any{Object{"type": "message", "content": []any{Object{"type": "output_text", "text": "partial"}}}},
			}, "responses")
			if err == nil {
				t.Fatal("非令牌上限的 incomplete 被映射为 length")
			}
		})
	}
}

func TestConvertedStreamRequiresExplicitBlockCompletion(t *testing.T) {
	t.Run("messages missing content block stop", func(t *testing.T) {
		source := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"read","input":{}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

event: message_stop
data: {"type":"message_stop"}

`
		w := httptest.NewRecorder()
		err := convertedStream(w, strings.NewReader(source), "messages", "chat", "m", "r", &Usage{}, false)
		if err == nil || strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatalf("未完成工具块被成功终止: err=%v body=%s", err, w.Body.String())
		}
	})

	t.Run("responses missing item done", func(t *testing.T) {
		source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}

event: response.completed
data: {"type":"response.completed","response":{}}

`
		w := httptest.NewRecorder()
		err := convertedStream(w, strings.NewReader(source), "responses", "messages", "m", "r", &Usage{}, false)
		if err == nil || strings.Contains(w.Body.String(), "message_stop") {
			t.Fatalf("未完成 Responses 工具项被成功终止: err=%v body=%s", err, w.Body.String())
		}
	})

	t.Run("later tool block is still required", func(t *testing.T) {
		source := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"read","input":{}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

event: message_stop
data: {"type":"message_stop"}

`
		w := httptest.NewRecorder()
		err := convertedStream(w, strings.NewReader(source), "messages", "chat", "m", "r", &Usage{}, false)
		if err == nil || strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatalf("后续未完成工具块被遗漏: err=%v body=%s", err, w.Body.String())
		}
	})

	t.Run("complete tool json followed by truncation", func(t *testing.T) {
		source := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"read","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}

event: message_stop
data: {"type":"message_stop"}

`
		w := httptest.NewRecorder()
		err := convertedStream(w, strings.NewReader(source), "messages", "messages", "m", "r", &Usage{}, false)
		if err == nil || strings.Contains(w.Body.String(), "content_block_stop") || strings.Contains(w.Body.String(), "message_stop") {
			t.Fatalf("截断工具被发成可执行完成项: err=%v body=%s", err, w.Body.String())
		}
	})

	t.Run("responses done item cannot be incomplete", func(t *testing.T) {
		source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","status":"incomplete","call_id":"call_1","name":"read","arguments":"{}"}}

event: response.completed
data: {"type":"response.completed","response":{}}

`
		w := httptest.NewRecorder()
		if err := convertedStream(w, strings.NewReader(source), "responses", "messages", "m", "r", &Usage{}, false); err == nil || strings.Contains(w.Body.String(), "content_block_stop") {
			t.Fatalf("incomplete item 被发成完成项: err=%v body=%s", err, w.Body.String())
		}
	})
}

func TestResponsesTerminalCompletionEvidence(t *testing.T) {
	t.Run("completed response item closes matching tool", func(t *testing.T) {
		source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}

event: response.completed
data: {"type":"response.completed","response":{"output":[{"type":"function_call","status":"completed","call_id":"call_1","name":"read","arguments":"{}"}]}}

`
		w := httptest.NewRecorder()
		if err := convertedStream(w, strings.NewReader(source), "responses", "messages", "m", "r", &Usage{}, false); err != nil {
			t.Fatalf("完整终止项被拒绝: %v\n%s", err, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "message_stop") {
			t.Fatalf("未输出成功终止: %s", w.Body.String())
		}
	})

	t.Run("completed response item must match deltas", func(t *testing.T) {
		source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}

event: response.completed
data: {"type":"response.completed","response":{"output":[{"type":"function_call","status":"completed","call_id":"call_1","name":"read","arguments":"{\"x\":1}"}]}}

`
		if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "responses", "messages", "m", "r", &Usage{}, false); err == nil {
			t.Fatal("终止项与增量不一致仍被接受")
		}
	})

	t.Run("text done without delta", func(t *testing.T) {
		source := `event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ok"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{}}

`
		w := httptest.NewRecorder()
		if err := convertedStream(w, strings.NewReader(source), "responses", "chat", "m", "r", &Usage{}, false); err != nil {
			t.Fatalf("仅 done 文本事件被拒绝: %v\n%s", err, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"content":"ok"`) || !strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatalf("done 文本未转换: %s", w.Body.String())
		}
	})

	t.Run("terminal reasoning requires explicit drop", func(t *testing.T) {
		source := `event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ok"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"ok","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"ok","annotations":[]}]},{"type":"reasoning","summary":[]}]}}

`
		if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "responses", "chat", "m", "r", &Usage{}, false); err == nil {
			t.Fatal("终态 reasoning 未经允许被丢弃")
		}
		w := httptest.NewRecorder()
		if err := convertedStream(w, strings.NewReader(source), "responses", "chat", "m", "r", &Usage{}, true); err != nil {
			t.Fatalf("显式丢弃 reasoning 失败: %v\n%s", err, w.Body.String())
		}
	})

	t.Run("stream content filter incomplete is rejected", func(t *testing.T) {
		source := `event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"partial"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"partial","annotations":[]}]}}

event: response.incomplete
data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}

`
		if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "responses", "chat", "m", "r", &Usage{}, false); err == nil {
			t.Fatal("内容过滤被映射为 length")
		}
	})
}
