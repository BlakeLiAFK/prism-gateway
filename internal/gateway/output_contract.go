package gateway

import (
	"encoding/json"
	"errors"
	"strings"
)

func hasReasoningField(message Object) bool {
	for _, key := range []string{"reasoning", "reasoning_content", "reasoning_details"} {
		if hasReasoningValue(message[key]) {
			return true
		}
	}
	return false
}

func hasReasoningValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		// 非空但类型无效也不能按“没有推理内容”放行。
		return true
	}
}

func hasNonEmptyValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

func validateToolCall(id, name, arguments string) error {
	if id == "" || name == "" {
		return errors.New("upstream returned tool call without ID or name")
	}
	var input Object
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || input == nil {
		return errors.New("upstream returned invalid tool argument object")
	}
	return nil
}

// stripReasoning 从上游响应里移除推理内容，供显式开启 drop_reasoning 的模型使用。
func stripReasoning(o Object, protocol string) bool {
	dropped := false
	switch protocol {
	case "chat":
		for _, v := range arr(o["choices"]) {
			m := obj(obj(v)["message"])
			for _, key := range []string{"reasoning", "reasoning_content", "reasoning_details"} {
				if _, ok := m[key]; ok {
					delete(m, key)
					dropped = true
				}
			}
		}
	case "messages":
		kept := []any{}
		for _, v := range arr(o["content"]) {
			switch str(obj(v), "type") {
			case "thinking", "redacted_thinking":
				dropped = true
			default:
				kept = append(kept, v)
			}
		}
		if dropped {
			o["content"] = kept
		}
	case "responses":
		kept := []any{}
		for _, v := range arr(o["output"]) {
			if str(obj(v), "type") == "reasoning" {
				dropped = true
				continue
			}
			kept = append(kept, v)
		}
		if dropped {
			o["output"] = kept
		}
	}
	return dropped
}

// hasVisibleOutput 接受原生协议的文本、推理、媒体和服务端工具产出。
// 跨协议能否表示由 decodeCompletion 继续严格验证。
func hasVisibleOutput(o Object, protocol string) bool {
	switch protocol {
	case "chat":
		for _, v := range arr(o["choices"]) {
			m := obj(obj(v)["message"])
			if strings.TrimSpace(str(m, "content")) != "" || hasTypedContent(m["content"]) ||
				len(arr(m["tool_calls"])) > 0 || m["audio"] != nil || len(arr(m["images"])) > 0 ||
				strings.TrimSpace(str(m, "refusal")) != "" || hasReasoningField(m) && !truncatedStop(o, protocol) {
				return true
			}
		}
	case "messages":
		for _, v := range arr(o["content"]) {
			b := obj(v)
			typ := str(b, "type")
			if typ == "thinking" || typ == "redacted_thinking" {
				if !truncatedStop(o, protocol) {
					return true
				}
				continue
			}
			if typ != "" && (typ != "text" || strings.TrimSpace(str(b, "text")) != "") {
				return true
			}
		}
	case "responses":
		for _, v := range arr(o["output"]) {
			b := obj(v)
			typ := str(b, "type")
			if typ == "reasoning" {
				if !truncatedStop(o, protocol) {
					return true
				}
				continue
			}
			if typ == "" {
				continue
			}
			if typ != "message" {
				return true
			}
			for _, x := range arr(b["content"]) {
				part := obj(x)
				if str(part, "type") != "output_text" || strings.TrimSpace(str(part, "text")) != "" {
					return true
				}
			}
		}
	case "systemone":
		return len(obj(o["answers"])) > 0
	}
	return false
}

func hasTypedContent(value any) bool {
	for _, part := range arr(value) {
		if typ := str(obj(part), "type"); typ != "" {
			return true
		}
	}
	return false
}

func truncatedStop(o Object, protocol string) bool {
	switch protocol {
	case "chat":
		for _, v := range arr(o["choices"]) {
			if str(obj(v), "finish_reason") == "length" {
				return true
			}
		}
	case "messages":
		return str(o, "stop_reason") == "max_tokens"
	case "responses":
		return str(o, "status") == "incomplete"
	}
	return false
}
