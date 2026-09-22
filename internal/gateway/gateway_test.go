package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"prism-gateway/internal/sqlite"
	"prism-gateway/internal/webui"
)

type harness struct {
	s     *Store
	a     *App
	token string
	ctx   context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := NewApp(ctx, s, webui.Handler())
	token, err := s.AdminToken(false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); a.Wait(); s.DB.Close() })
	return &harness{s, a, token, ctx}
}
func (h *harness) rpc(t *testing.T, action string, p Object, token string) (*httptest.ResponseRecorder, Object) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://localhost/api.json", strings.NewReader(raw(Object{"action": action, "params": p})))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	o := Object{}
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatalf("bad RPC JSON %s: %v", w.Body, err)
	}
	return w, o
}
func (h *harness) change(t *testing.T, fn func(*Config)) {
	t.Helper()
	_, err := h.s.Change(h.s.Config().Version, "test", "test", func(c *Config) error { fn(c); return nil })
	if err != nil {
		t.Fatal(err)
	}
}
func (h *harness) createKey(t *testing.T) string {
	t.Helper()
	_, o := h.rpc(t, "apikey.create", Object{"name": "test", "allowed": []any{}}, h.token)
	k := str(obj(o["data"]), "key")
	if k == "" {
		t.Fatalf("创建调用 Key 失败: %v", o)
	}
	return k
}
func modelFixture(id, proto string) Model {
	return Model{ID: id, ProviderID: "p_test", Upstream: "upstream-" + id, Name: id, Protocol: proto, Enabled: true, Tools: true, Vision: true, Context: 128000, MaxOutput: 4096, Concurrency: 8}
}
func requestFixture(p, model string) Object {
	if p == "responses" {
		return Object{"model": model, "input": "hello", "max_output_tokens": 256}
	}
	return Object{"model": model, "messages": []any{Object{"role": "user", "content": "hello"}}, "max_tokens": 256}
}
func (h *harness) configure(t *testing.T, url string, models ...Model) {
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p_test", Name: "Test upstream", Kind: "custom", BaseURL: url, Auth: "auto", Secret: "upstream-secret", HasKey: true, Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		c.Models = models
	})
}
func (h *harness) generate(t *testing.T, p string, o Object) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", pathFor(p), strings.NewReader(raw(o)))
	r.Header.Set("X-Prism-Session", "test-conversation")
	w := httptest.NewRecorder()
	h.a.Engine.Handle(w, r, p, Principal{ID: "test-key"})
	return w
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body)
	}
}

func TestManagementAuthCSRFAndSingleEndpoint(t *testing.T) {
	h := newHarness(t)
	w, _ := h.rpc(t, "config.get", Object{}, "")
	requireStatus(t, w, 401)
	w, o := h.rpc(t, "auth.login", Object{"token": h.token}, "")
	requireStatus(t, w, 200)
	csrf := str(obj(o["data"]), "csrf")
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe session cookie")
	}
	for _, tc := range []struct {
		csrf, origin string
		status       int
	}{{"", "", 403}, {csrf, "http://evil.example", 403}, {csrf, "http://localhost", 200}} {
		r := httptest.NewRequest("POST", "http://localhost/api.json", strings.NewReader(`{"action":"config.get","params":{}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-CSRF", tc.csrf)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		r.AddCookie(cookies[0])
		rr := httptest.NewRecorder()
		h.a.ServeHTTP(rr, r)
		requireStatus(t, rr, tc.status)
	}
	for _, path := range []string{"/api/v1/providers", "/admin/v1/models", "/api.json/foo", "/assets/missing.js"} {
		r := httptest.NewRequest("GET", path, nil)
		rr := httptest.NewRecorder()
		h.a.ServeHTTP(rr, r)
		requireStatus(t, rr, 404)
	}
	for _, path := range []string{"/", "/models", "/assets/app.js"} {
		rr := httptest.NewRecorder()
		h.a.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		requireStatus(t, rr, 200)
	}
	w, _ = h.rpc(t, "unknown.action", Object{}, h.token)
	requireStatus(t, w, 400)
}
func TestConfigVersionEncryptionAndPersistence(t *testing.T) {
	h := newHarness(t)
	v := h.s.Config().Version
	w, o := h.rpc(t, "provider.save", Object{"version": v, "id": "p_test", "provider": Object{"name": "Encrypted provider", "base_url": "https://example.com/v1", "api_key": "secret-not-readable"}}, h.token)
	requireStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "secret-not-readable") {
		t.Fatal("secret exposed in API")
	}
	if num(obj(o["data"]), "version") != float64(v+1) {
		t.Fatal("version did not increment")
	}
	rows, err := h.s.DB.Query("SELECT secret FROM providers")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw(rows), "secret-not-readable") {
		t.Fatal("secret not encrypted")
	}
	err = h.s.load()
	c := h.s.Config()
	if err != nil {
		t.Fatal(err)
	}
	if c.Providers[0].Secret != "secret-not-readable" {
		t.Fatal("decrypt failed")
	}
	w, _ = h.rpc(t, "settings.update", Object{"version": v, "settings": Object{"app_name": "stale"}}, h.token)
	requireStatus(t, w, 409)
	w, _ = h.rpc(t, "settings.update", Object{"version": h.s.Config().Version, "settings": Object{"global_concurrency": 0}}, h.token)
	requireStatus(t, w, 400)
}
func TestAPIKeysScopeRevokeAndRedaction(t *testing.T) {
	h := newHarness(t)
	_, err := h.a.EnableDemo(h.s.Config().Version)
	if err != nil {
		t.Fatal(err)
	}
	w, o := h.rpc(t, "apikey.create", Object{"name": "restricted", "allowed": []any{"demo-chat"}}, h.token)
	requireStatus(t, w, 200)
	key := str(obj(o["data"]), "key")
	id := str(obj(o["data"]), "id")
	w, _ = h.rpc(t, "apikey.list", Object{}, h.token)
	if strings.Contains(w.Body.String(), key) {
		t.Fatal("full key exposed on list")
	}
	for _, tc := range []struct {
		model, key string
		status     int
	}{{"demo-chat", key, 200}, {"demo-messages", key, 403}, {"demo-chat", h.token, 401}} {
		r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(raw(requestFixture("chat", tc.model))))
		r.Header.Set("Authorization", "Bearer "+tc.key)
		rr := httptest.NewRecorder()
		h.a.ServeHTTP(rr, r)
		requireStatus(t, rr, tc.status)
	}
	w, _ = h.rpc(t, "apikey.revoke", Object{"id": id}, h.token)
	requireStatus(t, w, 200)
	r := httptest.NewRequest("GET", "/openai/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w = httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	requireStatus(t, w, 401)
}
func TestAllProtocolPairsNonStream(t *testing.T) {
	for _, from := range []string{"chat", "messages", "responses"} {
		for _, to := range []string{"chat", "messages", "responses"} {
			t.Run(from+"_to_"+to, func(t *testing.T) {
				h := newHarness(t)
				var got Object
				var header http.Header
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					header = r.Header.Clone()
					json.NewDecoder(r.Body).Decode(&got)
					if r.URL.Path != pathFor(to) {
						t.Errorf("wrong path %s", r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(completionObject(Completion{Blocks: []Block{{Kind: "text", Text: "hello world"}, {Kind: "tool", ID: "call_1", Name: "read_file", Arguments: `{"path":"main.go"}`}}, Usage: Usage{Input: 100, Output: 20, Cache: 50, Known: true}, Stop: "tool"}, to, "upstream-m", "fixture"))
				}))
				defer up.Close()
				m := modelFixture("m", to)
				m.PricingSet = true
				m.InputPrice = 1
				m.OutputPrice = 2
				m.CachePrice = .1
				h.configure(t, up.URL, m)
				w := h.generate(t, from, requestFixture(from, "m"))
				requireStatus(t, w, 200)
				if str(got, "model") != "upstream-m" {
					t.Fatal("model not rewritten")
				}
				if header.Get("X-Opencode-Session") != "test-conversation" {
					t.Fatal("session header not forwarded")
				}
				if !strings.HasPrefix(header.Get("User-Agent"), "Prism-Gateway/") {
					t.Fatal("missing own user-agent")
				}
				if to == "messages" {
					if header.Get("X-Api-Key") != "upstream-secret" {
						t.Fatal("wrong Anthropic auth")
					}
				} else if header.Get("Authorization") != "Bearer upstream-secret" {
					t.Fatal("wrong bearer")
				}
				o := Object{}
				json.Unmarshal(w.Body.Bytes(), &o)
				co, err := decodeCompletion(o, from)
				if err != nil {
					t.Fatal(err)
				}
				if len(co.Blocks) != 2 || co.Blocks[0].Text != "hello world" || co.Blocks[1].ID != "call_1" || co.Blocks[1].Arguments != `{"path":"main.go"}` {
					t.Fatalf("wrong translated content: %+v", co)
				}
				rows, _ := h.s.DB.Query("SELECT * FROM requests")
				if len(rows) != 1 || rows[0].String("status") != "success" || rows[0].Int("input_tokens") != 100 || rows[0].Int("cache_tokens") != 50 {
					t.Fatalf("usage wrong %+v", rows)
				}
			})
		}
	}
}
func fixtureSSE(t *testing.T, p string) string {
	t.Helper()
	w := httptest.NewRecorder()
	e := newEmitter(w, p, "upstream-m", "fixture")
	e.usage = Usage{Input: 120, Output: 30, Cache: 80, Known: true}
	e.stop = "tool"
	for i, b := range []Block{{Kind: "text", Text: "hello world"}, {Kind: "tool", ID: "call_1", Name: "read_file", Arguments: `{"path":"main.go"}`}} {
		if er := e.start(i, b); er != nil {
			t.Fatal(er)
		}
		text := b.Text
		if b.Kind == "tool" {
			text = b.Arguments
		}
		if er := e.delta(i, text[:4]); er != nil {
			t.Fatal(er)
		}
		if er := e.delta(i, text[4:]); er != nil {
			t.Fatal(er)
		}
	}
	if er := e.finish(); er != nil {
		t.Fatal(er)
	}
	return w.Body.String()
}
func TestAllProtocolPairsStreaming(t *testing.T) {
	for _, from := range []string{"chat", "messages", "responses"} {
		for _, to := range []string{"chat", "messages", "responses"} {
			t.Run(from+"_to_"+to, func(t *testing.T) {
				h := newHarness(t)
				fixture := fixtureSSE(t, to)
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, fixture)
				}))
				defer up.Close()
				h.configure(t, up.URL, modelFixture("m", to))
				body := requestFixture(from, "m")
				body["stream"] = true
				w := h.generate(t, from, body)
				requireStatus(t, w, 200)
				u := Usage{}
				err := nativeStream(httptest.NewRecorder(), strings.NewReader(w.Body.String()), from, &u)
				if err != nil {
					t.Fatalf("invalid output SSE: %v\n%s", err, w.Body)
				}
				if !u.Known || u.Input != 120 || u.Output != 30 || u.Cache != 80 {
					t.Fatalf("bad usage %+v", u)
				}
				if !strings.Contains(w.Body.String(), "read_file") {
					t.Fatal("tool name lost")
				}
				rows, _ := h.s.DB.Query("SELECT status FROM requests")
				if rows[0].String("status") != "success" {
					t.Fatalf("stream did not succeed: %s", w.Body)
				}
			})
		}
	}
}
func TestCanonicalToolConversationRoundTrip(t *testing.T) {
	c := Canonical{MaxOutput: 256, Choice: "auto", Tools: []Tool{{Name: "read_file", Description: "Read file", Schema: Object{"type": "object", "properties": Object{"path": Object{"type": "string"}}}}}, Messages: []Message{{Role: "system", Blocks: []Block{{Kind: "text", Text: "You write code."}}}, {Role: "user", Blocks: []Block{{Kind: "text", Text: "read main.go"}}}, {Role: "assistant", Blocks: []Block{{Kind: "tool", ID: "call_1", Name: "read_file", Arguments: `{"path":"main.go"}`}}}, {Role: "user", Blocks: []Block{{Kind: "result", ID: "call_1", Text: "package main"}}}}}
	for _, p := range []string{"chat", "messages", "responses"} {
		t.Run(p, func(t *testing.T) {
			o, e := encodeCanonical(c, p, "m")
			if e != nil {
				t.Fatal(e)
			}
			d, e := decodeCanonical(o, p)
			if e != nil {
				t.Fatal(e)
			}
			if len(d.Tools) != 1 || len(d.Messages) != 4 {
				t.Fatalf("lost context %+v", d)
			}
			if d.Messages[2].Blocks[0].ID != "call_1" || d.Messages[3].Blocks[0].Text != "package main" {
				t.Fatalf("lost tool linkage %+v", d)
			}
		})
	}
}
func TestNativeOpaqueAndCrossStrict(t *testing.T) {
	h := newHarness(t)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		o := Object{}
		json.NewDecoder(r.Body).Decode(&o)
		if obj(o["thinking"])["type"] != "adaptive" {
			t.Error("native thinking dropped")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"opaque","signature":"signed"},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("m", "messages"), modelFixture("chatmodel", "chat"))
	body := requestFixture("messages", "m")
	body["thinking"] = Object{"type": "adaptive"}
	w := h.generate(t, "messages", body)
	requireStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"signature":"signed"`) {
		t.Fatal("opaque response lost")
	}
	body["model"] = "chatmodel"
	w = h.generate(t, "messages", body)
	requireStatus(t, w, 400)
	if calls.Load() != 1 {
		t.Fatal("unsupported conversion reached upstream")
	}
}
func TestQuotaRPMConcurrencyAndReservations(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:12345", modelFixture("m", "chat"))
	h.change(t, func(c *Config) {
		c.Models[0].Concurrency = 1
		c.Models[0].RPM = 1
		c.Models[0].PricingSet = true
		c.Models[0].InputPrice = 1
		c.Models[0].OutputPrice = 2
		c.Models[0].Limit5h = 1
	})
	c := h.s.Config()
	ss, _, e := h.a.Engine.selections(c, requestFixture("chat", "m"), "chat", "")
	if e != nil {
		t.Fatal(e)
	}
	s := ss[0]
	var accepted atomic.Int32
	var id string
	var lock sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x, err := h.a.Engine.admit(s, Principal{ID: "k"}, randomID("r_"), "m", "chat", "", client{})
			if err == nil {
				accepted.Add(1)
				lock.Lock()
				id = x
				lock.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d want 1", accepted.Load())
	}
	rows, _ := h.s.DB.Query("SELECT cost_nano FROM requests")
	if rows[0].Int("cost_nano") <= 0 {
		t.Fatal("missing inflight reservation")
	}
	h.a.Engine.finish(s, id, "", "k", Usage{Input: 10, Output: 10, Known: true}, 200, nil, now())
	_, e = h.a.Engine.admit(s, Principal{ID: "k"}, "r2", "m", "chat", "", client{})
	if e == nil {
		t.Fatal("RPM not enforced")
	}
	h.change(t, func(c *Config) { c.Models[0].RPM = 0; c.Models[0].Limit5h = .000001 })
	ss, _, _ = h.a.Engine.selections(h.s.Config(), requestFixture("chat", "m"), "chat", "")
	_, e = h.a.Engine.admit(ss[0], Principal{ID: "k"}, "r3", "m", "chat", "", client{})
	if e == nil {
		t.Fatal("budget not enforced")
	}
}
func TestFallback429AndNoMidStreamFallback(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			h := newHarness(t)
			var second atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				o := Object{}
				json.NewDecoder(r.Body).Decode(&o)
				if str(o, "model") == "upstream-first" {
					if partial {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
					} else {
						w.Header().Set("Retry-After", "2")
						w.WriteHeader(429)
					}
					return
				}
				second.Add(1)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(completionObject(Completion{Blocks: []Block{{Kind: "text", Text: "fallback success"}}, Usage: Usage{Input: 1, Output: 2, Known: true}}, "chat", "second", "f"))
			}))
			defer up.Close()
			h.configure(t, up.URL, modelFixture("first", "chat"), modelFixture("second", "chat"))
			h.change(t, func(c *Config) {
				c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{"first", 10}, {"second", 10}}}}
			})
			body := requestFixture("chat", "auto")
			body["stream"] = partial
			w := h.generate(t, "chat", body)
			requireStatus(t, w, 200)
			if partial {
				if second.Load() != 0 || !strings.Contains(w.Body.String(), "upstream_stream_error") || strings.Contains(w.Body.String(), "[DONE]") {
					t.Fatalf("unsafe partial stream %s", w.Body)
				}
			} else if second.Load() != 1 || !strings.Contains(w.Body.String(), "fallback success") {
				t.Fatalf("fallback failed %s", w.Body)
			}
		})
	}
}
func TestTokenCountModesAndNoFakeExact(t *testing.T) {
	h := newHarness(t)
	_, e := h.a.EnableDemo(h.s.Config().Version)
	if e != nil {
		t.Fatal(e)
	}
	body := requestFixture("messages", "demo-messages")
	delete(body, "max_tokens")
	r := httptest.NewRequest("POST", "/messages/count_tokens", strings.NewReader(raw(body)))
	w := httptest.NewRecorder()
	h.a.Engine.CountTokens(w, r, Principal{ID: "test"})
	requireStatus(t, w, 200)
	if w.Header().Get("X-Prism-Token-Count-Mode") != "estimated" {
		t.Fatal("estimate not disclosed")
	}
	h.change(t, func(c *Config) { c.Settings.AllowEstimatedCount = false })
	r = httptest.NewRequest("POST", "/messages/count_tokens", strings.NewReader(raw(body)))
	w = httptest.NewRecorder()
	h.a.Engine.CountTokens(w, r, Principal{ID: "test"})
	requireStatus(t, w, 501)
}
func TestSSRFBaseURLValidation(t *testing.T) {
	for _, u := range []string{"http://localhost:11434/v1", "https://127.0.0.1/v1", "https://[::1]/v1", "https://user:password@example.com/v1", "https://example.com/v1?key=secret"} {
		if validateBaseURL(u, false) == nil {
			t.Errorf("unsafe URL accepted: %s", u)
		}
	}
	if validateBaseURL("http://127.0.0.1:11434/v1", true) != nil {
		t.Fatal("explicit local access rejected")
	}
	if validateBaseURL("https://example.com/v1", false) != nil {
		t.Fatal("public HTTPS rejected")
	}
}
func TestUsageCacheArithmetic(t *testing.T) {
	u := Usage{}
	extractUsage(Object{"input_tokens": 100.0, "cache_read_input_tokens": 200.0, "cache_creation_input_tokens": 50.0, "output_tokens": 10.0}, "messages", &u)
	if u.Input != 350 || u.Cache != 200 || u.Write != 50 || u.Output != 10 {
		t.Fatalf("bad inclusive usage %+v", u)
	}
	m := Model{PricingSet: true, InputPrice: 1, OutputPrice: 2, CachePrice: .1, WritePrice: 1.25}
	want := nano((100.0 + 20 + 20 + 62.5) / 1e6)
	if cost(m, u) != want {
		t.Fatalf("cost=%d want=%d", cost(m, u), want)
	}
}
func TestStreamStrictEOFAndMalformedTool(t *testing.T) {
	u := Usage{}
	if nativeStream(httptest.NewRecorder(), strings.NewReader("data: {\"choices\":[]}\n\n"), "chat", &u) == nil {
		t.Fatal("partial stream accepted")
	}
	e := newEmitter(httptest.NewRecorder(), "messages", "m", "id")
	e.start(0, Block{Kind: "tool", ID: "c", Name: "read"})
	e.delta(0, `{"broken":`)
	if e.finish() == nil {
		t.Fatal("malformed tool JSON accepted")
	}
}
func TestReadOnlyBatchRejectsMutation(t *testing.T) {
	h := newHarness(t)
	w, _ := h.rpc(t, "batch.read", Object{"requests": []any{Object{"action": "provider.save", "params": Object{}}}}, h.token)
	requireStatus(t, w, 400)
}
func TestRestartPersistsConfigAndRecoversUnknownUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Change(1, "test", "", func(c *Config) error { c.Settings.AppName = "persisted"; return nil })
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Close()
	s, e = OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	if s.Config().Settings.AppName != "persisted" || s.Config().Version != 2 {
		t.Fatal("config not persisted")
	}
	st, e := os.Stat(path + ".key")
	if e != nil || st.Mode().Perm()&0077 != 0 {
		t.Fatal("master key permissions unsafe")
	}
}
func TestNativeCountProvider(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages/count_tokens" {
			t.Error("wrong count path")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":321}`)
	}))
	defer up.Close()
	m := modelFixture("m", "messages")
	m.NativeCount = true
	h.configure(t, up.URL, m)
	body := requestFixture("messages", "m")
	delete(body, "max_tokens")
	r := httptest.NewRequest("POST", "/count", strings.NewReader(raw(body)))
	w := httptest.NewRecorder()
	h.a.Engine.CountTokens(w, r, Principal{ID: "k"})
	requireStatus(t, w, 200)
	if w.Header().Get("X-Prism-Token-Count-Mode") != "provider" || !strings.Contains(w.Body.String(), "321") {
		t.Fatal("native count lost")
	}
}
func TestModelCatalogSchemas(t *testing.T) {
	h := newHarness(t)
	h.a.EnableDemo(h.s.Config().Version)
	for _, p := range []string{"chat", "messages"} {
		w := httptest.NewRecorder()
		h.a.Engine.Models(w, httptest.NewRequest("GET", map[string]string{"chat": "/openai/v1/models", "messages": "/anthropic/v1/models"}[p], nil), p, Principal{ID: "k"})
		requireStatus(t, w, 200)
		o := Object{}
		json.Unmarshal(w.Body.Bytes(), &o)
		if len(arr(o["data"])) != 4 {
			t.Fatalf("wrong model list %s", w.Body)
		}
		if p == "messages" {
			if _, ok := o["has_more"]; !ok {
				t.Fatal("missing Anthropic pagination")
			}
		} else if str(o, "object") != "list" {
			t.Fatal("wrong OpenAI model list")
		}
	}
}

