// 规范结构到各协议请求体的编码，以及上游响应的解析与回写。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func encodeCanonical(c Canonical, protocol, model string) (Object, error) {
	o := Object{"model": model, "stream": c.Stream}
	if c.Temperature != nil {
		o["temperature"] = c.Temperature
	}
	if c.TopP != nil {
		o["top_p"] = c.TopP
	}
	messages := []any{}
	systems := []string{}
	for _, m := range c.Messages {
		if protocol != "chat" && m.Role == "system" {
			for _, b := range m.Blocks {
				if b.Kind != "text" {
					return nil, unsupported("system 仅支持文本")
				}
				systems = append(systems, b.Text)
			}
			continue
		}
		bs := []any{}
		calls := []any{}
		flush := func() {
			if len(bs) > 0 || len(calls) > 0 {
				msg := Object{"role": m.Role, "content": bs}
				if protocol == "responses" {
					msg["type"] = "message"
				}
				if len(calls) > 0 {
					msg["tool_calls"] = calls
				}
				messages = append(messages, msg)
				bs = []any{}
				calls = []any{}
			}
		}
		for _, b := range m.Blocks {
			switch b.Kind {
			case "text":
				typ := "text"
				if protocol == "responses" {
					typ = "input_text"
					if m.Role == "assistant" {
						typ = "output_text"
					}
				}
				bs = append(bs, Object{"type": typ, "text": b.Text})
			case "image":
				switch protocol {
				case "chat":
					bs = append(bs, Object{"type": "image_url", "image_url": Object{"url": b.URL}})
				case "responses":
					bs = append(bs, Object{"type": "input_image", "image_url": b.URL})
				case "messages":
					src, e := imageSource(b.URL)
					if e != nil {
						return nil, e
					}
					bs = append(bs, Object{"type": "image", "source": src})
				}
			case "tool":
				if b.ID == "" || b.Name == "" || !json.Valid([]byte(b.Arguments)) {
					return nil, unsupported("工具 ID / name / arguments 不合法")
				}
				switch protocol {
				case "chat":
					calls = append(calls, Object{"id": b.ID, "type": "function", "function": Object{"name": b.Name, "arguments": b.Arguments}})
				case "messages":
					var v any
					json.Unmarshal([]byte(b.Arguments), &v)
					bs = append(bs, Object{"type": "tool_use", "id": b.ID, "name": b.Name, "input": v})
				case "responses":
					flush()
					messages = append(messages, Object{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": b.Arguments})
				}
			case "result":
				if b.ID == "" {
					return nil, unsupported("工具结果缺少 call ID")
				}
				switch protocol {
				case "chat":
					flush()
					messages = append(messages, Object{"role": "tool", "tool_call_id": b.ID, "content": b.Text})
				case "responses":
					flush()
					messages = append(messages, Object{"type": "function_call_output", "call_id": b.ID, "output": b.Text})
				case "messages":
					bs = append(bs, Object{"type": "tool_result", "tool_use_id": b.ID, "content": b.Text, "is_error": b.IsError})
				}
			}
		}
		flush()
	}
	// 客户端没写输出上限时不带这个字段，由 selections 按上游协议决定是否补
	if f := map[string]string{"chat": "max_tokens", "messages": "max_tokens", "responses": "max_output_tokens"}[protocol]; f != "" && c.MaxOutput > 0 {
		o[f] = c.MaxOutput
	}
	switch protocol {
	case "chat":
		o["messages"] = messages
		if c.Stream {
			o["stream_options"] = Object{"include_usage": true}
		}
		if c.Stop != nil {
			o["stop"] = c.Stop
		}
	case "messages":
		o["messages"] = messages
		if len(systems) > 0 {
			o["system"] = strings.Join(systems, "\n\n")
		}
		if c.Stop != nil {
			if s, ok := c.Stop.(string); ok {
				o["stop_sequences"] = []string{s}
			} else {
				o["stop_sequences"] = c.Stop
			}
		}
	case "responses":
		o["input"] = messages
		o["store"] = false
		if len(systems) > 0 {
			o["instructions"] = strings.Join(systems, "\n\n")
		}
		if c.Stop != nil {
			return nil, unsupported("Responses 跨协议目标无法等价映射 stop sequences")
		}
	}
	if len(c.Tools) > 0 {
		tools := []any{}
		for _, t := range c.Tools {
			f := Object{"name": t.Name, "description": t.Description}
			if protocol == "messages" {
				f["input_schema"] = t.Schema
				tools = append(tools, f)
			} else {
				f["parameters"] = t.Schema
				if protocol == "chat" {
					tools = append(tools, Object{"type": "function", "function": f})
				} else {
					f["type"] = "function"
					tools = append(tools, f)
				}
			}
		}
		o["tools"] = tools
		if protocol == "messages" {
			typ := c.Choice
			if typ == "required" {
				typ = "any"
			}
			if typ == "function" {
				typ = "tool"
			}
			tc := Object{"type": typ}
			if c.ToolName != "" {
				tc["name"] = c.ToolName
			}
			if c.Parallel != nil {
				tc["disable_parallel_tool_use"] = !*c.Parallel
			}
			o["tool_choice"] = tc
		} else {
			if c.Choice == "function" {
				if protocol == "chat" {
					o["tool_choice"] = Object{"type": "function", "function": Object{"name": c.ToolName}}
				} else {
					o["tool_choice"] = Object{"type": "function", "name": c.ToolName}
				}
			} else {
				o["tool_choice"] = c.Choice
			}
			if c.Parallel != nil {
				o["parallel_tool_calls"] = *c.Parallel
			}
		}
	}
	return o, nil
}

func extractUsage(o Object, protocol string, u *Usage) {
	if o == nil {
		return
	}
	switch protocol {
	case "chat":
		if _, ok := o["prompt_tokens"]; ok {
			u.Input = int64(num(o, "prompt_tokens"))
			u.Output = int64(num(o, "completion_tokens"))
			u.Cache = int64(num(obj(o["prompt_tokens_details"]), "cached_tokens"))
			u.Known = true
		}
	case "responses", "systemone":
		if _, ok := o["input_tokens"]; ok {
			u.Input = int64(num(o, "input_tokens"))
			u.Output = int64(num(o, "output_tokens"))
			u.Cache = int64(num(obj(o["input_tokens_details"]), "cached_tokens"))
			u.Known = true
		}
	case "messages":
		if _, ok := o["input_tokens"]; ok {
			u.Cache = int64(num(o, "cache_read_input_tokens"))
			u.Write = int64(num(o, "cache_creation_input_tokens"))
			u.Input = int64(num(o, "input_tokens")) + u.Cache + u.Write
			u.Known = true
		}
		if _, ok := o["output_tokens"]; ok {
			u.Output = int64(num(o, "output_tokens"))
		}
	}
	if u.Cache > u.Input {
		u.Cache = u.Input
	}
}

func decodeCompletion(o Object, protocol string) (Completion, error) {
	c := Completion{Stop: "stop"}
	extractUsage(obj(o["usage"]), protocol, &c.Usage)
	switch protocol {
	case "chat":
		choices := arr(o["choices"])
		if len(choices) != 1 {
			return c, errors.New("expected one chat choice")
		}
		v := obj(choices[0])
		c.Stop = str(v, "finish_reason")
		m := obj(v["message"])
		if str(m, "refusal") != "" {
			return c, unsupported("无法等价转换 refusal 内容")
		}
		if str(m, "reasoning_content") != "" || str(m, "reasoning") != "" {
			return c, unsupported("无法跨协议无损转换 reasoning 内容")
		}
		if s := str(m, "content"); s != "" {
			c.Blocks = append(c.Blocks, Block{Kind: "text", Text: s})
		}
		for _, v := range arr(m["tool_calls"]) {
			t := obj(v)
			f := obj(t["function"])
			args := str(f, "arguments")
			if !json.Valid([]byte(args)) {
				return c, errors.New("upstream returned invalid tool JSON")
			}
			c.Blocks = append(c.Blocks, Block{Kind: "tool", ID: str(t, "id"), Name: str(f, "name"), Arguments: args})
		}
	case "messages":
		c.Stop = str(o, "stop_reason")
		for _, v := range arr(o["content"]) {
			b := obj(v)
			switch str(b, "type") {
			case "text":
				c.Blocks = append(c.Blocks, Block{Kind: "text", Text: str(b, "text")})
			case "tool_use":
				c.Blocks = append(c.Blocks, Block{Kind: "tool", ID: str(b, "id"), Name: str(b, "name"), Arguments: raw(b["input"])})
			default:
				return c, unsupported("上游返回不能无损转换的 " + str(b, "type"))
			}
		}
	case "responses":
		if str(o, "status") == "incomplete" {
			c.Stop = "length"
		}
		for _, v := range arr(o["output"]) {
			b := obj(v)
			switch str(b, "type") {
			case "message":
				for _, x := range arr(b["content"]) {
					x := obj(x)
					if str(x, "type") != "output_text" {
						return c, unsupported("Responses output 内容不受支持")
					}
					if len(arr(x["annotations"])) > 0 {
						return c, unsupported("无法无损转换带 annotations 的输出")
					}
					c.Blocks = append(c.Blocks, Block{Kind: "text", Text: str(x, "text")})
				}
			case "function_call":
				c.Blocks = append(c.Blocks, Block{Kind: "tool", ID: str(b, "call_id"), Name: str(b, "name"), Arguments: str(b, "arguments")})
			default:
				return c, unsupported("无法无损转换 Responses item " + str(b, "type"))
			}
		}
	}
	return c, nil
}

func stopFor(c Completion, p string) string {
	hasTool := false
	for _, b := range c.Blocks {
		if b.Kind == "tool" {
			hasTool = true
		}
	}
	length := c.Stop == "length" || c.Stop == "max_tokens"
	if p == "messages" {
		if length {
			return "max_tokens"
		}
		if hasTool {
			return "tool_use"
		}
		return "end_turn"
	}
	if length {
		return "length"
	}
	if hasTool {
		return "tool_calls"
	}
	return "stop"
}

func usageObject(u Usage, p string) Object {
	switch p {
	case "chat":
		return Object{"prompt_tokens": u.Input, "completion_tokens": u.Output, "total_tokens": u.Input + u.Output, "prompt_tokens_details": Object{"cached_tokens": u.Cache}}
	case "messages":
		return Object{"input_tokens": max(int64(0), u.Input-u.Cache-u.Write), "output_tokens": u.Output, "cache_read_input_tokens": u.Cache, "cache_creation_input_tokens": u.Write}
	default:
		return Object{"input_tokens": u.Input, "output_tokens": u.Output, "total_tokens": u.Input + u.Output, "input_tokens_details": Object{"cached_tokens": u.Cache}, "output_tokens_details": Object{"reasoning_tokens": 0}}
	}
}

func completionObject(c Completion, p, model, id string) Object {
	switch p {
	case "chat":
		text := ""
		tools := []any{}
		for _, b := range c.Blocks {
			if b.Kind == "text" {
				text += b.Text
			} else if b.Kind == "tool" {
				tools = append(tools, Object{"id": b.ID, "type": "function", "function": Object{"name": b.Name, "arguments": b.Arguments}})
			}
		}
		m := Object{"role": "assistant", "content": text}
		if len(tools) > 0 {
			m["tool_calls"] = tools
		}
		return Object{"id": "chatcmpl_" + id, "object": "chat.completion", "created": now() / 1000, "model": model, "choices": []any{Object{"index": 0, "message": m, "finish_reason": stopFor(c, p)}}, "usage": usageObject(c.Usage, p)}
	case "messages":
		bs := []any{}
		for _, b := range c.Blocks {
			if b.Kind == "text" {
				bs = append(bs, Object{"type": "text", "text": b.Text})
			} else if b.Kind == "tool" {
				var v any
				json.Unmarshal([]byte(b.Arguments), &v)
				bs = append(bs, Object{"type": "tool_use", "id": b.ID, "name": b.Name, "input": v})
			}
		}
		return Object{"id": "msg_" + id, "type": "message", "role": "assistant", "model": model, "content": bs, "stop_reason": stopFor(c, p), "stop_sequence": nil, "usage": usageObject(c.Usage, p)}
	default:
		items := []any{}
		for i, b := range c.Blocks {
			bid := fmt.Sprintf("item_%s_%d", id, i)
			if b.Kind == "text" {
				items = append(items, Object{"id": bid, "type": "message", "role": "assistant", "status": "completed", "content": []any{Object{"type": "output_text", "text": b.Text, "annotations": []any{}, "logprobs": []any{}}}})
			} else if b.Kind == "tool" {
				items = append(items, Object{"id": bid, "type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": b.Arguments, "status": "completed"})
			}
		}
		status := "completed"
		var details any
		if stopFor(c, "chat") == "length" {
			status = "incomplete"
			details = Object{"reason": "max_output_tokens"}
		}
		return Object{"id": "resp_" + id, "object": "response", "created_at": now() / 1000, "status": status, "error": nil, "incomplete_details": details, "model": model, "output": items, "usage": usageObject(c.Usage, p), "parallel_tool_calls": true, "store": false}
	}
}

// stripReasoning 从上游响应里移除推理内容，供显式开启 drop_reasoning 的模型使用。
// 返回是否确实移除了东西——只有真的移除了，才值得重试解码并在响应头上标注。
// 它只动推理相关的部分；refusal、annotations 等其它不可转换内容仍然会被拒绝。
func stripReasoning(o Object, protocol string) bool {
	dropped := false
	switch protocol {
	case "chat":
		for _, v := range arr(o["choices"]) {
			m := obj(obj(v)["message"])
			if m == nil {
				continue
			}
			for _, k := range []string{"reasoning", "reasoning_content", "reasoning_details"} {
				if _, ok := m[k]; ok {
					delete(m, k)
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

// hasVisibleOutput 判断上游响应里有没有调用方真正用得上的产出：文本或工具调用。
// 推理内容不算数——它可能按配置被丢弃，也可能压根不会转发给调用方。
// 推理模型把输出预算全花在思考上时，响应是 200 但正文为空，对调用方等同于失败。
func hasVisibleOutput(o Object, protocol string) bool {
	switch protocol {
	case "chat":
		for _, v := range arr(o["choices"]) {
			m := obj(obj(v)["message"])
			if strings.TrimSpace(str(m, "content")) != "" || len(arr(m["tool_calls"])) > 0 {
				return true
			}
		}
	case "messages":
		for _, v := range arr(o["content"]) {
			b := obj(v)
			if str(b, "type") == "tool_use" || strings.TrimSpace(str(b, "text")) != "" {
				return true
			}
		}
	case "responses":
		for _, v := range arr(o["output"]) {
			b := obj(v)
			if str(b, "type") == "function_call" {
				return true
			}
			for _, x := range arr(b["content"]) {
				if strings.TrimSpace(str(obj(x), "text")) != "" {
					return true
				}
			}
		}
	case "systemone":
		// System One 的产出是结构化答案，不是自由文本
		return len(obj(o["answers"])) > 0
	}
	return false
}

// truncatedStop 判断上游是因为触到输出上限才停的。推理模型配上偏小的
// max_tokens 时会命中这里：预算全花在推理上，正文一个字都没留下。
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
