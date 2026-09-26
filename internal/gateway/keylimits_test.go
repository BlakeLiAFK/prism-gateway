package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func keyCall(h *harness, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(raw(requestFixture("chat", "m"))))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	return w
}

func keyHarness(t *testing.T, policy Object) (*harness, string, string) {
	t.Helper()
	h := newHarness(t)
	up, _ := captureUpstream(t, false)
	h.configure(t, up.URL, modelFixture("m", "chat"))
	p := Object{"name": "shared", "allowed": []any{}}
	for k, v := range policy {
		p[k] = v
	}
	w, out := h.rpc(t, "apikey.create", p, h.token)
	requireStatus(t, w, 200)
	return h, str(obj(out["data"]), "key"), str(obj(out["data"]), "id")
}

// Key 级 RPM：超过上限的请求直接 429，不进入路由
func TestKeyRPMLimit(t *testing.T) {
	h, key, _ := keyHarness(t, Object{"rpm": 2})
	requireStatus(t, keyCall(h, key), 200)
	requireStatus(t, keyCall(h, key), 200)
	w := keyCall(h, key)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "KEY_RPM_LIMIT") {
		t.Fatalf("第 3 次应被 Key 级 RPM 拒绝: %d %s", w.Code, w.Body)
	}
}

// 到期：过期的 Key 返回 401；改成未来时间后立即恢复（缓存随修改失效）
func TestKeyExpiryAndUpdate(t *testing.T) {
	h, key, id := keyHarness(t, Object{"expires_at": now() - 1000})
	if w := keyCall(h, key); w.Code != 401 || !strings.Contains(w.Body.String(), "KEY_EXPIRED") {
		t.Fatalf("过期 Key 应返回 401: %d %s", w.Code, w.Body)
	}
	w, _ := h.rpc(t, "apikey.update", Object{"id": id, "name": "shared", "expires_at": now() + 3600000}, h.token)
	requireStatus(t, w, 200)
	requireStatus(t, keyCall(h, key), 200)
}

// 花费上限：近 24 小时估算花费达到上限后拒绝，列表附带当前用量
func TestKeyBudgetLimit(t *testing.T) {
	h, key, id := keyHarness(t, Object{"limit_day": 0.5})
	if err := h.s.DB.Exec(`INSERT INTO usage_hourly VALUES (?,?,?,?,?,1,1,0,0,0,0,?,0,0,?)`, now()/3600000, id, "m", "m", "p_test", int64(6e8), now()); err != nil {
		t.Fatal(err)
	}
	w := keyCall(h, key)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "KEY_BUDGET_LIMIT") {
		t.Fatalf("超过近 24 小时花费上限应拒绝: %d %s", w.Code, w.Body)
	}
	_, out := h.rpc(t, "apikey.list", Object{}, h.token)
	if k := obj(arr(out["data"])[0]); num(k, "spend_day") != 0.6 || num(k, "limit_day") != 0.5 {
		t.Fatalf("列表应附带已用与上限: %v", k)
	}
}
