package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// 上游 400 是确定性拒绝、没有生成内容：同一请求内换下一个候选。
func TestUpstream400SwitchesCandidate(t *testing.T) {
	h := failoverHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"unsupported parameter: thinking"}}`)
	})
	w := h.generate(t, "chat", requestFixture("chat", "pair"))
	requireStatus(t, w, 200)
	if got := w.Header().Get("X-Prism-Model"); got != "cand-ok" {
		t.Fatalf("400 应当换候选，实得 %s", got)
	}
	rows, _ := h.s.DB.Query("SELECT error_code,error_detail FROM requests WHERE status='error'")
	if len(rows) != 1 || rows[0].String("error_code") != "UPSTREAM_400" || !strings.Contains(rows[0].String("error_detail"), "unsupported parameter") {
		t.Fatalf("被拒候选的记录应带上游原因: %v", rows)
	}
}

// 全部候选都拒绝时，最后一个上游的原话按客户端协议原样交出：Claude Code 靠这句话自动调小 max_tokens。
func TestUpstream400PassthroughWhenAllReject(t *testing.T) {
	msg := "input length and `max_tokens` exceed context limit: 190000 + 64000 > 200000"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"`+msg+`"}}`)
	}))
	t.Cleanup(up.Close)
	h := newHarness(t)
	h.configure(t, up.URL, modelFixture("a", "messages"), modelFixture("b", "messages"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "a", Weight: 10}, {ModelID: "b", Weight: 10}}}}
	})
	w := h.generate(t, "messages", requestFixture("messages", "auto"))
	requireStatus(t, w, 400)
	if b := w.Body.String(); !strings.Contains(b, msg) || !strings.Contains(b, `"invalid_request_error"`) {
		t.Fatalf("应原样透传上游原话与类型: %s", b)
	}
	rows, _ := h.s.DB.Query("SELECT COUNT(*) n FROM requests WHERE error_code='UPSTREAM_400'")
	if rows[0].Int("n") != 2 {
		t.Fatalf("两个候选都应试过并留下记录: %v", rows)
	}
}

// 兜底候选：主候选健康时零流量；主候选被限流后接手；成功后不写会话亲和，下一轮仍回主候选。
func TestFallbackCandidateOnlyAfterPrimaryFails(t *testing.T) {
	var primaryFail atomic.Bool
	var fallbackHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := Object{}
		json.NewDecoder(r.Body).Decode(&o)
		if str(o, "model") == "upstream-safe" {
			fallbackHits.Add(1)
		} else if primaryFail.Load() {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	h := newHarness(t)
	h.configure(t, up.URL, modelFixture("main", "chat"), modelFixture("safe", "chat"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "weighted", Affinity: true,
			Candidates: []Candidate{{ModelID: "main", Weight: 10}, {ModelID: "safe", Weight: 100, Fallback: true}}}}
	})
	for i := 0; i < 5; i++ {
		w := h.generate(t, "chat", requestFixture("chat", "auto"))
		requireStatus(t, w, 200)
		if w.Header().Get("X-Prism-Model") != "main" {
			t.Fatalf("主候选健康时不该用兜底: %s", w.Header().Get("X-Prism-Model"))
		}
	}
	if fallbackHits.Load() != 0 {
		t.Fatal("主候选健康时兜底候选不该有流量")
	}
	primaryFail.Store(true)
	w := h.generate(t, "chat", requestFixture("chat", "auto"))
	requireStatus(t, w, 200)
	if w.Header().Get("X-Prism-Model") != "safe" || fallbackHits.Load() != 1 {
		t.Fatalf("主候选限流后应由兜底接手: %s", w.Header().Get("X-Prism-Model"))
	}
	rows, _ := h.s.DB.Query("SELECT model_id FROM sessions")
	for _, r := range rows {
		if r.String("model_id") == "safe" {
			t.Fatal("会话不该绑定到兜底候选")
		}
	}
}

// 主候选是跨协议、接不了 Claude Code 的 safeguards；兜底是原生 Messages，能接住。
func TestFallbackCandidateServesIncompatibleRequest(t *testing.T) {
	up, sent := historyUpstream(t)
	h := newHarness(t)
	h.configure(t, up.URL, dropModel("main", "chat", true), modelFixture("safe", "messages"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "weighted",
			Candidates: []Candidate{{ModelID: "main", Weight: 10}, {ModelID: "safe", Weight: 10, Fallback: true}}}}
	})
	body := claudeCodeRequest("auto", Object{"type": "adaptive", "display": "omitted"})
	body["safeguards"] = []any{Object{"type": "dangerous_tool_use", "classifier_context": Object{"v": 1}}}
	w := h.generate(t, "messages", body)
	requireStatus(t, w, 200)
	if w.Header().Get("X-Prism-Model") != "safe" || !strings.Contains(sent("messages"), "safeguards") {
		t.Fatalf("应由原生兜底接住并原样转发 safeguards: %s", w.Header().Get("X-Prism-Model"))
	}
}

