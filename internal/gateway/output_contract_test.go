package gateway

import "testing"

func TestHasReasoningFieldOnlyReportsContent(t *testing.T) {
	empty := []Object{
		{},
		{"reasoning": nil},
		{"reasoning": ""},
		{"reasoning_content": "  "},
		{"reasoning_details": []any{}},
		{"reasoning_details": Object{}},
	}
	for _, message := range empty {
		if hasReasoningField(message) {
			t.Fatalf("空推理字段被误判为内容: %s", raw(message))
		}
	}

	nonEmpty := []Object{
		{"reasoning": "thought"},
		{"reasoning_details": []any{Object{"type": "reasoning.encrypted", "data": "sig"}}},
		{"reasoning_details": false},
	}
	for _, message := range nonEmpty {
		if !hasReasoningField(message) {
			t.Fatalf("非空或无效推理字段未被识别: %s", raw(message))
		}
	}
}

func TestEmptyUntypedOutputIsNotVisible(t *testing.T) {
	tests := []struct {
		protocol string
		body     Object
	}{
		{"chat", Object{"choices": []any{Object{"message": Object{"content": []any{Object{}}}}}}},
		{"messages", Object{"content": []any{Object{}}}},
		{"responses", Object{"output": []any{Object{}}}},
	}
	for _, tc := range tests {
		if hasVisibleOutput(tc.body, tc.protocol) {
			t.Fatalf("%s 的空对象被误判为有效产出", tc.protocol)
		}
	}
	if !hasVisibleOutput(Object{"output": []any{Object{"type": "future_tool_call"}}}, "responses") {
		t.Fatal("原生协议未知但具名的输出类型应保真")
	}
}

func TestDecodeCompletionRejectsUnmappedOutputAndInvalidTools(t *testing.T) {
	chat := func(message Object) Object {
		return Object{"choices": []any{Object{"message": message}}}
	}
	tests := []struct {
		name     string
		protocol string
		body     Object
	}{
		{name: "chat-audio", protocol: "chat", body: chat(Object{"content": "text", "audio": Object{"id": "a"}})},
		{name: "chat-images", protocol: "chat", body: chat(Object{"content": "text", "images": []any{Object{"url": "x"}}})},
		{name: "chat-annotations", protocol: "chat", body: chat(Object{"content": "text", "annotations": []any{Object{"type": "url_citation"}}})},
		{name: "chat-content-array", protocol: "chat", body: chat(Object{"content": []any{}})},
		{name: "chat-custom-tool", protocol: "chat", body: chat(Object{"tool_calls": []any{
			Object{"id": "c", "type": "custom", "function": Object{"name": "x", "arguments": "{}"}},
		}})},
		{name: "chat-array-arguments", protocol: "chat", body: chat(Object{"tool_calls": []any{
			Object{"id": "c", "type": "function", "function": Object{"name": "x", "arguments": "[]"}},
		}})},
		{name: "messages-citations", protocol: "messages", body: Object{"content": []any{
			Object{"type": "text", "text": "answer", "citations": []any{Object{"type": "page_location"}}},
		}}},
		{name: "messages-null-input", protocol: "messages", body: Object{"content": []any{
			Object{"type": "tool_use", "id": "c", "name": "x", "input": nil},
		}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeCompletion(tc.body, tc.protocol); err == nil {
				t.Fatalf("%s 未拒绝不能无损转换的输出", tc.name)
			}
		})
	}

	for _, protocol := range []string{"chat", "messages", "responses"} {
		var body Object
		switch protocol {
		case "chat":
			body = Object{"choices": []any{Object{"message": Object{"tool_calls": []any{Object{"id": "c", "type": "function", "function": Object{"name": "x", "arguments": "{}"}}}}}}}
		case "messages":
			body = Object{"content": []any{Object{"type": "tool_use", "id": "c", "name": "x", "input": Object{}}}}
		case "responses":
			body = Object{"output": []any{Object{"type": "function_call", "status": "completed", "call_id": "c", "name": "x", "arguments": "{}"}}}
		}
		if _, err := decodeCompletion(body, protocol); err != nil {
			t.Fatalf("%s 不应拒绝空对象工具参数: %v", protocol, err)
		}
	}
}