// A handwritten vendor-shaped trace independent of the emitter implementation.
func TestAnthropicHandwrittenStreamToChat(t *testing.T) {
	src := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`
	w := httptest.NewRecorder()
	u := Usage{}
	err := convertedStream(w, strings.NewReader(src), "messages", "chat", "m", "r", &u, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), "toolu_1") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("wrong tool translation %s", w.Body)
	}
	if u.Input != 12 || u.Output != 20 {
		t.Fatalf("incremental usage lost %+v", u)
	}
}

// Ensure cancellation reaches the real upstream rather than leaving detached generation work.
func TestRequestCancellationReachesUpstream(t *testing.T) {
	h := newHarness(t)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("m", "chat"))
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/chat/completions", bytes.NewBufferString(raw(requestFixture("chat", "m")))).WithContext(ctx)
	done := make(chan struct{})
	go func() { h.a.Engine.Handle(httptest.NewRecorder(), r, "chat", Principal{ID: "k"}); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not reached")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe cancel")
	}
	<-done
}

func TestAnthropicZeroArgumentToolStream(t *testing.T) {
	src := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_1\",\"name\":\"clock\",\"input\":{}}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	u := Usage{}
	w := httptest.NewRecorder()
	if err := convertedStream(w, strings.NewReader(src), "messages", "chat", "m", "r", &u, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `"arguments":"{}"`) {
		t.Fatal("zero-argument tool not emitted")
	}
}

func TestHealthzIsPublicAndReportsDatabase(t *testing.T) {
	h := newHarness(t)
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, httptest.NewRequest(method, "http://localhost/healthz", nil))
		if w.Code != 200 {
			t.Fatalf("%s /healthz = %d，健康检查必须免鉴权", method, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.a.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/healthz", nil))
	o := Object{}
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatalf("healthz 不是 JSON: %v", err)
	}
	if o["ok"] != true || o["version"] != Version {
		t.Fatalf("healthz 返回异常: %s", w.Body)
	}
	// 探针不得泄露配置或运行时细节
	for _, k := range []string{"config_version", "runtime", "storage"} {
		if _, bad := o[k]; bad {
			t.Fatalf("healthz 泄露了 %s", k)
		}
	}
	w = httptest.NewRecorder()
	h.a.ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/healthz", nil))
	if w.Code != 405 {
		t.Fatalf("POST /healthz = %d，应为 405", w.Code)
	}
}

func TestConfigExportImportRoundTripKeepsSecrets(t *testing.T) {
	h := newHarness(t)
	const secret = "sk-plaintext-must-not-leak"
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "auto",
			BaseURL: "https://upstream.example.com/v1", Enabled: true, TimeoutSec: 30, Secret: secret})
		c.Models = append(c.Models, modelFixture("m_alpha", "chat"))
		c.Aliases = append(c.Aliases, Alias{ID: "a_alpha", Target: "m_alpha", Enabled: true})
	})
	w, o := h.rpc(t, "config.export", Object{}, h.token)
	if w.Code != 200 || o["ok"] != true {
		t.Fatalf("导出失败: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("导出文件包含上游 API Key 明文")
	}
	exported := obj(obj(o["data"])["config"])
	if exported == nil {
		t.Fatalf("导出缺少 config: %s", w.Body)
	}

	// 破坏当前配置，再用导出内容还原
	h.change(t, func(c *Config) { c.Models = nil; c.Aliases = nil })
	if len(h.s.Config().Models) != 0 {
		t.Fatal("前置条件失败：模型未清空")
	}
	w, o = h.rpc(t, "config.import", Object{
		"version": h.s.Config().Version, "format": "prism.config/1", "config": exported,
	}, h.token)
	if w.Code != 200 || o["ok"] != true {
		t.Fatalf("导入失败: %s", w.Body)
	}
	cur := h.s.Config()
	if len(cur.Models) != 1 || cur.Models[0].ID != "m_alpha" || len(cur.Aliases) != 1 {
		t.Fatalf("导入未还原模型与别名: %+v", cur)
	}
	p, ok := cur.provider("p_test")
	if !ok || p.Secret != secret || !p.HasKey {
		t.Fatal("导入丢失了库中已有的上游凭证")
	}
	// 导入必须落库，重启后仍在
	dbPath := strings.TrimSuffix(h.s.KeyPath, ".key")
	h.s.DB.Close()
	s2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.DB.Close()
	if len(s2.Config().Models) != 1 {
		t.Fatal("导入结果未持久化")
	}
}

func TestConfigImportRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "auto",
			BaseURL: "https://upstream.example.com/v1", Enabled: true, TimeoutSec: 30, Secret: "sk-old"})
	})
	base := obj(obj(mustData(t, h, "config.export"))["config"])

	// 版本冲突
	_, o := h.rpc(t, "config.import", Object{"version": h.s.Config().Version + 99, "config": base}, h.token)
	if o["ok"] != false || str(obj(o["error"]), "code") != "VERSION_CONFLICT" {
		t.Fatalf("陈旧版本应被拒绝: %v", o)
	}
	// 未知格式
	_, o = h.rpc(t, "config.import", Object{"version": h.s.Config().Version, "format": "other/9", "config": base}, h.token)
	if str(obj(o["error"]), "code") != "UNSUPPORTED_FORMAT" {
		t.Fatalf("未知格式应被拒绝: %v", o)
	}
	// 结构非法
	_, o = h.rpc(t, "config.import", Object{"version": h.s.Config().Version, "config": Object{"providers": "not-a-list"}}, h.token)
	if o["ok"] != false {
		t.Fatalf("非法结构应被拒绝: %v", o)
	}
	// 文件带来的新供应商没有凭证，必须显式报告而不是静默启用
	withNew := obj(obj(mustData(t, h, "config.export"))["config"])
	withNew["providers"] = append(arr(withNew["providers"]), Object{
		"id": "p_new", "name": "New", "kind": "custom", "auth": "auto",
		"base_url": "https://other.example.com/v1", "enabled": true, "timeout_sec": 30, "has_key": true,
	})
	_, o = h.rpc(t, "config.import", Object{"version": h.s.Config().Version, "config": withNew}, h.token)
	if o["ok"] != true {
		t.Fatalf("导入应成功: %v", o)
	}
	missing := arr(obj(o["data"])["providers_missing_key"])
	if len(missing) != 1 || missing[0] != "p_new" {
		t.Fatalf("应报告 p_new 缺少凭证: %v", missing)
	}
	if p, _ := h.s.Config().provider("p_new"); p.HasKey {
		t.Fatal("不得凭导入文件伪造 has_key")
	}
	if p, _ := h.s.Config().provider("p_test"); p.Secret != "sk-old" {
		t.Fatal("已有供应商凭证被覆盖")
	}
}

func mustData(t *testing.T, h *harness, action string) Object {
	t.Helper()
	w, o := h.rpc(t, action, Object{}, h.token)
	if w.Code != 200 || o["ok"] != true {
		t.Fatalf("%s 失败: %s", action, w.Body)
	}
	return obj(o["data"])
}

// syncBuffer 让测试安全地收集来自后台 goroutine 的日志。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func TestLogLevelAndFormatAreRuntimeConfigurable(t *testing.T) {
	buf := &syncBuffer{}
	SetupLogging(buf)
	t.Cleanup(func() { SetupLogging(os.Stderr) })
	h := newHarness(t)

	// 默认 info：debug 不落盘，管理错误详情不外泄
	buf.Reset()
	slog.Debug("hidden detail", "secret", "must-not-appear")
	if strings.Contains(buf.String(), "must-not-appear") {
		t.Fatal("默认级别不应输出 debug")
	}

	// 后台改设置即刻生效，不需要重启进程
	w, o := h.rpc(t, "settings.update", Object{
		"version":  h.s.Config().Version,
		"settings": Object{"log_level": "debug", "log_format": "json"},
	}, h.token)
	if w.Code != 200 || o["ok"] != true {
		t.Fatalf("设置日志级别失败: %s", w.Body)
	}
	buf.Reset()
	slog.Debug("now visible", "request_id", "rpc_test")
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("切到 debug 后仍未输出")
	}
	var line Object
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("log_format=json 必须输出合法 JSON 行: %q", out)
	}
	if line["msg"] != "now visible" || line["request_id"] != "rpc_test" {
		t.Fatalf("结构化字段缺失: %v", line)
	}

	// 调回 info 同样立即生效
	_, o = h.rpc(t, "settings.update", Object{
		"version":  h.s.Config().Version,
		"settings": Object{"log_level": "info", "log_format": "text"},
	}, h.token)
	if o["ok"] != true {
		t.Fatalf("恢复设置失败: %v", o)
	}
	buf.Reset()
	slog.Debug("hidden again", "secret", "must-not-appear")
	if strings.Contains(buf.String(), "must-not-appear") {
		t.Fatal("调回 info 后 debug 应重新关闭")
	}

	// 非法值被拒绝，不会把 handler 置于未知状态
	_, o = h.rpc(t, "settings.update", Object{
		"version":  h.s.Config().Version,
		"settings": Object{"log_level": "verbose"},
	}, h.token)
	if o["ok"] != false {
		t.Fatalf("非法日志级别应被拒绝: %v", o)
	}
}

func TestManagementErrorDetailStaysOutOfDefaultLog(t *testing.T) {
	buf := &syncBuffer{}
	SetupLogging(buf)
	t.Cleanup(func() { SetupLogging(os.Stderr) })
	h := newHarness(t)
	// APIError 是预期内的业务错误，连 Error 行都不该产生
	buf.Reset()
	w, _ := h.rpc(t, "model.save", Object{"version": h.s.Config().Version, "model": Object{"id": "no-such-provider", "provider_id": "missing"}}, h.token)
	if w.Code < 400 {
		t.Fatal("前置条件失败：该请求应当出错")
	}
	if strings.Contains(buf.String(), "management call") {
		t.Fatalf("业务校验错误不应写入日志: %s", buf.String())
	}
}

// freePort 取一个当前空闲的端口号。端口 0 在产品里被拒绝（重启后会漂移），
// 所以测试必须使用具体端口。
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	return port
}

func TestListenAddressValidation(t *testing.T) {
	ok := []string{"127.0.0.1:8080", "localhost:80", "0.0.0.0:8443", "[::1]:8080", ":8080"}
	for _, a := range ok {
		if err := validateListenAddr(a); err != nil {
			t.Fatalf("%q 应合法: %v", a, err)
		}
	}
	bad := []string{"127.0.0.1", "", "example.com:80", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:abc"}
	for _, a := range bad {
		if validateListenAddr(a) == nil {
			t.Fatalf("%q 应被拒绝", a)
		}
	}
	for _, a := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if !loopbackAddr(a) {
			t.Fatalf("%q 应判为回环", a)
		}
	}
	for _, a := range []string{"0.0.0.0:8080", "192.168.1.5:8080", ":8080"} {
		if loopbackAddr(a) {
			t.Fatalf("%q 不应判为回环", a)
		}
	}
}

func TestListenSwitchRespectsAllowRemote(t *testing.T) {
	h := newHarness(t)
	srv := &http.Server{Handler: h.a}
	h.a.Listen = NewListener(srv, false, nil)
	ln, err := h.a.Listen.Bind("127.0.0.1:" + freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	h.a.Listen.Adopt(ln)
	t.Cleanup(func() { srv.Close() })
	first := h.a.Listen.Addr()

	// 未带 --allow-remote 时，后台不能把服务暴露到非回环地址
	_, o := h.rpc(t, "settings.update", Object{
		"version": h.s.Config().Version, "settings": Object{"listen": "0.0.0.0:" + freePort(t)},
	}, h.token)
	if str(obj(o["error"]), "code") != "REMOTE_NOT_ALLOWED" {
		t.Fatalf("对外监听应被拒绝: %v", o)
	}
	if h.a.Listen.Addr() != first {
		t.Fatal("被拒绝的切换不应影响当前监听")
	}
	if strings.HasPrefix(h.s.Config().Settings.Listen, "0.0.0.0:") {
		t.Fatal("被拒绝的切换不应写入配置")
	}

	// 地址非法同样是配置与监听都不变
	_, o = h.rpc(t, "settings.update", Object{
		"version": h.s.Config().Version, "settings": Object{"listen": "not-an-address"},
	}, h.token)
	if o["ok"] != false {
		t.Fatalf("非法地址应被拒绝: %v", o)
	}
	if h.a.Listen.Addr() != first {
		t.Fatal("非法地址不应影响当前监听")
	}

	// 合法的回环地址切换成功，且新地址真的在服务
	_, o = h.rpc(t, "settings.update", Object{
		"version": h.s.Config().Version, "settings": Object{"listen": "127.0.0.1:" + freePort(t)},
	}, h.token)
	if o["ok"] != true {
		t.Fatalf("回环地址切换应成功: %v", o)
	}
	second := h.a.Listen.Addr()
	if second == first {
		t.Fatal("监听地址未实际切换")
	}
	resp, err := http.Get("http://" + second + "/healthz")
	if err != nil {
		t.Fatalf("新监听地址不可用: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("新地址 healthz = %d", resp.StatusCode)
	}
	if _, err = net.DialTimeout("tcp", first, time.Second); err == nil {
		t.Fatal("旧监听地址应已关闭")
	}
}

func TestListenSwitchAllowedWithRemoteFlag(t *testing.T) {
	h := newHarness(t)
	srv := &http.Server{Handler: h.a}
	h.a.Listen = NewListener(srv, true, nil)
	ln, err := h.a.Listen.Bind("127.0.0.1:" + freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	h.a.Listen.Adopt(ln)
	t.Cleanup(func() { srv.Close() })
	_, o := h.rpc(t, "settings.update", Object{
		"version": h.s.Config().Version, "settings": Object{"listen": "0.0.0.0:" + freePort(t)},
	}, h.token)
	if o["ok"] != true {
		t.Fatalf("带 --allow-remote 时应允许: %v", o)
	}
}

func TestSavingOtherSettingsKeepsRescueListener(t *testing.T) {
	h := newHarness(t)
	srv := &http.Server{Handler: h.a}
	h.a.Listen = NewListener(srv, false, nil)
	ln, err := h.a.Listen.Bind("127.0.0.1:" + freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	h.a.Listen.Adopt(ln)
	t.Cleanup(func() { srv.Close() })

	// 配置里写的是默认 127.0.0.1:8080，实际监听在别处：等同 --listen 救援覆盖的状态。
	// 此时保存其它设置（表单会连同未改动的 listen 一起提交）不得把服务拽回坏地址。
	before := h.a.Listen.Addr()
	cfgListen := h.s.Config().Settings.Listen
	if cfgListen == before {
		t.Fatal("前置条件失败：配置地址应与实际监听不同")
	}
	_, o := h.rpc(t, "settings.update", Object{
		"version":  h.s.Config().Version,
		"settings": Object{"app_name": "救援中", "listen": cfgListen},
	}, h.token)
	if o["ok"] != true {
		t.Fatalf("保存其它设置应成功: %v", o)
	}
	if h.a.Listen.Addr() != before {
		t.Fatalf("监听被意外切换: %s -> %s", before, h.a.Listen.Addr())
	}
	if h.s.Config().Settings.AppName != "救援中" {
		t.Fatal("其它设置未生效")
	}
}

func TestSameListenNormalisesWildcards(t *testing.T) {
	same := [][2]string{{":8080", "0.0.0.0:8080"}, {"[::]:8080", ":8080"}, {"127.0.0.1:80", "127.0.0.1:80"}}
	for _, p := range same {
		if !sameListen(p[0], p[1]) {
			t.Fatalf("%q 与 %q 应视为同一监听点", p[0], p[1])
		}
	}
	diff := [][2]string{{":8080", ":8081"}, {"127.0.0.1:80", "192.168.1.1:80"}, {"bad", ":80"}}
	for _, p := range diff {
		if sameListen(p[0], p[1]) {
			t.Fatalf("%q 与 %q 不应视为同一监听点", p[0], p[1])
		}
	}
}

func TestMetricsDisabledByDefaultAndRequiresAdmin(t *testing.T) {
	h := newHarness(t)
	get := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://localhost/metrics", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		return w
	}
	// 默认关闭：连端点存在这件事都不暴露
	if w := get(h.token); w.Code != 404 {
		t.Fatalf("默认应为 404，得到 %d", w.Code)
	}
	if _, err := h.s.Change(h.s.Config().Version, "test", "test", func(c *Config) error {
		c.Settings.MetricsEnabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := get(""); w.Code != 401 {
		t.Fatalf("开启后匿名抓取应 401，得到 %d", w.Code)
	}
	if w := get("prism_admin_wrong_token_value_padding"); w.Code != 401 {
		t.Fatalf("错误令牌应 401，得到 %d", w.Code)
	}
	w := get(h.token)
	if w.Code != 200 {
		t.Fatalf("管理员抓取应 200，得到 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("Content-Type 应为 Prometheus 文本: %s", w.Header().Get("Content-Type"))
	}
	for _, want := range []string{
		"# TYPE prism_build_info gauge",
		`prism_build_info{version="` + Version + `"} 1`,
		"# TYPE prism_uptime_seconds gauge",
		"# TYPE prism_requests_total counter",
		"prism_config_version",
		"prism_active_requests",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("指标缺少 %q\n%s", want, body)
		}
	}
	// 每个非注释行必须是 name{labels} value 形式
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !regexp.MustCompile(`^[a-z_]+(\{[^}]*\})? -?\d+$`).MatchString(line) {
			t.Fatalf("非法指标行: %q", line)
		}
	}
	if strings.Contains(body, h.token) {
		t.Fatal("指标中不得出现管理员令牌")
	}
}

func TestMetricsCountsRealRequests(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Settings.MetricsEnabled = true
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
		c.Models = append(c.Models, modelFixture("m_metric", "chat"))
	})
	key := h.createKey(t)
	r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions",
		strings.NewReader(raw(Object{"model": "m_metric", "messages": []any{Object{"role": "user", "content": "hi"}}})))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("上游调用失败: %d %s", w.Code, w.Body)
	}
	mr := httptest.NewRequest("GET", "http://localhost/metrics", nil)
	mr.Header.Set("Authorization", "Bearer "+h.token)
	mw := httptest.NewRecorder()
	h.a.ServeHTTP(mw, mr)
	body := mw.Body.String()
	if !strings.Contains(body, `prism_requests_total{model="m_metric",protocol="chat",status="success"} 1`) {
		t.Fatalf("请求计数缺失:\n%s", body)
	}
	if !strings.Contains(body, `kind="input"} 7`) || !strings.Contains(body, `kind="output"} 3`) {
		t.Fatalf("token 计数缺失:\n%s", body)
	}
}

func TestBackupProducesUsableSnapshot(t *testing.T) {
	h := newHarness(t)
	const secret = "sk-backup-secret"
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "auto",
			BaseURL: "https://upstream.example.com/v1", Enabled: true, TimeoutSec: 30, Secret: secret})
		c.Models = append(c.Models, modelFixture("m_backup", "chat"))
	})
	w, o := h.rpc(t, "backup.create", Object{}, h.token)
	if w.Code != 200 || o["ok"] != true {
		t.Fatalf("备份失败: %s", w.Body)
	}
	data := obj(o["data"])
	path := str(data, "path")
	if num(data, "bytes") <= 0 {
		t.Fatal("备份文件为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("备份文件不可读: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("备份文件中出现了上游凭证明文")
	}

	// 没有配套 .key 时必须打不开：这正是「快照不含主密钥」的含义
	if _, err = OpenStore(path); err == nil {
		t.Fatal("缺少配套 .key 时不应能解密上游凭证")
	}
	// 配上原主密钥后，快照就是一个可直接使用的数据库
	master, err := os.ReadFile(h.s.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".key", master, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatalf("配套 .key 后仍无法打开备份: %v", err)
	}
	defer restored.DB.Close()
	cfg := restored.Config()
	if len(cfg.Models) != 1 || cfg.Models[0].ID != "m_backup" {
		t.Fatalf("备份内容不完整: %+v", cfg.Models)
	}
	if p, ok := cfg.provider("p_test"); !ok || p.Secret != secret {
		t.Fatal("配套主密钥后应能还原上游凭证")
	}

	_, o = h.rpc(t, "backup.list", Object{}, h.token)
	list := arr(o["data"])
	if len(list) != 1 || str(obj(list[0]), "path") != path {
		t.Fatalf("备份列表不正确: %v", list)
	}
}

func TestReadOnlyViewsAcrossWorkspace(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
		m := modelFixture("m_view", "chat")
		m.PricingSet, m.InputPrice, m.OutputPrice = true, 0.000001, 0.000002
		m.Limit5h, m.Limit7d, m.Limit30d = 1, 2, 3
		c.Models = append(c.Models, m)
	})
	key := h.createKey(t)
	r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions",
		strings.NewReader(raw(Object{"model": "m_view", "messages": []any{Object{"role": "user", "content": "hi"}}})))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Session", "view-session")
	w := httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("调用失败: %d %s", w.Code, w.Body)
	}

	dash := obj(mustData(t, h, "dashboard.get"))
	summary := obj(dash["summary"])
	if int64(num(summary, "requests")) != 1 {
		t.Fatalf("dashboard 请求数不对: %v", summary)
	}
	if arr(dash["series"]) == nil && arr(dash["timeseries"]) == nil {
		t.Fatalf("dashboard 缺少时间序列: %v", dash)
	}

	_, lo := h.rpc(t, "request.list", Object{}, h.token)
	list := lo["data"]
	rows := arr(obj(list)["items"])
	if rows == nil {
		rows = arr(list)
	}
	if len(rows) != 1 {
		t.Fatalf("请求记录应有 1 条: %v", list)
	}
	id := str(obj(rows[0]), "id")
	_, o := h.rpc(t, "request.get", Object{"id": id}, h.token)
	if o["ok"] != true {
		t.Fatalf("request.get 失败: %v", o)
	}

	_, qo := h.rpc(t, "quota.list", Object{}, h.token)
	quotas := arr(qo["data"])
	if len(quotas) != 1 || str(obj(quotas[0]), "mode") != "local_rolling_estimate" {
		t.Fatalf("额度视图不正确: %v", quotas)
	}
	info := obj(mustData(t, h, "system.info"))
	if obj(info["runtime"]) == nil {
		t.Fatalf("system.info 缺少运行时健康数据: %v", info)
	}
	_, so := h.rpc(t, "session.list", Object{}, h.token)
	if len(arr(so["data"])) != 1 {
		t.Fatal("会话亲和记录缺失")
	}
	for _, action := range []string{"audit.list", "job.list", "usage.summary", "usage.timeseries"} {
		if _, o := h.rpc(t, action, Object{}, h.token); o["ok"] != true {
			t.Fatalf("%s 失败: %v", action, o)
		}
	}
}

func TestProviderTestAndModelSync(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"data":[{"id":"minimax-m3"},{"id":"grok-4.6"},{"id":"plain-model"}]}`)
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_sync", Name: "Sync", Kind: "opencode", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
	})
	if _, o := h.rpc(t, "provider.test", Object{"id": "p_sync"}, h.token); o["ok"] != true {
		t.Fatalf("连接测试失败: %v", o)
	}
	_, o := h.rpc(t, "provider.sync_models", Object{"id": "p_sync"}, h.token)
	if o["ok"] != true {
		t.Fatalf("同步任务创建失败: %v", o)
	}
	jobID := str(obj(o["data"]), "job_id")
	if jobID == "" {
		t.Fatalf("未返回任务 id: %v", o)
	}
	var done Object
	for i := 0; i < 200; i++ {
		_, jo := h.rpc(t, "job.get", Object{"id": jobID}, h.token)
		done = obj(jo["data"])
		if st := str(done, "status"); st != "queued" && st != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if str(done, "status") != "succeeded" {
		t.Fatalf("同步任务未成功: %v", done)
	}
	cfg := h.s.Config()
	if len(cfg.Models) != 3 {
		t.Fatalf("应同步出 3 个模型: %d", len(cfg.Models))
	}
	for _, m := range cfg.Models {
		if m.Enabled {
			t.Fatalf("同步的模型 %s 必须默认禁用", m.ID)
		}
		if m.PricingSet {
			t.Fatalf("同步不得擅自认定 %s 的计价", m.ID)
		}
	}
	byUpstream := map[string]string{}
	for _, m := range cfg.Models {
		byUpstream[m.Upstream] = m.Protocol
	}
	if byUpstream["minimax-m3"] != "messages" {
		t.Fatalf("内置映射应把 minimax-m3 识别为 messages: %v", byUpstream)
	}
	if byUpstream["grok-4.6"] != "responses" {
		t.Fatalf("内置映射应把 grok-4.6 识别为 responses: %v", byUpstream)
	}
	if byUpstream["plain-model"] != "chat" {
		t.Fatalf("未知型号应落到 chat: %v", byUpstream)
	}
}

