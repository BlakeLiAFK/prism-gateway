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
func meterFrame(f frame, p string, u *Usage) (terminal bool, err error) {
	if strings.TrimSpace(f.Data) == "[DONE]" {
		return true, nil
	}
	if f.Data == "" {
		return false, nil
	}
	var o Object
	if e := json.Unmarshal([]byte(f.Data), &o); e != nil {
		return false, errors.New("invalid SSE JSON")
	}
	typ := str(o, "type")
	if typ == "error" || o["error"] != nil {
		return false, errors.New("upstream error event")
	}
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
			return true, nil
		}
	case "responses":
		if typ == "response.completed" || typ == "response.incomplete" {
			extractUsage(obj(obj(o["response"])["usage"]), p, u)
			return true, nil
		}
		if typ == "response.failed" {
			return false, errors.New("upstream response failed")
		}
	}
	return false, nil
}
func nativeStream(w http.ResponseWriter, r io.Reader, p string, u *Usage) error {
	done := false
	e := readSSE(r, func(f frame) error {
		terminal, err := meterFrame(f, p, u)
		if err != nil {
			return err
		}
		if _, e := io.WriteString(w, f.Raw); e != nil {
			return e
		}
		if e := http.NewResponseController(w).Flush(); e != nil {
			return e
		}
		done = done || terminal
		return nil
	})
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
	done := false
	err := readSSE(r, func(f frame) error {
		term, err := meterFrame(f, from, u)
		if err != nil {
			return err
		}
		em.usage = *u
		done = done || term
		if f.Data == "" || strings.TrimSpace(f.Data) == "[DONE]" {
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
				if str(d, "reasoning_content") != "" || str(d, "reasoning") != "" {
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
					em.stop = s
				}
			}
		case "messages":
			key := int(num(o, "index"))
			switch typ {
			case "content_block_start":
				b := obj(o["content_block"])
				switch str(b, "type") {
				case "text":
					if err = em.start(key, Block{Kind: "text"}); err != nil {
						return err
					}
					if t := str(b, "text"); t != "" {
						return em.delta(key, t)
					}
				case "tool_use":
					if err = em.start(key, Block{Kind: "tool", ID: str(b, "id"), Name: str(b, "name")}); err != nil {
						return err
					}
					if v := obj(b["input"]); len(v) > 0 {
						return em.delta(key, raw(v))
					}
				default:
					return errors.New("unmappable Anthropic content block")
				}
			case "content_block_delta":
				d := obj(o["delta"])
				switch str(d, "type") {
				case "text_delta":
					return em.delta(key, str(d, "text"))
				case "input_json_delta":
					return em.delta(key, str(d, "partial_json"))
				default:
					return errors.New("unmappable Anthropic delta")
				}
			case "content_block_stop":
				// Anthropic may omit input_json_delta for a zero-argument tool.
				if i, ok := em.byKey[key]; ok && em.blocks[i].Kind == "tool" && em.blocks[i].Arguments == "" {
					if err = em.delta(key, "{}"); err != nil {
						return err
					}
				}
				return em.close(key)
			case "message_delta":
				em.stop = str(obj(o["delta"]), "stop_reason")
			}
		case "responses":
			key := int(num(o, "output_index"))
			switch typ {
			case "response.output_item.added":
				b := obj(o["item"])
				switch str(b, "type") {
				case "message":
				case "function_call":
					return em.start(key, Block{Kind: "tool", ID: str(b, "call_id"), Name: str(b, "name")})
				default:
					return errors.New("unmappable Responses output item")
				}
			case "response.content_part.added":
				if num(o, "content_index") != 0 || str(obj(o["part"]), "type") != "output_text" {
					return errors.New("unmappable Responses content part")
				}
				return em.start(key, Block{Kind: "text"})
			case "response.output_text.delta", "response.function_call_arguments.delta":
				return em.delta(key, str(o, "delta"))
			case "response.output_item.done":
				return em.close(key)
			case "response.incomplete":
				em.stop = "length"
			case "response.output_text.annotation.added", "response.refusal.delta":
				return errors.New("unmappable Responses annotation/refusal")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !done {
		return io.ErrUnexpectedEOF
	}
	em.usage = *u
	return em.finish()
}