// 没有候选能接时，400 的消息必须保留英文字段名：Claude Code 在第三方端点上看到 safeguards 一词
// 就不再发这个字段，改为向用户询问权限。
func TestNoCompatibleMessageKeepsFieldName(t *testing.T) {
	up, _ := historyUpstream(t)
	h := newHarness(t)
	h.configure(t, up.URL, dropModel("main", "chat", true))
	body := claudeCodeRequest("main", Object{"type": "adaptive", "display": "omitted"})
	body["safeguards"] = []any{Object{"type": "dangerous_tool_use"}}
	w := h.generate(t, "messages", body)
	requireStatus(t, w, 400)
	if !regexp.MustCompile(`\bsafeguards\b`).MatchString(w.Body.String()) {
		t.Fatalf("400 消息必须含英文字段名 safeguards: %s", w.Body)
	}
}

// 所有候选都在冷却时，用客户端认识的头说明该不该等：长冷却（模型下架）直接放弃，短冷却按剩余时间等。
func TestRetryHintsOnCooldown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		giveUp     bool
	}{{"model gone", 404, "", true}, {"short rate limit", 429, "2", false}} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(up.Close)
			h := newHarness(t)
			h.configure(t, up.URL, modelFixture("m", "chat"))
			h.generate(t, "chat", requestFixture("chat", "m"))
			w := h.generate(t, "chat", requestFixture("chat", "m"))
			requireStatus(t, w, 429)
			ra := w.Header().Get("Retry-After")
			if tc.giveUp {
				if w.Header().Get("x-should-retry") != "false" || ra == "" || ra == "30" {
					t.Fatalf("长冷却应让客户端放弃并给出真实剩余时间: %v", w.Header())
				}
			} else if w.Header().Get("x-should-retry") != "" || (ra != "1" && ra != "2") {
				t.Fatalf("短冷却应按剩余时间等待: %v", w.Header())
			}
			if !strings.Contains(w.Body.String(), "秒后恢复") {
				t.Fatalf("错误信息应说明冷却原因与恢复时间: %s", w.Body)
			}
		})
	}
}

// 流首事件就是错误：还没提交 200，过载类换候选，其他错误原样透传并冷却该候选。
func TestStreamFirstEventError(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		switched    bool
	}{
		{"overloaded switches", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n", true},
		{"plain error passthrough", ": OPENROUTER PROCESSING\n\ndata: {\"error\":{\"message\":\"Provider returned error\",\"code\":502}}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var okHits atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				o := Object{}
				json.NewDecoder(r.Body).Decode(&o)
				w.Header().Set("Content-Type", "text/event-stream")
				if str(o, "model") == "upstream-bad" {
					io.WriteString(w, tc.frame)
					return
				}
				okHits.Add(1)
				io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"}}]}\n\n"+
					"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(up.Close)
			h := newHarness(t)
			h.configure(t, up.URL, modelFixture("bad", "chat"), modelFixture("ok", "chat"))
			h.change(t, func(c *Config) {
				c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "bad", Weight: 10}, {ModelID: "ok", Weight: 10}}}}
			})
			body := requestFixture("chat", "auto")
			body["stream"] = true
			w := h.generate(t, "chat", body)
			if tc.switched {
				requireStatus(t, w, 200)
				if w.Header().Get("X-Prism-Model") != "ok" || !strings.Contains(w.Body.String(), "[DONE]") {
					t.Fatalf("过载首事件应换候选并完整输出: %s %s", w.Header().Get("X-Prism-Model"), w.Body)
				}
				return
			}
			requireStatus(t, w, 502)
			if !strings.Contains(w.Body.String(), "Provider returned error") || strings.Contains(w.Body.String(), "data:") {
				t.Fatalf("首事件错误应以 JSON 透传: %s", w.Body)
			}
			w = h.generate(t, "chat", body)
			requireStatus(t, w, 200)
			if w.Header().Get("X-Prism-Model") != "ok" || okHits.Load() != 1 {
				t.Fatalf("失败候选应在冷却中，重试落到下一个: %s", w.Header().Get("X-Prism-Model"))
			}
		})
	}
}

func TestRequestIDHeadersOnSuccess(t *testing.T) {
	up, _ := historyUpstream(t)
	h := newHarness(t)
	h.configure(t, up.URL, modelFixture("m", "messages"))
	w := h.generate(t, "messages", requestFixture("messages", "m"))
	requireStatus(t, w, 200)
	if w.Header().Get("Request-Id") == "" || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("成功响应也要带两种 SDK 读的请求 ID: %v", w.Header())
	}
}