func TestSmallHelpersAndEmbeddedUI(t *testing.T) {
	if clamp(5, 1, 3) != 3 || clamp(0, 1, 3) != 1 || clamp(2, 1, 3) != 2 {
		t.Fatal("clamp 边界不正确")
	}
	err := fail("X", "boom", 400)
	if err.Error() != "boom" {
		t.Fatalf("APIError.Error 应返回消息: %q", err.Error())
	}
	if !Loopback("127.0.0.1:1") || Loopback("8.8.8.8:1") {
		t.Fatal("Loopback 判定不正确")
	}
	if sqlite.Version() == "" {
		t.Fatal("SQLite 版本不应为空")
	}
	row := sqlite.Row{"a": int64(3), "b": 1.5}
	if row.Float("a") != 3 || row.Float("b") != 1.5 || row.Float("missing") != 0 {
		t.Fatal("Row.Float 转换不正确")
	}
	w := httptest.NewRecorder()
	webui.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/assets/app.js", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "export function renderPage") {
		t.Fatalf("内嵌前端入口不可用: %d", w.Code)
	}
	w = httptest.NewRecorder()
	webui.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/no-such-asset.js", nil))
	if w.Code == 200 {
		t.Fatal("不存在的资源不应返回 200")
	}
}

// 会话亲和曾因 upsert 语句里写错表名而整条失败，且错误被丢弃，
// 结果 sessions 表一条记录都没有、亲和从未生效。这个测试锁住该路径。
func TestSessionAffinityActuallyPersists(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
		c.Models = append(c.Models, modelFixture("m_aff", "chat"))
	})
	key := h.createKey(t)
	call := func() {
		t.Helper()
		r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions",
			strings.NewReader(raw(Object{"model": "m_aff", "messages": []any{Object{"role": "user", "content": "hi"}}})))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Session", "same-conversation")
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("调用失败: %d %s", w.Code, w.Body)
		}
	}
	call()
	rows, err := h.s.DB.Query("SELECT id,model_id,requests FROM sessions")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("第一次调用后应有 1 条会话记录，实得 %d", len(rows))
	}
	if rows[0].String("model_id") != "m_aff" || rows[0].Int("requests") != 1 {
		t.Fatalf("会话记录内容不对: %v", rows[0])
	}
	// 同一会话再调一次，走 ON CONFLICT 分支
	call()
	rows, _ = h.s.DB.Query("SELECT id,requests FROM sessions")
	if len(rows) != 1 {
		t.Fatalf("同一会话不应新增记录，实得 %d 条", len(rows))
	}
	if rows[0].Int("requests") != 2 {
		t.Fatalf("冲突分支应把计数累加到 2，实得 %d", rows[0].Int("requests"))
	}
	// 不同会话独立成条
	r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions",
		strings.NewReader(raw(Object{"model": "m_aff", "messages": []any{Object{"role": "user", "content": "hi"}}})))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Session", "other-conversation")
	h.a.ServeHTTP(httptest.NewRecorder(), r)
	rows, _ = h.s.DB.Query("SELECT id FROM sessions")
	if len(rows) != 2 {
		t.Fatalf("不同会话应各自成条，实得 %d", len(rows))
	}
}

