package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// historyUpstream 记录每个协议端点最近收到的请求体，并按端点返回最小的合法响应。
func historyUpstream(t *testing.T) (*httptest.Server, func(protocol string) string) {
	t.Helper()
	var mu sync.Mutex
	got := map[string]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = string(b)
		mu.Unlock()
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: "+raw(Object{"id": "c", "choices": []any{Object{"index": 0, "delta": Object{"role": "assistant", "content": "OK"}}}})+"\n\n"+
				"data: "+raw(Object{"id": "c", "choices": []any{Object{"index": 0, "delta": Object{}, "finish_reason": "stop"}}})+"\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == pathFor("messages") {
			io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	return up, func(protocol string) string {
		mu.Lock()
		defer mu.Unlock()
		return got[pathFor(protocol)]
	}
}

// reasoningHistoryRequest 是带推理历史的 Messages 请求，形态与 Claude Code 在
// 原生推理候选服务过几轮之后发出的一致：带签名的 thinking、工具往返、只含推理的一轮。
func reasoningHistoryRequest(model string) Object {
	return Object{"model": model, "max_tokens": 256,
		"tools": []any{Object{"name": "read", "description": "read a file", "input_schema": Object{"type": "object", "properties": Object{"path": Object{"type": "string"}}}}},
		"messages": []any{
			Object{"role": "user", "content": "read the file"},
			Object{"role": "assistant", "content": []any{
				Object{"type": "thinking", "thinking": "private plan", "signature": "sig-a"},
				Object{"type": "text", "text": "reading now"},
				Object{"type": "tool_use", "id": "call_1", "name": "read", "input": Object{"path": "a.go"}},
			}},
			Object{"role": "user", "content": []any{Object{"type": "tool_result", "tool_use_id": "call_1", "content": "package a"}}},
			Object{"role": "assistant", "content": []any{Object{"type": "redacted_thinking", "data": "opaque-b"}}},
			Object{"role": "user", "content": "continue"},
		}}
}

func parse(t *testing.T, body string) Object {
	t.Helper()
	o := Object{}
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatalf("上游没有收到合法 JSON: %q", body)
	}
	return o
}

func dropModel(id, protocol string, drop bool) Model {
	m := modelFixture(id, protocol)
	m.DropReasoning = drop
	return m
}

func requireNoReasoning(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{"private plan", "sig-a", "opaque-b", "thinking", "reasoning"} {
		if strings.Contains(body, secret) {
			t.Fatalf("推理内容 %q 被发给了跨协议上游: %s", secret, body)
		}
	}
}

func TestDropReasoningCoversMessagesHistory(t *testing.T) {
	h := newHarness(t)
	up, sent := historyUpstream(t)
	h.configure(t, up.URL, dropModel("b", "chat", true))
	w := h.generate(t, "messages", reasoningHistoryRequest("b"))
	requireStatus(t, w, 200)
	if got := w.Header().Get("X-Prism-Dropped"); got != "reasoning" {
		t.Fatalf("丢弃历史推理必须披露，X-Prism-Dropped=%q", got)
	}
	body := sent("chat")
	requireNoReasoning(t, body)
	for _, kept := range []string{"read the file", "reading now", "call_1", "a.go", "package a", "continue"} {
		if !strings.Contains(body, kept) {
			t.Fatalf("对话内容 %q 不应丢失: %s", kept, body)
		}
	}
	// 只含推理块的那一轮没有可见内容，不应变成一条空的 assistant 消息
	roles := []string{}
	for _, m := range arr(parse(t, body)["messages"]) {
		roles = append(roles, str(obj(m), "role"))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,user" {
		t.Fatalf("消息序列不对: %v", roles)
	}
}

// Claude Code 走的是流式：剥离发生在选候选阶段，流开始前就必须已经披露。
func TestDropReasoningCoversStreamingHistory(t *testing.T) {
	h := newHarness(t)
	up, sent := historyUpstream(t)
	h.configure(t, up.URL, dropModel("b", "chat", true))
	body := reasoningHistoryRequest("b")
	body["stream"] = true
	w := h.generate(t, "messages", body)
	requireStatus(t, w, 200)
	if w.Header().Get("X-Prism-Dropped") != "reasoning" {
		t.Fatal("丢弃历史推理必须披露")
	}
	requireNoReasoning(t, sent("chat"))
	if out := w.Body.String(); !strings.Contains(out, "message_stop") || !strings.Contains(out, "OK") {
		t.Fatalf("流未正常结束: %s", out)
	}
}

// Claude Code 实际会同时带上思考偏好与 clear_thinking 上下文编辑。历史推理去掉之后，
// 这条编辑已无对象可清，属于可披露的空操作，不能反过来把候选淘汰掉。
func TestDropReasoningHistoryWithClaudeCodeControls(t *testing.T) {
	h := newHarness(t)
	up, sent := historyUpstream(t)
	h.configure(t, up.URL, dropModel("b", "chat", true))
	body := reasoningHistoryRequest("b")
	body["thinking"] = Object{"type": "adaptive"}
	body["context_management"] = Object{"edits": []any{Object{"type": "clear_thinking_20251015", "keep": Object{"type": "thinking_turns", "value": 2}}}}
	w := h.generate(t, "messages", body)
	requireStatus(t, w, 200)
	requireNoReasoning(t, sent("chat"))
	if w.Header().Get("X-Prism-Dropped") != "reasoning" {
		t.Fatal("丢弃历史推理必须披露")
	}
	ignored := w.Header().Get("X-Prism-Ignored")
	if !strings.Contains(ignored, "thinking") || !strings.Contains(ignored, "context_management.edits.clear_thinking_20251015") {
		t.Fatalf("被忽略的请求字段必须披露: %q", ignored)
	}
}

