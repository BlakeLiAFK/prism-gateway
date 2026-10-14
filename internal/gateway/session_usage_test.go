package gateway

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

type sessionRequestFixture struct {
	session, model, provider, status, mode string
	cost                                   int64
	known, demo                            bool
}

func seedSessionRequest(t *testing.T, h *harness, r sessionRequestFixture) {
	t.Helper()
	known, demo := 0, 0
	if r.known {
		known = 1
	}
	if r.demo {
		demo = 1
	}
	if err := h.s.DB.Exec(`INSERT INTO requests
		(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,cost_nano,cost_known,usage_mode,reason,is_demo)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, randomID("att_"), "parent", "key", r.model, r.model, r.provider, "chat", "chat", r.session, r.status, now(), r.cost, known, r.mode, "seed", demo); err != nil {
		t.Fatal(err)
	}
}

func listedSessions(t *testing.T, h *harness) map[string]Object {
	t.Helper()
	w, out := h.rpc(t, "session.list", Object{}, h.token)
	requireStatus(t, w, 200)
	rows := map[string]Object{}
	for _, value := range arr(out["data"]) {
		row := obj(value)
		rows[str(row, "id")] = row
	}
	return rows
}

func seedSessionUsage(t *testing.T, h *harness, timestamp int64) {
	t.Helper()
	for _, s := range []struct {
		id, model string
		at        int64
	}{
		{"ses_main", "current", timestamp}, {"ses_main:g1", "fallback", timestamp - 1},
		{"ses_other", "other", timestamp - 2}, {"ses_empty", "empty", timestamp - 3},
		{"ses_expired", "old", timestamp - 30*86400000},
	} {
		if err := h.s.DB.Exec("INSERT INTO sessions VALUES (?,?,?,?,?,1)", s.id, "key", s.model, "provider", s.at); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []sessionRequestFixture{
		{"ses_main", "m1", "p1", "success", "reported_tokens", 100, true, false},
		{"ses_main", "m1", "p1", "error", "reported_cost", 200, true, false},
		{"ses_main", "m2", "p2", "unknown", "unknown", 300, false, false},
		{"ses_main", "m2", "p2", "unknown", "reserved_unknown", 400, false, false},
		{"ses_main", "m2", "p2", "running", "reserved", 500, true, false},
		{"ses_main", "free", "p1", "success", "reported_tokens", 0, true, false},
		{"ses_main", "demo", "local-demo", "success", "reported_tokens", 900, true, true},
		{"ses_main", "m2", "p2", "error", "rejected", 700, false, false},
		{"ses_main", "", "", "error", "incompatible", 800, false, false},
		{"ses_other", "other", "p3", "success", "reported_cost", 999, true, false},
	} {
		seedSessionRequest(t, h, r)
	}
}

func requireMainSessionUsage(t *testing.T, rows map[string]Object) {
	t.Helper()
	if len(rows) != 4 || rows["ses_expired"] != nil {
		t.Fatalf("过期会话过滤不正确: %v", rows)
	}
	main := rows["ses_main"]
	if str(main, "key_id") != "key" || str(main, "model_id") != "current" || num(main, "expires_at") == 0 {
		t.Fatalf("旧字段应完整保留: %v", main)
	}
	if num(main, "usage_requests") != 9 || num(main, "cost_nano") != 300 || num(main, "cost_known_requests") != 4 || num(main, "cost_unknown_requests") != 3 || num(main, "demo_requests") != 1 {
		t.Fatalf("会话汇总不正确: %v", main)
	}
	models := map[string]Object{}
	for _, value := range arr(main["models"]) {
		model := obj(value)
		models[str(model, "model_id")] = model
	}
	if len(models) != 4 || num(models["m1"], "requests") != 2 || num(models["m1"], "success_requests") != 1 || num(models["m2"], "requests") != 4 || num(models["m2"], "success_requests") != 0 {
		t.Fatalf("模型次数应包含失败记录并排除无模型记录: %v", main["models"])
	}
	group := rows["ses_main:g1"]
	for _, field := range []string{"models", "usage_requests", "cost_nano", "cost_known_requests", "cost_unknown_requests", "demo_requests"} {
		if raw(group[field]) != raw(main[field]) {
			t.Fatalf("多组映射应共享 %s 汇总: main=%v group=%v", field, main[field], group[field])
		}
	}
	other := rows["ses_other"]
	if num(other, "usage_requests") != 1 || num(other, "cost_nano") != 999 || num(other, "cost_known_requests") != 1 {
		t.Fatalf("其他会话不应串入: %v", other)
	}
	if empty := rows["ses_empty"]; raw(empty["models"]) != "[]" || num(empty, "usage_requests") != 0 || num(empty, "cost_nano") != 0 {
		t.Fatalf("无历史会话应返回空数组与零计数: %v", empty)
	}
}

func TestSessionListAggregatesUsageAndConfirmedCost(t *testing.T) {
	h := newHarness(t)
	seedSessionUsage(t, h, now())
	requireMainSessionUsage(t, listedSessions(t, h))
	indexes, err := h.s.DB.Query("SELECT name FROM sqlite_master WHERE type='index' AND name='requests_session'")
	if err != nil || len(indexes) != 1 {
		t.Fatalf("缺少会话批量查询索引: rows=%v err=%v", indexes, err)
	}
}

func TestSessionListKeepsKeyAndSubagentHistorySeparate(t *testing.T) {
	h, _, call := affinityHarness(t)
	call(nil)
	call(map[string]string{"x-claude-code-agent-id": "agent-1"})
	key := h.createKey(t)
	r := httptest.NewRequest("POST", "http://localhost/openai/v1/chat/completions", strings.NewReader(raw(requestFixture("chat", "auto"))))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("x-claude-code-session-id", "cc-session")
	w := httptest.NewRecorder()
	h.a.ServeHTTP(w, r)
	requireStatus(t, w, 200)

	rows := listedSessions(t, h)
	if len(rows) != 3 {
		t.Fatalf("主线程、子代理和另一把 Key 应各自保留会话: %v", rows)
	}
	for id, row := range rows {
		models := arr(row["models"])
		if num(row, "usage_requests") != 1 || len(models) != 1 || str(obj(models[0]), "model_id") != "first" || num(obj(models[0]), "success_requests") != 1 {
			t.Fatalf("会话 %s 的真实请求历史关联不正确: %v", id, row)
		}
	}
}

func TestSessionListKeepsTwoHundredMappingLimit(t *testing.T) {
	h := newHarness(t)
	timestamp := now()
	for i := 0; i < 201; i++ {
		id := fmt.Sprintf("ses_%03d", i)
		if err := h.s.DB.Exec("INSERT INTO sessions VALUES (?,?,?,?,?,1)", id, "key", "model", "provider", timestamp+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	rows := listedSessions(t, h)
	if len(rows) != 200 || rows["ses_000"] != nil || rows["ses_200"] == nil {
		t.Fatalf("应保留最近 200 条映射: count=%d oldest=%v newest=%v", len(rows), rows["ses_000"], rows["ses_200"])
	}
}
