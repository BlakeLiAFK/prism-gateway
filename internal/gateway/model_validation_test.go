package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func limitJSONFixture(protocol string) string {
	switch protocol {
	case "chat":
		return `{"id":"chat-limit","object":"chat.completion","model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}`
	case "messages":
		return `{"id":"msg-limit","type":"message","role":"assistant","model":"upstream","content":[],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":11}}`
	default:
		return `{"id":"resp-limit","object":"response","model":"upstream","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":7,"output_tokens":11,"total_tokens":18}}`
	}
}

func limitStreamFixture(protocol string) string {
	switch protocol {
	case "chat":
		return "data: {\"id\":\"chat-limit\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":11,\"total_tokens\":18}}\n\ndata: [DONE]\n\n"
	case "messages":
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-limit\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":11}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	default:
		return "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-limit\",\"object\":\"response\",\"model\":\"upstream\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":11,\"total_tokens\":18}}}\n\n"
	}
}

func validationBudgets(from, to string) []int {
	if from == "chat" && to == "messages" {
		return []int{1, 64}
	}
	return []int{1}
}

func validationRequest(protocol, model string, budget int, stream bool) Object {
	body := requestFixture(protocol, model)
	if protocol == "responses" {
		body["max_output_tokens"] = budget
	} else {
		body["max_tokens"] = budget
	}
	if protocol == "messages" {
		body["messages"] = []any{Object{"role": "user", "content": []any{Object{
			"type": "text", "text": "Hi", "cache_control": Object{"type": "ephemeral"},
		}}}}
	}
	if stream {
		body["stream"] = true
	}
	return body
}

func assertForwardedBudget(t *testing.T, protocol string, budget int, seen <-chan Object) {
	t.Helper()
	var body Object
	select {
	case body = <-seen:
	default:
		t.Fatal("请求未到达上游")
	}
	field := "max_tokens"
	if protocol == "responses" {
		field = "max_output_tokens"
	}
	if got := int(num(body, field)); got != budget {
		t.Fatalf("输出预算被改写: %s=%d want=%d body=%v", field, got, budget, body)
	}
}

func assertLimitJSON(t *testing.T, protocol string, w *httptest.ResponseRecorder) {
	t.Helper()
	requireStatus(t, w, http.StatusOK)
	o := Object{}
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, w.Body.String())
	}
	var usage Object
	switch protocol {
	case "chat":
		choices := arr(o["choices"])
		if len(choices) != 1 || str(obj(choices[0]), "finish_reason") != "length" {
			t.Fatalf("Chat 未保留 length: %s", w.Body.String())
		}
		usage = obj(o["usage"])
		if num(usage, "prompt_tokens") != 7 || num(usage, "completion_tokens") != 11 {
			t.Fatalf("Chat 用量丢失: %v", usage)
		}
	case "messages":
		if str(o, "stop_reason") != "max_tokens" {
			t.Fatalf("Messages 未保留 max_tokens: %s", w.Body.String())
		}
		usage = obj(o["usage"])
		if num(usage, "input_tokens") != 7 || num(usage, "output_tokens") != 11 {
			t.Fatalf("Messages 用量丢失: %v", usage)
		}
	default:
		if str(o, "status") != "incomplete" || str(obj(o["incomplete_details"]), "reason") != "max_output_tokens" {
			t.Fatalf("Responses 未保留输出上限终止: %s", w.Body.String())
		}
		usage = obj(o["usage"])
		if num(usage, "input_tokens") != 7 || num(usage, "output_tokens") != 11 {
			t.Fatalf("Responses 用量丢失: %v", usage)
		}
	}
}

func assertLimitStream(t *testing.T, protocol string, w *httptest.ResponseRecorder) {
	t.Helper()
	requireStatus(t, w, http.StatusOK)
	body := w.Body.String()
	if strings.Contains(body, "upstream_stream_error") || strings.Contains(body, "event: error") {
		t.Fatalf("合法输出上限流被改成错误帧: %s", body)
	}
	switch protocol {
	case "chat":
		if !strings.Contains(body, `"finish_reason":"length"`) || !strings.Contains(body, `"prompt_tokens":7`) || !strings.Contains(body, `"completion_tokens":11`) || !strings.Contains(body, "data: [DONE]") {
			t.Fatalf("Chat 流未保留终止或用量: %s", body)
		}
	case "messages":
		if !strings.Contains(body, `"stop_reason":"max_tokens"`) || !strings.Contains(body, `"input_tokens":7`) || !strings.Contains(body, `"output_tokens":11`) || !strings.Contains(body, "event: message_stop") {
			t.Fatalf("Messages 流未保留终止或用量: %s", body)
		}
	default:
		if !strings.Contains(body, `"status":"incomplete"`) || !strings.Contains(body, `"reason":"max_output_tokens"`) || !strings.Contains(body, `"input_tokens":7`) || !strings.Contains(body, `"output_tokens":11`) || !strings.Contains(body, "event: response.incomplete") {
			t.Fatalf("Responses 流未保留终止或用量: %s", body)
		}
	}
}

