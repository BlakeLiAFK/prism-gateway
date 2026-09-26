package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 上游列表里找不到的已启用模型被标出；列表拉取失败时沿用上次结论
func TestUpstreamMissingModels(t *testing.T) {
	var broken atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, `{"data":[{"id":"upstream-a"}]}`)
	}))
	defer up.Close()
	h := newHarness(t)
	h.configure(t, up.URL, modelFixture("a", "chat"), modelFixture("b", "chat"))
	ids := func() []string {
		out := []string{}
		for _, v := range h.a.missingList() {
			out = append(out, str(obj(v), "id"))
		}
		return out
	}
	h.a.checkUpstreamModels(context.Background())
	if got := ids(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("应只标出 b: %v", got)
	}
	broken.Store(true)
	h.a.checkUpstreamModels(context.Background())
	if got := ids(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("拉取失败应沿用上次结论: %v", got)
	}
	_, out := h.rpc(t, "dashboard.get", Object{"range": "24h"}, h.token)
	if len(arr(obj(out["data"])["missing_models"])) != 1 {
		t.Fatalf("总览应附带 missing_models: %v", obj(out["data"])["missing_models"])
	}
}

// 流式片段到达即计入实时速度；上游请求带 x-session-id；过期会话不在列表里
func TestLiveStreamAndSessionHeader(t *testing.T) {
	var sessionHeader atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionHeader.Store(r.Header.Get("x-session-id"))
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 10; i++ {
			io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{"content":"123456789"},"finish_reason":null}]}`+"\n\n")
		}
		io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":30}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	h := newHarness(t)
	h.configure(t, up.URL, modelFixture("a", "chat"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{"a", 10}}}}
	})
	body := requestFixture("chat", "auto")
	body["stream"] = true
	w := h.generate(t, "chat", body)
	requireStatus(t, w, 200)
	if got, _ := sessionHeader.Load().(string); got != "test-conversation" {
		t.Fatalf("上游应收到 x-session-id，实得 %q", got)
	}
	_, out := h.rpc(t, "route.live", Object{}, h.token)
	// 90 个字符 ÷ 3 ÷ 60 秒 = 0.5 tok/s
	if got := num(obj(obj(obj(out["data"])["routes"])["auto"]), "live_tok_s"); got != 0.5 {
		t.Fatalf("实时估算速度应为 0.5，实得 %v", got)
	}
	if err := h.s.DB.Exec(`INSERT INTO sessions VALUES ('old','k','a','p_test',?,1)`, now()-int64(48*3600000)); err != nil {
		t.Fatal(err)
	}
	_, out = h.rpc(t, "session.list", Object{}, h.token)
	for _, s := range arr(out["data"]) {
		if str(obj(s), "id") == "old" || num(obj(s), "expires_at") == 0 {
			t.Fatalf("会话列表不应含过期会话且应带 expires_at: %v", s)
		}
	}
	if !strings.Contains(raw(out["data"]), "expires_at") {
		t.Fatalf("会话列表应带 expires_at: %v", out["data"])
	}
}
