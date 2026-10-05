package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游 400 的原因要能在请求记录里看到，否则管理员面对「上游请求失败（HTTP 400）」无从判断
// 是协议声明错了还是请求有问题；但上游正文不转发给客户端，密钥形态的内容也不落库。
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
	if strings.Contains(w.Body.String(), "does not support") {
		t.Fatalf("上游错误正文不得转发给客户端: %s", w.Body)
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
