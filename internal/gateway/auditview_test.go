package gateway

import "testing"

func seedAudit(t *testing.T, h *harness) {
	t.Helper()
	for _, row := range []struct {
		id, action, target string
		version            int
		at                 int64
	}{
		{"audit_1", "provider.create", "provider-alpha", 2, 1000},
		{"audit_2", "provider.update", "provider-beta", 3, 2000},
		{"audit_3", "model.create", "model-alpha", 4, 3000},
		{"audit_4", "route.update", "route-main", 5, 4000},
		{"audit_5", "apikey.revoke", "key-alpha", 6, 5000},
	} {
		if err := h.s.DB.Exec("INSERT INTO audit_logs VALUES (?,?,?,?,?)", row.id, row.action, row.target, row.version, row.at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditSearchAuthFiltersAndTimeRange(t *testing.T) {
	h := newHarness(t)
	seedAudit(t, h)

	w, _ := h.rpc(t, "audit.search", Object{}, "")
	requireStatus(t, w, 401)

	for _, tc := range []struct {
		name   string
		params Object
		want   []string
	}{
		{"按操作筛选", Object{"action": "provider."}, []string{"audit_2", "audit_1"}},
		{"按目标筛选", Object{"target": "alpha"}, []string{"audit_5", "audit_3", "audit_1"}},
		{"全文筛选", Object{"q": "route-main"}, []string{"audit_4"}},
		{"按时间筛选", Object{"from": 2000, "to": 4000}, []string{"audit_4", "audit_3", "audit_2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, out := h.rpc(t, "audit.search", tc.params, h.token)
			requireStatus(t, w, 200)
			data := obj(out["data"])
			items := arr(data["items"])
			if len(items) != len(tc.want) || int64(num(data, "total")) != int64(len(tc.want)) {
				t.Fatalf("结果数量不正确: %v", data)
			}
			for i, want := range tc.want {
				if got := str(obj(items[i]), "id"); got != want {
					t.Fatalf("第 %d 条=%q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestAuditSearchPaginationAndBoundaries(t *testing.T) {
	h := newHarness(t)
	seedAudit(t, h)

	w, out := h.rpc(t, "audit.search", Object{"page": 2, "page_size": 2}, h.token)
	requireStatus(t, w, 200)
	data := obj(out["data"])
	items := arr(data["items"])
	if len(items) != 2 || str(obj(items[0]), "id") != "audit_3" || num(data, "total") != 5 || num(data, "pages") != 3 {
		t.Fatalf("分页结果不正确: %v", data)
	}

	_, out = h.rpc(t, "audit.search", Object{"page": 0, "page_size": 999}, h.token)
	data = obj(out["data"])
	if num(data, "page") != 1 || num(data, "page_size") != 100 || len(arr(data["items"])) != 5 {
		t.Fatalf("分页边界未生效: %v", data)
	}

	for _, params := range []Object{
		{"q": string(make([]byte, 121))},
		{"from": 5000, "to": 1000},
		{"to": 253402300800000},
	} {
		w, out = h.rpc(t, "audit.search", params, h.token)
		requireStatus(t, w, 400)
		if str(obj(out["error"]), "code") != "INVALID_PARAMS" {
			t.Fatalf("错误码不正确: %v", out)
		}
	}
}
