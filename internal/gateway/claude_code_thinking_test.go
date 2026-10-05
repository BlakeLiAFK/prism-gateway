package gateway

import (
	"strings"
	"testing"
)

// claudeCodeRequest 是 Claude Code 2.1.289 主对话请求的真实形态（本地监听抓包）：
// thinking 带 display，另有 output_config.effort、metadata 和 keep:"all" 的 clear_thinking。
func claudeCodeRequest(model string, thinking Object) Object {
	return Object{
		"model": model, "max_tokens": 64000, "stream": false,
		"system":   []any{Object{"type": "text", "text": "You are Claude Code"}},
		"messages": []any{Object{"role": "user", "content": []any{Object{"type": "text", "text": "hi"}}}},
		"thinking": thinking, "output_config": Object{"effort": "high"},
		"metadata":           Object{"user_id": "u"},
		"context_management": Object{"edits": []any{Object{"type": "clear_thinking_20251015", "keep": "all"}}},
	}
}

// 管理员已开启 drop_reasoning 时，Claude Code 的展示偏好 display 不能让候选被淘汰；
// 2026-10-05 线上 auto 对 Claude Code 全部 400 就是这个缺口。
func TestClaudeCodeThinkingDisplayDroppedWithOptIn(t *testing.T) {
	for _, display := range []string{"omitted", "summarized"} {
		h := newHarness(t)
		up, sent := historyUpstream(t)
		h.configure(t, up.URL, dropModel("b", "chat", true))
		w := h.generate(t, "messages", claudeCodeRequest("b", Object{"type": "adaptive", "display": display}))
		requireStatus(t, w, 200)
		if strings.Contains(sent("chat"), "thinking") || strings.Contains(sent("chat"), "display") {
			t.Fatalf("thinking 不应发给跨协议上游: %s", sent("chat"))
		}
		if !strings.Contains(w.Header().Get("X-Prism-Ignored"), "thinking") {
			t.Fatalf("被忽略的 thinking 必须披露: %q", w.Header().Get("X-Prism-Ignored"))
		}
	}
}

// 开关的授权范围不扩大：禁用思考、未知类型、未知额外控制、非法 display 仍然明确拒绝。
func TestClaudeCodeThinkingStillRejectedWhenNotDroppable(t *testing.T) {
	cases := map[string]Object{
		"disabled":   {"type": "disabled"},
		"未知类型":       {"type": "future"},
		"未知额外控制":     {"type": "adaptive", "display": "omitted", "effort": "max"},
		"display 非法": {"type": "adaptive", "display": 7},
	}
	for name, th := range cases {
		h := newHarness(t)
		up, sent := historyUpstream(t)
		h.configure(t, up.URL, dropModel("b", "chat", true))
		w := h.generate(t, "messages", claudeCodeRequest("b", th))
		requireStatus(t, w, 400)
		if sent("chat") != "" {
			t.Fatalf("%s: 被拒绝的请求不得发给上游", name)
		}
	}
}

// 未开启 drop_reasoning 时带 display 的 thinking 同样被拒绝，不因放行 display 而悄悄丢弃。
func TestClaudeCodeThinkingRejectedWithoutOptIn(t *testing.T) {
	h := newHarness(t)
	up, sent := historyUpstream(t)
	h.configure(t, up.URL, dropModel("b", "chat", false))
	w := h.generate(t, "messages", claudeCodeRequest("b", Object{"type": "adaptive", "display": "omitted"}))
	requireStatus(t, w, 400)
	if sent("chat") != "" {
		t.Fatal("未开启开关时不得发给上游")
	}
}
