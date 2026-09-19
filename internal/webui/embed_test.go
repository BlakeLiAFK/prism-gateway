package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedAssetsAreServable(t *testing.T) {
	h := Handler()
	for _, tc := range []struct{ path, contains string }{
		{"/", "assets/app.js"},
		{"/assets/app.js", "function settings"},
		{"/assets/app.css", ".setting-row"},
		{"/assets/api.js", "api.json"},
		{"/assets/icons.js", "svg"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+tc.path, nil))
		if w.Code != 200 {
			t.Fatalf("%s 返回 %d", tc.path, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.contains) {
			t.Fatalf("%s 内容不符，缺少 %q", tc.path, tc.contains)
		}
	}
	// 单页应用：未知路径回落到首页，但不得穿越到仓库其它文件
	for _, p := range []string{"/../go.mod", "/assets/../../go.mod", "/no-such.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+p, nil))
		if strings.Contains(w.Body.String(), "module prism-gateway") {
			t.Fatalf("%s 泄露了仓库文件", p)
		}
	}
}
