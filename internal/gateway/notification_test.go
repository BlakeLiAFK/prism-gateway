package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestFailureNoticeContainsAllSources(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	defer up.Close()
	h := newHarness(t)
	m := modelFixture("deepseek", "chat")
	m.Name = "deepseek-v4.1-flash"
	h.configure(t, up.URL, m)
	h.change(t, func(c *Config) { c.Providers[0].Name = "OpenCode Go" })
	key1, key2 := h.createKey(t), h.createKey(t)
	messages := make(chan string, 2)
	h.a.Engine.OnAlert = func(event, key, text string) { messages <- text }
	for i := 0; i < 5; i++ {
		key, ip, ua := key1, "203.0.113.10:1234", "opencode/2.0"
		if i >= 3 {
			key, ip, ua = key2, "203.0.113.20:1234", "claude-code/2.1"
		}
		r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(raw(requestFixture("chat", "deepseek"))))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("User-Agent", ua)
		r.RemoteAddr = ip
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("失败请求未执行: %d %s", w.Code, w.Body)
		}
	}
	select {
	case text := <-messages:
		for _, want := range []string{"OpenCode Go", "deepseek-v4.1-flash", "连续 5 次请求失败", "来源 1：3 次", "来源 2：2 次", "opencode/2.0", "claude-code/2.1", "203.0.113.10", "203.0.113.20", "调用凭证：", "请求 ID：req_", "UPSTREAM_400"} {
			if !strings.Contains(text, want) {
				t.Fatalf("缺少 %s: %s", want, text)
			}
		}
		if strings.Contains(text, key1) || strings.Contains(text, key2) {
			t.Fatal("通知泄漏API Key")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未产生连续失败通知")
	}
	h.a.Engine.mu.Lock()
	health := h.a.Engine.state(m.ID)
	h.a.Engine.trackFailure(health, selection{Model: m}, "", "success", "")
	if health.Fails != 0 || len(health.FailureSamples) != 0 {
		t.Error("成功未清理失败来源")
	}
	h.a.Engine.mu.Unlock()
}

func TestNoticePlainTextAndChunks(t *testing.T) {
	if got := noticeValue("bot\n伪造字段\x1b\u202e"); got != "bot 伪造字段" {
		t.Fatal(got)
	}
	text := strings.Repeat("供应商：测试\n客户端：𠀀<raw>&_*\n\n", 400)
	parts := noticeChunks(text)
	if len(parts) < 2 {
		t.Fatal("长消息未分段")
	}
	var rebuilt strings.Builder
	for _, p := range parts {
		if len(utf16.Encode([]rune(p))) > 4096 {
			t.Fatal("超过Telegram长度限制")
		}
		_, body, ok := strings.Cut(p, "\n\n")
		if !ok {
			t.Fatal("缺少分段号")
		}
		rebuilt.WriteString(body)
	}
	if rebuilt.String() != text {
		t.Fatal("消息分段丢失内容")
	}
	if strings.Contains(noticeEnvelope("测试"), "【") {
		t.Fatal("应使用朴素分段")
	}
}

func TestTelegramChecksBusinessResultAndPlainText(t *testing.T) {
	old := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = old })
	for _, response := range []struct {
		body    string
		success bool
	}{{`{"ok":true}`, true}, {`{"ok":false,"description":"secret"}`, false}, {`invalid`, false}} {
		http.DefaultClient = &http.Client{Transport: modelsDevRoundTrip(func(r *http.Request) (*http.Response, error) {
			var payload Object
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["parse_mode"] != nil || payload["entities"] != nil || payload["text"] != "<plain> _text_" {
				t.Fatalf("不应设置富文本模式: %v", payload)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response.body)), Header: make(http.Header)}, nil
		})}
		err := (&App{}).deliver(context.Background(), alertConfig{Kind: "telegram", Secret: "test-secret", ChatID: "test"}, "<plain> _text_")
		if (err == nil) != response.success {
			t.Fatalf("响应处理错误: %v", err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatal("上游描述不应泄漏凭据")
		}
	}
}

func TestNotificationBoundaries(t *testing.T) {
	a := &App{}
	for _, kind := range []string{"telegram", "webhook"} {
		err := a.deliver(context.Background(), alertConfig{Kind: kind, Secret: "https://private-secret/%zz"}, "test")
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("非法地址应脱敏: %v", err)
		}
	}
	if err := a.deliver(context.Background(), alertConfig{Kind: "telegram"}, ""); err == nil {
		t.Fatal("空消息不应成功")
	}
	if got := noticeTime(0); got != "未提供" {
		t.Fatal(got)
	}
	zone, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Fatal(err)
	}
	if got := noticeTimeIn(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), zone); !strings.Contains(got, "08:00:00 +08:00 (Asia/Taipei)") {
		t.Fatal(got)
	}
	c := Config{Providers: []Provider{{ID: "p", Name: "同名模型供应商"}}}
	ids := []string{}
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		c.Models = append(c.Models, Model{ID: id, Name: id, ProviderID: "p"})
		ids = append(ids, id)
	}
	got := noticeModels(c, append(ids, "m1"))
	if !strings.Contains(got, "同名模型供应商") || !strings.Contains(got, "另有 1 个候选") || strings.Contains(got, " / m6") {
		t.Fatal(got)
	}
}
