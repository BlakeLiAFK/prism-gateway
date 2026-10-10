package gateway

import (
	"fmt"
	"testing"
)

type requestFilterFixture struct {
	id, status, provider, model, mode string
	at, input, output, cost           int64
	costKnown                         bool
}

func seedRequestFilters(t *testing.T, h *harness, fixtures ...requestFilterFixture) {
	t.Helper()
	for _, f := range fixtures {
		known := 0
		if f.costKnown {
			known = 1
		}
		if err := h.s.DB.Exec(`INSERT INTO requests
			(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,input_tokens,output_tokens,cost_nano,cost_known,usage_mode,reason)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			f.id, "parent-"+f.id, "key", f.model, f.model, f.provider, "chat", "chat", "", f.status,
			f.at, f.input, f.output, f.cost, known, f.mode, "seed"); err != nil {
			t.Fatal(err)
		}
	}
}

func requestList(t *testing.T, h *harness, p Object) Object {
	t.Helper()
	w, o := h.rpc(t, "request.list", p, h.token)
	if w.Code != 200 || o["ok"] != true {
		t.Fatalf("request.list failed: status=%d body=%s", w.Code, w.Body)
	}
	return obj(o["data"])
}

func requestIDs(data Object) []string {
	items := arr(data["items"])
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = str(obj(item), "id")
	}
	return ids
}

func requireRequestIDs(t *testing.T, data Object, want ...string) {
	t.Helper()
	got := requestIDs(data)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ids=%v want=%v data=%v", got, want, data)
	}
}

func requireRequestListRejected(t *testing.T, h *harness, p Object) {
	t.Helper()
	w, o := h.rpc(t, "request.list", p, h.token)
	if w.Code != 400 || str(obj(o["error"]), "code") != "INVALID_PARAMS" {
		t.Fatalf("params=%v status=%d body=%s", p, w.Code, w.Body)
	}
}

func TestRequestListDefaultsToLast24Hours(t *testing.T) {
	h := newHarness(t)
	before := now()
	seedRequestFilters(t, h,
		requestFilterFixture{id: "recent", status: "success", provider: "p", model: "m", at: before - 1000, mode: "reported_tokens"},
		requestFilterFixture{id: "old", status: "success", provider: "p", model: "m", at: before - 24*60*60*1000 - 1000, mode: "reported_tokens"},
	)
	data := requestList(t, h, Object{})
	after := now()
	requireRequestIDs(t, data, "recent")
	from, to := int64(num(data, "from")), int64(num(data, "to"))
	if to-from != 24*60*60*1000 || to < before+1 || to > after+1 {
		t.Fatalf("unexpected default interval: from=%d to=%d before=%d after=%d", from, to, before, after)
	}
}

func TestRequestListDefaultRangeIncludesCurrentMillisecond(t *testing.T) {
	const timestamp int64 = 1_700_000_000_000
	q, err := buildRequestQuery(Object{}, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if q.to != timestamp+1 || q.from != q.to-24*60*60*1000 {
		t.Fatalf("default interval must include current millisecond: from=%d to=%d", q.from, q.to)
	}
}

func TestRequestListUsesHalfOpenBoundedTimeRange(t *testing.T) {
	h := newHarness(t)
	const day = int64(24 * 60 * 60 * 1000)
	from := now() - day
	seedRequestFilters(t, h,
		requestFilterFixture{id: "before", status: "success", provider: "p", model: "m", at: from - 1, mode: "reported_tokens"},
		requestFilterFixture{id: "at-from", status: "success", provider: "p", model: "m", at: from, mode: "reported_tokens"},
		requestFilterFixture{id: "inside", status: "success", provider: "p", model: "m", at: from + 1, mode: "reported_tokens"},
		requestFilterFixture{id: "at-to", status: "success", provider: "p", model: "m", at: from + day, mode: "reported_tokens"},
	)
	data := requestList(t, h, Object{"from": from, "to": from + day})
	requireRequestIDs(t, data, "inside", "at-from")
	if int64(num(data, "from")) != from || int64(num(data, "to")) != from+day {
		t.Fatalf("response interval mismatch: %v", data)
	}

	data = requestList(t, h, Object{"from": from, "to": from + 30*day})
	if int64(num(data, "to"))-int64(num(data, "from")) != 30*day {
		t.Fatalf("exactly 30 days must be accepted: %v", data)
	}
}

func TestRequestListRejectsInvalidTimeAndIntegerFilters(t *testing.T) {
	h := newHarness(t)
	const day = int64(24 * 60 * 60 * 1000)
	bad := []Object{
		{"all_time": true},
		{"from": 1},
		{"to": 2},
		{"from": -1, "to": 2},
		{"from": 2, "to": 2},
		{"from": 3, "to": 2},
		{"from": 1, "to": 1 + 30*day + 1},
		{"from": 1.5, "to": 2},
		{"from": "1", "to": 2},
		{"from": 9_007_199_254_740_992, "to": 9_007_199_254_740_993},
		{"min_input_tokens": -1},
		{"max_input_tokens": 1.5},
		{"min_output_tokens": "-1"},
		{"max_output_tokens": "1.0"},
		{"min_input_tokens": "9007199254740992"},
		{"min_input_tokens": 9_007_199_254_740_992},
		{"min_input_tokens": 2, "max_input_tokens": 1},
		{"min_output_tokens": 2, "max_output_tokens": 1},
	}
	for i, p := range bad {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			requireRequestListRejected(t, h, p)
		})
	}
}

func TestRequestListCombinesLegacyTokenAndCostFilters(t *testing.T) {
	h := newHarness(t)
	from := now() - 60*60*1000
	seedRequestFilters(t, h,
		requestFilterFixture{id: "match", status: "success", provider: "p1", model: "needle-model", at: from + 9, input: 10, output: 20, cost: 1, costKnown: true, mode: "reported_tokens"},
		requestFilterFixture{id: "wrong-status", status: "error", provider: "p1", model: "needle-model", at: from + 8, input: 10, output: 20, cost: 1, costKnown: true, mode: "reported_tokens"},
		requestFilterFixture{id: "wrong-provider", status: "success", provider: "p2", model: "needle-model", at: from + 7, input: 10, output: 20, cost: 1, costKnown: true, mode: "reported_tokens"},
		requestFilterFixture{id: "unknown-cost", status: "success", provider: "p1", model: "needle-model", at: from + 6, input: 10, output: 20, cost: 1, mode: "reported_tokens"},
		requestFilterFixture{id: "reserved", status: "success", provider: "p1", model: "needle-model", at: from + 5, input: 10, output: 20, cost: 1, costKnown: true, mode: "reserved"},
		requestFilterFixture{id: "wrong-query", status: "success", provider: "p1", model: "other", at: from + 4, input: 10, output: 20, cost: 1, costKnown: true, mode: "reported_tokens"},
	)
	data := requestList(t, h, Object{
		"from": from, "to": from + 100, "q": "needle", "status": "success", "provider_id": "p1",
		"min_input_tokens": "10", "max_input_tokens": 10, "min_output_tokens": 20, "max_output_tokens": "20",
		"min_cost": "0.000000001", "max_cost": "0.000000001",
	})
	requireRequestIDs(t, data, "match")
	if int64(num(data, "total")) != 1 {
		t.Fatalf("total must use the same filters: %v", data)
	}
}

func TestRequestListIncludesRepricedUsage(t *testing.T) {
	h := newHarness(t)
	from := now() - 1000
	seedRequestFilters(t, h,
		requestFilterFixture{id: "repriced", status: "success", at: from + 1, input: 100, output: 200, cost: 300, costKnown: true, mode: repricedMode},
		requestFilterFixture{id: "reported", status: "success", at: from + 2, input: 1, output: 2, cost: 3, costKnown: true, mode: "reported_tokens"},
	)
	requireRequestIDs(t, requestList(t, h, Object{"from": from, "to": from + 100, "min_input_tokens": 100, "min_output_tokens": 200, "min_cost": "0.000000300"}), "repriced")
	for _, sort := range []string{"input_desc", "output_desc", "cost_desc"} {
		requireRequestIDs(t, requestList(t, h, Object{"from": from, "to": from + 100, "sort": sort}), "repriced", "reported")
	}
}

func TestRequestListDistinguishesKnownZeroUsage(t *testing.T) {
	h := newHarness(t)
	from := now() - 60*60*1000
	seedRequestFilters(t, h,
		requestFilterFixture{id: "token-zero", status: "success", provider: "p", model: "m", at: from + 6, costKnown: true, mode: "reported_tokens"},
		requestFilterFixture{id: "cost-zero", status: "success", provider: "p", model: "m", at: from + 5, costKnown: true, mode: "reported_cost"},
		requestFilterFixture{id: "cost-with-token", status: "success", provider: "p", model: "m", at: from + 4, input: 1, output: 2, costKnown: true, mode: "reported_cost"},
		requestFilterFixture{id: "rejected-zero", status: "error", provider: "p", model: "m", at: from + 3, costKnown: true, mode: "rejected"},
		requestFilterFixture{id: "reserved-zero", status: "unknown", provider: "p", model: "m", at: from + 2, costKnown: true, mode: "reserved"},
		requestFilterFixture{id: "unknown-zero", status: "success", provider: "p", model: "m", at: from + 1, mode: "reserved"},
	)
	base := Object{"from": from, "to": from + 100}
	base["max_input_tokens"], base["max_output_tokens"] = 0, 0
	requireRequestIDs(t, requestList(t, h, base), "token-zero")
	base["min_input_tokens"], base["max_input_tokens"] = 1, 1
	base["min_output_tokens"], base["max_output_tokens"] = 2, 2
	requireRequestIDs(t, requestList(t, h, base), "cost-with-token")
	delete(base, "min_input_tokens")
	delete(base, "max_input_tokens")
	delete(base, "min_output_tokens")
	delete(base, "max_output_tokens")
	base["max_cost"] = "0"
	requireRequestIDs(t, requestList(t, h, base), "token-zero", "cost-zero", "cost-with-token", "rejected-zero")
	base["max_cost"] = ""
	requireRequestIDs(t, requestList(t, h, base), "token-zero", "cost-zero", "cost-with-token", "rejected-zero", "reserved-zero", "unknown-zero")
}

func TestRequestListRejectsInvalidCostsAndSort(t *testing.T) {
	h := newHarness(t)
	for _, value := range []any{0.1, "-1", "1e-9", ".1", "1.", "0.0000000001", "9223372036.854775808", true} {
		requireRequestListRejected(t, h, Object{"min_cost": value})
	}
	requireRequestListRejected(t, h, Object{"min_cost": "2", "max_cost": "1"})
	requireRequestListRejected(t, h, Object{"sort": "random"})
}

func TestRequestListSortsBeforeStablePagination(t *testing.T) {
	h := newHarness(t)
	from := now() - 60*60*1000
	seedRequestFilters(t, h,
		requestFilterFixture{id: "a", status: "success", provider: "p", model: "m", at: from + 1, input: 100, output: 1, cost: 20, costKnown: true, mode: "reported_tokens"},
		requestFilterFixture{id: "b", status: "success", provider: "p", model: "m", at: from + 5, input: 5, output: 90, cost: 10, costKnown: true, mode: "reported_tokens"},
		requestFilterFixture{id: "c", status: "success", provider: "p", model: "m", at: from + 4, input: 80, output: 80, cost: 30, costKnown: true, mode: "reported_tokens"},
		requestFilterFixture{id: "d", status: "success", provider: "p", model: "m", at: from + 4, input: 80, output: 80, cost: 30, costKnown: true, mode: "reported_tokens"},
		requestFilterFixture{id: "unknown", status: "success", provider: "p", model: "m", at: from + 9, input: 999, output: 999, cost: 999, mode: "reserved"},
	)
	base := Object{"from": from, "to": from + 100}
	for sort, want := range map[string][]string{
		"newest":      {"unknown", "b", "d", "c", "a"},
		"oldest":      {"a", "c", "d", "b", "unknown"},
		"input_asc":   {"b", "d", "c", "a", "unknown"},
		"output_asc":  {"a", "d", "c", "b", "unknown"},
		"cost_asc":    {"b", "a", "d", "c", "unknown"},
		"input_desc":  {"a", "d", "c", "b", "unknown"},
		"output_desc": {"b", "d", "c", "a", "unknown"},
		"cost_desc":   {"d", "c", "a", "b", "unknown"},
	} {
		base["sort"] = sort
		requireRequestIDs(t, requestList(t, h, base), want...)
	}
	base["sort"], base["page_size"], base["page"] = "input_desc", 2, 2
	data := requestList(t, h, base)
	requireRequestIDs(t, data, "c", "b")
	if int64(num(data, "total")) != 5 {
		t.Fatalf("unexpected total: %v", data)
	}
}
