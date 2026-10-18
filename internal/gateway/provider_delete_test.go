package gateway

import (
	"fmt"
	"strings"
	"testing"
)

func providerDeleteFixture(t *testing.T, h *harness) {
	t.Helper()
	h.change(t, func(c *Config) {
		c.Providers = []Provider{
			{ID: "p_drop", Name: "删除", Kind: "custom", BaseURL: "http://127.0.0.1:1", Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
			{ID: "p_keep", Name: "保留", Kind: "custom", BaseURL: "http://127.0.0.1:1", Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30},
		}
		c.Models = []Model{
			{ID: "drop_live", ProviderID: "p_drop", Upstream: "drop_live", Name: "删除启用", Protocol: "chat", Enabled: true, Context: 1000, MaxOutput: 100},
			{ID: "drop_disabled", ProviderID: "p_drop", Upstream: "drop_disabled", Name: "删除停用", Protocol: "chat", Context: 1000, MaxOutput: 100},
			{ID: "keep_live", ProviderID: "p_keep", Upstream: "keep_live", Name: "保留启用", Protocol: "chat", Enabled: true, Context: 1000, MaxOutput: 100},
			{ID: "keep_disabled", ProviderID: "p_keep", Upstream: "keep_disabled", Name: "保留停用", Protocol: "chat", Context: 1000, MaxOutput: 100},
		}
		c.Routes = providerDeleteRoutes()
		c.Aliases = []Alias{
			{ID: "drop_alias", Target: "drop_live", Enabled: true},
			{ID: "drop_disabled_alias", Target: "drop_disabled", Enabled: true},
			{ID: "route_alias", Target: "mixed", Enabled: true},
			{ID: "keep_alias", Target: "keep_live", Enabled: true},
		}
	})
}

func providerDeleteRoutes() []Route {
	return []Route{
		{ID: "mixed", Name: "混合", Enabled: true, Sort: 2, AffinityDefault: boolPtr(false), Groups: []Group{
			{Name: "第一组", Strategy: "priority", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "drop_live", Weight: 10}, {ModelID: "keep_disabled", Weight: 7, Disabled: true}}},
			{Name: "第二组", Strategy: "weighted", Affinity: boolPtr(false), Candidates: []Candidate{{ModelID: "drop_disabled", Weight: 5}, {ModelID: "keep_live", Weight: 3}}},
		}},
		{ID: "emptied", Name: "被清空", Enabled: true, Groups: []Group{{Name: "主", Strategy: "cost", Affinity: boolPtr(false), Candidates: []Candidate{{ModelID: "drop_live", Weight: 4}}}}},
		{ID: "empty_before", Name: "原本为空", Enabled: true, Groups: []Group{{Name: "主", Strategy: "priority", Affinity: boolPtr(true), Candidates: []Candidate{}}}},
		{ID: "disabled", Name: "原本停用", Enabled: false, Groups: []Group{{Name: "主", Strategy: "priority", Affinity: boolPtr(true), Candidates: []Candidate{{ModelID: "drop_disabled", Weight: 2}}}}},
	}
}

