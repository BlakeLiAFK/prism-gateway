// 跨协议流式转换的事件发射器。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

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