func TestConcurrencyCeilingAndCounterRelease(t *testing.T) {
	h := newHarness(t)
	var inflight, peak int64
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inflight, 1)
		mu.Lock()
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt64(&inflight, -1)
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	const limit = 4
	h.change(t, func(c *Config) {
		c.Settings.GlobalConcurrency = limit
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
		m := modelFixture("m_conc", "chat")
		m.Concurrency = 128
		c.Models = append(c.Models, m)
	})
	key := h.createKey(t)

	const callers = 40
	var ok, limited, other int64
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions",
				strings.NewReader(raw(Object{"model": "m_conc", "messages": []any{Object{"role": "user", "content": "hi"}}})))
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.a.ServeHTTP(w, r)
			switch {
			case w.Code == 200:
				atomic.AddInt64(&ok, 1)
			case w.Code == 429:
				atomic.AddInt64(&limited, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}()
	}
	wg.Wait()

	if other != 0 {
		t.Fatalf("并发下出现了非预期状态码，%d 次", other)
	}
	if ok+limited != callers {
		t.Fatalf("请求总数对不上: %d + %d != %d", ok, limited, callers)
	}
	if ok == 0 {
		t.Fatal("不应全部被限流")
	}
	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	if gotPeak > limit {
		t.Fatalf("同时在飞的上游请求 %d 超过全局上限 %d", gotPeak, limit)
	}
	// 计数器必须完全释放，否则网关会逐渐「假满」直到重启
	health := h.a.Engine.Health()
	if active, _ := health["active"].(int); active != 0 {
		t.Fatalf("全局并发计数未归零: %v", health["active"])
	}
	models := obj(health["models"])
	if m := obj(models["m_conc"]); m != nil {
		if active, _ := m["active"].(int); active != 0 {
			t.Fatalf("模型并发计数未归零: %v", m["active"])
		}
	}
}

func TestRPMLimitUnderConcurrency(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	const rpm = 5
	h.change(t, func(c *Config) {
		c.Settings.GlobalConcurrency = 64
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
		m := modelFixture("m_rpm", "chat")
		m.RPM, m.Concurrency = rpm, 64
		c.Models = append(c.Models, m)
	})
	key := h.createKey(t)
	var ok int64
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions",
				strings.NewReader(raw(Object{"model": "m_rpm", "messages": []any{Object{"role": "user", "content": "hi"}}})))
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.a.ServeHTTP(w, r)
			if w.Code == 200 {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok > rpm {
		t.Fatalf("放行 %d 次，超过 RPM 上限 %d", ok, rpm)
	}
	if ok == 0 {
		t.Fatal("RPM 限制不应把所有请求都挡掉")
	}
}

func TestManagementMutationsAndDependencyGuards(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	ver := func() int64 { return h.s.Config().Version }
	call := func(action string, p Object) Object {
		t.Helper()
		p["version"] = ver()
		w, o := h.rpc(t, action, p, h.token)
		if w.Code != 200 || o["ok"] != true {
			t.Fatalf("%s 失败: %s", action, w.Body)
		}
		return obj(o["data"])
	}
	reject := func(action string, p Object, wantCode string) {
		t.Helper()
		p["version"] = ver()
		_, o := h.rpc(t, action, p, h.token)
		if o["ok"] != false {
			t.Fatalf("%s 应被拒绝: %v", action, o)
		}
		if got := str(obj(o["error"]), "code"); wantCode != "" && got != wantCode {
			t.Fatalf("%s 错误码为 %s，期望 %s", action, got, wantCode)
		}
	}

	call("provider.save", Object{"provider": Object{"name": "Up", "kind": "custom", "auth": "none",
		"base_url": upstream.URL, "allow_private": true, "enabled": true, "timeout_sec": 30, "api_key": "sk-x"}})
	pid := h.s.Config().Providers[0].ID
	call("model.save", Object{"model": Object{"id": "m_one", "provider_id": pid, "upstream": "up-one",
		"name": "One", "protocol": "chat", "enabled": true, "context_window": 128000,
		"max_output_tokens": 4096, "concurrency": 4}})
	call("model.save", Object{"model": Object{"id": "m_two", "provider_id": pid, "upstream": "up-two",
		"name": "Two", "protocol": "chat", "enabled": true, "context_window": 128000,
		"max_output_tokens": 4096, "concurrency": 4}})
	call("route.save", Object{"route": Object{"id": "r_auto", "name": "Auto", "strategy": "priority",
		"enabled": true, "affinity": true, "candidates": []any{
			Object{"model_id": "m_one", "weight": 10}, Object{"model_id": "m_two", "weight": 5}}}})
	call("alias.save", Object{"alias": Object{"id": "a_auto", "target": "r_auto", "enabled": true}})

	// 静态模拟选择不产生真实调用
	sim := call("route.test", Object{"id": "r_auto"})
	if sim == nil {
		t.Fatal("route.test 应返回模拟结果")
	}
	// 连接测试走真实 HTTP，但不消耗模型额度
	call("provider.test", Object{"id": pid})

	// 依赖仍在时不允许删除，避免留下悬空引用
	reject("provider.delete", Object{"id": pid}, "")
	reject("model.delete", Object{"id": "m_one"}, "")
	reject("alias.save", Object{"alias": Object{"id": "a_bad", "target": "nowhere", "enabled": true}}, "INVALID_CONFIG")
	reject("route.save", Object{"route": Object{"id": "r_bad", "name": "B", "strategy": "priority",
		"enabled": true, "candidates": []any{Object{"model_id": "ghost", "weight": 1}}}}, "INVALID_CONFIG")
	reject("model.save", Object{"model": Object{"id": "m_bad", "provider_id": "ghost", "upstream": "x",
		"protocol": "chat", "context_window": 1000, "max_output_tokens": 100, "concurrency": 1}}, "INVALID_CONFIG")

	// 按依赖顺序拆除
	call("alias.delete", Object{"id": "a_auto"})
	call("route.delete", Object{"id": "r_auto"})
	call("model.delete", Object{"id": "m_one"})
	call("model.delete", Object{"id": "m_two"})
	call("provider.delete", Object{"id": pid})
	cfg := h.s.Config()
	if len(cfg.Providers)+len(cfg.Models)+len(cfg.Routes)+len(cfg.Aliases) != 0 {
		t.Fatalf("对象未清空: %+v", cfg)
	}

	// 未知 action 与不存在的对象
	reject("no.such.action", Object{}, "UNKNOWN_ACTION")
	reject("model.delete", Object{"id": "ghost"}, "")
	reject("job.get", Object{"id": "ghost"}, "NOT_FOUND")
	reject("provider.test", Object{"id": "ghost"}, "NOT_FOUND")
}

func TestPlaygroundAndDemoLifecycle(t *testing.T) {
	h := newHarness(t)
	w, o := h.rpc(t, "demo.enable", Object{"version": h.s.Config().Version}, h.token)
	if w.Code != 200 || o["ok"] != true {
		t.Fatalf("启用演示失败: %s", w.Body)
	}
	if len(h.s.Config().Models) != 3 {
		t.Fatalf("演示应带来 3 个模型: %d", len(h.s.Config().Models))
	}
	// 重复启用是幂等的
	h.rpc(t, "demo.enable", Object{"version": h.s.Config().Version}, h.token)
	if len(h.s.Config().Models) != 3 {
		t.Fatal("重复启用演示不应重复添加模型")
	}
	for _, proto := range []string{"chat", "messages", "responses"} {
		_, o := h.rpc(t, "playground.run", Object{
			"model": "demo-" + proto, "protocol": proto, "prompt": "hello", "stream": false,
		}, h.token)
		if o["ok"] != true {
			t.Fatalf("调试台 %s 失败: %v", proto, o)
		}
		body := raw(o["data"])
		if !strings.Contains(body, "本地演示") && !strings.Contains(body, "DEMO") && !strings.Contains(body, "demo") {
			t.Fatalf("调试台结果未标注演示来源: %s", body)
		}
	}
	// 演示 Provider 不支持同步模型
	_, o = h.rpc(t, "provider.sync_models", Object{"id": "local-demo"}, h.token)
	if str(obj(o["error"]), "code") != "NOT_SUPPORTED" {
		t.Fatalf("演示供应商不应允许同步: %v", o)
	}
	// 解绑会话是幂等操作：重复调用或对不存在的 id 调用都不报错
	_, o = h.rpc(t, "session.delete", Object{"id": "ses_nonexistent"}, h.token)
	if o["ok"] != true {
		t.Fatalf("解绑会话应幂等: %v", o)
	}
}