func TestHistoryReasoningRejectedWithoutOptIn(t *testing.T) {
	h := newHarness(t)
	up, sent := historyUpstream(t)
	h.configure(t, up.URL, dropModel("b", "chat", false))
	w := h.generate(t, "messages", reasoningHistoryRequest("b"))
	requireStatus(t, w, 400)
	if !strings.Contains(w.Body.String(), "跨协议不支持内容类型 thinking") {
		t.Fatalf("未开启开关时应说明拒绝原因: %s", w.Body)
	}
	if w.Header().Get("X-Prism-Dropped") != "" || sent("chat") != "" {
		t.Fatal("未开启开关时不得丢弃，也不得把请求发给上游")
	}
}

// 会话在原生候选与跨协议候选之间来回切换：跨协议候选拿到去掉推理的副本，
// 原生候选每次都必须拿到客户端发出的原始历史，签名不能被动到。
func TestHistoryReasoningSurvivesSwitchingBack(t *testing.T) {
	h := newHarness(t)
	up, sent := historyUpstream(t)
	h.configure(t, up.URL, dropModel("b", "chat", true), dropModel("a", "messages", false))
	pause := func(b, a bool) {
		h.change(t, func(c *Config) {
			c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority",
				Candidates: []Candidate{{ModelID: "b", Weight: 10, Disabled: b}, {ModelID: "a", Weight: 10, Disabled: a}}}}
		})
	}
	want := raw(reasoningHistoryRequest("auto")["messages"])
	native := func(step string) {
		t.Helper()
		w := h.generate(t, "messages", reasoningHistoryRequest("auto"))
		requireStatus(t, w, 200)
		if got := raw(parse(t, sent("messages"))["messages"]); got != want {
			t.Fatalf("%s: 原生候选收到的历史被改动\n got=%s\nwant=%s", step, got, want)
		}
		if w.Header().Get("X-Prism-Dropped") != "" {
			t.Fatalf("%s: 原生透传没有丢弃，不应标注", step)
		}
	}
	pause(true, false)
	native("首次原生")
	pause(false, true)
	w := h.generate(t, "messages", reasoningHistoryRequest("auto"))
	requireStatus(t, w, 200)
	requireNoReasoning(t, sent("chat"))
	if w.Header().Get("X-Prism-Dropped") != "reasoning" {
		t.Fatal("切到跨协议候选时必须披露丢弃")
	}
	pause(true, false)
	native("切回原生")
}

// 同一次请求里跨协议候选排在前面先被评估，剥离只能作用于它自己的副本。
func TestDropHistoryReasoningDoesNotMutateRequest(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", dropModel("b", "chat", true), dropModel("a", "messages", false))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority",
			Candidates: []Candidate{{ModelID: "b", Weight: 10}, {ModelID: "a", Weight: 10}}}}
	})
	o := reasoningHistoryRequest("auto")
	want := raw(o["messages"])
	selections, _, err := h.a.Engine.selections(h.s.Config(), o, "messages", "")
	if err != nil || len(selections) != 2 {
		t.Fatalf("两个候选都应合格: %d %v", len(selections), err)
	}
	if got := raw(o["messages"]); got != want {
		t.Fatalf("原请求被改动: %s", got)
	}
	for _, s := range selections {
		switch s.Model.ID {
		case "a":
			if got := raw(s.Body["messages"]); got != want || s.DroppedReasoning {
				t.Fatalf("原生候选的请求体被改动: dropped=%t %s", s.DroppedReasoning, got)
			}
		case "b":
			requireNoReasoning(t, raw(s.Body))
			if !s.DroppedReasoning {
				t.Fatal("跨协议候选应记录发生了丢弃")
			}
		}
	}
}

func TestDropReasoningCoversChatAndResponsesHistory(t *testing.T) {
	cases := []struct {
		name, from, to string
		body           Object
		roles          string
	}{
		{"chat", "chat", "messages", Object{"model": "b", "max_tokens": 256, "messages": []any{
			Object{"role": "user", "content": "question"},
			Object{"role": "assistant", "content": "first answer", "reasoning_content": "private plan", "reasoning_details": []any{Object{"text": "sig-a"}}},
			Object{"role": "user", "content": "again"},
			Object{"role": "assistant", "content": nil, "reasoning": "opaque-b"},
			Object{"role": "user", "content": "continue"},
		}}, "user,assistant,user,user"},
		{"responses", "responses", "chat", Object{"model": "b", "max_output_tokens": 256, "input": []any{
			Object{"type": "message", "role": "user", "content": "question"},
			Object{"type": "reasoning", "id": "rs_1", "summary": []any{Object{"type": "summary_text", "text": "private plan"}}, "encrypted_content": "opaque-b"},
			Object{"type": "message", "role": "assistant", "content": []any{Object{"type": "output_text", "text": "first answer"}}},
			Object{"type": "message", "role": "user", "content": "continue"},
		}}, "user,assistant,user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			up, sent := historyUpstream(t)
			h.configure(t, up.URL, dropModel("b", tc.to, true))
			w := h.generate(t, tc.from, tc.body)
			requireStatus(t, w, 200)
			if w.Header().Get("X-Prism-Dropped") != "reasoning" {
				t.Fatal("丢弃历史推理必须披露")
			}
			body := sent(tc.to)
			requireNoReasoning(t, body)
			if !strings.Contains(body, "first answer") || !strings.Contains(body, "continue") {
				t.Fatalf("对话内容不应丢失: %s", body)
			}
			roles := []string{}
			for _, m := range arr(parse(t, body)["messages"]) {
				roles = append(roles, str(obj(m), "role"))
			}
			if strings.Join(roles, ",") != tc.roles {
				t.Fatalf("消息序列不对: %v", roles)
			}
		})
	}
}