func seedProviderDeleteHistory(t *testing.T, h *harness) {
	t.Helper()
	h.createKey(t)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sessions VALUES ('history_session','history_key','drop_live','p_drop',?,2)`, []any{now()}},
		{`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,reason,usage_mode) VALUES ('history_request','parent','history_key','drop_live','drop_live','p_drop','chat','chat','history_session','unknown',?,'seed','reserved_after_restart')`, []any{now()}},
		{`INSERT INTO usage_hourly (hour,key_id,requested_model,model_id,provider_id,requests,success,errors,input_tokens,output_tokens,cache_tokens,cost_nano,unpriced,success_ms,last_at) VALUES (?,?,?,?,?,1,0,1,10,0,0,0,1,0,?)`, []any{now() / 3600000, "history_key", "drop_live", "drop_live", "p_drop", now()}},
	} {
		if err := h.s.DB.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func requireProviderCascade(t *testing.T, c Config) {
	t.Helper()
	if len(c.Providers) != 1 || c.Providers[0].ID != "p_keep" || len(c.Models) != 2 {
		t.Fatalf("供应商或模型删除范围错误: providers=%+v models=%+v", c.Providers, c.Models)
	}
	models := map[string]bool{}
	for _, m := range c.Models {
		models[m.ID] = m.ProviderID == "p_keep"
	}
	if !models["keep_live"] || !models["keep_disabled"] {
		t.Fatalf("保留模型错误: %+v", c.Models)
	}
	mixed, _ := c.route("mixed")
	if !mixed.Enabled || len(mixed.Groups) != 2 || mixed.Groups[0].Name != "第一组" || mixed.Groups[1].Strategy != "weighted" || mixed.affinity(mixed.Groups[1]) {
		t.Fatalf("混合路由属性被改写: %+v", mixed)
	}
	if got := mixed.Groups[0].Candidates; len(got) != 1 || got[0].ModelID != "keep_disabled" || got[0].Weight != 7 || !got[0].Disabled {
		t.Fatalf("第一组剩余候选未原样保留: %+v", got)
	}
	if got := mixed.Groups[1].Candidates; len(got) != 1 || got[0].ModelID != "keep_live" || got[0].Weight != 3 {
		t.Fatalf("第二组剩余候选未原样保留: %+v", got)
	}
	if r, _ := c.route("emptied"); r.Enabled || len(r.Groups[0].Candidates) != 0 {
		t.Fatalf("本次清空的路由应停用: %+v", r)
	}
	if r, _ := c.route("empty_before"); !r.Enabled {
		t.Fatalf("原本为空的未受影响路由不应停用: %+v", r)
	}
	if r, _ := c.route("disabled"); r.Enabled || len(r.Groups[0].Candidates) != 0 {
		t.Fatalf("原本停用的受影响路由应保持停用: %+v", r)
	}
	aliases := map[string]bool{}
	for _, a := range c.Aliases {
		aliases[a.ID] = true
	}
	if len(c.Aliases) != 2 || !aliases["route_alias"] || !aliases["keep_alias"] {
		t.Fatalf("别名删除范围错误: %+v", c.Aliases)
	}
}

func requireHistoryPreserved(t *testing.T, h *harness) {
	t.Helper()
	for table, want := range map[string]int64{"sessions": 1, "requests": 1, "usage_hourly": 1, "api_keys": 1} {
		rows, err := h.s.DB.Query("SELECT COUNT(*) n FROM " + table)
		if err != nil || rows[0].Int("n") != want {
			t.Fatalf("%s 历史数据未保留: rows=%v err=%v", table, rows, err)
		}
	}
	rows, _ := h.s.DB.Query("SELECT usage_mode FROM requests WHERE id='history_request'")
	if len(rows) != 1 || rows[0].String("usage_mode") != "reserved_after_restart" {
		t.Fatalf("崩溃预留请求被改写: %v", rows)
	}
}

func TestProviderDeleteCascadesConfigurationOnly(t *testing.T) {
	h := newHarness(t)
	providerDeleteFixture(t, h)
	seedProviderDeleteHistory(t, h)
	before, version := h.s.Config(), h.s.Config().Version
	w, out := h.rpc(t, "provider.delete", Object{"id": "p_drop", "version": version}, h.token)
	requireStatus(t, w, 200)
	if out["ok"] != true || h.s.Config().Version != version+1 {
		t.Fatalf("关联删除应只增加一次版本: %s", w.Body)
	}
	requireProviderCascade(t, h.s.Config())
	oldMixed, _ := before.route("mixed")
	oldEmptied, _ := before.route("emptied")
	if len(before.Models) != 4 || len(before.Aliases) != 4 || len(oldMixed.Groups[0].Candidates) != 2 || oldMixed.Groups[0].Candidates[0].ModelID != "drop_live" || !oldEmptied.Enabled {
		t.Fatalf("旧快照被关联删除改写: %+v", before)
	}
	_, got := h.rpc(t, "config.get", Object{}, h.token)
	payload := raw(got["data"])
	if strings.Contains(payload, "drop_live") || !strings.Contains(payload, "keep_live") || !strings.Contains(payload, "route_alias") {
		t.Fatalf("config.get 仍返回已删除模型: %v", got["data"])
	}
	if err := h.s.load(); err != nil {
		t.Fatal(err)
	}
	requireProviderCascade(t, h.s.Config())
	requireHistoryPreserved(t, h)
	rows, _ := h.s.DB.Query("SELECT COUNT(*) n FROM audit_logs WHERE action='provider.delete'")
	if rows[0].Int("n") != 1 {
		t.Fatalf("成功删除应只写一条审计: %v", rows)
	}
}

func TestProviderDeleteFailuresDoNotPublish(t *testing.T) {
	t.Run("不存在与版本冲突", func(t *testing.T) {
		h := newHarness(t)
		providerDeleteFixture(t, h)
		version := h.s.Config().Version
		for _, tc := range []struct {
			id string
			v  int64
			co string
		}{{"missing", version, "NOT_FOUND"}, {"p_drop", version - 1, "VERSION_CONFLICT"}} {
			w, out := h.rpc(t, "provider.delete", Object{"id": tc.id, "version": tc.v}, h.token)
			if w.Code == 200 || str(obj(out["error"]), "code") != tc.co || h.s.Config().Version != version {
				t.Fatalf("失败请求改变了配置: status=%d out=%v", w.Code, out)
			}
		}
		requireProviderFixtureIntact(t, h)
		rows, _ := h.s.DB.Query("SELECT COUNT(*) n FROM audit_logs WHERE action='provider.delete'")
		if rows[0].Int("n") != 0 {
			t.Fatalf("失败请求仍写入审计: %v", rows)
		}
	})
	t.Run("事务失败", func(t *testing.T) {
		h := newHarness(t)
		providerDeleteFixture(t, h)
		version := h.s.Config().Version
		if err := h.s.DB.Exec(`CREATE TRIGGER abort_provider_delete BEFORE DELETE ON models WHEN OLD.provider_id='p_drop' BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
			t.Fatal(err)
		}
		w, out := h.rpc(t, "provider.delete", Object{"id": "p_drop", "version": version}, h.token)
		if w.Code == 200 || out["ok"] != false || h.s.Config().Version != version {
			t.Fatalf("事务失败仍发布了配置: status=%d out=%v", w.Code, out)
		}
		requireProviderFixtureIntact(t, h)
		rows, _ := h.s.DB.Query("SELECT COUNT(*) n FROM audit_logs WHERE action='provider.delete'")
		if rows[0].Int("n") != 0 {
			t.Fatalf("事务失败仍写入审计: %v", rows)
		}
	})
}

func requireProviderFixtureIntact(t *testing.T, h *harness) {
	t.Helper()
	c := h.s.Config()
	mixed, _ := c.route("mixed")
	if len(c.Providers) != 2 || len(c.Models) != 4 || len(c.Aliases) != 4 || len(mixed.Groups[0].Candidates) != 2 {
		t.Fatalf("内存配置被失败请求改写: %+v", c)
	}
	for table, want := range map[string]int64{"providers": 2, "models": 4, "routes": 4, "aliases": 4} {
		rows, err := h.s.DB.Query("SELECT COUNT(*) n FROM " + table)
		if err != nil || rows[0].Int("n") != want {
			t.Fatalf("%s 数据库配置被失败请求改写: rows=%v err=%v", table, rows, err)
		}
	}
	rows, err := h.s.DB.Query("SELECT value FROM meta WHERE key='config_version'")
	if err != nil || rows[0].String("value") != fmt.Sprint(c.Version) {
		t.Fatalf("数据库配置版本被失败请求改写: rows=%v err=%v", rows, err)
	}
}
