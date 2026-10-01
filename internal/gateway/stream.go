package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type frame struct {
	Event string
	Data  string
	Raw   string
}

var errSSEComplete = errors.New("SSE complete")

func readSSE(r io.Reader, fn func(frame) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 8192), 2<<20)
	var f frame
	lines := []string{}
	var size int
	flush := func() error {
		if len(lines) == 0 && f.Raw == "" {
			return nil
		}
		f.Data = strings.Join(lines, "\n")
		e := fn(f)
		f = frame{}
		lines = nil
		size = 0
		return e
	}
	for sc.Scan() {
		line := sc.Text()
		size += len(line)
		if size > 4<<20 {
			return errors.New("SSE frame exceeds 4 MiB")
		}
		f.Raw += line + "\n"
		if line == "" {
			if e := flush(); e != nil {
				return e
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if strings.HasPrefix(line, "event:") {
			f.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	if e := sc.Err(); e != nil {
		return e
	}
	if len(lines) > 0 || f.Raw != "" {
		f.Raw += "\n"
		return flush()
	}
	return nil
}
func writeEvent(w http.ResponseWriter, event string, v any) error {
	if event != "" {
		if _, e := fmt.Fprintf(w, "event: %s\n", event); e != nil {
			return e
		}
	}
	if _, e := fmt.Fprintf(w, "data: %s\n\n", raw(v)); e != nil {
		return e
	}
	return http.NewResponseController(w).Flush()
}
func streamError(w http.ResponseWriter, p, id string) {
	switch p {
	case "messages":
		writeEvent(w, "error", Object{"type": "error", "error": Object{"type": "api_error", "message": "上游流中断或包含无法转换的内容；请用 request_id 查询日志"}, "request_id": id})
	case "responses":
		writeEvent(w, "error", Object{"type": "error", "code": "upstream_stream_error", "message": "Upstream stream interrupted", "param": nil, "request_id": id})
	default:
		writeEvent(w, "", Object{"error": Object{"type": "upstream_stream_error", "message": "Upstream stream interrupted", "code": "upstream_stream_error"}, "request_id": id})
	}
}
func meterFrame(f frame, p string, u *Usage) (terminal, visible, reasoning, truncated bool, err error) {
	if strings.TrimSpace(f.Data) == "[DONE]" {
		return true, false, false, false, nil
	}
	if f.Data == "" {
		return false, false, false, false, nil
	}
	var o Object
	if e := json.Unmarshal([]byte(f.Data), &o); e != nil {
		return false, false, false, false, errors.New("invalid SSE JSON")
	}
	typ := str(o, "type")
	if typ == "error" || o["error"] != nil {
		return false, false, false, false, errors.New("upstream error event")
	}
	visible, reasoning = streamFrameOutput(o, p, typ)
	truncated = truncatedStreamFrame(o, p, typ)
	if u.tap != nil {
		if n := deltaChars(o, p, typ); n > 0 {
			u.tap(n)
		}
	}
	switch p {
	case "chat":
		extractUsage(obj(o["usage"]), p, u)
	case "messages":
		if typ == "message_start" {
			extractUsage(obj(obj(o["message"])["usage"]), p, u)
		}
		extractUsage(obj(o["usage"]), p, u)
		if typ == "message_stop" {
			return true, visible, reasoning, truncated, nil
		}
	case "responses":
		if typ == "response.completed" || typ == "response.incomplete" {
			extractUsage(obj(obj(o["response"])["usage"]), p, u)
			return true, visible, reasoning, truncated, nil
		}
		if typ == "response.failed" {
			return false, visible, reasoning, truncated, errors.New("upstream response failed")
		}
	}
	return false, visible, reasoning, truncated, nil
}

func streamFrameOutput(o Object, p, typ string) (visible, reasoning bool) {
	switch p {
	case "chat":
		for _, v := range arr(o["choices"]) {
			d := obj(obj(v)["delta"])
			reasoning = reasoning || hasReasoningField(d)
			if strings.TrimSpace(str(d, "content")) != "" || strings.TrimSpace(str(d, "refusal")) != "" ||
				len(arr(d["tool_calls"])) > 0 || len(arr(d["images"])) > 0 || d["audio"] != nil {
				visible = true
			}
		}
	case "messages":
		if typ == "content_block_start" {
			b := obj(o["content_block"])
			kind := str(b, "type")
			if kind == "thinking" || kind == "redacted_thinking" {
				return false, true
			}
			return kind != "" && (kind != "text" || strings.TrimSpace(str(b, "text")) != ""), false
		}
		if typ == "content_block_delta" {
			d := obj(o["delta"])
			kind := str(d, "type")
			if kind == "thinking_delta" || kind == "signature_delta" {
				return false, true
			}
			if kind == "text_delta" {
				return strings.TrimSpace(str(d, "text")) != "", false
			}
			return kind != "", false
		}
	case "responses":
		if typ == "response.output_item.added" || typ == "response.output_item.done" {
			kind := str(obj(o["item"]), "type")
			if kind == "reasoning" {
				return false, true
			}
			return kind != "" && kind != "message", false
		}
		if strings.HasPrefix(typ, "response.reasoning_") {
			return false, true
		}
		if typ == "response.content_part.added" {
			part := obj(o["part"])
			kind := str(part, "type")
			return kind != "" && (kind != "output_text" || strings.TrimSpace(str(part, "text")) != ""), false
		}
		if typ == "response.output_text.delta" || typ == "response.refusal.delta" || strings.HasSuffix(typ, ".partial_image") {
			return strings.TrimSpace(str(o, "delta")) != "" || str(o, "partial_image_b64") != "", false
		}
	}
	return visible, reasoning
}

func truncatedStreamFrame(o Object, p, typ string) bool {
	switch p {
	case "chat":
		for _, v := range arr(o["choices"]) {
			if str(obj(v), "finish_reason") == "length" {
				return true
			}
		}
	case "messages":
		if typ != "message_delta" {
			return false
		}
		stop := str(obj(o["delta"]), "stop_reason")
		return stop == "max_tokens" || stop == "model_context_window_exceeded"
	case "responses":
		return typ == "response.incomplete" && str(obj(obj(o["response"])["incomplete_details"]), "reason") == "max_output_tokens"
	}
	return false
}
func nativeStream(w http.ResponseWriter, r io.Reader, p string, u *Usage) error {
	done := false
	visible := false
	reasoning := false
	truncated := false
	e := readSSE(r, func(f frame) error {
		terminal, output, thought, limited, err := meterFrame(f, p, u)
		if err != nil {
			return err
		}
		visible = visible || output
		reasoning = reasoning || thought
		truncated = truncated || limited
		if terminal && !visible && !reasoning && !truncated {
			return errors.New("upstream returned empty output")
		}
		if _, e := io.WriteString(w, f.Raw); e != nil {
			return e
		}
		if e := http.NewResponseController(w).Flush(); e != nil {
			return e
		}
		done = done || terminal
		if terminal {
			return errSSEComplete
		}
		return nil
	})
	if errors.Is(e, errSSEComplete) {
		e = nil
	}
	if e != nil {
		return e
	}
	if !done {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func convertedStream(w http.ResponseWriter, r io.Reader, from, to, model, id string, u *Usage, dropReasoning bool) error {
	em := newEmitter(w, to, model, id)
	em.requireClose = true
	done := false
	dropped := map[int]bool{}
	err := readSSE(r, func(f frame) error {
		term, _, _, _, err := meterFrame(f, from, u)
		if err != nil {
			return err
		}
		em.usage = *u
		done = done || term
		if f.Data == "" || strings.TrimSpace(f.Data) == "[DONE]" {
			if term {
				return errSSEComplete
			}
			return nil
		}
		var o Object
		if err = json.Unmarshal([]byte(f.Data), &o); err != nil {
			return err
		}
		typ := str(o, "type")
		switch from {
		case "chat":
			for _, v := range arr(o["choices"]) {
				v := obj(v)
				if num(v, "index") != 0 {
					return errors.New("multiple stream choices unsupported")
				}
				d := obj(v["delta"])
				if str(d, "refusal") != "" {
					return errors.New("unmappable refusal in stream")
				}
				if d["audio"] != nil || d["images"] != nil || hasNonEmptyValue(d["annotations"]) {
					return errors.New("unmappable media output in stream")
				}
				if content := d["content"]; content != nil {
					if _, ok := content.(string); !ok && hasNonEmptyValue(content) {
						return errors.New("unmappable Chat content in stream")
					}
				}
				if hasReasoningField(d) {
					// 未显式开启丢弃时，宁可中断也不悄悄吞掉推理内容
					if !dropReasoning {
						return errors.New("unmappable reasoning in stream")
					}
					// 已开启：不产出任何块，响应头在流开始前已标注
				}
				if t := str(d, "content"); t != "" {
					if err = em.start(0, Block{Kind: "text"}); err != nil {
						return err
					}
					if err = em.delta(0, t); err != nil {
						return err
					}
				}
				for _, v := range arr(d["tool_calls"]) {
					tc := obj(v)
					if kind := str(tc, "type"); kind != "" && kind != "function" {
						return errors.New("unmappable Chat tool type")
					}
					key := 100 + int(num(tc, "index"))
					fn := obj(tc["function"])
					if err = em.start(key, Block{Kind: "tool", ID: str(tc, "id"), Name: str(fn, "name")}); err != nil {
						return err
					}
					if t := str(fn, "arguments"); t != "" {
						if err = em.delta(key, t); err != nil {
							return err
						}
					}
				}
				if s := str(v, "finish_reason"); s != "" {
					if err = em.closeForChatStop(s); err != nil {
						return err
					}
				}
			}
		case "messages":
			key := int(num(o, "index"))
			switch typ {
			case "content_block_start":
				b := obj(o["content_block"])
				switch str(b, "type") {
				case "text":
					if hasNonEmptyValue(b["citations"]) {
						return errors.New("unmappable Anthropic text citations")
					}
					if err = em.start(key, Block{Kind: "text"}); err != nil {
						return err
					}
					if t := str(b, "text"); t != "" {
						return em.delta(key, t)
					}
				case "tool_use":
					return startMessagesTool(em, key, b)
				case "thinking", "redacted_thinking":
					if !dropReasoning {
						return errors.New("unmappable Anthropic reasoning block")
					}
					dropped[key] = true
				default:
					return errors.New("unmappable Anthropic content block")
				}
			case "content_block_delta":
				d := obj(o["delta"])
				if dropped[key] {
					return nil
				}
				switch str(d, "type") {
				case "text_delta":
					return em.delta(key, str(d, "text"))
				case "input_json_delta":
					return em.delta(key, str(d, "partial_json"))
				case "thinking_delta", "signature_delta":
					if !dropReasoning {
						return errors.New("unmappable Anthropic reasoning delta")
					}
					dropped[key] = true
					return nil
				default:
					return errors.New("unmappable Anthropic delta")
				}
			case "content_block_stop":
				if dropped[key] {
					delete(dropped, key)
					return nil
				}
				// Anthropic may omit input_json_delta for a zero-argument tool.
				if i, ok := em.byKey[key]; ok && em.blocks[i].Kind == "tool" && em.blocks[i].Arguments == "" {
					if err = em.delta(key, "{}"); err != nil {
						return err
					}
				}
				if i, ok := em.byKey[key]; ok && em.blocks[i].Kind == "tool" {
					return em.sourceClose(key)
				}
				return em.close(key)
			case "message_delta":
				return em.confirmMessagesStop(str(obj(o["delta"]), "stop_reason"))
			}
		case "responses":
			key := int(num(o, "output_index"))
			if strings.HasPrefix(typ, "response.reasoning_") {
				if !dropReasoning {
					return errors.New("unmappable Responses reasoning delta")
				}
				dropped[key] = true
				return nil
			}
			switch typ {
			case "response.output_item.added":
				b := obj(o["item"])
				switch str(b, "type") {
				case "message":
				case "function_call":
					return em.start(key, Block{Kind: "tool", ID: str(b, "call_id"), Name: str(b, "name")})
				case "reasoning":
					if !dropReasoning {
						return errors.New("unmappable Responses reasoning item")
					}
					dropped[key] = true
				default:
					return errors.New("unmappable Responses output item")
				}
			case "response.content_part.added", "response.content_part.done":
				part := obj(o["part"])
				if num(o, "content_index") != 0 || str(part, "type") != "output_text" {
					return errors.New("unmappable Responses content part")
				}
				if hasNonEmptyValue(part["annotations"]) {
					return errors.New("unmappable Responses output annotations")
				}
				if typ == "response.content_part.added" {
					return em.start(key, Block{Kind: "text"})
				}
			case "response.output_text.done":
				if hasNonEmptyValue(o["annotations"]) {
					return errors.New("unmappable Responses output annotations")
				}
				if num(o, "content_index") != 0 {
					return errors.New("multiple Responses content parts unsupported")
				}
				return em.finalText(key, str(o, "text"))
			case "response.output_text.delta", "response.function_call_arguments.delta":
				if hasNonEmptyValue(o["annotations"]) {
					return errors.New("unmappable Responses output annotations")
				}
				return em.delta(key, str(o, "delta"))
			case "response.function_call_arguments.done":
				return em.finalArguments(key, str(o, "arguments"))
			case "response.output_item.done":
				b := obj(o["item"])
				if status := str(b, "status"); status != "" && status != "completed" {
					return errors.New("Responses done item is not completed")
				}
				if dropped[key] || str(b, "type") == "reasoning" {
					if !dropReasoning {
						return errors.New("unmappable Responses reasoning item")
					}
					delete(dropped, key)
					return nil
				}
				if str(b, "type") == "function_call" {
					if i, ok := em.byKey[key]; !ok || em.blocks[i].ID != str(b, "call_id") || em.blocks[i].Name != str(b, "name") {
						return errors.New("Responses final tool item does not match start")
					}
					if err = em.finalArguments(key, str(b, "arguments")); err != nil {
						return err
					}
				} else if str(b, "type") == "message" && len(arr(b["content"])) > 0 {
					if err = finalResponsesText(em, key, arr(b["content"])); err != nil {
						return err
					}
				}
				return em.close(key)
			case "response.completed":
				for i, item := range arr(obj(o["response"])["output"]) {
					if err = completeResponsesItem(em, i, obj(item), dropReasoning); err != nil {
						return err
					}
				}
			case "response.incomplete":
				if str(obj(obj(o["response"])["incomplete_details"]), "reason") != "max_output_tokens" {
					return errors.New("unsupported Responses incomplete reason")
				}
				em.stop = "length"
			case "response.output_text.annotation.added", "response.refusal.delta":
				return errors.New("unmappable Responses annotation/refusal")
			}
		}
		if term {
			return errSSEComplete
		}
		return nil
	})
	if errors.Is(err, errSSEComplete) {
		err = nil
	}
	if err != nil {
		return err
	}
	if !done {
		return io.ErrUnexpectedEOF
	}
	em.usage = *u
	return em.finish()
}
