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

type streamBlock struct {
	Block
	Key       int
	Closed    bool
	ToolIndex int
}
type emitter struct {
	w            http.ResponseWriter
	p, model, id string
	seq          int
	blocks       []*streamBlock
	byKey        map[int]int
	started      bool
	usage        Usage
	stop         string
	toolCount    int
}

func newEmitter(w http.ResponseWriter, p, model, id string) *emitter {
	return &emitter{w: w, p: p, model: model, id: id, byKey: map[int]int{}, stop: "stop"}
}
func (e *emitter) send(event string, v Object) error {
	if e.p == "responses" {
		v["sequence_number"] = e.seq
		e.seq++
	}
	return writeEvent(e.w, event, v)
}
func (e *emitter) chat(delta Object, finish any) error {
	return e.send("", Object{"id": "chatcmpl_" + e.id, "object": "chat.completion.chunk", "created": now() / 1000, "model": e.model, "choices": []any{Object{"index": 0, "delta": delta, "finish_reason": finish}}})
}
func (e *emitter) begin() error {
	if e.started {
		return nil
	}
	e.started = true
	switch e.p {
	case "chat":
		return e.chat(Object{"role": "assistant", "content": ""}, nil)
	case "messages":
		return e.send("message_start", Object{"type": "message_start", "message": Object{"id": "msg_" + e.id, "type": "message", "role": "assistant", "model": e.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usageObject(e.usage, "messages")}})
	default:
		o := completionObject(Completion{}, "responses", e.model, e.id)
		o["status"] = "in_progress"
		o["usage"] = nil
		if err := e.send("response.created", Object{"type": "response.created", "response": o}); err != nil {
			return err
		}
		return e.send("response.in_progress", Object{"type": "response.in_progress", "response": o})
	}
}
func (e *emitter) item(i int, done bool) Object {
	b := e.blocks[i]
	status := "in_progress"
	if done {
		status = "completed"
	}
	id := fmt.Sprintf("item_%s_%d", e.id, i)
	if b.Kind == "tool" {
		args := ""
		if done {
			args = b.Arguments
		}
		return Object{"id": id, "type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": args, "status": status}
	}
	content := []any{}
	if done {
		content = append(content, Object{"type": "output_text", "text": b.Text, "annotations": []any{}, "logprobs": []any{}})
	}
	return Object{"id": id, "type": "message", "role": "assistant", "status": status, "content": content}
}
func (e *emitter) start(key int, b Block) error {
	if _, ok := e.byKey[key]; ok {
		return nil
	}
	if err := e.begin(); err != nil {
		return err
	}
	i := len(e.blocks)
	sb := &streamBlock{Block: b, Key: key}
	sb.Text = ""
	sb.Arguments = ""
	if b.Kind == "tool" {
		if b.ID == "" || b.Name == "" {
			return errors.New("tool stream lacks id/name")
		}
		sb.ToolIndex = e.toolCount
		e.toolCount++
	}
	e.byKey[key] = i
	e.blocks = append(e.blocks, sb)
	switch e.p {
	case "chat":
		if b.Kind == "tool" {
			return e.chat(Object{"tool_calls": []any{Object{"index": sb.ToolIndex, "id": b.ID, "type": "function", "function": Object{"name": b.Name, "arguments": ""}}}}, nil)
		}
	case "messages":
		block := Object{"type": "text", "text": ""}
		if b.Kind == "tool" {
			block = Object{"type": "tool_use", "id": b.ID, "name": b.Name, "input": Object{}}
		}
		return e.send("content_block_start", Object{"type": "content_block_start", "index": i, "content_block": block})
	default:
		if err := e.send("response.output_item.added", Object{"type": "response.output_item.added", "output_index": i, "item": e.item(i, false)}); err != nil {
			return err
		}
		if b.Kind == "text" {
			return e.send("response.content_part.added", Object{"type": "response.content_part.added", "item_id": e.item(i, false)["id"], "output_index": i, "content_index": 0, "part": Object{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})
		}
	}
	return nil
}
func (e *emitter) delta(key int, text string) error {
	i, ok := e.byKey[key]
	if !ok {
		return errors.New("delta before block start")
	}
	b := e.blocks[i]
	if b.Closed {
		return errors.New("delta after block stop")
	}
	if b.Kind == "text" {
		b.Text += text
	} else {
		b.Arguments += text
	}
	if len(b.Text)+len(b.Arguments) > 16<<20 {
		return errors.New("stream response exceeds memory limit")
	}
	switch e.p {
	case "chat":
		if b.Kind == "text" {
			return e.chat(Object{"content": text}, nil)
		}
		return e.chat(Object{"tool_calls": []any{Object{"index": b.ToolIndex, "function": Object{"arguments": text}}}}, nil)
	case "messages":
		d := Object{"type": "text_delta", "text": text}
		if b.Kind == "tool" {
			d = Object{"type": "input_json_delta", "partial_json": text}
		}
		return e.send("content_block_delta", Object{"type": "content_block_delta", "index": i, "delta": d})
	default:
		typ := "response.output_text.delta"
		o := Object{"item_id": e.item(i, false)["id"], "output_index": i, "delta": text}
		if b.Kind == "tool" {
			typ = "response.function_call_arguments.delta"
		} else {
			o["content_index"] = 0
			o["logprobs"] = []any{}
		}
		o["type"] = typ
		return e.send(typ, o)
	}
}
func (e *emitter) close(key int) error {
	i, ok := e.byKey[key]
	if !ok {
		return errors.New("stop before block start")
	}
	b := e.blocks[i]
	if b.Closed {
		return nil
	}
	if b.Kind == "tool" && !json.Valid([]byte(b.Arguments)) {
		return errors.New("incomplete tool JSON")
	}
	b.Closed = true
	switch e.p {
	case "messages":
		return e.send("content_block_stop", Object{"type": "content_block_stop", "index": i})
	case "responses":
		item := e.item(i, true)
		if b.Kind == "tool" {
			if er := e.send("response.function_call_arguments.done", Object{"type": "response.function_call_arguments.done", "item_id": item["id"], "output_index": i, "arguments": b.Arguments}); er != nil {
				return er
			}
		} else {
			if er := e.send("response.output_text.done", Object{"type": "response.output_text.done", "item_id": item["id"], "output_index": i, "content_index": 0, "text": b.Text, "logprobs": []any{}}); er != nil {
				return er
			}
			if er := e.send("response.content_part.done", Object{"type": "response.content_part.done", "item_id": item["id"], "output_index": i, "content_index": 0, "part": Object{"type": "output_text", "text": b.Text, "annotations": []any{}, "logprobs": []any{}}}); er != nil {
				return er
			}
		}
		return e.send("response.output_item.done", Object{"type": "response.output_item.done", "output_index": i, "item": item})
	}
	return nil
}
func (e *emitter) finish() error {
	if err := e.begin(); err != nil {
		return err
	}
	c := Completion{Usage: e.usage, Stop: e.stop}
	for _, b := range e.blocks {
		if err := e.close(b.Key); err != nil {
			return err
		}
		c.Blocks = append(c.Blocks, b.Block)
	}
	switch e.p {
	case "chat":
		if err := e.chat(Object{}, stopFor(c, "chat")); err != nil {
			return err
		}
		if err := e.send("", Object{"id": "chatcmpl_" + e.id, "object": "chat.completion.chunk", "created": now() / 1000, "model": e.model, "choices": []any{}, "usage": usageObject(c.Usage, "chat")}); err != nil {
			return err
		}
		_, err := io.WriteString(e.w, "data: [DONE]\n\n")
		if err != nil {
			return err
		}
		return http.NewResponseController(e.w).Flush()
	case "messages":
		if err := e.send("message_delta", Object{"type": "message_delta", "delta": Object{"stop_reason": stopFor(c, "messages"), "stop_sequence": nil}, "usage": usageObject(c.Usage, "messages")}); err != nil {
			return err
		}
		return e.send("message_stop", Object{"type": "message_stop"})
	default:
		r := completionObject(c, "responses", e.model, e.id)
		typ := "response.completed"
		if str(r, "status") == "incomplete" {
			typ = "response.incomplete"
		}
		return e.send(typ, Object{"type": typ, "response": r})
	}
}
func convertedStream(w http.ResponseWriter, r io.Reader, from, to, model, id string, u *Usage) error {
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
				if str(d, "reasoning_content") != "" || str(d, "reasoning") != "" || str(d, "refusal") != "" {
					return errors.New("unmappable reasoning/refusal in stream")
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
