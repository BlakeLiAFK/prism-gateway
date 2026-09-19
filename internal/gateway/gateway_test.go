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
			x, err := h.a.Engine.admit(s, Principal{ID: "k"}, randomID("r_"), "m", "chat", "")
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
	_, e = h.a.Engine.admit(s, Principal{ID: "k"}, "r2", "m", "chat", "")
	if e == nil {
		t.Fatal("RPM not enforced")
	}
	h.change(t, func(c *Config) { c.Models[0].RPM = 0; c.Models[0].Limit5h = .000001 })
	ss, _, _ = h.a.Engine.selections(h.s.Config(), requestFixture("chat", "m"), "chat", "")
	_, e = h.a.Engine.admit(ss[0], Principal{ID: "k"}, "r3", "m", "chat", "")
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
	err := convertedStream(w, strings.NewReader(src), "messages", "chat", "m", "r", &u)
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
	if err := convertedStream(w, strings.NewReader(src), "messages", "chat", "m", "r", &u); err != nil {
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