func TestPruneRemovesExpiredRows(t *testing.T) {
	h := newHarness(t)
	old := now() - 400*86400000
	if err := h.s.DB.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,reason) VALUES ('r_old','p','k','m','m','p','chat','chat','s','success',?,'')`, old); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.Exec(`INSERT INTO admin_sessions VALUES ('dead','csrf',?)`, now()-1000); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.Exec(`INSERT INTO sessions VALUES ('ses_old','k','m','p',?,1)`, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go h.a.Engine.Prune(ctx)
	defer cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := h.s.DB.Query("SELECT COUNT(*) n FROM requests")
		adm, _ := h.s.DB.Query("SELECT COUNT(*) n FROM admin_sessions")
		ses, _ := h.s.DB.Query("SELECT COUNT(*) n FROM sessions")
		if req[0].Int("n") == 0 && adm[0].Int("n") == 0 && ses[0].Int("n") == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("过期记录未在预期时间内清理")
}

func TestImageAndBlockConversionAcrossProtocols(t *testing.T) {
	const png = "data:image/png;base64,iVBORw0KGgo="
	// 三种协议各自的图片表达，解码后应落到同一套规范结构
	cases := []struct{ protocol, body string }{
		{"chat", raw(Object{"model": "m", "messages": []any{Object{"role": "user", "content": []any{
			Object{"type": "text", "text": "看图"},
			Object{"type": "image_url", "image_url": Object{"url": png}}}}}})},
		{"messages", raw(Object{"model": "m", "max_tokens": 16, "messages": []any{Object{"role": "user", "content": []any{
			Object{"type": "text", "text": "看图"},
			Object{"type": "image", "source": Object{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}}}}})},
		{"responses", raw(Object{"model": "m", "max_output_tokens": 16, "input": []any{Object{"role": "user", "content": []any{
			Object{"type": "input_text", "text": "看图"},
			Object{"type": "input_image", "image_url": png}}}}})},
	}
	for _, c := range cases {
		var o Object
		if err := json.Unmarshal([]byte(c.body), &o); err != nil {
			t.Fatal(err)
		}
		canon, err := decodeCanonical(o, c.protocol)
		if err != nil {
			t.Fatalf("%s 图片请求解码失败: %v", c.protocol, err)
		}
		// 转换到另外两种协议都必须保留文本与图片两个块
		for _, target := range []string{"chat", "messages", "responses"} {
			out, err := encodeCanonical(canon, target, "m")
			if err != nil {
				t.Fatalf("%s -> %s 转换失败: %v", c.protocol, target, err)
			}
			s := raw(out)
			if !strings.Contains(s, "看图") {
				t.Fatalf("%s -> %s 丢失文本: %s", c.protocol, target, s)
			}
			if !strings.Contains(s, "iVBORw0KGgo=") {
				t.Fatalf("%s -> %s 丢失图片数据: %s", c.protocol, target, s)
			}
		}
	}
	// 远程图片 URL 在 Anthropic 侧是 url 源，不应被伪造成 base64
	if src, err := imageSource("https://example.com/a.png"); err != nil {
		t.Fatalf("远程图片应被接受: %v", err)
	} else if str(src, "type") != "url" {
		t.Fatalf("远程图片应保持 url 源: %v", src)
	}
	if src, err := imageSource(png); err != nil {
		t.Fatalf("data URL 应被接受: %v", err)
	} else if str(src, "type") != "base64" || str(src, "media_type") != "image/png" {
		t.Fatalf("data URL 应解析出 base64 与媒体类型: %v", src)
	}
	for _, bad := range []string{"", "ftp://x/a.png", "data:image/png,notbase64", "javascript:alert(1)"} {
		if _, err := imageSource(bad); err == nil {
			t.Fatalf("非法图片来源 %q 应被拒绝", bad)
		}
	}
}

func TestUnsupportedFeaturesRejectedNotSilentlyDropped(t *testing.T) {
	// 跨协议不支持的能力必须显式报错，不能悄悄丢掉语义
	cases := []struct{ protocol, body string }{
		{"chat", raw(Object{"model": "m", "messages": []any{Object{"role": "user", "content": "hi"}}, "n": 3})},
		{"chat", raw(Object{"model": "m", "messages": []any{Object{"role": "user", "content": []any{
			Object{"type": "input_audio", "input_audio": Object{"data": "AA", "format": "wav"}}}}}})},
		{"messages", raw(Object{"model": "m", "max_tokens": 8, "messages": []any{Object{"role": "user", "content": []any{
			Object{"type": "document", "source": Object{"type": "base64", "media_type": "application/pdf", "data": "AA"}}}}}})},
		{"responses", raw(Object{"model": "m", "input": "hi", "background": true})},
	}
	rejected := 0
	for _, c := range cases {
		var o Object
		if err := json.Unmarshal([]byte(c.body), &o); err != nil {
			t.Fatal(err)
		}
		if _, err := decodeCanonical(o, c.protocol); err != nil {
			rejected++
			var ae *APIError
			if !errors.As(err, &ae) || ae.Code != "UNSUPPORTED_FEATURE" {
				continue // 其它明确错误也算拒绝
			}
		}
	}
	if rejected == 0 {
		t.Fatal("至少应有一类不支持的能力被显式拒绝")
	}
	if err := unsupported("测试"); err == nil {
		t.Fatal("unsupported 应产生错误")
	}
}

// 真实上游（OpenRouter 上的 DeepSeek / Nemotron 等）会在 OpenAI 响应里附带
// reasoning 与 reasoning_details。跨协议转换必须拒绝——Anthropic 的 thinking
// 需要签名，凭空造一个就是伪造推理。但拒绝的理由必须说清楚，
// 不能报成「上游请求失败」让人去查上游。
func TestReasoningBearingResponseIsRejectedWithClearReason(t *testing.T) {
	upstreamBody := `{"id":"gen-1","object":"chat.completion","model":"deepseek/deepseek-v4-flash-0731:free",
	 "choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello!",
	 "refusal":null,"reasoning":"We need answer. User says hi.",
	 "reasoning_details":[{"type":"reasoning.text","text":"We need answer."}]}}],
	 "usage":{"prompt_tokens":9,"completion_tokens":20}}`

	var ro Object
	if err := json.Unmarshal([]byte(upstreamBody), &ro); err != nil {
		t.Fatal(err)
	}
	// 规范结构表达不了 reasoning，所以解码一定失败。
	// 原生路径不走这里（响应原样透传），只有跨协议才会碰到。
	if _, err := decodeCompletion(ro, "chat"); err == nil {
		t.Fatal("含 reasoning 的响应不应被解码成规范结构")
	}

	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
		c.Models = append(c.Models, modelFixture("m_reason", "chat"))
	})
	key := h.createKey(t)

	// OpenAI 客户端调 chat 模型：原生路径，必须成功
	r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions",
		strings.NewReader(raw(Object{"model": "m_reason", "messages": []any{Object{"role": "user", "content": "hi"}}})))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("原生协议应当成功: %d %s", w.Code, w.Body)
	}

	// Anthropic 客户端调同一个 chat 模型：跨协议，必须被拒绝且理由明确
	r = httptest.NewRequest("POST", "http://localhost/anthropic/v1/messages",
		strings.NewReader(raw(Object{"model": "m_reason", "max_tokens": 64,
			"messages": []any{Object{"role": "user", "content": "hi"}}})))
	r.Header.Set("x-api-key", key)
	r.Header.Set("anthropic-version", "2023-06-01")
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("跨协议不得静默丢弃 reasoning")
	}
	body := w.Body.String()
	if strings.Contains(body, "上游请求失败") {
		t.Fatalf("错误把转换问题说成上游故障，会把人引向错误的排查方向: %s", body)
	}
	for _, want := range []string{"reasoning", "原生协议"} {
		if !strings.Contains(body, want) {
			t.Fatalf("错误信息应说明原因与出路，缺少 %q: %s", want, body)
		}
	}
}

// 网关几乎总是跑在反向代理后面，TLS 在代理侧终止。若只看 r.TLS，
// HTTPS 部署下的会话 Cookie 反而会丢掉 Secure 标志。
func TestSessionCookieSecureBehindProxy(t *testing.T) {
	h := newHarness(t)
	login := func(remote, proto string, tls bool) *http.Cookie {
		t.Helper()
		r := httptest.NewRequest("POST", "http://localhost/api.json",
			strings.NewReader(raw(Object{"action": "auth.login", "params": Object{"token": h.token}})))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = remote
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		if tls {
			r.TLS = &tlsConnectionState
		}
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("登录失败: %d %s", w.Code, w.Body)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == "prism_session" {
				return c
			}
		}
		t.Fatal("未返回会话 Cookie")
		return nil
	}

	if c := login("127.0.0.1:5555", "https", false); !c.Secure {
		t.Fatal("回环代理转发的 HTTPS 请求，Cookie 必须带 Secure")
	}
	if c := login("[::1]:5555", "https", false); !c.Secure {
		t.Fatal("IPv6 回环同样应被信任")
	}
	if c := login("127.0.0.1:5555", "", false); c.Secure {
		t.Fatal("没有 X-Forwarded-Proto 时不应假定 HTTPS")
	}
	if c := login("127.0.0.1:5555", "http", false); c.Secure {
		t.Fatal("明文转发不应带 Secure")
	}
	// 公网客户端可以随意伪造这个头，不能作数
	if c := login("203.0.113.9:5555", "https", false); c.Secure {
		t.Fatal("非回环来源的 X-Forwarded-Proto 不得被信任")
	}
	// 网关自己终止 TLS 时无需该头
	if c := login("203.0.113.9:5555", "", true); !c.Secure {
		t.Fatal("直连 TLS 时 Cookie 必须带 Secure")
	}
}

var tlsConnectionState = tls.ConnectionState{HandshakeComplete: true}

// drop_reasoning 是管理员对单个模型显式作出的取舍：接受丢弃推理内容，
// 换取跨协议可用。默认必须关闭，且丢弃时必须在响应头标注——
// 「不静默丢弃」的关键在于「不静默」，而不是「不丢弃」。
func TestDropReasoningIsOptInAndAnnounced(t *testing.T) {
	const withReasoning = `{"id":"g","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop",
	 "message":{"role":"assistant","content":"Hello!","reasoning":"internal thought",
	 "reasoning_details":[{"type":"reasoning.text","text":"internal"}]}}],
	 "usage":{"prompt_tokens":5,"completion_tokens":7}}`

	setup := func(t *testing.T, drop bool, body string) *harness {
		t.Helper()
		h := newHarness(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		}))
		t.Cleanup(upstream.Close)
		h.change(t, func(c *Config) {
			c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
				BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
			m := modelFixture("m_drop", "chat")
			m.DropReasoning = drop
			c.Models = append(c.Models, m)
		})
		return h
	}
	call := func(h *harness) *httptest.ResponseRecorder {
		key := h.createKey(t)
		r := httptest.NewRequest("POST", "http://localhost/anthropic/v1/messages",
			strings.NewReader(raw(Object{"model": "m_drop", "max_tokens": 64,
				"messages": []any{Object{"role": "user", "content": "hi"}}})))
		r.Header.Set("x-api-key", key)
		r.Header.Set("anthropic-version", "2023-06-01")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		return w
	}

	// 默认关闭：仍然拒绝
	w := call(setup(t, false, withReasoning))
	if w.Code == 200 {
		t.Fatal("未开启开关时不得丢弃推理内容")
	}
	if w.Header().Get("X-Prism-Dropped") != "" {
		t.Fatal("没有丢弃就不该标注")
	}

	// 开启：成功，且必须标注
	w = call(setup(t, true, withReasoning))
	if w.Code != 200 {
		t.Fatalf("开启后应当成功: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("X-Prism-Dropped") != "reasoning" {
		t.Fatalf("丢弃必须标注，得到 %q", w.Header().Get("X-Prism-Dropped"))
	}
	body := w.Body.String()
	if !strings.Contains(body, "Hello!") {
		t.Fatalf("正文内容丢失: %s", body)
	}
	if strings.Contains(body, "internal thought") {
		t.Fatal("推理内容不应出现在转换结果里")
	}

	// 上游没有推理内容时，开着开关也不该平白标注
	const clean = `{"id":"g","choices":[{"index":0,"finish_reason":"stop",
	 "message":{"role":"assistant","content":"Hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	w = call(setup(t, true, clean))
	if w.Code != 200 {
		t.Fatalf("干净响应应当成功: %d", w.Code)
	}
	if w.Header().Get("X-Prism-Dropped") != "" {
		t.Fatal("没有实际丢弃时不应标注")
	}

	// 开关只针对推理内容，其它不可转换内容照旧拒绝
	const refusal = `{"id":"g","choices":[{"index":0,"finish_reason":"stop",
	 "message":{"role":"assistant","content":"","refusal":"I cannot help"}}],"usage":{}}`
	if w = call(setup(t, true, refusal)); w.Code == 200 {
		t.Fatal("refusal 不在开关覆盖范围内，必须仍然拒绝")
	}
}

func TestStripReasoningAcrossProtocols(t *testing.T) {
	chat := Object{"choices": []any{Object{"message": Object{
		"content": "x", "reasoning": "r", "reasoning_details": []any{Object{"text": "r"}}}}}}
	if !stripReasoning(chat, "chat") {
		t.Fatal("chat 应报告发生了剥离")
	}
	m := obj(obj(arr(chat["choices"])[0])["message"])
	for _, k := range []string{"reasoning", "reasoning_content", "reasoning_details"} {
		if _, ok := m[k]; ok {
			t.Fatalf("chat 残留字段 %s", k)
		}
	}
	if m["content"] != "x" {
		t.Fatal("正文不应被动到")
	}

	msg := Object{"content": []any{
		Object{"type": "thinking", "thinking": "t"},
		Object{"type": "text", "text": "keep"},
		Object{"type": "redacted_thinking", "data": "z"}}}
	if !stripReasoning(msg, "messages") {
		t.Fatal("messages 应报告发生了剥离")
	}
	if left := arr(msg["content"]); len(left) != 1 || str(obj(left[0]), "type") != "text" {
		t.Fatalf("messages 剥离结果不对: %v", left)
	}

	resp := Object{"output": []any{
		Object{"type": "reasoning", "summary": []any{}},
		Object{"type": "message", "content": []any{}}}}
	if !stripReasoning(resp, "responses") {
		t.Fatal("responses 应报告发生了剥离")
	}
	if left := arr(resp["output"]); len(left) != 1 || str(obj(left[0]), "type") != "message" {
		t.Fatalf("responses 剥离结果不对: %v", left)
	}

	// 没有推理内容时必须返回 false，否则会产生无意义的标注和重试
	if stripReasoning(Object{"choices": []any{Object{"message": Object{"content": "x"}}}}, "chat") {
		t.Fatal("无推理内容时不应报告剥离")
	}
}

// 流式没法在发现推理内容之后再补响应头，所以开关开启且跨协议时必须先声明。
// 这条断言曾经漏掉过：声明被加到了演示模式的分支上，真实上游路径没有。
func TestDropReasoningAnnouncedOnConvertedStream(t *testing.T) {
	sse := "data: " + raw(Object{"id": "x", "choices": []any{Object{"index": 0,
		"delta": Object{"role": "assistant", "reasoning": "thinking"}}}}) + "\n\n" +
		"data: " + raw(Object{"id": "x", "choices": []any{Object{"index": 0,
		"delta": Object{"content": "Hello"}}}}) + "\n\n" +
		"data: " + raw(Object{"id": "x", "choices": []any{Object{"index": 0,
		"delta": Object{}, "finish_reason": "stop"}}}) + "\n\ndata: [DONE]\n\n"

	run := func(drop bool) *httptest.ResponseRecorder {
		h := newHarness(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sse)
		}))
		t.Cleanup(upstream.Close)
		h.change(t, func(c *Config) {
			c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
				BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
			m := modelFixture("m_stream_drop", "chat")
			m.DropReasoning = drop
			c.Models = append(c.Models, m)
		})
		key := h.createKey(t)
		r := httptest.NewRequest("POST", "http://localhost/anthropic/v1/messages",
			strings.NewReader(raw(Object{"model": "m_stream_drop", "max_tokens": 64, "stream": true,
				"messages": []any{Object{"role": "user", "content": "hi"}}})))
		r.Header.Set("x-api-key", key)
		r.Header.Set("anthropic-version", "2023-06-01")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		return w
	}

	w := run(true)
	if got := w.Header().Get("X-Prism-Dropped"); got != "reasoning" {
		t.Fatalf("跨协议流式开启丢弃时必须先声明，得到 %q", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("流未正常结束: %s", body)
	}
	if strings.Contains(body, "thinking") {
		t.Fatal("推理内容不应出现在转换后的流里")
	}
	if !strings.Contains(body, "Hello") {
		t.Fatalf("正文内容丢失: %s", body)
	}

	// 未开启时不声明，并且流会以错误帧结束而不是悄悄少掉内容
	w = run(false)
	if got := w.Header().Get("X-Prism-Dropped"); got != "" {
		t.Fatalf("未开启不应声明，得到 %q", got)
	}
	if !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("未开启时应以错误帧结束: %s", w.Body.String())
	}
}

