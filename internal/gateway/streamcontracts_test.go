package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertedStreamReasoningContract(t *testing.T) {
	tests := []struct {
		name, from, to, source, wantText, wantEnd string
	}{
		{
			name: "chat reasoning details",
			from: "chat", to: "messages", wantText: "answer", wantEnd: "message_stop",
			source: `data: {"choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"secret"}]}}]}

data: {"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}

data: [DONE]

`,
		},
		{
			name: "messages thinking deltas",
			from: "messages", to: "chat", wantText: "answer", wantEnd: "[DONE]",
			source: `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_stop
data: {"type":"message_stop"}

`,
		},
		{
			name: "responses reasoning item",
			from: "responses", to: "chat", wantText: "answer", wantEnd: "[DONE]",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"secret"}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"secret"}

event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","output_index":0,"delta":"secret"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"secret"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":1,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"answer"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}

event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}

`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, drop := range []bool{false, true} {
				w := httptest.NewRecorder()
				err := convertedStream(w, strings.NewReader(tc.source), tc.from, tc.to, "m", "r", &Usage{}, drop)
				if !drop {
					if err == nil {
						t.Fatal("未开启丢弃时接受了推理流")
					}
					continue
				}
				if err != nil {
					t.Fatalf("开启丢弃后转换失败: %v\n%s", err, w.Body.String())
				}
				body := w.Body.String()
				if !strings.Contains(body, tc.wantText) || !strings.Contains(body, tc.wantEnd) || strings.Contains(body, "secret") {
					t.Fatalf("推理剥离结果错误: %s", body)
				}
			}
		})
	}
}

func TestConvertedReasoningStreamThroughHandler(t *testing.T) {
	tests := []struct {
		name, from, to, source, want string
	}{
		{
			name: "messages tool", from: "messages", to: "chat", want: "tool_1",
			source: `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"secret"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_1","name":"clock","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_stop
data: {"type":"message_stop"}

			`,
		},
		{
			name: "chat text", from: "chat", to: "messages", want: "answer",
			source: `data: {"choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"secret"}]}}]}

data: {"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}

data: [DONE]

`,
		},
		{
			name: "responses text", from: "responses", to: "chat", want: "answer",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"secret"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"secret"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":1,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"answer"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant"}}

event: response.completed
data: {"type":"response.completed","response":{}}

`,
		},
	}
	for _, tc := range tests {
		for _, drop := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "reject", true: "drop"}[drop], func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tc.source)
				}))
				defer up.Close()
				h := newHarness(t)
				m := modelFixture("m", tc.from)
				m.DropReasoning = drop
				h.configure(t, up.URL, m)
				body := requestFixture(tc.to, "m")
				body["stream"] = true
				w := h.generate(t, tc.to, body)
				if drop {
					if w.Header().Get("X-Prism-Dropped") != "reasoning" || !strings.Contains(w.Body.String(), tc.want) || strings.Contains(w.Body.String(), "secret") {
						t.Fatalf("handler 未完成转换流: %s", w.Body.String())
					}
					return
				}
				if w.Header().Get("X-Prism-Dropped") != "" || !strings.Contains(w.Body.String(), "error") {
					t.Fatalf("handler 未拒绝推理流: %s", w.Body.String())
				}
			})
		}
	}
}

type terminalThenErrorReader struct {
	data []byte
	done bool
}

func (r *terminalThenErrorReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if !r.done {
		r.done = true
		return 0, errors.New("终止事件后的读取错误")
	}
	return 0, io.EOF
}

func TestStreamStopsReadingAtTerminalEvent(t *testing.T) {
	tests := []struct{ protocol, source string }{
		{"chat", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"},
		{"messages", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"responses", "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\nevent: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"ok\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.protocol, func(t *testing.T) {
			r := &terminalThenErrorReader{data: []byte(tc.source)}
			if err := nativeStream(httptest.NewRecorder(), r, tc.protocol, &Usage{}); err != nil {
				t.Fatalf("终止后仍读取上游: %v", err)
			}
		})
	}
}

func TestNativeStreamAcceptsVendorOutputKinds(t *testing.T) {
	tests := []struct{ protocol, source string }{
		{"chat", "data: {\"choices\":[{\"index\":0,\"delta\":{\"audio\":{\"id\":\"audio_1\"}}}]}\n\ndata: [DONE]\n\n"},
		{"messages", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"reason\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"responses", "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"image_generation_call\",\"id\":\"ig_1\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.protocol, func(t *testing.T) {
			w := httptest.NewRecorder()
			if err := nativeStream(w, strings.NewReader(tc.source), tc.protocol, &Usage{}); err != nil {
				t.Fatalf("原生输出类型被空流检查拦截: %v\n%s", err, w.Body.String())
			}
		})
	}
}

func TestNativeStreamRejectsTruncatedReasoningOnly(t *testing.T) {
	source := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"reason"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}

event: message_stop
data: {"type":"message_stop"}

