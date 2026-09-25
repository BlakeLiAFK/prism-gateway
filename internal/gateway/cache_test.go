package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// last_used 每分钟最多落库一次；Key 校验结果走内存
func TestKeyLastUsedThrottled(t *testing.T) {
	h := newHarness(t)
	key := h.createKey(t)
	call := func() {
		r := httptest.NewRequest("GET", "/openai/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		requireStatus(t, w, 200)
	}
	lastUsed := func() int64 {
		rows, err := h.s.DB.Query("SELECT COALESCE(last_used,0) v FROM api_keys")
		if err != nil || len(rows) != 1 {
			t.Fatal(err, rows)
		}
		return rows[0].Int("v")
	}
	call()
	if lastUsed() == 0 {
		t.Fatal("首次使用应写入 last_used")
	}
	if err := h.s.DB.Exec("UPDATE api_keys SET last_used=1"); err != nil {
		t.Fatal(err)
	}
	call()
	if lastUsed() != 1 {
		t.Fatal("一分钟内再次使用不应重复写 last_used")
	}
}

// 没设预算的模型不查用量也能放行；设了预算的仍按预留金额拒绝
func TestBudgetOnlyCheckedWhenSet(t *testing.T) {
	h := newHarness(t)
	free := modelFixture("free", "chat")
	capped := modelFixture("capped", "chat")
	for _, m := range []*Model{&free, &capped} {
		m.PricingSet, m.InputPrice, m.OutputPrice = true, 10, 10
	}
	capped.Limit5h = 0.000001
	h.configure(t, "http://127.0.0.1:1", free, capped)
	if !hasBudget(capped) || hasBudget(free) {
		t.Fatal("hasBudget 判断错误")
	}
	if p, err := h.a.Engine.pressure(free); err != nil || p != 0 {
		t.Fatalf("无预算时压力应为 0: %v %v", p, err)
	}
	w := h.generate(t, "chat", requestFixture("chat", "capped"))
	if w.Code != 429 || !strings.Contains(w.Body.String(), "LOCAL_QUOTA_LIMIT") {
		t.Fatalf("超出本地预算应拒绝: %d %s", w.Code, w.Body)
	}
}
