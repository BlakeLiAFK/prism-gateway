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
	seenConversation := false
	// Chat 工具消息不能放图片；等并行工具结果齐全后再附带带来源标记的图文。
	toolImages := []any{}
	flushToolImages := func() {
		if len(toolImages) > 0 {
			messages = append(messages, Object{"role": "user", "content": toolImages})
			toolImages = nil
		}
	}
	for _, m := range c.Messages {
		if protocol == "messages" && (m.Role == "system" || m.Role == "developer") {
			if seenConversation {
				return nil, unsupported("Messages 不能保持中途 system/developer 消息的顺序")
			}
			for _, b := range m.Blocks {
				if b.Kind != "text" {
					return nil, unsupported("system 仅支持文本")
				}
				systems = append(systems, b.Text)
			}
			continue
		}
		seenConversation = true
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
			if b.Kind != "result" {
				flushToolImages()
			}
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
				image, err := encodeImage(b, protocol)
				if err != nil {
					return nil, err
				}
				bs = append(bs, image)
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
				content, err := encodeToolContent(b, protocol)
				if err != nil {
					return nil, err
				}
				switch protocol {
				case "chat":
					flush()
					if len(b.Content) > 0 {
						toolImages = append(toolImages, Object{"type": "text", "text": toolImageLabel(b.ID)})
						toolImages = append(toolImages, content.([]any)...)
						content = "Tool output is attached in the following user message. " + toolImageLabel(b.ID)
					}
					messages = append(messages, Object{"role": "tool", "tool_call_id": b.ID, "content": content})
				case "responses":
					flush()
					messages = append(messages, Object{"type": "function_call_output", "call_id": b.ID, "output": content})
				case "messages":
					bs = append(bs, Object{"type": "tool_result", "tool_use_id": b.ID, "content": content, "is_error": b.IsError})
				}
			}
		}
		flush()
	}
	flushToolImages()
	if key, value, err := encodeJSONSchemaFormat(c.Format, protocol); err != nil {
		return nil, err
	} else if key != "" {
		o[key] = value
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
			if t.Strict != nil {
				f["strict"] = *t.Strict
			}
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
	if v, ok := reportedCostNano(o["cost"]); ok {
		u.ReportedCostNano = v
		u.ReportedCostKnown = true
	}
	switch protocol {
	case "chat":
		if _, ok := o["prompt_tokens"]; ok {
			u.Input = int64(num(o, "prompt_tokens"))
			u.Output = int64(num(o, "completion_tokens"))
			u.Cache, u.CacheKnown, u.CacheNull = cacheUsage(obj(o["prompt_tokens_details"]), "cached_tokens")
			u.Known = true
		}
	case "responses", "systemone":
		if _, ok := o["input_tokens"]; ok {
			u.Input = int64(num(o, "input_tokens"))
			u.Output = int64(num(o, "output_tokens"))
			u.Cache, u.CacheKnown, u.CacheNull = cacheUsage(obj(o["input_tokens_details"]), "cached_tokens")
			u.Known = true
		}
	case "messages":
		if _, ok := o["input_tokens"]; ok {
			var readKnown, readNull, writeKnown, writeNull bool
			u.Cache, readKnown, readNull = cacheUsage(o, "cache_read_input_tokens")
			u.Write, writeKnown, writeNull = cacheUsage(o, "cache_creation_input_tokens")
			invalid := (o["cache_read_input_tokens"] != nil && !readKnown) || (o["cache_creation_input_tokens"] != nil && !writeKnown)
			u.CacheNull = readNull || writeNull
			u.CacheKnown = (readKnown || writeKnown) && !invalid && !u.CacheNull
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
		for _, field := range []string{"audio", "images", "annotations"} {
			if hasNonEmptyValue(m[field]) {
				return c, unsupported("无法等价转换 Chat " + field + " 内容")
			}
		}
		if hasReasoningField(m) {
			return c, unsupported("无法跨协议无损转换 reasoning 内容")
		}
		if content := m["content"]; content != nil {
			s, ok := content.(string)
			if !ok {
				return c, unsupported("无法跨协议转换非字符串 Chat content")
			}
			if s != "" {
				c.Blocks = append(c.Blocks, Block{Kind: "text", Text: s})
			}
		}
		for _, v := range arr(m["tool_calls"]) {
			t := obj(v)
			if str(t, "type") != "function" {
				return c, unsupported("无法跨协议转换非 function Chat tool call")
			}
			f := obj(t["function"])
			args := str(f, "arguments")
			if err := validateToolCall(str(t, "id"), str(f, "name"), args); err != nil {
				return c, err
			}
			c.Blocks = append(c.Blocks, Block{Kind: "tool", ID: str(t, "id"), Name: str(f, "name"), Arguments: args})
		}
	case "messages":
		c.Stop = str(o, "stop_reason")
		for _, v := range arr(o["content"]) {
			b := obj(v)
			switch str(b, "type") {
			case "text":
				if len(arr(b["citations"])) > 0 {
					return c, unsupported("无法无损转换带 citations 的 Messages 文本")
				}
				c.Blocks = append(c.Blocks, Block{Kind: "text", Text: str(b, "text")})
			case "tool_use":
				id, name, arguments := str(b, "id"), str(b, "name"), raw(b["input"])
				if err := validateToolCall(id, name, arguments); err != nil {
					return c, err
				}
				c.Blocks = append(c.Blocks, Block{Kind: "tool", ID: id, Name: name, Arguments: arguments})
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
				id, name, arguments := str(b, "call_id"), str(b, "name"), str(b, "arguments")
				if status := str(b, "status"); status != "" && status != "completed" {
					return c, errors.New("upstream returned incomplete tool call")
				}
				if err := validateToolCall(id, name, arguments); err != nil {
					return c, err
				}
				c.Blocks = append(c.Blocks, Block{Kind: "tool", ID: id, Name: name, Arguments: arguments})
			default:
				return c, unsupported("无法无损转换 Responses item " + str(b, "type"))
			}
		}
	}
	if len(c.Blocks) == 0 {
		return c, errors.New("upstream returned no cross-protocol output")
	}
	return c, nil
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
