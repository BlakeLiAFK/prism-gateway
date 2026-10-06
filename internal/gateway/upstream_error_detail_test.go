package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游 400 的原因要能在请求记录里看到；最后一个候选的原话也要原样交给客户端（Claude Code 据此自行调整），
// 两处都不能带密钥形态的内容。
func TestUpstreamErrorDetailRecorded(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"model does not support /messages; key sk-abcdefghijklmnop1234 Bearer abc.def-123"}}`)
	}))
	t.Cleanup(up.Close)
	h.configure(t, up.URL, modelFixture("b", "messages"))
	w := h.generate(t, "messages", claudeCodeRequest("b", Object{"type": "disabled"}))
	requireStatus(t, w, 400)
	if b := w.Body.String(); !strings.Contains(b, "model does not support /messages") || strings.Contains(b, "abcdefghijklmnop1234") || !strings.Contains(b, `"invalid_request_error"`) {
		t.Fatalf("上游原话应脱敏后透传给客户端: %s", b)
	}
	if w.Header().Get("Request-Id") == "" || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("两种 SDK 读的请求 ID 头都要有: %v", w.Header())
	}
	rows, err := h.s.DB.Query("SELECT error_detail FROM requests WHERE status='error'")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	d := rows[0].String("error_detail")
	if !strings.Contains(d, "model does not support /messages") {
		t.Fatalf("请求记录缺少上游错误原因: %q", d)
	}
	if strings.Contains(d, "abcdefghijklmnop1234") || strings.Contains(d, "abc.def-123") {
		t.Fatalf("密钥形态的内容必须脱敏: %q", d)
	}
}

func TestUpstreamErrorSummary(t *testing.T) {
	long := `{"message":"` + strings.Repeat("长", 600) + `"}`
	cases := map[string]string{
		`{"error":{"message":"bad"}}`: "bad",
		`{"error":"plain"}`:           "plain",
		`{"detail":"d"}`:              "d",
		"<html>502\n  gateway</html>": "<html>502 gateway</html>",
		"":                            "",
	}
	for in, want := range cases {
		if got := upstreamErrorSummary([]byte(in)); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	if got := []rune(upstreamErrorSummary([]byte(long))); len(got) > 301 {
		t.Fatalf("摘要必须限长: %d", len(got))
	}
}
