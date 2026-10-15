package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func openCodeUsageProvider(id, base, secret string) Provider {
	return Provider{ID: id, Name: id, Kind: "opencode", Auth: "auto", Secret: secret, AdminSecret: "admin-unused", BaseURL: base + "/inference/go/v1", AllowPrivate: true, Enabled: true, TimeoutSec: 30}
}
func providerUsageItem(t *testing.T, h *harness, id string) (*httptest.ResponseRecorder, Object) {
	t.Helper()
	w, out := h.rpc(t, "provider.usage", Object{}, h.token)
	for _, item := range arr(out["data"]) {
		if v := obj(item); str(v, "id") == id {
			return w, v
		}
	}
	t.Fatalf("缺少供应商 %s 的用量结果", id)
	return w, nil
}
func usageFieldObject(t *testing.T, usage Object, label string) Object {
	t.Helper()
	for _, item := range arr(usage["fields"]) {
		if v := obj(item); str(v, "label") == label {
			return v
		}
	}
	t.Fatalf("缺少字段 %s: %v", label, usage)
	return nil
}
func usageField(t *testing.T, usage Object, label string) string {
	t.Helper()
	return str(usageFieldObject(t, usage, label), "value")
}
func goMeter(used, limit string) Object {
	return Object{"usedMicroCents": used, "limitMicroCents": limit, "startsAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "resetsAt": time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)}
}
func goStatus(meters Object) Object { return Object{"access": Object{"meters": meters}} }

func TestOpenCodeUsageHandlerParsesWindows(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" || r.URL.Path != "/console/api/go/status" || r.Header.Get("Authorization") != "Bearer infer-key" || r.Header.Get("User-Agent") != "" {
			t.Errorf("错误请求: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(403)
			return
		}
		fmt.Fprint(w, raw(goStatus(Object{"fiveHour": goMeter("9033410", "1200000000"), "week": goMeter("9381267", "3000000000"), "month": goMeter("9381267", "6000000000")})))
	}))
	defer server.Close()
	h := newHarness(t)
	h.change(t, func(c *Config) { c.Providers = []Provider{openCodeUsageProvider("go", server.URL, "infer-key")} })
	w, got := providerUsageItem(t, h, "go")
	if got["supported"] != true || str(got, "headline") != "5 小时已用 0.75%" {
		t.Fatalf("精确用量解析错误: %v", got)
	}
	if value := usageField(t, got, "5 小时"); value != "已用 0.75% · $0.0903341 / $12.00" {
		t.Fatal(value)
	}
	if !strings.Contains(usageField(t, got, "每周"), "0.31%") || !strings.Contains(usageField(t, got, "每月"), "0.16%") {
		t.Fatal(got)
	}
	if num(usageFieldObject(t, got, "5 小时"), "reset_at") <= float64(now()) {
		t.Fatal("有效重置时间丢失")
	}
	if strings.Contains(w.Body.String(), "infer-key") || strings.Contains(w.Body.String(), "admin-unused") {
		t.Fatal("凭证回显")
	}
	_, again := providerUsageItem(t, h, "go")
	if hits.Load() != 1 || again["headline"] != got["headline"] {
		t.Fatal("缓存未生效")
	}
}

