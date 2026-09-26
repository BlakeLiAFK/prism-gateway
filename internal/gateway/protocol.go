package gateway

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
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
	// Ignored 记录那些被接受但未传递给上游的字段。
	// 忽略本身可以接受，不告诉调用方不行——它们会出现在 X-Prism-Ignored 响应头里。
	Ignored []string
}
type Usage struct {
	Input  int64
	Output int64
	Cache  int64
	Write  int64
	Known  bool
	// tap 在流式片段到达时收到新增输出的字符数，供实时面板估算速度；可为空
	tap func(chars int)
}
type Completion struct {
	Blocks []Block
	Usage  Usage
	Stop   string
}

// ignorable 是那些不影响生成语义的字段：跨协议时没有对应概念，
// 丢掉它们不会改变模型看到的内容，也不会改变返回内容的含义。
// 与之相对，thinking 这类字段会真正改变行为，不在此列。
var ignorable = map[string]string{
	"metadata":           "Anthropic 规范中它不参与生成",
	"context_management": "上下文管理由客户端负责，网关不介入",
	"output_config":      "生成偏好提示，跨协议无对应字段",
	"service_tier":       "供应商侧调度提示",
	"store":              "上游侧留存开关，跨协议无对应语义",
	"user":               "调用方标识，不参与生成",
	"cache_control":      "提示缓存标记，跨协议无对应概念，只影响成本不影响内容",
}

// containsKey 递归查找某个键是否在请求里出现过。
// cache_control 嵌在内容块里而不是顶层，块级白名单放行之后
// 顶层就看不到它了，需要单独扫一遍才能如实汇报。
func containsKey(v any, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x[key]; ok {
			return true
		}
		for _, e := range x {
			if containsKey(e, key) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if containsKey(e, key) {
				return true
			}
		}
	}
	return false
}

func allowed(o Object, keys ...string) ([]string, error) {
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	ignored := []string{}
	for k, v := range o {
		if set[k] || v == nil {
			continue
		}
		if _, ok := ignorable[k]; ok {
			ignored = append(ignored, k)
			continue
		}
		return nil, fmt.Errorf("跨协议转换不支持字段 %q；请改用对应原生协议模型", k)
	}
	sort.Strings(ignored)
	return ignored, nil
}
func unsupported(s string) error { return fail("UNSUPPORTED_FEATURE", s, 400) }
func decodeCanonical(o Object, protocol string) (Canonical, error) {
	c := Canonical{Stream: boolean(o, "stream"), Temperature: o["temperature"], TopP: o["top_p"], Choice: "auto"}
	common := []string{"model", "stream", "temperature", "top_p", "tools", "tool_choice"}
	var e error
	switch protocol {
	case "chat":
		c.Ignored, e = allowed(o, append(common, "messages", "max_tokens", "max_completion_tokens", "stop", "stream_options", "n", "parallel_tool_calls")...)
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
			if _, er := allowed(m, "role", "content", "tool_calls", "tool_call_id"); er != nil {
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
		c.Ignored, e = allowed(o, append(common, "messages", "system", "max_tokens", "stop_sequences")...)
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
			if _, er := allowed(m, "role", "content"); er != nil {
				return c, er
			}
			bs, er := readBlocks(m["content"], protocol)
			if er != nil {
				return c, er
			}
			c.Messages = append(c.Messages, Message{Role: str(m, "role"), Blocks: bs})
		}
	case "responses":
		c.Ignored, e = allowed(o, append(common, "input", "instructions", "max_output_tokens", "store", "parallel_tool_calls")...)
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
					if _, er := allowed(m, "type", "id", "call_id", "name", "arguments", "status"); er != nil {
						return c, er
					}
					c.Messages = append(c.Messages, Message{Role: "assistant", Blocks: []Block{{Kind: "tool", ID: str(m, "call_id"), Name: str(m, "name"), Arguments: str(m, "arguments")}}})
				case "function_call_output":
					if _, er := allowed(m, "type", "id", "call_id", "output", "status"); er != nil {
						return c, er
					}
					text, ok := m["output"].(string)
					if !ok {
						return c, unsupported("function_call_output 仅支持文本")
					}
					c.Messages = append(c.Messages, Message{Role: "user", Blocks: []Block{{Kind: "result", ID: str(m, "call_id"), Text: text}}})
				case "", "message":
					if _, er := allowed(m, "type", "id", "role", "content", "status"); er != nil {
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
	// 0 表示客户端没写，交给目标协议决定（见 selections）
	if c.MaxOutput < 0 {
		return c, unsupported("跨协议 max output tokens 不能为负数")
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
			if _, er := allowed(t, "type", "function"); er != nil {
				return c, er
			}
			f = obj(t["function"])
			if _, er := allowed(f, "name", "description", "parameters"); er != nil {
				return c, er
			}
		case "responses":
			if str(t, "type") != "function" {
				return c, unsupported("不支持跨协议 server tools")
			}
			if _, er := allowed(t, "type", "name", "description", "parameters"); er != nil {
				return c, er
			}
			f = t
		case "messages":
			if _, er := allowed(t, "name", "description", "input_schema", "cache_control"); er != nil {
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
			if _, er := allowed(v, "type", "name", "disable_parallel_tool_use"); er != nil {
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
	if containsKey(o, "cache_control") {
		c.Ignored = append(c.Ignored, "cache_control")
		sort.Strings(c.Ignored)
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
			if _, e := allowed(o, "type", "text", "cache_control"); e != nil {
				return nil, e
			}
			out = append(out, Block{Kind: "text", Text: str(o, "text")})
		case "image_url":
			if _, e := allowed(o, "type", "image_url", "cache_control"); e != nil {
				return nil, e
			}
			im := obj(o["image_url"])
			if _, e := allowed(im, "url"); e != nil {
				return nil, e
			}
			out = append(out, Block{Kind: "image", URL: str(im, "url")})
		case "input_image":
			if _, e := allowed(o, "type", "image_url", "detail", "cache_control"); e != nil {
				return nil, e
			}
			if d := str(o, "detail"); d != "" && d != "auto" {
				return nil, unsupported("跨协议图片 detail 仅支持 auto")
			}
			out = append(out, Block{Kind: "image", URL: str(o, "image_url")})
		case "image":
			if _, e := allowed(o, "type", "source", "cache_control"); e != nil {
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
			if _, e := allowed(o, "type", "id", "name", "input", "cache_control"); e != nil {
				return nil, e
			}
			out = append(out, Block{Kind: "tool", ID: str(o, "id"), Name: str(o, "name"), Arguments: raw(o["input"])})
		case "tool_result":
			if _, e := allowed(o, "type", "tool_use_id", "content", "is_error", "cache_control"); e != nil {
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
