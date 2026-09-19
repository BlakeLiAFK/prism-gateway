package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This IR is only used for the explicitly supported cross-protocol subset.
// Native requests and responses bypass it, preserving opaque vendor fields.
type Block struct {
	Kind      string
	Text      string
	ID        string
	Name      string
	Arguments string
	URL       string
	IsError   bool
}
type Message struct {
	Role   string
	Blocks []Block
}
type Tool struct {
	Name        string
	Description string
	Schema      any
}
type Canonical struct {
	Messages    []Message
	Tools       []Tool
	MaxOutput   int
	Temperature any
	TopP        any
	Stop        any
	Choice      string
	ToolName    string
	Stream      bool
	Parallel    *bool
}
type Usage struct {
	Input  int64
	Output int64
	Cache  int64
	Write  int64
	Known  bool
}
type Completion struct {
	Blocks []Block
	Usage  Usage
	Stop   string
}

func allowed(o Object, keys ...string) error {
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	for k, v := range o {
		if !set[k] && v != nil {
			return fmt.Errorf("跨协议转换不支持字段 %q；请改用对应原生协议模型", k)
		}
	}
	return nil
}
func unsupported(s string) error { return fail("UNSUPPORTED_FEATURE", s, 400) }
func decodeCanonical(o Object, protocol string) (Canonical, error) {
	c := Canonical{MaxOutput: 4096, Stream: boolean(o, "stream"), Temperature: o["temperature"], TopP: o["top_p"], Choice: "auto"}
	common := []string{"model", "stream", "temperature", "top_p", "tools", "tool_choice"}
	var e error
	switch protocol {
	case "chat":
		e = allowed(o, append(common, "messages", "max_tokens", "max_completion_tokens", "stop", "stream_options", "n", "parallel_tool_calls")...)
		if num(o, "n") > 1 {
			return c, unsupported("跨协议仅支持 n=1")
		}
		if v := num(o, "max_completion_tokens"); v > 0 {
			c.MaxOutput = int(v)
		} else if v := num(o, "max_tokens"); v > 0 {
			c.MaxOutput = int(v)
		}
		c.Stop = o["stop"]
		for _, v := range arr(o["messages"]) {
			m := obj(v)
			if er := allowed(m, "role", "content", "tool_calls", "tool_call_id"); er != nil {
				return c, er
			}
			cm := Message{Role: str(m, "role")}
			if cm.Role == "developer" {
				cm.Role = "system"
			}
			if cm.Role == "tool" {
				cm.Role = "user"
				text, ok := m["content"].(string)
				if !ok {
					return c, unsupported("tool result 仅支持文本")
				}
				cm.Blocks = []Block{{Kind: "result", ID: str(m, "tool_call_id"), Text: text}}
			} else {
				cm.Blocks, e = readBlocks(m["content"], protocol)
				if e != nil {
					return c, e
				}
				for _, t := range arr(m["tool_calls"]) {
					t := obj(t)
					if str(t, "type") != "function" {
						return c, unsupported("仅支持 function tools")
					}
					f := obj(t["function"])
					cm.Blocks = append(cm.Blocks, Block{Kind: "tool", ID: str(t, "id"), Name: str(f, "name"), Arguments: str(f, "arguments")})
				}
			}
			c.Messages = append(c.Messages, cm)
		}
	case "messages":
		e = allowed(o, append(common, "messages", "system", "max_tokens", "stop_sequences")...)
		c.MaxOutput = int(num(o, "max_tokens"))
		c.Stop = o["stop_sequences"]
		if o["system"] != nil {
			bs, er := readBlocks(o["system"], protocol)
			if er != nil {
				return c, er
			}
			c.Messages = append(c.Messages, Message{Role: "system", Blocks: bs})
		}
		for _, v := range arr(o["messages"]) {
			m := obj(v)
			if er := allowed(m, "role", "content"); er != nil {
				return c, er
			}
			bs, er := readBlocks(m["content"], protocol)
			if er != nil {
				return c, er
			}
			c.Messages = append(c.Messages, Message{Role: str(m, "role"), Blocks: bs})
		}
	case "responses":
		e = allowed(o, append(common, "input", "instructions", "max_output_tokens", "store", "parallel_tool_calls")...)
		if boolean(o, "store") {
			return c, unsupported("跨协议不支持 store=true 或服务端会话状态")
		}
		if v := num(o, "max_output_tokens"); v > 0 {
			c.MaxOutput = int(v)
		}
		if s := str(o, "instructions"); s != "" {
			c.Messages = append(c.Messages, Message{Role: "system", Blocks: []Block{{Kind: "text", Text: s}}})
		}
		if text, ok := o["input"].(string); ok {
			c.Messages = append(c.Messages, Message{Role: "user", Blocks: []Block{{Kind: "text", Text: text}}})
		} else {
			for _, v := range arr(o["input"]) {
				m := obj(v)
				switch str(m, "type") {
				case "function_call":
					if er := allowed(m, "type", "id", "call_id", "name", "arguments", "status"); er != nil {
						return c, er
					}
					c.Messages = append(c.Messages, Message{Role: "assistant", Blocks: []Block{{Kind: "tool", ID: str(m, "call_id"), Name: str(m, "name"), Arguments: str(m, "arguments")}}})
				case "function_call_output":
					if er := allowed(m, "type", "id", "call_id", "output", "status"); er != nil {
						return c, er
					}
					text, ok := m["output"].(string)
					if !ok {
						return c, unsupported("function_call_output 仅支持文本")
					}
					c.Messages = append(c.Messages, Message{Role: "user", Blocks: []Block{{Kind: "result", ID: str(m, "call_id"), Text: text}}})
				case "", "message":
					if er := allowed(m, "type", "id", "role", "content", "status"); er != nil {
						return c, er
					}
					bs, er := readBlocks(m["content"], protocol)
					if er != nil {
						return c, er
					}
					role := str(m, "role")
					if role == "developer" {
						role = "system"
					}
					c.Messages = append(c.Messages, Message{Role: role, Blocks: bs})
				default:
					return c, unsupported("跨协议不支持 Responses input item: " + str(m, "type"))
				}
			}
		}
	default:
		return c, errors.New("unknown protocol")
	}
	if e != nil {
		return c, e
	}
	if c.MaxOutput < 1 {
		return c, unsupported("跨协议 max output tokens 必须大于 0")
	}
	if len(c.Messages) == 0 {
		return c, unsupported("messages/input 不能为空")
	}
	for _, m := range c.Messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			return c, unsupported("不支持的消息角色 " + m.Role)
		}
	}
	for _, v := range arr(o["tools"]) {
		t := obj(v)
		var f Object
		switch protocol {
		case "chat":
			if str(t, "type") != "function" {
				return c, unsupported("仅支持 function tools")
			}
			if er := allowed(t, "type", "function"); er != nil {
				return c, er
			}
			f = obj(t["function"])
			if er := allowed(f, "name", "description", "parameters"); er != nil {
				return c, er
			}
		case "responses":
			if str(t, "type") != "function" {
				return c, unsupported("不支持跨协议 server tools")
			}
			if er := allowed(t, "type", "name", "description", "parameters"); er != nil {
				return c, er
			}
			f = t
		case "messages":
			if er := allowed(t, "name", "description", "input_schema"); er != nil {
				return c, er
			}
			f = t
		}
		schema := f["parameters"]
		if protocol == "messages" {
			schema = f["input_schema"]
		}
		if schema == nil {
			schema = Object{"type": "object", "properties": Object{}}
		}
		if str(f, "name") == "" {
			return c, unsupported("工具 name 不能为空")
		}
		c.Tools = append(c.Tools, Tool{str(f, "name"), str(f, "description"), schema})
	}
	if v, ok := o["parallel_tool_calls"].(bool); ok {
		c.Parallel = &v
	}
	switch v := o["tool_choice"].(type) {
	case string:
		c.Choice = v
	case map[string]any:
		typ := str(v, "type")
		if protocol == "messages" {
			if er := allowed(v, "type", "name", "disable_parallel_tool_use"); er != nil {
				return c, er
			}
			if b, ok := v["disable_parallel_tool_use"].(bool); ok {
				b = !b
				c.Parallel = &b
			}
			if typ == "any" {
				typ = "required"
			}
			if typ == "tool" {
				typ = "function"
			}
			c.Choice = typ
			c.ToolName = str(v, "name")
		} else {
			c.Choice = typ
			c.ToolName = str(v, "name")
			if f := obj(v["function"]); f != nil {
				c.ToolName = str(f, "name")
			}
		}
	}
	if c.Choice != "auto" && c.Choice != "none" && c.Choice != "required" && c.Choice != "function" {
		return c, unsupported("tool_choice 不受支持")
	}
	return c, nil
}
func readBlocks(v any, protocol string) ([]Block, error) {
	if v == nil {
		return []Block{}, nil
	}
	if s, ok := v.(string); ok {
		return []Block{{Kind: "text", Text: s}}, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, unsupported("content 必须是字符串或数组")
	}
	out := []Block{}
	for _, v := range a {
		o := obj(v)
		typ := str(o, "type")
		switch typ {
		case "text", "input_text", "output_text":
			if e := allowed(o, "type", "text"); e != nil {
				return nil, e
			}
			out = append(out, Block{Kind: "text", Text: str(o, "text")})
		case "image_url":
			if e := allowed(o, "type", "image_url"); e != nil {
				return nil, e
			}
			im := obj(o["image_url"])
			if e := allowed(im, "url"); e != nil {
				return nil, e
			}
			out = append(out, Block{Kind: "image", URL: str(im, "url")})
		case "input_image":
			if e := allowed(o, "type", "image_url", "detail"); e != nil {
				return nil, e
			}
			if d := str(o, "detail"); d != "" && d != "auto" {
				return nil, unsupported("跨协议图片 detail 仅支持 auto")
			}
			out = append(out, Block{Kind: "image", URL: str(o, "image_url")})
		case "image":
			if e := allowed(o, "type", "source"); e != nil {
				return nil, e
			}
			s := obj(o["source"])
			switch str(s, "type") {
			case "base64":
				out = append(out, Block{Kind: "image", URL: "data:" + str(s, "media_type") + ";base64," + str(s, "data")})
			case "url":
				out = append(out, Block{Kind: "image", URL: str(s, "url")})
			default:
				return nil, unsupported("图片来源不受支持")
			}
		case "tool_use":
			if e := allowed(o, "type", "id", "name", "input"); e != nil {
				return nil, e
			}
			out = append(out, Block{Kind: "tool", ID: str(o, "id"), Name: str(o, "name"), Arguments: raw(o["input"])})
		case "tool_result":
			if e := allowed(o, "type", "tool_use_id", "content", "is_error"); e != nil {
				return nil, e
			}
			bs, e := readBlocks(o["content"], "messages")
			if e != nil {
				return nil, e
			}
			parts := []string{}
			for _, b := range bs {
				if b.Kind != "text" {
					return nil, unsupported("跨协议工具结果仅支持文本")
				}
				parts = append(parts, b.Text)
			}
			text := strings.Join(parts, "\n")
			if boolean(o, "is_error") {
				text = "[tool error]\n" + text
			}
			out = append(out, Block{Kind: "result", ID: str(o, "tool_use_id"), Text: text, IsError: boolean(o, "is_error")})
		default:
			return nil, unsupported("跨协议不支持内容类型 " + typ + "；使用原生协议保留其语义")
		}
	}
	return out, nil
}
func imageSource(u string) (Object, error) {
	if strings.HasPrefix(u, "data:") {
		p := strings.SplitN(strings.TrimPrefix(u, "data:"), ";base64,", 2)
		if len(p) != 2 {
			return nil, unsupported("image data URL 无效")
		}
		if _, e := base64.StdEncoding.DecodeString(p[1]); e != nil {
			return nil, unsupported("image base64 无效")
		}
		return Object{"type": "base64", "media_type": p[0], "data": p[1]}, nil
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return nil, unsupported("image URL 无效")
	}
	return Object{"type": "url", "url": u}, nil
}
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
	switch protocol {
	case "chat":
		o["messages"] = messages
		o["max_tokens"] = c.MaxOutput
		if c.Stream {
			o["stream_options"] = Object{"include_usage": true}
		}
		if c.Stop != nil {
			o["stop"] = c.Stop
		}
	case "messages":
		o["messages"] = messages
		o["max_tokens"] = c.MaxOutput
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
		o["max_output_tokens"] = c.MaxOutput
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
	case "responses":
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
