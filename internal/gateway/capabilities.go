package gateway

import (
	"errors"
	"net/http"
	"slices"
)

func validProtocol(p string) bool {
	return p == "chat" || p == "messages" || p == "responses" || p == "systemone"
}

func (m Model) supportsProtocol(p string) bool {
	if m.Protocols == nil {
		return m.Protocol == p
	}
	return slices.Contains(m.Protocols, p)
}

func validateProtocols(m Model) error {
	seen := map[string]bool{}
	for _, p := range m.Protocols {
		if !validProtocol(p) || seen[p] || (p == "systemone") != (m.Protocol == "systemone") {
			return errors.New("supported_protocols 包含未知、重复或不兼容的协议")
		}
		seen[p] = true
	}
	if len(m.Protocols) > 0 && !seen[m.Protocol] {
		return errors.New("supported_protocols 必须包含默认 protocol")
	}
	return nil
}

// 只检查协议内容位置；工具参数、JSON Schema 和用户元数据均为不透明数据。
func requestHasImage(o Object, protocol string) bool {
	items := arr(o["messages"])
	if protocol == "responses" {
		items = arr(o["input"])
	}
	for _, v := range items {
		m := obj(v)
		if contentHasImage(m["content"]) {
			return true
		}
		if protocol == "responses" && (str(m, "type") == "function_call_output" || str(m, "type") == "custom_tool_call_output") && contentHasImage(m["output"]) {
			return true
		}
	}
	return false
}

func contentHasImage(content any) bool {
	for _, v := range arr(content) {
		b := obj(v)
		switch str(b, "type") {
		case "image", "input_image", "image_url":
			return true
		case "tool_result":
			if contentHasImage(b["content"]) {
				return true
			}
		}
	}
	return false
}

// ZDR 是请求约束，必须在准入和任何上游请求之前筛选供应商。
func constrainSelections(in []selection, h http.Header) ([]selection, error) {
	values := h.Values("x-cmd-zdr")
	if len(values) == 0 {
		return in, nil
	}
	if len(values) != 1 || (values[0] != "0" && values[0] != "1") {
		return nil, fail("INVALID_ZDR", "x-cmd-zdr 必须是 0 或 1", 400)
	}
	if values[0] == "0" {
		return in, nil
	}
	out := make([]selection, 0, len(in))
	for _, s := range in {
		if s.Provider.Kind == "commandcode" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fail("ZDR_UNSUPPORTED", "没有可执行 x-cmd-zdr 零数据保留约束的候选供应商", 400)
	}
	for i := range out {
		if !slices.ContainsFunc(out, func(s selection) bool { return s.Model.ID == out[i].Pinned }) {
			out[i].Pinned = ""
		}
	}
	return out, nil
}

// 管理员显式修改能力值时清除未知标记，避免覆盖人工禁用决定。
func confirmCapabilities(m *Model, patch Object) {
	if _, set := patch["tools"]; set && patch["tools_unknown"] == nil {
		m.ToolsUnknown = false
	}
	if _, set := patch["vision"]; set && patch["vision_unknown"] == nil {
		m.VisionUnknown = false
	}
}

// 输出丢弃开关不授权改变禁用思考或其他硬性控制。
func canDropThinking(value any) bool {
	o := obj(value)
	if str(o, "type") != "enabled" && str(o, "type") != "adaptive" {
		return false
	}
	for key := range o {
		if key != "type" && key != "budget_tokens" {
			return false
		}
	}
	return true
}

func normalizeProviderContext(p Provider, protocol string, body Object) []string {
	if p.Kind != "deepseek" || protocol != "messages" || body["context_management"] == nil {
		return nil
	}
	ignored := []string{}
	if readContextManagement(body["context_management"], &ignored) != nil {
		return nil
	}
	if len(arr(obj(body["context_management"])["edits"])) > 0 {
		for _, value := range arr(body["messages"]) {
			m := obj(value)
			if hasReasoningField(m) {
				return nil
			}
			for _, value := range arr(m["content"]) {
				switch str(obj(value), "type") {
				case "thinking", "redacted_thinking", "reasoning":
					return nil
				}
			}
		}
	}
	// 已确认不会改变历史的空操作无需交给不支持它的上游执行。
	delete(body, "context_management")
	return ignored
}

// DeepSeek 对部分参数会直接忽略；硬性输出、状态和执行约束不能因此伪装成功。
func providerRequestError(p Provider, protocol string, o Object) string {
	if p.Kind != "deepseek" {
		return ""
	}
	if protocol == "messages" {
		if obj(o["output_config"])["format"] != nil {
			return "DeepSeek Messages 不支持 output_config.format；请使用 Responses 或其他供应商"
		}
		if o["context_management"] != nil {
			return "DeepSeek 不支持服务端 context_management；请使用支持该功能的供应商"
		}
	}
	if protocol == "responses" {
		if o["previous_response_id"] != nil || o["conversation"] != nil || boolean(o, "store") || boolean(o, "background") {
			return "DeepSeek Responses 不保存服务端状态，请发送完整对话历史"
		}
		if parallel, ok := o["parallel_tool_calls"].(bool); ok && !parallel {
			return "DeepSeek Responses 不能关闭并行工具调用"
		}
		for _, v := range arr(o["tools"]) {
			t := obj(v)
			if str(t, "type") != "function" && !(str(t, "type") == "custom" && str(t, "name") == "apply_patch") {
				return "DeepSeek Responses 不支持此工具类型；请使用支持它的供应商"
			}
		}
	}
	return ""
}
