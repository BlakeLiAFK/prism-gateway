package gateway

import "testing"

func TestRouteSaveMoveCandidateAcrossGroups(t *testing.T) {
	h := twoGroupHarness(t)
	h.change(t, func(c *Config) {
		c.Routes[0].Groups[0].Candidates[0].Weight = 37
		c.Routes[0].Groups[0].Candidates[0].Disabled = true
	})
	before := h.s.Config()
	route, _ := cloneConfig(before).route("r")
	moved := route.Groups[0].Candidates[0]
	route.Groups[0].Candidates = []Candidate{}
	route.Groups[1].Candidates = append([]Candidate{moved}, route.Groups[1].Candidates...)
	w, _ := h.routeSave(t, Object{"id": "r", "groups": route.Groups})
	requireStatus(t, w, 200)
	if err := h.s.load(); err != nil {
		t.Fatal(err)
	}
	after, _ := h.s.Config().route("r")
	if len(after.Groups[0].Candidates) != 0 || len(after.Groups[1].Candidates) != 3 {
		t.Fatalf("跨组移动后数量不正确: %+v", after.Groups)
	}
	for i, id := range []string{"a", "b", "c"} {
		if after.Groups[1].Candidates[i].ModelID != id {
			t.Fatalf("跨组顺序未持久化: %+v", after.Groups[1].Candidates)
		}
	}
	if got := after.Groups[1].Candidates[0]; got.Weight != 37 || !got.Disabled {
		t.Fatalf("跨组移动丢失权重或暂停状态: %+v", got)
	}
	old, _ := before.route("r")
	if len(old.Groups[0].Candidates) != 1 || len(old.Groups[1].Candidates) != 2 {
		t.Fatal("移动污染了旧快照")
	}
}

func TestRouteSaveStaleGroupDraftRejected(t *testing.T) {
	h := twoGroupHarness(t)
	version := h.s.Config().Version
	draft, _ := cloneConfig(h.s.Config()).route("r")
	draft.Groups[0].Candidates = append(draft.Groups[0].Candidates, draft.Groups[1].Candidates[0])
	draft.Groups[1].Candidates = draft.Groups[1].Candidates[1:]
	w, _ := h.routeSave(t, Object{"id": "r", "description": "其他窗口已经保存"})
	requireStatus(t, w, 200)
	before := raw(h.s.Config())
	w, _ = h.rpc(t, "route.save", Object{"id": "r", "version": version, "route": Object{"groups": draft.Groups}}, h.token)
	requireStatus(t, w, 409)
	if raw(h.s.Config()) != before {
		t.Fatal("过期分组草稿覆盖了当前配置")
	}
}
