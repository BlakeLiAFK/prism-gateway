package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ccUpstream 模拟 Command Code：窗口额度只在带 orgId 时返回、位于响应顶层（与 CLI 的读法一致），
// 推理端点一律 429 且不带任何限额响应头
func ccUpstream(t *testing.T, resetAt int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alpha/whoami":
			io.WriteString(w, `{"org":{"id":"org_1"}}`)
		case "/alpha/billing/credits":
			if r.URL.Query().Get("orgId") != "org_1" {
				io.WriteString(w, `{"credits":{"monthlyCredits":34.99}}`)
				return
			}
			fmt.Fprintf(w, `{"credits":{"monthlyCredits":34.99,"purchasedCredits":0,"freeCredits":0},
				"windowLimits":{"fiveHour":{"used":2,"cap":14,"resetAt":%d},"weekly":{"used":35,"cap":35,"resetAt":%d}}}`, resetAt, resetAt)
		default:
			w.WriteHeader(429)
		}
	}))
}

func ccProvider(base string) Provider {
	return Provider{ID: "p_cc", Name: "CC", Kind: "commandcode", Auth: "none", BaseURL: base + "/provider/v1", AllowPrivate: true, Enabled: true, TimeoutSec: 30}
}

// 周额度用完时卡片主位要直说，并给出重置时间；窗口数据要经 whoami 取 orgId 才拿得到
func TestCommandCodeWindowExhaustedShown(t *testing.T) {
	up := ccUpstream(t, now()+int64(50*time.Hour/time.Millisecond))
	defer up.Close()
	h := newHarness(t)
	h.change(t, func(c *Config) { c.Providers = append(c.Providers, ccProvider(up.URL)) })
	_, o := h.rpc(t, "provider.usage", Object{}, h.token)
	var cc Object
	for _, v := range arr(o["data"]) {
		if str(obj(v), "id") == "p_cc" {
			cc = obj(v)
		}
	}
	if str(cc, "headline") != "本周额度已用完" {
		t.Fatalf("主位应提示周额度用完: %v", cc)
	}
	weekly := ""
	for _, f := range arr(cc["fields"]) {
		if str(obj(f), "label") == "本周" {
			weekly = str(obj(f), "value")
		}
	}
	if !strings.HasPrefix(weekly, "$35.00 / $35 · 2 天 1 小时") {
		t.Fatalf("周窗口明细应带用量与重置倒计时: %q", weekly)
	}
}

// 429 之后查到周额度已用完：同一供应商的全部模型冷却到重置时刻（封顶 24 小时）
func TestCommandCode429CoolsProviderUntilReset(t *testing.T) {
	up := ccUpstream(t, now()+int64(2*time.Hour/time.Millisecond))
	defer up.Close()
	h := newHarness(t)
	a, b := modelFixture("cc-a", "chat"), modelFixture("cc-b", "chat")
	a.ProviderID, b.ProviderID = "p_cc", "p_cc"
	h.change(t, func(c *Config) {
		c.Providers = []Provider{ccProvider(up.URL)}
		c.Models = []Model{a, b}
	})
	w := h.generate(t, "chat", requestFixture("chat", "cc-a"))
	if w.Code != 429 {
		t.Fatalf("上游 429 应透出为 429，实得 %d", w.Code)
	}
	// 查额度是异步的，等它把 cc-b 也冷却上
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.a.Engine.mu.Lock()
		cool := h.a.Engine.state("cc-b").Cooldown
		h.a.Engine.mu.Unlock()
		if cool > now()+int64(time.Hour/time.Millisecond) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("同供应商的模型应冷却到周额度重置，实得 cooldown=%d", cool)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDurationText(t *testing.T) {
	for ms, want := range map[int64]string{
		int64(50 * time.Hour / time.Millisecond):   "2 天 2 小时",
		int64(90 * time.Minute / time.Millisecond): "1 小时 30 分",
		int64(10 * time.Second / time.Millisecond): "1 分钟",
	} {
		if got := durationText(ms); got != want {
			t.Errorf("durationText(%d)=%q，期望 %q", ms, got, want)
		}
	}
}
