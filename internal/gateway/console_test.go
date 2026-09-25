package gateway

import "testing"

func TestRouteStatsGroupsByRequestedModel(t *testing.T) {
	h := newHarness(t)
	for _, r := range []struct{ req, model, status, reason string }{
		{"auto", "a", "success", "balanced; affinity=true"}, {"auto", "a", "error", "balanced; affinity=false"}, {"auto", "b", "success", ""}, {"lite", "c", "success", ""},
	} {
		if err := h.s.DB.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,reason)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, randomID("att_"), "p", "k", r.req, r.model, "p", "chat", "chat", "", r.status, now(), r.reason); err != nil {
			t.Fatal(err)
		}
	}
	w, out := h.rpc(t, "route.stats", Object{}, h.token)
	requireStatus(t, w, 200)
	d := obj(out["data"])
	a := obj(obj(obj(d["share"])["auto"])["a"])
	if num(a, "attempts") != 2 || num(a, "success") != 1 || num(a, "affinity") != 1 {
		t.Fatalf("auto→a 应为 2 次尝试 1 次成功、1 次亲和: %v", d["share"])
	}
	if obj(d["runtime"]) == nil || num(d, "minutes") != 60 {
		t.Fatalf("应附带运行状态与默认 60 分钟窗口: %v", d)
	}
}

func TestSessionDeleteByModel(t *testing.T) {
	h := newHarness(t)
	for _, s := range []struct{ id, model string }{{"s1", "glm"}, {"s2", "glm"}, {"s3", "other"}} {
		if err := h.s.DB.Exec(`INSERT INTO sessions VALUES (?,?,?,?,?,1)`, s.id, "k", s.model, "p", now()); err != nil {
			t.Fatal(err)
		}
	}
	w, out := h.rpc(t, "session.delete", Object{"model_id": "glm"}, h.token)
	requireStatus(t, w, 200)
	if num(obj(out["data"]), "unbound") != 2 {
		t.Fatalf("应解除 2 个绑定: %v", out)
	}
	rows, _ := h.s.DB.Query("SELECT id FROM sessions")
	if len(rows) != 1 || rows[0].String("id") != "s3" {
		t.Fatalf("只应删除绑定到 glm 的会话: %v", rows)
	}
	if w, _ := h.rpc(t, "session.delete", Object{}, h.token); w.Code != 400 {
		t.Fatalf("既无 id 也无 model_id 应拒绝，实得 %d", w.Code)
	}
}