func TestOpenCodeZeroAndUnknownMeters(t *testing.T) {
	for _, tc := range []struct {
		name         string
		meter        Object
		known, reset bool
		value        string
	}{
		{"零用量已开始", goMeter("0", "1200000000"), true, true, "已用 0.00%"},
		{"零用量未开始", Object{"usedMicroCents": "0", "limitMicroCents": "1200000000", "resetsAt": time.Now().Add(5 * time.Hour).Format(time.RFC3339)}, true, false, "窗口尚未开始"},
		{"微小用量", goMeter("1", "1200000000"), true, true, "<0.01%"},
		{"将满", goMeter("1199999999", "1200000000"), true, true, ">99.99%"},
		{"已满", goMeter("1200000000", "1200000000"), true, true, "已达限额"},
		{"缺失", Object{}, false, false, "待确认"},
		{"负数", goMeter("-1", "1200000000"), false, false, "待确认"},
		{"零上限", goMeter("1", "0"), false, false, "待确认"},
		{"浮点字符串", goMeter("1.5", "1200000000"), false, false, "待确认"},
		{"整数溢出", goMeter("999999999999999999999", "1200000000"), false, false, "待确认"},
		{"非法时间", Object{"usedMicroCents": "5", "limitMicroCents": "100", "resetsAt": "bad"}, true, false, "重置时间待确认"},
		{"过期", Object{"usedMicroCents": "5", "limitMicroCents": "100", "resetsAt": "2000-01-01T00:00:00Z"}, true, false, "等待上游刷新"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headline, fields := parseOpenCodeUsage(goStatus(Object{"fiveHour": tc.meter}))
			f := usageFieldObject(t, Object{"fields": fields}, "5 小时")
			if (headline != "") != tc.known || (f["reset_at"] != nil) != tc.reset || !strings.Contains(str(f, "value"), tc.value) {
				t.Fatalf("headline=%s field=%v", headline, f)
			}
		})
	}
	_, fields := parseOpenCodeUsage(goStatus(Object{"week": goMeter("0", "3000000000")}))
	if usageFieldObject(t, Object{"fields": fields}, "每周")["reset_at"] == nil {
		t.Fatal("零周用量仍应显示重置时间")
	}
}

func TestOpenCodeUsageFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"旧接口形状", `{"usage":{"rolling":{"status":"ok","percent":0}}}`, 200},
		{"无套餐", `{"access":null}`, 200},
		{"权限不足", `{"_tag":"Forbidden"}`, 403},
		{"无效JSON", `<html>login</html>`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			h := newHarness(t)
			h.change(t, func(c *Config) { c.Providers = []Provider{openCodeUsageProvider("go", server.URL, "key")} })
			_, got := providerUsageItem(t, h, "go")
			if str(got, "error") == "" || strings.Contains(raw(got), "已用 0%") {
				t.Fatalf("错误被当成零用量: %v", got)
			}
		})
	}
}

func TestOpenCodeUsageZenCacheAndKeyChange(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		used := "1"
		if r.Header.Get("Authorization") == "Bearer infer-two" {
			used = "50"
		}
		fmt.Fprint(w, raw(goStatus(Object{"fiveHour": goMeter(used, "100")})))
	}))
	defer server.Close()
	h := newHarness(t)
	p := openCodeUsageProvider("go", server.URL, "infer-one")
	zen := p
	zen.ID = "zen"
	zen.BaseURL = server.URL + "/zen/v1"
	h.change(t, func(c *Config) { c.Providers = []Provider{p, zen} })
	_, first := providerUsageItem(t, h, "go")
	_, again := providerUsageItem(t, h, "go")
	if hits.Load() != 1 || again["headline"] != first["headline"] {
		t.Fatal("缓存失效")
	}
	_, z := providerUsageItem(t, h, "zen")
	if z["supported"] != false || hits.Load() != 1 {
		t.Fatal("Zen 不应查询Go套餐")
	}
	h.rpc(t, "provider.save", Object{"id": "go", "version": h.s.Config().Version, "provider": Object{"api_key": "infer-two"}}, h.token)
	_, changed := providerUsageItem(t, h, "go")
	if hits.Load() != 2 || str(changed, "headline") != "5 小时已用 50.00%" {
		t.Fatal(changed)
	}
	for _, path := range []string{"/zen/go/v1", "/inference/go/v1"} {
		p.BaseURL = server.URL + path
		addr, ok, _ := providerUsageURL(p)
		if !ok || addr != server.URL+"/console/api/go/status" {
			t.Fatal(addr)
		}
	}
}
