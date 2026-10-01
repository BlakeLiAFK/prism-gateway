package gateway

import (
	"math"
	"sort"
)

// JSONSchemaFormat 是三种协议都能表达的结构化输出公共部分。
type JSONSchemaFormat struct {
	Name        string
	Description string
	Schema      Object
	Strict      bool
}

func readJSONSchemaFormat(v any, protocol string) (*JSONSchemaFormat, error) {
	if v == nil {
		return nil, nil
	}
	o := obj(v)
	if protocol != "messages" && str(o, "type") == "text" && len(o) == 1 {
		return nil, nil
	}
	if o == nil || str(o, "type") != "json_schema" {
		return nil, unsupported("跨协议结构化输出仅支持 json_schema")
	}
	if protocol == "chat" {
		if _, err := allowed(o, "type", "json_schema"); err != nil {
			return nil, err
		}
		o = obj(o["json_schema"])
		if o == nil {
			return nil, unsupported("response_format.json_schema 必须是对象")
		}
	}

	if protocol == "messages" {
		if _, err := allowed(o, "type", "schema"); err != nil {
			return nil, err
		}
		schema := obj(o["schema"])
		if schema == nil {
			return nil, unsupported("json_schema.schema 必须是对象")
		}
		return &JSONSchemaFormat{Name: "response", Schema: schema, Strict: true}, nil
	}

	if _, err := allowed(o, "type", "name", "description", "schema", "strict"); err != nil {
		return nil, err
	}
	name := str(o, "name")
	schema := obj(o["schema"])
	if name == "" || schema == nil {
		return nil, unsupported("json_schema 需要 name 和对象 schema")
	}
	strict, ok := o["strict"].(bool)
	if o["strict"] != nil && !ok {
		return nil, unsupported("json_schema.strict 必须是布尔值")
	}
	return &JSONSchemaFormat{Name: name, Description: str(o, "description"), Schema: schema, Strict: strict}, nil
}

func readMessagesOutputConfig(v any, ignored *[]string) (*JSONSchemaFormat, error) {
	if v == nil {
		return nil, nil
	}
	o := obj(v)
	if o == nil {
		return nil, unsupported("output_config 必须是对象")
	}
	if _, err := allowed(o, "effort", "format"); err != nil {
		return nil, err
	}
	if o["effort"] != nil {
		effort := str(o, "effort")
		if effort != "low" && effort != "medium" && effort != "high" && effort != "xhigh" && effort != "max" {
			return nil, unsupported("output_config.effort 不合法")
		}
		*ignored = append(*ignored, "output_config.effort")
		sort.Strings(*ignored)
	}
	return readJSONSchemaFormat(o["format"], "messages")
}

func readResponsesTextFormat(v any) (*JSONSchemaFormat, error) {
	if v == nil {
		return nil, nil
	}
	o := obj(v)
	if o == nil {
		return nil, unsupported("text 必须是对象")
	}
	if _, err := allowed(o, "format"); err != nil {
		return nil, err
	}
	return readJSONSchemaFormat(o["format"], "responses")
}

func readContextManagement(v any, ignored *[]string) error {
	if v == nil {
		return nil
	}
	o := obj(v)
	if o == nil {
		return unsupported("context_management 必须是对象")
	}
	if _, err := allowed(o, "edits"); err != nil {
		return err
	}
	edits, ok := o["edits"].([]any)
	if o["edits"] != nil && !ok {
		return unsupported("context_management.edits 必须是数组")
	}
	if len(edits) == 0 {
		*ignored = append(*ignored, "context_management")
		return nil
	}
	for _, value := range edits {
		edit := obj(value)
		if _, err := allowed(edit, "type", "keep"); err != nil {
			return err
		}
		if str(edit, "type") != "clear_thinking_20251015" {
			return unsupported("跨协议仅能忽略已无推理块时的 clear_thinking context edit")
		}
		if keep, set := edit["keep"]; set {
			if err := validateThinkingKeep(keep); err != nil {
				return err
			}
		}
	}
	*ignored = append(*ignored, "context_management.edits.clear_thinking_20251015")
	sort.Strings(*ignored)
	return nil
}

func validateThinkingKeep(v any) error {
	if s, ok := v.(string); ok && s == "all" {
		return nil
	}
	o := obj(v)
	if str(o, "type") == "all" && len(o) == 1 {
		return nil
	}
	if _, err := allowed(o, "type", "value"); err != nil {
		return err
	}
	n := num(o, "value")
	if str(o, "type") != "thinking_turns" || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
		return unsupported("clear_thinking.keep 必须为 all 或 thinking_turns 正整数")
	}
	return nil
}

func encodeJSONSchemaFormat(format *JSONSchemaFormat, protocol string) (string, Object, error) {
	if format == nil {
		return "", nil, nil
	}
	switch protocol {
	case "messages":
		if !format.Strict {
			return "", nil, unsupported("Messages 不能等价表示非严格 json_schema")
		}
		if format.Description != "" {
			return "", nil, unsupported("Messages 不能无损表示 json_schema description")
		}
		return "output_config", Object{"format": Object{"type": "json_schema", "schema": format.Schema}}, nil
	case "chat":
		return "response_format", Object{"type": "json_schema", "json_schema": Object{
			"name": format.Name, "description": format.Description, "schema": format.Schema, "strict": format.Strict,
		}}, nil
	case "responses":
		return "text", Object{"format": Object{
			"type": "json_schema", "name": format.Name, "description": format.Description, "schema": format.Schema, "strict": format.Strict,
		}}, nil
	default:
		return "", nil, unsupported("目标协议不支持结构化输出")
	}
}
