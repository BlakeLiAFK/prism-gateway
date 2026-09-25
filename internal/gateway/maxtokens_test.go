package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureUpstream 记录上游收到的最后一个请求体；empty 为真时回一个被输出上限截断的空回答
func captureUpstream(t *testing.T, empty bool) (*httptest.Server, func() Object) {
	t.Helper()
	var mu sync.Mutex
	last := Object{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := Object{}
		json.NewDecoder(r.Body).Decode(&o)
		mu.Lock()
		last = o
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case empty:
			io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":8}}`)
		default:
			io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}
	}))
	t.Cleanup(up.Close)
	return up, func() Object { mu.Lock(); defer mu.Unlock(); return last }
}

// 客户端没写输出上限：chat 上游不补（交给上游默认），messages 上游必填，补模型上限
func TestMaxTokensDefaultByUpstreamProtocol(t *testing.T) {
	h := newHarness(t)
	up, last := captureUpstream(t, false)
	chat, msgs := modelFixture("c", "chat"), modelFixture("m", "messages")
	chat.MaxOutput, msgs.MaxOutput = 32000, 16000
	h.configure(t, up.URL, chat, msgs)
	body := Object{"model": "c", "messages": []any{Object{"role": "user", "content": "hi"}}}
	requireStatus(t, h.generate(t, "chat", body), 200)
	if _, ok := last()["max_tokens"]; ok {
		t.Fatalf("chat 上游不应被补 max_tokens: %v", last())
	}
	body["model"] = "m"
	requireStatus(t, h.generate(t, "chat", body), 200)
	if num(last(), "max_tokens") != 16000 {
		t.Fatalf("messages 上游应补模型上限 16000: %v", last()["max_tokens"])
	}
}

// 要求的输出上限超过模型配置时降到上限而不是淘汰候选，思考预算随之收紧
func TestMaxTokensClampedToModel(t *testing.T) {
	h := newHarness(t)
	up, last := captureUpstream(t, false)
	m := modelFixture("m", "messages")
	m.MaxOutput = 8192
	h.configure(t, up.URL, m)
	body := Object{"model": "m", "max_tokens": 64000, "thinking": Object{"type": "enabled", "budget_tokens": 10000},
		"messages": []any{Object{"role": "user", "content": "hi"}}}
	requireStatus(t, h.generate(t, "messages", body), 200)
	got := last()
	if num(got, "max_tokens") != 8192 || num(obj(got["thinking"]), "budget_tokens") != 8191 {
		t.Fatalf("应降到 8192 并把思考预算收紧到 8191: max_tokens=%v thinking=%v", got["max_tokens"], got["thinking"])
	}
}

// 所有候选都因输出预算被思考占满而没有正文：报错写明客户端给的 max_tokens
func TestTruncatedErrorNamesMaxTokens(t *testing.T) {
	h := newHarness(t)
	up, _ := captureUpstream(t, true)
	h.configure(t, up.URL, modelFixture("a", "chat"), modelFixture("b", "chat"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{"a", 10}, {"b", 10}}}}
	})
	body := requestFixture("chat", "auto")
	body["max_tokens"] = 8
	w := h.generate(t, "chat", body)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "max_tokens=8 太小") {
		t.Fatalf("应明确提示 max_tokens 太小: %d %s", w.Code, w.Body)
	}
}

// Z.AI 5 小时窗口用满后 429：查额度接口得到重置时刻，同供应商模型冷却到那时（封顶 24 小时）
func TestZaiExhaustedCoolsUntilReset(t *testing.T) {
	reset := now() + int64(2*3600000)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/monitor/usage/quota/limit" {
			io.WriteString(w, `{"code":200,"success":true,"data":{"limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"percentage":100},{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":100,"nextResetTime":`+strconv.FormatInt(reset, 10)+`}]}}`)
			return
		}
		w.WriteHeader(429)
	}))
	defer up.Close()
	h := newHarness(t)
	a, b := modelFixture("g1", "chat"), modelFixture("g2", "chat")
	a.ProviderID, b.ProviderID = "zai", "zai"
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "zai", Name: "Z.AI", Kind: "zai", Auth: "none", BaseURL: up.URL + "/api/coding/paas/v4", AllowPrivate: true, Enabled: true, TimeoutSec: 30}}
		c.Models = []Model{a, b}
	})
	requireStatus(t, h.generate(t, "chat", requestFixture("chat", "g1")), 429)
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.a.Engine.mu.Lock()
		cool := h.a.Engine.state("g2").Cooldown
		h.a.Engine.mu.Unlock()
		if cool >= reset-1000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("g2 应冷却到 5 小时窗口重置，实得 %d", cool)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
