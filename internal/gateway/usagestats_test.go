package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func insertUsage(t *testing.T, h *harness, model, provider, status, role string, at, cost int64, known, demo bool) {
	t.Helper()
	if err := h.s.DB.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,duration_ms,input_tokens,output_tokens,cost_nano,cost_known,is_demo,reason,agent_role,usage_mode)
		VALUES (?,?,?,?,?,?,'chat','chat','',?,?,100,10,5,?,?,?,'',?,'reported_tokens')`, randomID("att_"), "p", "k", model, model, provider, status, at, cost, known, demo, role); err != nil {
		t.Fatal(err)
	}
}

// 花费只算已结束的非演示请求；未计价的成功请求单独计数；供应商由模型汇总
func TestModelStatsAggregates(t *testing.T) {
	h := newHarness(t)
	n := now()
	insertUsage(t, h, "a", "p1", "success", "", n, 2e9, true, false)
	insertUsage(t, h, "a", "p1", "running", "", n, 5e9, true, false)
	insertUsage(t, h, "a", "p1", "error", "", n, 0, true, false)
	insertUsage(t, h, "b", "p1", "success", "", n, 0, false, false)
	insertUsage(t, h, "c", "p2", "success", "", n, 7e9, true, true)
	insertUsage(t, h, "a", "p1", "success", "", n-10*86400000, 9e9, true, false)
	w, out := h.rpc(t, "model.stats", Object{}, h.token)
	requireStatus(t, w, 200)
	d := obj(out["data"])
	a := obj(obj(d["models"])["a"])
	if num(d, "days") != 7 || num(a, "requests") != 3 || num(a, "success") != 1 || num(a, "errors") != 1 || num(a, "cost") != 2 || num(a, "output_tokens") != 15 {
		t.Fatalf("模型 a 的 7 天汇总不对: %v", a)
	}
	p1 := obj(obj(d["providers"])["p1"])
	if num(p1, "requests") != 4 || num(p1, "unpriced") != 1 || num(p1, "cost") != 2 {
		t.Fatalf("供应商 p1 汇总不对: %v", p1)
	}
	if c := obj(obj(d["models"])["c"]); num(c, "cost") != 0 {
		t.Fatalf("演示请求不应计入花费: %v", c)
	}
	// 30 秒内同一时间范围走内存缓存，不重新聚合
	insertUsage(t, h, "a", "p1", "success", "", n, 0, true, false)
	_, out = h.rpc(t, "model.stats", Object{}, h.token)
	if num(obj(obj(obj(out["data"])["models"])["a"]), "requests") != 3 {
		t.Fatal("30 秒内应返回缓存的统计")
	}
}

// 每日分桶补齐空白日期，角色按 agent_role 拆分
func TestModelUsageDaily(t *testing.T) {
	h := newHarness(t)
	n := now()
	insertUsage(t, h, "a", "p1", "success", "subagent", n, 1e9, true, false)
	insertUsage(t, h, "a", "p1", "success", "", n, 1e9, true, false)
	insertUsage(t, h, "a", "p1", "success", "", n-3*86400000, 1e9, true, false)
	w, out := h.rpc(t, "model.usage", Object{"id": "a", "days": 7, "tz_offset_min": -480}, h.token)
	requireStatus(t, w, 200)
	d := obj(out["data"])
	daily := arr(d["daily"])
	if len(daily) != 7 || num(obj(daily[6]), "requests") != 2 || num(obj(daily[3]), "requests") != 1 || num(obj(daily[5]), "requests") != 0 {
		t.Fatalf("每日分桶不对: %v", daily)
	}
	if num(obj(daily[1]), "time")-num(obj(daily[0]), "time") != 86400000 {
		t.Fatal("相邻两天应相差一天")
	}
	roles := arr(d["roles"])
	if len(roles) != 2 || str(obj(roles[0]), "role") != "" || num(obj(roles[0]), "requests") != 2 {
		t.Fatalf("角色拆分不对: %v", roles)
	}
	if w, _ := h.rpc(t, "model.usage", Object{}, h.token); w.Code != 400 {
		t.Fatalf("缺少 id 应返回 400，实得 %d", w.Code)
	}
}

// Z.AI 每个窗口带 nextResetTime（毫秒）：主位窗口单列重置倒计时，其他窗口附在数值后；Lite 套餐的 CREDIT_LIMIT 按 unit 命名
func TestZaiUsageShowsResetTime(t *testing.T) {
	h5, week := now()+int64(2*3600000+30*60000+30000), now()+int64(3*86400000+30*60000)
	o := Object{}
	body := fmt.Sprintf(`{"data":{"limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"percentage":5},
		{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":91,"nextResetTime":%d},
		{"type":"CREDIT_LIMIT","unit":6,"number":1,"percentage":40,"nextResetTime":%d}]}}`, h5, week)
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatal(err)
	}
	head, fields := parseUsage(Provider{Kind: "zai"}, o)
	got := []string{}
	for _, f := range fields {
		got = append(got, str(obj(f), "label")+"="+str(obj(f), "value"))
	}
	want := "5 小时重置=2 小时 30 分后|MCP 用量 · 1 个月=5%|每周=40% · 3 天 0 小时后重置"
	if head != "5 小时 91%" || strings.Join(got, "|") != want {
		t.Fatalf("Z.AI 解析不对: %q %q", head, got)
	}
}

// 补录只处理上游报了 tokens、未计价、非演示且模型已确认价格的记录；预览不写入，重复执行不重复计价
func TestRepriceBackfillsOnlyKnownUsage(t *testing.T) {
	h := newHarness(t)
	priced, free := modelFixture("priced", "chat"), modelFixture("free", "chat")
	priced.PricingSet, priced.InputPrice, priced.OutputPrice = true, 1, 2
	h.configure(t, "http://127.0.0.1:1", priced, free)
	n := now()
	insertUsage(t, h, "priced", "p", "success", "", n, 0, false, false) // 10 入 5 出 → (10*1+5*2)*1000 = 20000 nano
	insertUsage(t, h, "priced", "p", "success", "", n, 0, false, true)  // 演示：不动
	insertUsage(t, h, "free", "p", "success", "", n, 0, false, false)   // 仍未确认价格：跳过
	if err := h.s.DB.Exec("INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,reason,usage_mode) VALUES ('att_unknown','p','k','priced','priced','p','chat','chat','','unknown',?,'','reserved_unknown')", n); err != nil {
		t.Fatal(err)
	}
	run := func(dry bool) Object {
		w, out := h.rpc(t, "request.reprice", Object{"dry_run": dry}, h.token)
		requireStatus(t, w, 200)
		return obj(out["data"])
	}
	if d := run(true); num(d, "count") != 1 || num(d, "cost") != 0.00002 || len(arr(d["unpriced_models"])) != 1 {
		t.Fatalf("预览结果不对: %v", d)
	}
	if rows, _ := h.s.DB.Query("SELECT COUNT(*) n FROM requests WHERE cost_known=1"); rows[0].Int("n") != 0 {
		t.Fatal("预览不应写入")
	}
	run(false)
	rows, _ := h.s.DB.Query("SELECT id, cost_nano, cost_known, usage_mode FROM requests WHERE cost_known=1")
	if len(rows) != 1 || rows[0].Int("cost_nano") != 20000 || rows[0].String("usage_mode") != repricedMode {
		t.Fatalf("应只补录一条并标记: %v", rows)
	}
	if d := run(false); num(d, "count") != 0 {
		t.Fatalf("重复执行不应再次计价: %v", d)
	}
}
