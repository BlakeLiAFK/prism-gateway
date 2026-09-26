package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 对拍：真实请求经 finish 累加进汇总表，与直接聚合请求明细的结果一致
func TestRollupMatchesRequests(t *testing.T) {
	h := newHarness(t)
	up, _ := captureUpstream(t, false)
	m := modelFixture("m", "chat")
	m.PricingSet, m.InputPrice, m.OutputPrice = true, 1, 2
	h.configure(t, up.URL, m)
	key := h.createKey(t)
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(raw(requestFixture("chat", "m"))))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.a.ServeHTTP(w, r)
		requireStatus(t, w, 200)
	}
	sum := func(q string) Object {
		rows, err := h.s.DB.Query(q)
		if err != nil || len(rows) != 1 {
			t.Fatal(err, rows)
		}
		o := Object{}
		for k, v := range rows[0] {
			o[k] = v
		}
		return o
	}
	want := sum(`SELECT COUNT(*) r, SUM(output_tokens) o, SUM(cost_nano) c, MAX(key_id) k FROM requests`)
	got := sum(`SELECT SUM(requests) r, SUM(output_tokens) o, SUM(cost_nano) c, MAX(key_id) k FROM usage_hourly`)
	for _, k := range []string{"r", "o", "c", "k"} {
		if want[k] != got[k] {
			t.Fatalf("汇总表 %s=%v，明细 %v", k, got[k], want[k])
		}
	}
	// 访问密钥列表附带用量；key.usage 给出每日、常用模型与来源
	_, out := h.rpc(t, "apikey.list", Object{"days": 1}, h.token)
	if u := obj(obj(arr(out["data"])[0])["usage"]); num(u, "requests") != 3 || num(u, "success") != 3 {
		t.Fatalf("Key 列表的用量不对: %v", u)
	}
	kid := str(want, "k")
	_, out = h.rpc(t, "key.usage", Object{"id": kid, "days": 7}, h.token)
	d := obj(out["data"])
	if daily := arr(d["daily"]); len(daily) != 7 || num(obj(daily[6]), "requests") != 3 {
		t.Fatalf("每日用量不对: %v", daily)
	}
	if ms := arr(d["models"]); len(ms) != 1 || str(obj(ms[0]), "id") != "m" || len(arr(d["sources"])) != 1 {
		t.Fatalf("常用模型或来源不对: %v", d)
	}
}

// 计价补录后，汇总表的花费同步更新
func TestRepriceRebuildsRollup(t *testing.T) {
	h := newHarness(t)
	priced := modelFixture("priced", "chat")
	priced.PricingSet, priced.InputPrice, priced.OutputPrice = true, 1, 2
	h.configure(t, "http://127.0.0.1:1", priced)
	insertUsage(t, h, "priced", "p", "success", "", now(), 0, false, false)
	h.rpc(t, "request.reprice", Object{}, h.token)
	rows, _ := h.s.DB.Query("SELECT SUM(cost_nano) c, SUM(unpriced) u FROM usage_hourly")
	if rows[0].Int("c") != 20000 || rows[0].Int("u") != 0 {
		t.Fatalf("补录后汇总表应有 20000 nano、0 次未计价: %v", rows[0])
	}
}

// 老库升级：汇总表为空时从历史请求回填一次，之后重开不重复回填
func TestRollupBackfillOnMigrate(t *testing.T) {
	h := newHarness(t)
	insertUsage(t, h, "a", "p", "success", "", now(), 1e9, true, false)
	if err := h.s.DB.Exec("DELETE FROM usage_hourly"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := h.s.migrate(); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := h.s.DB.Query("SELECT SUM(requests) n, SUM(cost_nano) c FROM usage_hourly")
	if rows[0].Int("n") != 1 || rows[0].Int("c") != 1e9 {
		t.Fatalf("应回填且只回填一次: %v", rows[0])
	}
}