`
	w := httptest.NewRecorder()
	if err := nativeStream(w, strings.NewReader(source), "messages", &Usage{}); err == nil || strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("截断且只有推理的流被当作成功: err=%v body=%s", err, w.Body.String())
	}
}

func TestConvertedStreamRejectsUnmappableMediaOutput(t *testing.T) {
	source := "data: {\"choices\":[{\"index\":0,\"delta\":{\"audio\":{\"id\":\"audio_1\"}}}]}\n\ndata: [DONE]\n\n"
	if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "chat", "messages", "m", "r", &Usage{}, false); err == nil {
		t.Fatal("跨协议静默丢弃了媒体输出")
	}
}

func TestConvertedStreamRejectsUnmappedOutputFields(t *testing.T) {
	tests := []struct {
		name, from, source string
	}{
		{"chat custom tool", "chat", "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"custom\",\"function\":{\"name\":\"x\"}}]}}]}\n\n"},
		{"chat content array", "chat", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":[{\"type\":\"text\",\"text\":\"x\"}]}}]}\n\n"},
		{"chat annotations", "chat", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\",\"annotations\":[{\"type\":\"url_citation\"}]}}]}\n\n"},
		{"messages citations", "messages", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"x\",\"citations\":[{\"type\":\"char_location\"}]}}\n\n"},
		{"responses annotations", "responses", "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[{\"type\":\"url_citation\"}]}}\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := convertedStream(httptest.NewRecorder(), strings.NewReader(tc.source), tc.from, "chat", "m", "r", &Usage{}, false); err == nil {
				t.Fatal("跨协议静默丢弃了输出字段")
			}
		})
	}
}

func TestStreamRejectsEmptyAndIncompleteOutput(t *testing.T) {
	t.Run("empty native", func(t *testing.T) {
		w := httptest.NewRecorder()
		if err := nativeStream(w, strings.NewReader("data: [DONE]\n\n"), "chat", &Usage{}); err == nil || strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatalf("空流被当作成功: err=%v body=%s", err, w.Body.String())
		}
	})
	t.Run("incomplete tool", func(t *testing.T) {
		source := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\ndata: [DONE]\n\n"
		w := httptest.NewRecorder()
		err := convertedStream(w, strings.NewReader(source), "chat", "messages", "m", "r", &Usage{}, false)
		if err == nil || strings.Contains(w.Body.String(), "message_stop") {
			t.Fatalf("截断工具参数被当作成功: err=%v body=%s", err, w.Body.String())
		}
	})
	t.Run("tool arguments must be object", func(t *testing.T) {
		for _, arguments := range []string{"null", "[]", `"text"`} {
			source := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":" + raw(arguments) + "}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
			if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "chat", "messages", "m", "r", &Usage{}, false); err == nil {
				t.Fatalf("非对象工具参数被接受: %s", arguments)
			}
		}
	})
	t.Run("truncated tool is not completed", func(t *testing.T) {
		source := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
		w := httptest.NewRecorder()
		if err := convertedStream(w, strings.NewReader(source), "chat", "messages", "m", "r", &Usage{}, false); err == nil || strings.Contains(w.Body.String(), "content_block_stop") || strings.Contains(w.Body.String(), "message_stop") {
			t.Fatalf("截断工具被标记完成: err=%v body=%s", err, w.Body.String())
		}
	})
	t.Run("responses final arguments", func(t *testing.T) {
		source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read","arguments":""}}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"a.go\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"path\":\"a.go\"}"}}

event: response.completed
data: {"type":"response.completed","response":{}}

`
		w := httptest.NewRecorder()
		if err := convertedStream(w, strings.NewReader(source), "responses", "messages", "m", "r", &Usage{}, false); err != nil {
			t.Fatalf("完整最终参数被拒绝: %v\n%s", err, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `\"path\":\"a.go\"`) || !strings.Contains(w.Body.String(), "message_stop") {
			t.Fatalf("最终参数未输出: %s", w.Body.String())
		}
	})
	t.Run("responses mismatched final arguments", func(t *testing.T) {
		source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"a.go\"}"}

`
		if err := convertedStream(httptest.NewRecorder(), strings.NewReader(source), "responses", "messages", "m", "r", &Usage{}, false); err == nil {
			t.Fatal("不一致的最终工具参数被接受")
		}
	})
}

func TestStreamFailureThroughHandler(t *testing.T) {
	tests := []struct {
		name, source string
	}{
		{"upstream error", "data: {\"error\":{\"message\":\"failed\"}}\n\n"},
		{"unexpected EOF", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"},
		{"empty output", "data: [DONE]\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.source)
			}))
			defer up.Close()
			h := newHarness(t)
			h.configure(t, up.URL, modelFixture("m", "chat"))
			body := requestFixture("chat", "m")
			body["stream"] = true
			w := h.generate(t, "chat", body)
			if !strings.Contains(w.Body.String(), "upstream_stream_error") {
				t.Fatalf("流失败未发送错误帧: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "[DONE]") {
				t.Fatalf("失败流发送了成功终止标记: %s", w.Body.String())
			}
		})
	}
}
