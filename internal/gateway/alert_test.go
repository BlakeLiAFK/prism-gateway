package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAlertDelivery(t *testing.T) {
	var mu sync.Mutex
	got := []string{}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer up.Close()
	h := newHarness(t)
	m := modelFixture("m", "chat")
	h.configure(t, up.URL, m)
	w, out := h.rpc(t, "alert.save", Object{"enabled": true, "kind": "webhook", "secret": hook.URL, "events": Object{"failures": true, "route": true}}, h.token)
	requireStatus(t, w, 200)
	if d := obj(out["data"]); d["has_secret"] != true || strings.Contains(raw(d), hook.URL) {
		t.Fatalf("接口不应回显凭证: %v", d)
	}
	// 留空 secret 表示沿用已保存的值
	requireStatus(t, func() *httptest.ResponseRecorder {
		w, _ := h.rpc(t, "alert.save", Object{"enabled": true, "kind": "webhook", "events": Object{"failures": true, "route": true}}, h.token)
		return w
	}(), 200)
	wait := func(n int) []string {
		deadline := time.Now().Add(3 * time.Second)
		for {
			mu.Lock()
			c := append([]string{}, got...)
			mu.Unlock()
			if len(c) >= n || time.Now().After(deadline) {
				return c
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for i := 0; i < 10; i++ {
		h.generate(t, "chat", requestFixture("chat", "m"))
	}
	wait(1)
	time.Sleep(200 * time.Millisecond) // 再等一会儿，确认限频期内没有第二条
	if msgs := wait(1); len(msgs) != 1 || !strings.Contains(msgs[0], "连续 5 次请求失败") {
		t.Fatalf("连续失败应只推送一次（30 分钟限频）: %v", msgs)
	}
	// 路由全部候选不可用：并发占满
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{"m", 10}}}}
	})
	h.a.Engine.mu.Lock()
	h.a.Engine.state("m").Active = m.Concurrency
	h.a.Engine.mu.Unlock()
	requireStatus(t, h.generate(t, "chat", requestFixture("chat", "auto")), 429)
	if msgs := wait(2); len(msgs) != 2 || !strings.Contains(msgs[1], "路由 auto 的全部候选都不可用") {
		t.Fatalf("路由无可用候选应推送: %v", msgs)
	}
}