func TestModelValidationAcceptsEmptyTokenLimitResponse(t *testing.T) {
	protocols := []string{"chat", "messages", "responses"}
	for _, from := range protocols {
		for _, to := range protocols {
			for _, budget := range validationBudgets(from, to) {
				t.Run(from+"_to_"+to+"_"+strconv.Itoa(budget), func(t *testing.T) {
					seen := make(chan Object, 1)
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body := Object{}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Errorf("解析上游请求失败: %v", err)
						}
						seen <- body
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, limitJSONFixture(from))
					}))
					defer up.Close()
					h := newHarness(t)
					h.configure(t, up.URL, modelFixture("m", from))
					w := h.generate(t, to, validationRequest(to, "m", budget, false))
					assertForwardedBudget(t, from, budget, seen)
					assertLimitJSON(t, to, w)
				})
			}
		}
	}
}

func TestModelValidationAcceptsEmptyTokenLimitStream(t *testing.T) {
	protocols := []string{"chat", "messages", "responses"}
	for _, from := range protocols {
		for _, to := range protocols {
			for _, budget := range validationBudgets(from, to) {
				t.Run(from+"_to_"+to+"_"+strconv.Itoa(budget), func(t *testing.T) {
					seen := make(chan Object, 1)
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body := Object{}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Errorf("解析上游请求失败: %v", err)
						}
						seen <- body
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, limitStreamFixture(from))
					}))
					defer up.Close()
					h := newHarness(t)
					h.configure(t, up.URL, modelFixture("m", from))
					w := h.generate(t, to, validationRequest(to, "m", budget, true))
					assertForwardedBudget(t, from, budget, seen)
					assertLimitStream(t, to, w)
				})
			}
		}
	}
}

func TestModelValidationStillRejectsUnexplainedEmptyOutput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"empty","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":0}}`)
			}))
			defer up.Close()
			h := newHarness(t)
			h.configure(t, up.URL, modelFixture("m", "chat"))
			w := h.generate(t, "chat", validationRequest("chat", "m", 1, stream))
			if stream {
				if !strings.Contains(w.Body.String(), "upstream_stream_error") {
					t.Fatalf("普通空流未失败: %s", w.Body.String())
				}
			} else if w.Code != http.StatusBadGateway {
				t.Fatalf("普通空响应未失败: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestModelValidationRejectsMalformedLimitResponse(t *testing.T) {
	tests := []struct {
		name, protocol, response string
	}{
		{"chat missing message", "chat", `{"choices":[{"finish_reason":"length"}]}`},
		{"messages invalid content", "messages", `{"type":"message","content":{},"stop_reason":"max_tokens"}`},
		{"responses content filter", "responses", `{"object":"response","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer up.Close()
			h := newHarness(t)
			h.configure(t, up.URL, modelFixture("m", tc.protocol))
			w := h.generate(t, tc.protocol, validationRequest(tc.protocol, "m", 1, false))
			if w.Code != http.StatusBadGateway {
				t.Fatalf("畸形输出上限响应被接受: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestModelValidationDropsReasoningOnlyByOptIn(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, drop := range []bool{false, true} {
			t.Run(map[bool]string{false: "json/", true: "stream/"}[stream]+map[bool]string{false: "reject", true: "drop"}[drop], func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"secret\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":11}}\n\ndata: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"reasoning-limit","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning":"secret"},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":11}}`)
				}))
				defer up.Close()
				h := newHarness(t)
				m := modelFixture("m", "chat")
				m.DropReasoning = drop
				h.configure(t, up.URL, m)
				w := h.generate(t, "messages", validationRequest("messages", "m", 1, stream))
				if !drop {
					if stream && !strings.Contains(w.Body.String(), "event: error") {
						t.Fatalf("未授权时丢弃了推理流: %s", w.Body.String())
					}
					if !stream && w.Code != http.StatusBadGateway {
						t.Fatalf("未授权时丢弃了推理: status=%d body=%s", w.Code, w.Body.String())
					}
					return
				}
				if stream {
					assertLimitStream(t, "messages", w)
				} else {
					assertLimitJSON(t, "messages", w)
				}
				if w.Header().Get("X-Prism-Dropped") != "reasoning" || strings.Contains(w.Body.String(), "secret") {
					t.Fatalf("显式丢弃未正确标注: headers=%v body=%s", w.Header(), w.Body.String())
				}
			})
		}
	}
}