// Claude Code 真实发出的请求里带着 metadata、context_management、output_config、
// thinking 这些字段。前三个不影响生成语义，第四个影响。
// 整个请求因此被拒绝，而错误只说「没有兼容此请求的候选模型」，
// 连是哪个字段都不讲——这是这条测试要挡住的两件事。
func TestClaudeCodeShapedRequestIsAcceptedAndDisclosed(t *testing.T) {
	body := func() Object {
		return Object{
			"model": "m_cc", "max_tokens": 256,
			"system": []any{Object{"type": "text", "text": "You are helpful",
				"cache_control": Object{"type": "ephemeral"}}},
			"messages":           []any{Object{"role": "user", "content": "hi"}},
			"metadata":           Object{"user_id": "device-abc"},
			"context_management": Object{"edits": []any{Object{"type": "clear_thinking_20251015"}}},
			"output_config":      Object{"effort": "high"},
			"thinking":           Object{"type": "adaptive"},
		}
	}
	setup := func(t *testing.T, drop bool) *harness {
		t.Helper()
		h := newHarness(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ := io.ReadAll(r.Body)
			var got Object
			json.Unmarshal(payload, &got)
			// 被忽略的字段绝不能泄漏到上游请求里
			for _, k := range []string{"metadata", "context_management", "output_config", "thinking"} {
				if got[k] != nil {
					t.Errorf("字段 %s 不应转发给上游", k)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"x","choices":[{"index":0,"finish_reason":"stop",
			 "message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
		}))
		t.Cleanup(upstream.Close)
		h.change(t, func(c *Config) {
			c.Providers = append(c.Providers, Provider{ID: "p_test", Name: "T", Kind: "custom", Auth: "none",
				BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
			m := modelFixture("m_cc", "chat")
			m.DropReasoning = drop
			c.Models = append(c.Models, m)
		})
		return h
	}
	call := func(h *harness) *httptest.ResponseRecorder {
		key := h.createKey(t)
		r := httptest.NewRequest("POST", "http://localhost/anthropic/v1/messages", strings.NewReader(raw(body())))
		r.Header.Set("x-api-key", key)
		r.Header.Set("anthropic-version", "2023-06-01")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		return w
	}

	// 开启 drop_reasoning：请求应当被接受，且必须列清楚忽略了什么
	w := call(setup(t, true))
	if w.Code != 200 {
		t.Fatalf("Claude Code 形状的请求应当可用: %d %s", w.Code, w.Body)
	}
	ignored := w.Header().Get("X-Prism-Ignored")
	for _, want := range []string{"thinking", "metadata", "context_management", "output_config"} {
		if !strings.Contains(ignored, want) {
			t.Fatalf("X-Prism-Ignored 应包含 %s，实得 %q", want, ignored)
		}
	}

	// 未开启时 thinking 会改变行为，必须拒绝；而且要说清楚是哪个字段
	w = call(setup(t, false))
	if w.Code == 200 {
		t.Fatal("未开启 drop_reasoning 时 thinking 不应被悄悄忽略")
	}
	msg := w.Body.String()
	if !strings.Contains(msg, "thinking") {
		t.Fatalf("错误必须点名是哪个字段导致不兼容: %s", msg)
	}
	if !strings.Contains(msg, "m_cc") {
		t.Fatalf("错误应指出是哪个候选模型被排除: %s", msg)
	}
}

// 同步过来的模型此前只有 id 和名字，其余全是硬编码：上下文 128000、
// 输出上限 4096、工具 true。4096 意味着同步来的模型连 Claude Code 都跑不了，
// 于是「同步」形同虚设，只能一个个手工补齐。
func TestSyncCarriesUpstreamCapabilities(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[
		  {"id":"vendor/big-model","name":"Vendor: Big Model","context_length":1048576,
		   "architecture":{"input_modalities":["text","image"]},
		   "pricing":{"prompt":"0.00000045","completion":"0.0000009","input_cache_read":"0.0000001"},
		   "top_provider":{"max_completion_tokens":384000},
		   "supported_parameters":["max_tokens","tools","temperature"]},
		  {"id":"vendor/plain-model","name":"Vendor: Plain","context_length":8192,
		   "architecture":{"input_modalities":["text"]},
		   "pricing":{"prompt":"0","completion":"0"},
		   "supported_parameters":["max_tokens"]}
		]}`)
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_sync", Name: "S", Kind: "custom", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
	})
	_, o := h.rpc(t, "provider.sync_models", Object{"id": "p_sync"}, h.token)
	jobID := str(obj(o["data"]), "job_id")
	for i := 0; i < 200; i++ {
		_, jo := h.rpc(t, "job.get", Object{"id": jobID}, h.token)
		if st := str(obj(jo["data"]), "status"); st != "queued" && st != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	byID := map[string]Model{}
	for _, m := range h.s.Config().Models {
		byID[m.ID] = m
	}
	big, ok := byID["vendor/big-model"]
	if !ok {
		t.Fatalf("模型 ID 应直接用上游原名: %v", byID)
	}
	if big.Name != "Vendor: Big Model" {
		t.Fatalf("名称未同步: %q", big.Name)
	}
	if big.Context != 1048576 {
		t.Fatalf("上下文未同步: %d", big.Context)
	}
	if big.MaxOutput != 384000 {
		t.Fatalf("输出上限未同步: %d", big.MaxOutput)
	}
	if !big.Tools || !big.Vision {
		t.Fatalf("能力未同步: tools=%v vision=%v", big.Tools, big.Vision)
	}
	// 单价按每百万 token 存，上游给的是每 token
	if big.InputPrice != 0.45 || big.OutputPrice != 0.9 || big.CachePrice != 0.1 {
		t.Fatalf("单价换算不对: %v/%v/%v", big.InputPrice, big.OutputPrice, big.CachePrice)
	}
	// 关键约束：价格抄来了，但没有人确认过，不能当作已知费用
	if big.PricingSet {
		t.Fatal("同步不得代替人确认计价")
	}
	if big.Enabled {
		t.Fatal("同步的模型必须默认禁用")
	}

	plain := byID["vendor/plain-model"]
	if plain.Tools || plain.Vision {
		t.Fatalf("不支持的能力不应被标为支持: %+v", plain)
	}
	// 上游没声明输出上限时按上下文估，而不是退回 4096
	if plain.MaxOutput != 8192 {
		t.Fatalf("输出上限回退不对: %d", plain.MaxOutput)
	}

	// 同步来的模型应当可以直接启用并使用，不需要先手工补字段
	cfg := h.s.Config()
	for i := range cfg.Models {
		if cfg.Models[i].ID == "vendor/big-model" {
			m := cfg.Models[i]
			m.Enabled = true
			if _, err := h.s.Change(cfg.Version, "test", "t", func(c *Config) error {
				for j := range c.Models {
					if c.Models[j].ID == m.ID {
						c.Models[j] = m
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("同步来的模型应当能直接通过校验并启用: %v", err)
			}
		}
	}
}

func TestSyncedIDFallsBackOnCollision(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"shared-name","context_length":4096,"supported_parameters":[]}]}`)
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_sync", Name: "S", Kind: "custom", Auth: "none",
			BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
		// 先占掉这个名字：模型、路由、别名共用命名空间
		m := modelFixture("shared-name", "chat")
		m.ProviderID = "p_sync"
		c.Models = append(c.Models, m)
	})
	_, o := h.rpc(t, "provider.sync_models", Object{"id": "p_sync"}, h.token)
	jobID := str(obj(o["data"]), "job_id")
	for i := 0; i < 200; i++ {
		_, jo := h.rpc(t, "job.get", Object{"id": jobID}, h.token)
		if st := str(obj(jo["data"]), "status"); st != "queued" && st != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	found := false
	for _, m := range h.s.Config().Models {
		if m.Upstream == "shared-name" && m.ID != "shared-name" {
			found = true
			if !strings.HasPrefix(m.ID, "p_sync/") {
				t.Fatalf("重名时应退回带 provider 前缀的 ID: %s", m.ID)
			}
		}
	}
	if !found {
		t.Fatal("重名的上游模型仍应被同步进来，只是换个 ID")
	}
}

// 路由列表顺序：按 Sort 升序，Sort 相同时退回 ID 字母序。
// 光按 ID 排，lite / auto / pro / max 会变成 auto / lite / max / pro。
func TestRouteOrderFollowsSortField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Change(1, "test", "", func(c *Config) error {
		c.Routes = append(c.Routes,
			Route{ID: "max", Strategy: "priority", Sort: 40}, Route{ID: "auto", Strategy: "priority", Sort: 20},
			Route{ID: "pro", Strategy: "priority", Sort: 30}, Route{ID: "lite", Strategy: "priority", Sort: 10},
			Route{ID: "zeta", Strategy: "priority"}, Route{ID: "alpha", Strategy: "priority"})
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Close()
	s, e = OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	var got []string
	for _, r := range s.Config().Routes {
		got = append(got, r.ID)
	}
	// Sort=0 的两条排在最前，它们之间维持字母序
	want := []string{"alpha", "zeta", "lite", "auto", "pro", "max"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("路由顺序错误: %v，期望 %v", got, want)
	}
}

func systemOneFixture(model string) Object {
	return Object{"model": model, "state": "客户说账单扣了两次钱", "questions": Object{
		"department": Object{"type": "choice", "instructions": "这条工单该给哪个部门",
			"criteria": Object{"billing": "账单与付款", "technical": "功能故障"}},
		"urgent": Object{"type": "noul", "instructions": "是否需要立即处理"},
		"severity": Object{"type": "score", "instructions": "严重程度",
			"criteria": []any{"可忽略", "一般", "严重"}},
	}}
}

// System One 的请求校验必须在发往上游之前完成：上游同样会拒绝，
// 但那时候配额已经预留、请求记录也写下了，而且它的错误正文我们不转发。
func TestSystemOneRequestValidation(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不合法的请求不应该到达上游")
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("jev", "systemone"))
	for _, tc := range []struct {
		name string
		body Object
		want string
	}{
		{"缺 state", Object{"model": "jev", "questions": Object{"a": Object{"type": "noul", "instructions": "x"}}}, "state"},
		{"缺 questions", Object{"model": "jev", "state": "x"}, "questions"},
		{"题型未知", Object{"model": "jev", "state": "x", "questions": Object{"a": Object{"type": "guess", "instructions": "x"}}}, "noul / choice / score"},
		{"缺 instructions", Object{"model": "jev", "state": "x", "questions": Object{"a": Object{"type": "noul"}}}, "instructions"},
		{"choice 选项不足", Object{"model": "jev", "state": "x", "questions": Object{"a": Object{"type": "choice", "instructions": "x", "criteria": Object{"only": "一个"}}}}, "至少要有 2 个选项"},
		{"score 分级不足", Object{"model": "jev", "state": "x", "questions": Object{"a": Object{"type": "score", "instructions": "x", "criteria": []any{"一级"}}}}, "至少要有 2 个分级"},
		{"流式不支持", Object{"model": "jev", "state": "x", "stream": true, "questions": Object{"a": Object{"type": "noul", "instructions": "x"}}}, "不支持流式"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.generate(t, "systemone", tc.body)
			requireStatus(t, w, 400)
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("错误信息应说明原因 %q，实得 %s", tc.want, w.Body.String())
			}
		})
	}
}

// System One 与三种对话协议之间不得互相转换。能转换就意味着要么编造文本，
// 要么把类型化答案字符串化，两种都会静默改变调用方拿到的东西。
func TestSystemOneNeverConvertsAcrossProtocols(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("跨协议请求不应该到达上游: %s", r.URL.Path)
	}))
	defer up.Close()
	h.configure(t, up.URL, modelFixture("jev", "systemone"), modelFixture("talker", "chat"))

	// 对话协议打 System One 模型
	for _, p := range []string{"chat", "messages", "responses"} {
		w := h.generate(t, p, requestFixture(p, "jev"))
		requireStatus(t, w, 400)
		if !strings.Contains(w.Body.String(), "没有等价语义") {
			t.Fatalf("%s → systemone 应说明不可转换，实得 %s", p, w.Body.String())
		}
	}
	// System One 打对话模型
	w := h.generate(t, "systemone", systemOneFixture("talker"))
	requireStatus(t, w, 400)
	if !strings.Contains(w.Body.String(), "没有等价语义") {
		t.Fatalf("systemone → chat 应说明不可转换，实得 %s", w.Body.String())
	}
	// System One 模型不应出现在对话协议的模型列表里
	r := httptest.NewRequest("GET", "/openai/v1/models", nil)
	mw := httptest.NewRecorder()
	h.a.Engine.Models(mw, r, "chat", Principal{ID: "test-key"})
	if strings.Contains(mw.Body.String(), `"jev"`) {
		t.Fatalf("System One 模型不该列在对话模型目录里: %s", mw.Body.String())
	}
	if !strings.Contains(mw.Body.String(), "talker") {
		t.Fatal("对话模型应当仍在目录里")
	}
}

