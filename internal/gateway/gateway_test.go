package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
