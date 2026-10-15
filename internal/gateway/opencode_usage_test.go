package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openCodeUsageProvider(id, base, secret string) Provider {
	return Provider{ID: id, Name: id, Kind: "opencode", Auth: "auto", Secret: secret, AdminSecret: "admin-unused", BaseURL: base, AllowPrivate: true, Enabled: true, TimeoutSec: 30}
}

func providerUsageItem(t *testing.T, h *harness, id string) (*httptest.ResponseRecorder, Object) {
	t.Helper()
	w, out := h.rpc(t, "provider.usage", Object{}, h.token)
	for _, item := range arr(out["data"]) {
		if v := obj(item); str(v, "id") == id {
			return w, v
		}
	}
	t.Fatalf("缺少供应商 %s 的用量结果: %v", id, out)
	return w, nil
}

func usageField(t *testing.T, usage Object, label string) string {
	t.Helper()
	for _, item := range arr(usage["fields"]) {
		if field := obj(item); str(field, "label") == label {
			return str(field, "value")
		}
	}
	t.Fatalf("缺少字段 %s: %v", label, usage)
	return ""
}

func TestOpenCodeUsageHandlerParsesWindows(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/usage" || r.Header.Get("Authorization") != "Bearer infer-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":%q},"weekly":{"status":"rate-limited","percent":100,"resetsAt":"2000-01-01T00:00:00Z"},"monthly":{"status":"ok","percent":17}}}`, reset)
	}))
	defer server.Close()
	h := newHarness(t)
	h.change(t, func(c *Config) { c.Providers = []Provider{openCodeUsageProvider("go", server.URL, "infer-key")} })
	w, got := providerUsageItem(t, h, "go")
	if got["supported"] != true || str(got, "headline") != "5 小时已用 0%" || len(arr(got["fields"])) != 4 {
		t.Fatalf("OpenCode 三窗口解析错误: %v", got)
	}
	if value := usageField(t, got, "5 小时"); !strings.Contains(value, "已用 0%") || !strings.Contains(value, "后重置") {
		t.Fatalf("滚动窗口重置格式错误: %q", value)
	}
	if value := usageField(t, got, "每周"); !strings.Contains(value, "已用 100%") || !strings.Contains(value, "已达限额") || !strings.Contains(value, "等待上游刷新") {
		t.Fatalf("每周限额格式错误: %q", value)
	}
	if value := usageField(t, got, "每月"); value != "已用 17% · 重置时间待确认" {
		t.Fatalf("缺少重置时间时格式错误: %q", value)
	}
	if value := usageField(t, got, "现金余额"); value != "接口未提供" {
		t.Fatalf("现金余额文案错误: %q", value)
	}
	if strings.Contains(w.Body.String(), "infer-key") || strings.Contains(w.Body.String(), "admin-unused") {
		t.Fatal("provider.usage 回显了供应商密钥")
	}
}

func TestOpenCodeUsageUnknownAndErrors(t *testing.T) {
	tests := []struct {
		name, body, headline, errorText string
		status                          int
	}{
		{"部分有效", `{"usage":{"rolling":{"status":"ok","percent":"42"},"weekly":{"status":"ok","percent":17,"resetsAt":"bad"},"monthly":{"status":"paused","percent":3}}}`, "每周已用 17%", "", http.StatusOK},
		{"全部未知", `{"usage":{"rolling":{"status":"ok","percent":"42"},"weekly":{"status":"ok","percent":101},"monthly":{"status":"paused","percent":3}}}`, "", "未返回可识别的套餐用量", http.StatusOK},
		{"上游错误", `{"usage":{"rolling":{"status":"ok","percent":0}}}`, "", "HTTP 503", http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			h := newHarness(t)
			h.change(t, func(c *Config) { c.Providers = []Provider{openCodeUsageProvider("go", server.URL, "key")} })
			_, got := providerUsageItem(t, h, "go")
			if str(got, "headline") != tc.headline || !strings.Contains(str(got, "error"), tc.errorText) {
				t.Fatalf("未知或错误响应处理不正确: %v", got)
			}
			if tc.status == http.StatusOK && len(arr(got["fields"])) != 4 {
				t.Fatalf("窗口字段必须固定输出: %v", got)
			}
			if tc.name == "部分有效" && (!strings.Contains(usageField(t, got, "5 小时"), "待确认") || !strings.Contains(usageField(t, got, "每月"), "待确认") || !strings.Contains(usageField(t, got, "每周"), "重置时间待确认")) {
				t.Fatalf("部分有效响应应保留未知状态: %v", got)
			}
			if tc.name == "全部未知" {
				for _, label := range []string{"5 小时", "每周", "每月"} {
					if !strings.Contains(usageField(t, got, label), "待确认") {
						t.Fatalf("未知窗口未标记待确认: %v", got)
					}
				}
			}
			if strings.Contains(raw(got), "已用 0%") {
				t.Fatalf("未知百分比被误报为 0%%: %v", got)
			}
		})
	}
}

func TestOpenCodeUsageZenCacheAndKeyChange(t *testing.T) {
	var hits atomic.Int32
	var mu sync.Mutex
	auth := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		percent := 42
		if r.Header.Get("Authorization") == "Bearer infer-two" {
			percent = 80
		}
		fmt.Fprintf(w, `{"usage":{"rolling":{"status":"ok","percent":%d}}}`, percent)
	}))
	defer server.Close()
	h := newHarness(t)
	h.change(t, func(c *Config) {
		c.Providers = []Provider{openCodeUsageProvider("go", server.URL, "infer-one"), openCodeUsageProvider("zen", server.URL+"/zen/v1", "zen-key")}
	})
	_, first := providerUsageItem(t, h, "go")
	_, second := providerUsageItem(t, h, "go")
	if hits.Load() != 1 || str(first, "headline") != "5 小时已用 42%" || str(second, "headline") != str(first, "headline") {
		t.Fatalf("60 秒缓存未生效: hits=%d first=%v second=%v", hits.Load(), first, second)
	}
	_, zen := providerUsageItem(t, h, "zen")
	if zen["supported"] != false || !strings.Contains(str(zen, "note"), "Zen") || hits.Load() != 1 {
		t.Fatalf("Zen 不应访问不存在的额度接口: hits=%d usage=%v", hits.Load(), zen)
	}
	w, _ := h.rpc(t, "provider.save", Object{"id": "go", "version": h.s.Config().Version, "provider": Object{"api_key": "infer-two"}}, h.token)
	if strings.Contains(w.Body.String(), "infer-two") {
		t.Fatal("保存响应回显了新推理密钥")
	}
	_, changed := providerUsageItem(t, h, "go")
	mu.Lock()
	gotAuth := append([]string{}, auth...)
	mu.Unlock()
	if hits.Load() != 2 || str(changed, "headline") != "5 小时已用 80%" || fmt.Sprint(gotAuth) != "[Bearer infer-one Bearer infer-two]" {
		t.Fatalf("换推理 Key 后未立即重查: hits=%d auth=%v usage=%v", hits.Load(), gotAuth, changed)
	}
}