// 正常往返：请求原样转发、响应原样返回、用量与配额照常记账。
func TestSystemOnePassthroughAndUsage(t *testing.T) {
	h := newHarness(t)
	var got Object
	var gotAuth, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"department":{"type":"choice","choice":"billing","probabilities":{"billing":0.91,"technical":0.09},"confidence":0.88}},"usage":{"input_tokens":312,"output_tokens":48}}`))
	}))
	defer up.Close()
	m := modelFixture("jev", "systemone")
	m.Upstream = "jev-latest"
	m.PricingSet, m.InputPrice, m.OutputPrice = true, 1, 2
	h.configure(t, up.URL, m)

	w := h.generate(t, "systemone", systemOneFixture("jev"))
	requireStatus(t, w, 200)
	if gotPath != "/systemone" {
		t.Fatalf("上游路径应为 /systemone，实得 %s", gotPath)
	}
	if gotAuth != "Bearer upstream-secret" {
		t.Fatalf("鉴权头错误: %s", gotAuth)
	}
	// 请求体除 model 换成上游名外原样转发，不注入 max_tokens 之类的对话字段
	if str(got, "model") != "jev-latest" {
		t.Fatalf("model 应换成上游名，实得 %v", got["model"])
	}
	if got["max_tokens"] != nil || got["max_output_tokens"] != nil {
		t.Fatalf("不应注入对话协议的输出上限字段: %v", got)
	}
	if len(obj(got["questions"])) != 3 {
		t.Fatalf("questions 应原样转发，实得 %v", got["questions"])
	}
	// 响应原样返回，类型化答案不被改写
	var res Object
	json.Unmarshal(w.Body.Bytes(), &res)
	ans := obj(obj(res["answers"])["department"])
	if str(ans, "choice") != "billing" || num(obj(ans["probabilities"]), "billing") != 0.91 {
		t.Fatalf("类型化答案被改写了: %s", w.Body.String())
	}
	if w.Header().Get("X-Prism-Protocol-Mode") != "native" {
		t.Fatalf("应标记为原生调用，实得 %s", w.Header().Get("X-Prism-Protocol-Mode"))
	}
	// 用量按 input_tokens / output_tokens 记账
	rows, _ := h.a.Store.DB.Query("SELECT input_tokens,output_tokens,cost_known FROM requests WHERE model_id='jev' AND status='success'")
	if len(rows) != 1 || rows[0].Int("input_tokens") != 312 || rows[0].Int("output_tokens") != 48 {
		t.Fatalf("用量未正确记账: %v", rows)
	}
}

// 529 是 TypeSafe 的过载码，语义等同 503，应当切换到下一个候选。
func TestSystemOne529FallsOverLike503(t *testing.T) {
	h := newHarness(t)
	var hits int
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(529)
		w.Write([]byte(`{"error":"overloaded"}`))
	}))
	defer busy.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.3}},"usage":{"input_tokens":10,"output_tokens":4}}`))
	}))
	defer ok.Close()
	h.change(t, func(c *Config) {
		c.Providers = []Provider{
			{ID: "p_busy", Name: "busy", Kind: "custom", BaseURL: busy.URL, Auth: "auto", Secret: "s", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
			{ID: "p_ok", Name: "ok", Kind: "custom", BaseURL: ok.URL, Auth: "auto", Secret: "s", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
		}
		first := modelFixture("jev-busy", "systemone")
		first.ProviderID = "p_busy"
		second := modelFixture("jev-ok", "systemone")
		second.ProviderID = "p_ok"
		c.Models = []Model{first, second}
		c.Routes = []Route{{ID: "jev", Name: "jev", Strategy: "priority", Enabled: true,
			Candidates: []Candidate{{"jev-busy", 20}, {"jev-ok", 10}}}}
	})
	w := h.generate(t, "systemone", systemOneFixture("jev"))
	requireStatus(t, w, 200)
	if hits != 1 {
		t.Fatalf("过载的候选应当只试一次，实得 %d", hits)
	}
	if w.Header().Get("X-Prism-Model") != "jev-ok" {
		t.Fatalf("应当切到可用候选，实得 %s", w.Header().Get("X-Prism-Model"))
	}
}

// 拖拽排序一次写完整份顺序。逐个保存会在中途失败时留下比原来更错的顺序。
func TestRouteReorderWritesWholeOrderAtOnce(t *testing.T) {
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Routes = []Route{
			{ID: "auto", Strategy: "priority", Sort: 10},
			{ID: "lite", Strategy: "priority", Sort: 20},
			{ID: "max", Strategy: "priority", Sort: 30},
		}
	})
	_, out := h.rpc(t, "route.reorder", Object{
		"version": h.a.Store.Config().Version,
		"ids":     []any{"lite", "auto", "max"},
	}, h.token)
	if obj(out["data"]) == nil {
		t.Fatalf("重排失败: %v", out)
	}
	var got []string
	for _, r := range h.a.Store.Config().Routes {
		got = append(got, r.ID)
	}
	if strings.Join(got, ",") != "lite,auto,max" {
		t.Fatalf("顺序未按请求写入: %v", got)
	}
	// 空列表要明确拒绝，而不是把所有 sort 清零
	w, _ := h.rpc(t, "route.reorder", Object{"version": h.a.Store.Config().Version, "ids": []any{}}, h.token)
	if w.Code != 400 {
		t.Fatalf("空 ids 应被拒绝，实得 %d", w.Code)
	}
	// 重启后顺序必须还在：sort 是存下来的，不是界面上临时排的
	h.a.Store.DB.Close()
	s, e := OpenStore(strings.TrimSuffix(h.s.KeyPath, ".key"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	got = nil
	for _, r := range s.Config().Routes {
		got = append(got, r.ID)
	}
	if strings.Join(got, ",") != "lite,auto,max" {
		t.Fatalf("重启后顺序丢失: %v", got)
	}
}

// 演示响应的形状必须和真实上游一致。形状不一致的话，调试台和冒烟测试
// 都在验证一个不存在的响应：真实 Jev 返回的 legend 是 {序号: 文字} 的对象、
// 序号从 0 开始，score 是连续值而不是整数下标。
func TestDemoSystemOneMatchesUpstreamShape(t *testing.T) {
	res := demoSystemOne(Object{"questions": Object{
		"sev": Object{"type": "score", "instructions": "严重度", "criteria": []any{"可忽略", "一般", "严重"}},
		"dep": Object{"type": "choice", "instructions": "部门", "criteria": Object{"a": "A", "b": "B"}},
		"yes": Object{"type": "noul", "instructions": "是否"},
	}}, "jev-latest")
	sev := obj(obj(res["answers"])["sev"])
	legend := obj(sev["legend"])
	if legend == nil {
		t.Fatalf("legend 必须是对象，实得 %T", sev["legend"])
	}
	for _, k := range []string{"0", "1", "2"} {
		if legend[k] == nil {
			t.Fatalf("legend 的序号应从 0 开始且连续，实得 %v", legend)
		}
	}
	if obj(sev["probabilities"])["0"] == nil {
		t.Fatalf("probabilities 的键应与 legend 对齐，实得 %v", sev["probabilities"])
	}
	if _, ok := sev["score"].(float64); !ok {
		t.Fatalf("score 应是连续值，实得 %T", sev["score"])
	}
	dep := obj(obj(res["answers"])["dep"])
	if str(dep, "choice") == "" || obj(dep["probabilities"]) == nil {
		t.Fatalf("choice 答案形状不对: %v", dep)
	}
	yes := obj(obj(res["answers"])["yes"])
	if _, ok := yes["noul"].(float64); !ok {
		t.Fatalf("noul 应返回概率值: %v", yes)
	}
}

// Command Code 在模型列表里直接声明每个模型支持的端点，协议应当照抄，
// 而不是像 OpenCode 那样靠模型名表猜。
func TestCommandCodeSyncUsesSupportedEndpoints(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":"cc-claude","name":"Claude","context_length":1000000,"supported_endpoints":["/messages"]},
			{"id":"cc-gpt","name":"GPT","context_length":400000,"supported_endpoints":["/chat/completions","/responses"]},
			{"id":"cc-resp","name":"Resp","context_length":200000,"supported_endpoints":["/responses"]},
			{"id":"cc-bare","name":"Bare","context_length":128000}]}`)
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers, Provider{ID: "p_cc", Name: "Command Code", Kind: "commandcode",
			Auth: "none", BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30})
	})
	_, o := h.rpc(t, "provider.sync_models", Object{"id": "p_cc"}, h.token)
	jobID := str(obj(o["data"]), "job_id")
	for i := 0; i < 200; i++ {
		_, jo := h.rpc(t, "job.get", Object{"id": jobID}, h.token)
		if st := str(obj(jo["data"]), "status"); st != "queued" && st != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	want := map[string]string{"cc-claude": "messages", "cc-gpt": "chat", "cc-resp": "responses", "cc-bare": "chat"}
	got := map[string]string{}
	for _, m := range h.s.Config().Models {
		if m.ProviderID == "p_cc" {
			got[m.Upstream] = m.Protocol
		}
	}
	for up, proto := range want {
		if got[up] != proto {
			t.Fatalf("模型 %s 的协议应为 %s，实际 %q", up, proto, got[up])
		}
	}
}

// Z.AI 有 OpenAI 兼容面和 Anthropic 兼容面两个地址，协议由填入的地址决定。
func TestZaiProtocolFollowsBaseURL(t *testing.T) {
	cases := []struct {
		base, want string
	}{
		{"https://api.z.ai/api/coding/paas/v4", "chat"},
		{"https://api.z.ai/api/paas/v4", "chat"},
		{"https://api.z.ai/api/anthropic/v1", "messages"},
	}
	for _, c := range cases {
		if got := providerProtocol(Provider{Kind: "zai", BaseURL: c.base}); got != c.want {
			t.Fatalf("%s 应判定为 %s，实际 %s", c.base, c.want, got)
		}
	}
}

// 额度耗尽（402）和套餐不含该模型（403）都不是「上游暂时忙」，
// 但对本次请求同样是确定性不可用，应当换到下一个候选，
// 并给出比 429 长得多的冷却：这类问题要等人去充值或升级套餐。
func TestExhaustedFallsOverAndCoolsDownLonger(t *testing.T) {
	for _, status := range []int{402, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newHarness(t)
			var second atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				o := Object{}
				json.NewDecoder(r.Body).Decode(&o)
				if str(o, "model") == "upstream-first" {
					w.WriteHeader(status)
					return
				}
				second.Add(1)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(completionObject(Completion{Blocks: []Block{{Kind: "text", Text: "fallback success"}}, Usage: Usage{Input: 1, Output: 2, Known: true}}, "chat", "second", "f"))
			}))
			defer up.Close()
			h.configure(t, up.URL, modelFixture("first", "chat"), modelFixture("second", "chat"))
			h.change(t, func(c *Config) {
				c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{"first", 10}, {"second", 10}}}}
			})
			w := h.generate(t, "chat", requestFixture("chat", "auto"))
			requireStatus(t, w, 200)
			if second.Load() != 1 || !strings.Contains(w.Body.String(), "fallback success") {
				t.Fatalf("HTTP %d 应当切换到下一个候选，实得 %s", status, w.Body)
			}
			// 冷却要明显长于 429 的 30 秒默认值，否则下一个请求又会去撞一次没额度的上游
			left := obj(obj(h.a.Engine.Health()["models"])["first"])
			until := int64(num(left, "cooldown_until"))
			if d := until - now(); d < int64(14*time.Minute/time.Millisecond) {
				t.Fatalf("耗尽类故障的冷却应接近 15 分钟，实得 %d ms", d)
			}
		})
	}
}

// 上游额度查询：DeepSeek 这类有公开余额接口的精确解析，
// OpenCode 这类形状未知的走通用扫描，Command Code 这类没有接口的必须明确说不支持。
func TestProviderUsageQuery(t *testing.T) {
	h := newHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/balance":
			io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}]}`)
		case "/dual/user/balance":
			// 同时持有两种币种时，各算各的，不能只认第一条
			io.WriteString(w, `{"is_available":true,"balance_infos":[
				{"currency":"USD","total_balance":"0.00","granted_balance":"0.00","topped_up_balance":"0.00"},
				{"currency":"CNY","total_balance":"10.30","granted_balance":"0.00","topped_up_balance":"10.30"}]}`)
		case "/zero/user/balance":
			io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10.30","granted_balance":"0.00","topped_up_balance":"10.30"}]}`)
		case "/usage":
			io.WriteString(w, `{"balance":12.5,"plan":"go"}`)
		case "/alpha/billing/credits":
			io.WriteString(w, `{"credits":{"planId":"individual-goat","monthlyCredits":41.5,"purchasedCredits":8,"freeCredits":0.5,
				"windowLimits":{"limited":true,"fiveHour":{"used":3.2,"cap":14},"weekly":{"used":9.75,"cap":35}}}}`)
		case "/api/monitor/usage/quota/limit":
			if r.Header.Get("Authorization") != "zai-raw-token" {
				io.WriteString(w, `{"code":1001,"msg":"Authentication parameter not received in Header, unable to authenticate","success":false}`)
				return
			}
			io.WriteString(w, `{"code":200,"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","percentage":32},{"type":"TIME_LIMIT","percentage":7,"currentValue":3,"usage":40}]}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	h.change(t, func(c *Config) {
		c.Providers = append(c.Providers,
			Provider{ID: "p_ds", Name: "DeepSeek", Kind: "deepseek", Auth: "none", BaseURL: upstream.URL + "/v1", AllowPrivate: true, Enabled: true, TimeoutSec: 30},
			Provider{ID: "p_oc", Name: "OpenCode", Kind: "opencode", Auth: "none", BaseURL: upstream.URL, AllowPrivate: true, Enabled: true, TimeoutSec: 30},
			Provider{ID: "p_cc2", Name: "Command Code", Kind: "commandcode", Auth: "none", BaseURL: upstream.URL + "/provider/v1", AllowPrivate: true, Enabled: true, TimeoutSec: 30},
			Provider{ID: "p_zai", Name: "Z.AI", Kind: "zai", Auth: "bearer", Secret: "zai-raw-token", HasKey: true, BaseURL: upstream.URL + "/api/coding/paas/v4", AllowPrivate: true, Enabled: true, TimeoutSec: 30},
			Provider{ID: "p_ds2", Name: "DeepSeek 双币", Kind: "deepseek", Auth: "none", BaseURL: upstream.URL + "/dual", AllowPrivate: true, Enabled: true, TimeoutSec: 30},
			Provider{ID: "p_ds3", Name: "DeepSeek 无赠金", Kind: "deepseek", Auth: "none", BaseURL: upstream.URL + "/zero", AllowPrivate: true, Enabled: true, TimeoutSec: 30})
	})
	_, o := h.rpc(t, "provider.usage", Object{}, h.token)
	got := map[string]Object{}
	for _, v := range arr(o["data"]) {
		got[str(obj(v), "id")] = obj(v)
	}
	ds := got["p_ds"]
	if ds["supported"] != true || str(ds, "headline") != "¥110.00" {
		t.Fatalf("DeepSeek 余额解析不正确: %v", ds)
	}
	if len(arr(ds["fields"])) != 2 {
		t.Fatalf("DeepSeek 应有赠送与充值两项明细: %v", ds)
	}
	oc := got["p_oc"]
	if oc["supported"] != true || str(oc, "headline") != "12.5" || len(arr(oc["fields"])) != 1 {
		t.Fatalf("OpenCode 通用扫描应把 balance 提为主数值、plan 作明细: %v", oc)
	}
	// Command Code 的额度在 CLI 用的 /alpha/billing/credits 上，不在 Provider API 里
	cc := got["p_cc2"]
	if cc["supported"] != true || str(cc, "headline") != "$50.00" {
		t.Fatalf("Command Code 剩余额度应为三项之和: %v", cc)
	}
	ccFields := arr(cc["fields"])
	if len(ccFields) != 4 || str(obj(ccFields[2]), "value") != "$3.20 / $14" {
		t.Fatalf("Command Code 应带上两个滚动窗口: %v", ccFields)
	}
	// Z.AI 的监控接口要裸 token，加了 Bearer 前缀会被判未鉴权
	// 双币种：美元那条是 0，主数值要落到真正有钱的人民币上
	dual := got["p_ds2"]
	if str(dual, "headline") != "¥10.30" || len(arr(dual["fields"])) != 2 {
		t.Fatalf("双币种余额应两条都列出、主数值取非零的: %v", dual)
	}
	// 赠送余额为 0 时不占位置，否则一眼看过去像是余额为零
	zero := got["p_ds3"]
	zf := arr(zero["fields"])
	if str(zero, "headline") != "¥10.30" || len(zf) != 1 || str(obj(zf[0]), "label") != "充值余额" {
		t.Fatalf("为 0 的明细不应展示: %v", zero)
	}
	zai := got["p_zai"]
	if zai["supported"] != true || str(zai, "headline") != "5 小时 32%" {
		t.Fatalf("Z.AI 额度解析不正确: %v", zai)
	}
	if _, ok, note := providerUsageURL(Provider{Kind: "mock"}); ok || note == "" {
		t.Fatalf("本地演示不应查询上游额度")
	}
}

// 来源记录：网关跑在同机反代后面，所以回环直连时才认 X-Forwarded-For，
// 公网直连带上的 XFF 一律不认——否则任何人都能把来源伪装成别的地址。
func TestClientOfTrustsForwardedOnlyFromLoopback(t *testing.T) {
	cases := []struct {
		remote, xff, want string
	}{
		{"127.0.0.1:5510", "203.0.113.9", "203.0.113.9"},
		{"[::1]:5510", "203.0.113.9, 10.0.0.1", "203.0.113.9"},
		{"198.51.100.4:5510", "203.0.113.9", "198.51.100.4"},
		{"127.0.0.1:5510", "not-an-ip", "127.0.0.1"},
		{"127.0.0.1:5510", "", "127.0.0.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/openai/v1/chat/completions", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientOf(r).IP; got != c.want {
			t.Fatalf("RemoteAddr %q + XFF %q 应解析为 %q，实际 %q", c.remote, c.xff, c.want, got)
		}
	}
	long := httptest.NewRequest("POST", "/x", nil)
	long.Header.Set("User-Agent", strings.Repeat("a", 500))
	if got := len(clientOf(long).Agent); got != 200 {
		t.Fatalf("User-Agent 应截断到 200 字符，实际 %d", got)
	}
}

// 来源 IP 与客户端标识要真的落到请求记录里，并且能被搜索命中。
func TestRequestLogRecordsClientSource(t *testing.T) {
	h := newHarness(t)
	if _, err := h.a.EnableDemo(h.s.Config().Version); err != nil {
		t.Fatal(err)
	}
	_, o := h.rpc(t, "apikey.create", Object{"name": "src"}, h.token)
	key := str(obj(o["data"]), "key")
	r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(raw(requestFixture("chat", "demo-chat"))))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("User-Agent", "claude-code/2.0.1 (external, cli)")
	r.RemoteAddr = "127.0.0.1:5510"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	rr := httptest.NewRecorder()
	h.a.ServeHTTP(rr, r)
	requireStatus(t, rr, 200)

	_, lo := h.rpc(t, "request.list", Object{}, h.token)
	items := arr(obj(lo["data"])["items"])
	if len(items) == 0 {
		t.Fatal("没有请求记录")
	}
	row := obj(items[0])
	if str(row, "client_ip") != "203.0.113.9" {
		t.Fatalf("来源 IP 未记录: %v", row["client_ip"])
	}
	if !strings.HasPrefix(str(row, "user_agent"), "claude-code/2.0.1") {
		t.Fatalf("客户端标识未记录: %v", row["user_agent"])
	}
	// 搜索框要能按 IP 找记录
	_, so := h.rpc(t, "request.list", Object{"q": "203.0.113"}, h.token)
	if len(arr(obj(so["data"])["items"])) != 1 {
		t.Fatalf("按来源 IP 搜索应命中 1 条: %v", so["data"])
	}
	_, mo := h.rpc(t, "request.list", Object{"q": "198.51.100"}, h.token)
	if len(arr(obj(mo["data"])["items"])) != 0 {
		t.Fatalf("不匹配的 IP 不应命中: %v", mo["data"])
	}
}

// failoverHarness 搭一个两候选的路由：第一个候选的上游按 bad 的行为回应，
// 第二个永远正常。用来验证各种「该换候选」的判定。
func failoverHarness(t *testing.T, bad http.HandlerFunc) *harness {
	t.Helper()
	h := newHarness(t)
	first := httptest.NewServer(bad)
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"在的"}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
	}))
	t.Cleanup(second.Close)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{
			{ID: "p_bad", Name: "bad", Kind: "custom", BaseURL: first.URL, Auth: "auto", Secret: "s", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
			{ID: "p_ok", Name: "ok", Kind: "custom", BaseURL: second.URL, Auth: "auto", Secret: "s", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
		}
		bad := modelFixture("cand-bad", "chat")
		bad.ProviderID = "p_bad"
		ok := modelFixture("cand-ok", "chat")
		ok.ProviderID = "p_ok"
		c.Models = []Model{bad, ok}
		c.Routes = []Route{{ID: "pair", Name: "pair", Strategy: "priority", Enabled: true,
			Candidates: []Candidate{{"cand-bad", 20}, {"cand-ok", 10}}}}
	})
	return h
}

// 上游下架某个模型变体就会回 404。这对当前候选是确定性失败，该换下一个，
// 而不是把 404 直接甩给调用方——同档位后面的候选还活着。
func TestNotFoundFallsOverToNextCandidate(t *testing.T) {
	hits := 0
	h := failoverHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"message":"model not found"}}`)
	})
	w := h.generate(t, "chat", requestFixture("chat", "pair"))
	requireStatus(t, w, 200)
	if got := w.Header().Get("X-Prism-Model"); got != "cand-ok" {
		t.Fatalf("404 应当换候选，实得 %s", got)
	}
	if hits != 1 {
		t.Fatalf("404 的候选只该试一次，实得 %d", hits)
	}
	rows, err := h.s.DB.Query("SELECT model_id,status,error_code FROM requests ORDER BY started_at")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].String("error_code") != "UPSTREAM_404" || rows[1].String("status") != "success" {
		t.Fatalf("记录应为一条 404 一条成功: %v", rows)
	}
}

