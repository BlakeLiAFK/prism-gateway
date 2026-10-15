package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type agentObservation struct {
	value   string
	present bool
	proto   int
}

func (h *harness) generateWithUserAgent(t *testing.T, protocol string, body Object, agent string, set bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", pathFor(protocol), strings.NewReader(raw(body)))
	r.Header.Set("X-Prism-Session", "test-conversation")
	if set {
		r.Header["User-Agent"] = []string{agent}
	}
	w := httptest.NewRecorder()
	h.a.Engine.Handle(w, r, protocol, Principal{ID: "test-key"})
	return w
}

func observeAgent(r *http.Request) agentObservation {
	_, present := r.Header["User-Agent"]
	return agentObservation{value: r.Header.Get("User-Agent"), present: present, proto: r.ProtoMajor}
}

func TestUserAgentForwardingAndSuppression(t *testing.T) {
	long := "client/" + strings.Repeat("x", 220)
	for _, tc := range []struct {
		name, agent  string
		set, present bool
	}{{"长UA完整透传", long, true, true}, {"缺失UA", "", false, false}, {"显式空UA", "", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan agentObservation, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- observeAgent(r)
				io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer up.Close()
			h := newHarness(t)
			h.configure(t, up.URL, modelFixture("m", "chat"))
			w := h.generateWithUserAgent(t, "chat", requestFixture("chat", "m"), tc.agent, tc.set)
			requireStatus(t, w, 200)
			got := <-seen
			if got.present != tc.present || got.value != tc.agent {
				t.Fatalf("上游 User-Agent 不正确: got=%+v want=%q present=%v", got, tc.agent, tc.present)
			}
		})
	}
}

func TestUserAgentPreservedAcrossFallbackAndCountTokens(t *testing.T) {
	const agent = "fallback-client/9.4"
	seen := make(chan agentObservation, 3)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observeAgent(r)
		if r.URL.Path == "/messages/count_tokens" {
			io.WriteString(w, `{"input_tokens":321}`)
			return
		}
		o := Object{}
		json.NewDecoder(r.Body).Decode(&o)
		if str(o, "model") == "upstream-first" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	h := newHarness(t)
	first, second := modelFixture("first", "chat"), modelFixture("second", "chat")
	h.configure(t, up.URL, first, second)
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "first", Weight: 10}, {ModelID: "second", Weight: 10}}}}
	})
	requireStatus(t, h.generateWithUserAgent(t, "chat", requestFixture("chat", "auto"), agent, true), 200)
	for range 2 {
		if got := <-seen; !got.present || got.value != agent {
			t.Fatalf("候选切换丢失 User-Agent: %+v", got)
		}
	}
	checkNativeCountUserAgent(t, up.URL, seen, agent)
}

func checkNativeCountUserAgent(t *testing.T, base string, seen <-chan agentObservation, agent string) {
	t.Helper()
	h := newHarness(t)
	m := modelFixture("count", "messages")
	m.NativeCount = true
	h.configure(t, base, m)
	body := requestFixture("messages", "count")
	delete(body, "max_tokens")
	r := httptest.NewRequest("POST", "/messages/count_tokens", strings.NewReader(raw(body)))
	r.Header.Set("User-Agent", agent)
	w := httptest.NewRecorder()
	h.a.Engine.CountTokens(w, r, Principal{ID: "test"})
	requireStatus(t, w, 200)
	if got := <-seen; !got.present || got.value != agent {
		t.Fatalf("count_tokens 丢失 User-Agent: %+v", got)
	}
}

func TestBackgroundRequestsSuppressUserAgent(t *testing.T) {
	seen := make(chan agentObservation, 2)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observeAgent(r)
		if r.URL.Path == "/usage" {
			io.WriteString(w, `{"usage":{"rolling":{"status":"ok","percent":1}}}`)
			return
		}
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer up.Close()
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p", Name: "p", Kind: "opencode", Auth: "none", BaseURL: up.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30}}
	})
	if w, _ := h.rpc(t, "provider.test", Object{"id": "p"}, h.token); w.Code != 200 {
		t.Fatalf("provider.test 失败: %s", w.Body)
	}
	if w, out := h.rpc(t, "provider.usage", Object{}, h.token); w.Code != 200 || out["ok"] != true {
		t.Fatalf("provider.usage 失败: %s", w.Body)
	}
	for range 2 {
		if got := <-seen; got.present || got.value != "" {
			t.Fatalf("后台请求带了 User-Agent: %+v", got)
		}
	}
}

func TestEmptyUserAgentSuppressedOverHTTP2(t *testing.T) {
	seen := make(chan agentObservation, 1)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- observeAgent(r) }))
	up.EnableHTTP2 = true
	up.StartTLS()
	defer up.Close()
	req, _ := http.NewRequest(http.MethodGet, up.URL, nil)
	requestHeaders(req, &http.Request{Header: http.Header{}}, Provider{Auth: "none"}, "chat", "")
	res, err := up.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	got := <-seen
	if got.proto != 2 || got.present || got.value != "" {
		t.Fatalf("HTTP/2 自动补入 User-Agent: %+v", got)
	}
}
