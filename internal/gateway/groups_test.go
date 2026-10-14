package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func (h *harness) routeSave(t *testing.T, route Object) (*httptest.ResponseRecorder, Object) {
	t.Helper()
	return h.rpc(t, "route.save", Object{"id": str(route, "id"), "version": h.s.Config().Version, "route": route}, h.token)
}

func groupRoute(h *harness, t *testing.T, groups ...Group) {
	t.Helper()
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "r", Name: "r", Enabled: true, Groups: groups}}
	})
}

func twoGroupHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("a", "chat"), modelFixture("b", "chat"), modelFixture("c", "chat"))
	groupRoute(h, t,
		Group{Name: "日常", Strategy: "priority", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "a", Weight: 10}}},
		Group{Name: "兜底", Strategy: "weighted", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "b", Weight: 10}, {ModelID: "c", Weight: 10}}})
	return h
}

// 已发布的配置快照不可变（AGENTS.md 第 7 条）：保存路由时 v = old 再解码，
// 不能经由共享的切片底层数组改到正在服务请求的快照。
func TestRouteSaveDoesNotMutatePublishedSnapshot(t *testing.T) {
	h := twoGroupHarness(t)
	snap := h.s.Config()
	w, _ := h.routeSave(t, Object{"id": "r", "groups": []any{
		Object{"name": "只剩一组", "strategy": "cost", "affinity": false, "candidates": []any{Object{"model_id": "a", "weight": 99}}}}})
	requireStatus(t, w, 200)
	r, _ := snap.route("r")
	if len(r.Groups) != 2 || r.Groups[0].Candidates[0].Weight != 10 || r.Groups[0].Strategy != "priority" || len(r.Groups[1].Candidates) != 2 {
		t.Fatalf("已发布的快照被改写: %+v", r.Groups)
	}
	// 只改描述：组与候选保持不变，且不触碰旧快照
	w, _ = h.routeSave(t, Object{"id": "r", "description": "x"})
	requireStatus(t, w, 200)
	now, _ := h.s.Config().route("r")
	if len(now.Groups) != 1 || now.Description != "x" {
		t.Fatalf("只改描述不该动组: %+v", now)
	}
	// cloneConfig 逐组深拷贝
	cp := cloneConfig(h.s.Config())
	cp.Routes[0].Groups[0].Candidates[0].Weight = 1
	if cur, _ := h.s.Config().route("r"); cur.Groups[0].Candidates[0].Weight != 99 {
		t.Fatal("cloneConfig 没有深拷贝组内候选")
	}
}