// 上游网关层的 502 / 504 意味着请求根本没到模型，换候选是安全的。
func TestUpstreamGatewayErrorFallsOver(t *testing.T) {
	for _, status := range []int{502, 504} {
		h := failoverHarness(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"bad gateway"}}`)
		})
		w := h.generate(t, "chat", requestFixture("chat", "pair"))
		requireStatus(t, w, 200)
		if got := w.Header().Get("X-Prism-Model"); got != "cand-ok" {
			t.Fatalf("上游 %d 应当换候选，实得 %s", status, got)
		}
	}
}

// 网关自己标成 502 的情况（这里是读不懂的响应体）不该换候选：
// 请求可能已经被上游部分处理，重放并不安全。
func TestGatewaySideFailureDoesNotFallOver(t *testing.T) {
	h := failoverHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"not":"a completion"`)
	})
	w := h.generate(t, "chat", requestFixture("chat", "pair"))
	requireStatus(t, w, 502)
	if got := w.Header().Get("X-Prism-Model"); got != "cand-bad" {
		t.Fatalf("网关侧失败不该换候选，实得 %s", got)
	}
}

// 推理模型把输出预算花光在思考上时，上游回 200 但正文为空。
// 这对调用方等同于失败：要换候选、记成错误，而推理烧掉的 token 照实记账。
func TestEmptyOutputFallsOverAndStillBills(t *testing.T) {
	h := failoverHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c0","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"","reasoning":"想了很久"}}],"usage":{"prompt_tokens":10,"completion_tokens":16,"total_tokens":26}}`)
	})
	w := h.generate(t, "chat", requestFixture("chat", "pair"))
	requireStatus(t, w, 200)
	if got := w.Header().Get("X-Prism-Model"); got != "cand-ok" {
		t.Fatalf("空回答应当换候选，实得 %s", got)
	}
	rows, err := h.s.DB.Query("SELECT model_id,status,error_code,input_tokens,output_tokens,usage_mode FROM requests ORDER BY started_at")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有两条记录: %v", rows)
	}
	first := rows[0]
	if first.String("status") != "error" || first.String("error_code") != "EMPTY_OUTPUT_TRUNCATED" {
		t.Fatalf("空回答要记成错误并带自己的错误码: %v", first)
	}
	// 推理 token 是真花掉的，不能抹成零，也不能退回预留值
	if first.Int("input_tokens") != 10 || first.Int("output_tokens") != 16 || first.String("usage_mode") != "reported_tokens" {
		t.Fatalf("空回答仍应按上游上报的用量记账: %v", first)
	}
}

// 限额头的名字、单位、重置时间格式各家都不一样，先能原样留存，再尽力解析。
func TestParseUpstreamRateLimitHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "0")
	h.Set("anthropic-ratelimit-requests-reset", time.Now().Add(90*time.Second).UTC().Format(time.RFC3339))
	h.Set("x-ratelimit-remaining-tokens", "12000")
	h.Set("x-ratelimit-reset-tokens", "6m0s")
	h.Set("content-type", "application/json")

	raw := limitHeaders(h)
	if len(raw) != 5 {
		t.Fatalf("应当只留存 5 个限额头，实得 %v", raw)
	}
	if _, ok := raw["content-type"]; ok {
		t.Fatal("无关的响应头不该被留存")
	}

	parsed := parseLimits(raw)
	req := parsed["requests"]
	if !req.HasRemain || req.Remaining != 0 || !req.HasLimit || req.Limit != 1000 {
		t.Fatalf("requests 一族解析错误: %+v", req)
	}
	tok := parsed["tokens"]
	if !tok.HasRemain || tok.Remaining != 12000 {
		t.Fatalf("tokens 一族解析错误: %+v", tok)
	}
	// "6m0s" 这种 Go duration 写法要折算成绝对时刻
	if d := tok.Reset - now(); d < 5*60*1000 || d > 7*60*1000 {
		t.Fatalf("duration 形式的重置时间换算错误: %d", d)
	}
	// 只有 requests 剩 0，冷却应当落在它的重置时刻附近
	until := exhaustedUntil(raw)
	if d := until - now(); d < 60*1000 || d > 120*1000 {
		t.Fatalf("额度归零应冷却到重置时刻，实得 %d ms 之后", d)
	}
}

// 各种重置时间写法都要能折算成绝对毫秒时间戳。
func TestParseResetAcceptsEveryFormatSeen(t *testing.T) {
	ms := time.Now().Add(time.Minute).UnixMilli()
	cases := []struct{ in string }{
		{fmt.Sprint(ms)},        // 毫秒时间戳
		{fmt.Sprint(ms / 1000)}, // 秒时间戳
		{"60"},                  // 还有多少秒
		{"1m0s"},                // Go duration
		{time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}, // RFC3339
	}
	for _, c := range cases {
		got := parseReset(c.in)
		if d := got - now(); d < 30*1000 || d > 90*1000 {
			t.Fatalf("%q 应解析到约一分钟后，实得 %d ms", c.in, d)
		}
	}
	if parseReset("not-a-time") != 0 {
		t.Fatal("解析不了的值应当返回 0，而不是猜一个")
	}
}

// 上游明说额度归零时，下一个请求不该再去撞一次 429，直接换候选。
func TestZeroRemainingCoolsDownModel(t *testing.T) {
	h := failoverHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining-requests", "0")
		w.Header().Set("x-ratelimit-reset-requests", "120")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c0","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"这次还能答"}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
	})
	// 第一次正常返回：额度虽然归零，这次响应本身是好的
	w := h.generate(t, "chat", requestFixture("chat", "pair"))
	requireStatus(t, w, 200)
	if got := w.Header().Get("X-Prism-Model"); got != "cand-bad" {
		t.Fatalf("第一次应当正常落在首选候选，实得 %s", got)
	}
	// 第二次：首选已被冷却，应当换到下一个候选
	w = h.generate(t, "chat", requestFixture("chat", "pair"))
	requireStatus(t, w, 200)
	if got := w.Header().Get("X-Prism-Model"); got != "cand-ok" {
		t.Fatalf("额度归零后应当换候选，实得 %s", got)
	}
	runtime := h.a.Engine.Health()
	m := obj(obj(runtime["models"])["cand-bad"])
	if len(obj(m["limits"])) != 2 {
		t.Fatalf("限额头应当留存在运行状态里: %v", m["limits"])
	}
	if m["cooldown_until"].(int64) <= now() {
		t.Fatalf("冷却时刻应当在未来: %v", m["cooldown_until"])
	}
}

// 被上游拒掉的响应往往才带着 Retry-After 与剩余额度，这些同样要留存下来。
func TestRateLimitHeadersRecordedOnRejection(t *testing.T) {
	h := failoverHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.Header().Set("x-ratelimit-remaining-requests", "0")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"slow down"}}`)
	})
	w := h.generate(t, "chat", requestFixture("chat", "pair"))
	requireStatus(t, w, 200)
	m := obj(obj(h.a.Engine.Health()["models"])["cand-bad"])
	limits := obj(m["limits"])
	if limits["retry-after"] != "42" || limits["x-ratelimit-remaining-requests"] != "0" {
		t.Fatalf("被拒响应的限额头也要留存: %v", limits)
	}
}
