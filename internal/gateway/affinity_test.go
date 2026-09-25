package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// affinityHarness：first/second 两个候选、priority 策略、开启亲和；failFirst 为真时 first 回 429
func affinityHarness(t *testing.T) (*harness, *atomic.Bool, func(headers map[string]string) string) {
	t.Helper()
	h := newHarness(t)
	var failFirst atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := Object{}
		json.NewDecoder(r.Body).Decode(&o)
		if str(o, "model") == "upstream-first" && failFirst.Load() {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			return
		}
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	h.configure(t, up.URL, modelFixture("first", "chat"), modelFixture("second", "chat"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Affinity: true, Strategy: "priority", Candidates: []Candidate{{"first", 10}, {"second", 10}}}}
	})
	key := h.createKey(t)
	call := func(headers map[string]string) string {
		t.Helper()
		r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions", strings.NewReader(raw(requestFixture("chat", "auto"))))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("x-claude-code-session-id", "cc-session")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		requireStatus(t, w, 200)
		return w.Header().Get("X-Prism-Model")
	}
	return h, &failFirst, call
}

func boundModels(t *testing.T, h *harness) []string {
	t.Helper()
	rows, err := h.s.DB.Query("SELECT model_id FROM sessions ORDER BY updated_at")
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, r.String("model_id"))
	}
	return out
}

// 子代理与主线程共用会话 ID，但按 agent-id 分开亲和，并在请求记录里标出角色
func TestSubagentGetsOwnAffinity(t *testing.T) {
	h, _, call := affinityHarness(t)
	call(nil)
	call(map[string]string{"x-claude-code-agent-id": "agent-1"})
	call(map[string]string{"x-claude-code-agent-id": "agent-1", "x-claude-code-request-class": "subagent", "x-claude-code-agent-type": "Explore"})
	if got := boundModels(t, h); len(got) != 2 {
		t.Fatalf("主线程与子代理应是两条会话记录，实得 %v", got)
	}
	rows, _ := h.s.DB.Query("SELECT agent_role FROM requests ORDER BY started_at")
	roles := []string{}
	for _, r := range rows {
		roles = append(roles, r.String("agent_role"))
	}
	if strings.Join(roles, ",") != ",subagent,subagent:Explore" {
		t.Fatalf("请求角色记录不对: %q", roles)
	}
}

// 原模型临时 429：切到备选成功后仍绑定原模型；原模型长时间冷却时才改绑
func TestFailoverKeepsPinUnlessLongCooldown(t *testing.T) {
	h, failFirst, call := affinityHarness(t)
	if m := call(nil); m != "first" {
		t.Fatalf("首个请求应落在 first，实得 %s", m)
	}
	failFirst.Store(true)
	if m := call(nil); m != "second" {
		t.Fatalf("first 429 后应切到 second，实得 %s", m)
	}
	if got := boundModels(t, h); len(got) != 1 || got[0] != "first" {
		t.Fatalf("临时故障不应改绑，实得 %v", got)
	}
	h.a.Engine.coolUntil("first", now()+int64(time.Hour/time.Millisecond))
	call(nil)
	if got := boundModels(t, h); len(got) != 1 || got[0] != "second" {
		t.Fatalf("原模型冷却超过 5 分钟应改绑到 second，实得 %v", got)
	}
}