// 旧客户端（旧脚本、暂停开关、curl）发来单组字段：路由只有一组时落到这一组；
// 已经分组时明确拒绝，不能悄悄丢掉写入。
func TestRouteSaveLegacyFields(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("a", "chat"), modelFixture("b", "chat"))
	groupRoute(h, t, Group{Name: "主", Strategy: "priority", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "a", Weight: 10}}})
	w, _ := h.routeSave(t, Object{"id": "r", "candidates": []any{Object{"model_id": "a", "weight": 5}, Object{"model_id": "b", "weight": 6, "disabled": true}}})
	requireStatus(t, w, 200)
	r, _ := h.s.Config().route("r")
	if len(r.Groups) != 1 || r.Groups[0].Strategy != "priority" || len(r.Groups[0].Candidates) != 2 || !r.Groups[0].Candidates[1].Disabled || r.Groups[0].Candidates[0].Weight != 5 {
		t.Fatalf("旧字段 candidates 应落到第 1 组: %+v", r.Groups)
	}
	// 只改策略：候选保持
	w, _ = h.routeSave(t, Object{"id": "r", "strategy": "weighted", "affinity": false})
	requireStatus(t, w, 200)
	r, _ = h.s.Config().route("r")
	if r.Groups[0].Strategy != "weighted" || r.affinity(r.Groups[0]) || len(r.Groups[0].Candidates) != 2 {
		t.Fatalf("只改策略不该动候选: %+v", r.Groups)
	}
	// 旧的 fallback 标记拆成第 2 组
	w, _ = h.routeSave(t, Object{"id": "r", "candidates": []any{Object{"model_id": "a", "weight": 10}, Object{"model_id": "b", "weight": 10, "fallback": true}}})
	requireStatus(t, w, 200)
	r, _ = h.s.Config().route("r")
	if len(r.Groups) != 2 || r.Groups[1].Name != "兜底" || r.Groups[1].Candidates[0].ModelID != "b" || r.Groups[1].Candidates[0].Fallback {
		t.Fatalf("fallback 候选应拆成第 2 组: %+v", r.Groups)
	}
	// 已分组：旧字段没法表达改哪一组，必须拒绝
	w, out := h.routeSave(t, Object{"id": "r", "candidates": []any{Object{"model_id": "a", "weight": 3}}})
	requireStatus(t, w, 400)
	if !strings.Contains(raw(out), "已分组") {
		t.Fatalf("应说明为什么拒绝: %s", w.Body)
	}
	if r2, _ := h.s.Config().route("r"); r2.Groups[0].Candidates[0].Weight != 10 {
		t.Fatal("被拒绝的写入不得生效")
	}
	// groups 与旧字段同时出现、groups 里残留 fallback：都拒绝
	w, _ = h.routeSave(t, Object{"id": "r", "strategy": "cost", "groups": []any{Object{"name": "x", "strategy": "priority", "candidates": []any{}}}})
	requireStatus(t, w, 400)
	w, _ = h.routeSave(t, Object{"id": "r", "groups": []any{Object{"name": "x", "strategy": "priority", "candidates": []any{Object{"model_id": "a", "weight": 1, "fallback": true}}}}})
	requireStatus(t, w, 400)
}

func TestRouteGroupValidation(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("a", "chat"), modelFixture("b", "chat"))
	g := func(name string, ids ...string) Object {
		cs := []any{}
		for _, id := range ids {
			cs = append(cs, Object{"model_id": id, "weight": 10})
		}
		return Object{"name": name, "strategy": "priority", "affinity": true, "candidates": cs}
	}
	for name, groups := range map[string][]any{
		"同一模型出现在两个组": {g("1", "a"), g("2", "a")},
		"同一模型在组内重复":  {g("1", "a", "a")},
		"超过组数上限":     {g("1"), g("2"), g("3"), g("4"), g("5"), g("6")},
		"组策略非法":      {Object{"name": "x", "strategy": "random", "candidates": []any{}}},
	} {
		w, _ := h.routeSave(t, Object{"id": "r", "groups": groups})
		if w.Code != 400 {
			t.Errorf("%s 应被拒绝，实得 %d", name, w.Code)
		}
	}
	// 组内允许为空：新建路由与免费模型路由本来就允许没有候选
	w, _ := h.routeSave(t, Object{"id": "r", "groups": []any{g("空组"), g("有候选", "a", "b")}})
	requireStatus(t, w, 200)
	// 各组策略互相独立
	w, _ = h.routeSave(t, Object{"id": "r", "groups": []any{
		Object{"name": "1", "strategy": "latency", "affinity": false, "candidates": []any{}},
		Object{"name": "2", "strategy": "cost", "affinity": true, "candidates": []any{}}}})
	requireStatus(t, w, 200)
	r, _ := h.s.Config().route("r")
	if r.Groups[0].Strategy != "latency" || r.Groups[1].Strategy != "cost" || r.affinity(r.Groups[0]) || !r.affinity(r.Groups[1]) {
		t.Fatalf("各组应保留自己的策略与亲和: %+v", r.Groups)
	}
}

// 落库后重新加载得到同样的组；1.36.0 及以前存下的单组路由（含 fallback 标记）加载时自动归组。
func TestRouteGroupsPersistAndLegacyLoad(t *testing.T) {
	h := twoGroupHarness(t)
	if err := h.s.load(); err != nil {
		t.Fatal(err)
	}
	r, _ := h.s.Config().route("r")
	if len(r.Groups) != 2 || r.Groups[1].Name != "兜底" || len(r.Groups[1].Candidates) != 2 {
		t.Fatalf("重新加载后组丢失: %+v", r.Groups)
	}
	legacy := `{"id":"old","name":"old","strategy":"weighted","enabled":true,"affinity":false,"candidates":[{"model_id":"a","weight":10},{"model_id":"b","weight":5,"fallback":true}],"description":"d"}`
	if err := h.s.DB.Exec("INSERT INTO routes VALUES ('old', ?)", legacy); err != nil {
		t.Fatal(err)
	}
	if err := h.s.load(); err != nil {
		t.Fatal(err)
	}
	old, ok := h.s.Config().route("old")
	if !ok || len(old.Groups) != 2 || old.Groups[0].Strategy != "weighted" || old.affinity(old.Groups[0]) || old.Groups[1].Candidates[0].ModelID != "b" || old.Strategy != "" || len(old.Candidates) != 0 {
		t.Fatalf("旧路由应归成两组并清空旧字段: %+v", old)
	}
	if strings.Contains(raw(old), `"candidates":[{"model_id":"a"`) && strings.Contains(raw(old), `"strategy":"weighted","enabled"`) {
		t.Fatalf("规范化后不应再输出旧字段: %s", raw(old))
	}
}

func TestConfigExportImportKeepsGroups(t *testing.T) {
	h := twoGroupHarness(t)
	_, out := h.rpc(t, "config.export", Object{}, h.token)
	exported := obj(out["data"])
	groupRoute(h, t, Group{Name: "临时", Strategy: "priority", Candidates: []Candidate{}})
	w, _ := h.rpc(t, "config.import", Object{"version": h.s.Config().Version, "config": exported["config"], "format": exported["format"]}, h.token)
	requireStatus(t, w, 200)
	r, _ := h.s.Config().route("r")
	if len(r.Groups) != 2 || r.Groups[1].Strategy != "weighted" {
		t.Fatalf("导入后组不一致: %+v", r.Groups)
	}
}

// route.test 按组序列出候选并标明所在组；前一组的候选永远排在后一组前面
func TestRouteTestListsGroups(t *testing.T) {
	h := twoGroupHarness(t)
	w, out := h.rpc(t, "route.test", Object{"id": "r"}, h.token)
	requireStatus(t, w, 200)
	ranked := arr(obj(out["data"])["ranked"])
	if len(ranked) != 3 || str(obj(ranked[0]), "model_id") != "a" || str(obj(ranked[0]), "group") != "日常" || str(obj(ranked[1]), "group") != "兜底" {
		t.Fatalf("应按组序排列并标明组: %v", ranked)
	}
	if !strings.Contains(str(obj(out["data"]), "note"), "后一组只在前一组") {
		t.Fatalf("多组路由应提示组序语义: %v", out["data"])
	}
}

// 第 1 组挂了换第 2 组；第 2 组有自己的亲和，同一会话一直落在同一个兜底模型；
// 第 1 组的绑定不被改写；第 1 组恢复后请求回到第 1 组；落到后面的组时发告警。
func TestGroupSwitchAffinityAndReturn(t *testing.T) {
	var aFail atomic.Bool
	hits := map[string]*atomic.Int32{"a": {}, "b": {}, "c": {}}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := Object{}
		json.NewDecoder(r.Body).Decode(&o)
		id := strings.TrimPrefix(str(o, "model"), "upstream-")
		hits[id].Add(1)
		if id == "a" && aFail.Load() {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	h := newHarness(t)
	h.configure(t, up.URL, modelFixture("a", "chat"), modelFixture("b", "chat"), modelFixture("c", "chat"))
	groupRoute(h, t,
		Group{Name: "日常", Strategy: "priority", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "a", Weight: 10}}},
		Group{Name: "兜底", Strategy: "weighted", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "b", Weight: 10}, {ModelID: "c", Weight: 10}}})
	var alerts atomic.Int32
	var alertKey atomic.Value
	h.a.Engine.OnAlert = func(event, key, text string) {
		if event == "route" && strings.Contains(text, "第 2 组") {
			alerts.Add(1)
			alertKey.Store(key)
		}
	}
	req := func() *httptest.ResponseRecorder {
		w := h.generate(t, "chat", requestFixture("chat", "r"))
		requireStatus(t, w, 200)
		return w
	}
	for i := 0; i < 3; i++ {
		if req().Header().Get("X-Prism-Model") != "a" {
			t.Fatal("第 1 组健康时全部请求都该留在第 1 组")
		}
	}
	if hits["b"].Load()+hits["c"].Load() != 0 || alerts.Load() != 0 {
		t.Fatal("第 1 组健康时第 2 组不该有流量，也不该告警")
	}
	aFail.Store(true)
	first := req().Header().Get("X-Prism-Model")
	if first != "b" && first != "c" {
		t.Fatalf("第 1 组被限流后应落到第 2 组，实得 %s", first)
	}
	for i := 0; i < 12; i++ {
		if got := req().Header().Get("X-Prism-Model"); got != first {
			t.Fatalf("第 2 组亲和应让同一会话一直用 %s，实得 %s", first, got)
		}
	}
	rows, _ := h.s.DB.Query("SELECT id, model_id FROM sessions")
	var base, grp string
	for _, r := range rows {
		if strings.HasSuffix(r.String("id"), ":g1") {
			grp = r.String("model_id")
		} else {
			base = r.String("model_id")
		}
	}
	if base != "a" || grp != first {
		t.Fatalf("第 1 组绑定应保持 a、第 2 组绑定应是 %s: base=%s g1=%s", first, base, grp)
	}
	deadline := time.Now().Add(2 * time.Second)
	for alerts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if alerts.Load() == 0 || alertKey.Load() != "route-group:r" {
		t.Fatalf("落到第 2 组应按路由告警: n=%d key=%v", alerts.Load(), alertKey.Load())
	}
	// 第 1 组恢复：冷却结束后下一次请求回到第 1 组原来的模型
	aFail.Store(false)
	time.Sleep(1200 * time.Millisecond)
	if got := req().Header().Get("X-Prism-Model"); got != "a" {
		t.Fatalf("第 1 组恢复后应回到第 1 组，实得 %s", got)
	}
}

// 直接按模型 ID 调用是一个不记亲和的虚拟组，行为不变
func TestDirectModelCallStaysUngrouped(t *testing.T) {
	up, _ := historyUpstream(t)
	h := newHarness(t)
	h.configure(t, up.URL, modelFixture("a", "chat"))
	w := h.generate(t, "chat", requestFixture("chat", "a"))
	requireStatus(t, w, 200)
	if w.Header().Get("X-Prism-Model") != "a" {
		t.Fatal("直接调用应命中该模型")
	}
	rows, _ := h.s.DB.Query("SELECT id FROM sessions")
	for _, r := range rows {
		if strings.Contains(r.String("id"), ":g") {
			t.Fatalf("直接调用不该写分组键: %s", r.String("id"))
		}
	}
}

// 第 1 组被管理员暂停（或本来就接不了这类请求）时，请求落到第 2 组是预期行为，不是故障，不告警。
func TestNoGroupAlertWhenEarlierGroupWasNeverTried(t *testing.T) {
	up, _ := historyUpstream(t)
	h := newHarness(t)
	h.configure(t, up.URL, modelFixture("a", "chat"), modelFixture("b", "chat"))
	groupRoute(h, t,
		Group{Name: "日常", Strategy: "priority", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "a", Weight: 10, Disabled: true}}},
		Group{Name: "兜底", Strategy: "priority", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "b", Weight: 10}}})
	alerts := captureAlerts(h)
	w := h.generate(t, "chat", requestFixture("chat", "r"))
	requireStatus(t, w, 200)
	if w.Header().Get("X-Prism-Model") != "b" {
		t.Fatalf("第 1 组被暂停时应由第 2 组服务: %s", w.Header().Get("X-Prism-Model"))
	}
	requireNoAlert(t, alerts)
}
