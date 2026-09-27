package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"prism-gateway/internal/sqlite"
)

func cacheStatsUpstream(t *testing.T, protocol string, usage Object) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if protocol == "messages" {
			json.NewEncoder(w).Encode(Object{"id": "m", "type": "message", "role": "assistant", "content": []any{Object{"type": "text", "text": "ok"}}, "stop_reason": "end_turn", "usage": usage})
			return
		}
		json.NewEncoder(w).Encode(Object{"id": "c", "choices": []any{Object{"index": 0, "message": Object{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}, "usage": usage})
	}))
	t.Cleanup(up.Close)
	return up
}

func TestCacheStatsFromHandlers(t *testing.T) {
	tests := []struct {
		name, protocol string
		usage          Object
		wantState      int64
		wantCache      int64
		wantInput      int64
		wantSamples    int64
		wantNullRate   float64
	}{
		{"missing", "chat", Object{"prompt_tokens": 100, "completion_tokens": 5}, 0, 0, 0, 0, 0},
		{"explicit-zero", "chat", Object{"prompt_tokens": 100, "completion_tokens": 5, "prompt_tokens_details": Object{"cached_tokens": 0}}, 1, 0, 100, 1, 0},
		{"explicit-null", "chat", Object{"prompt_tokens": 100, "completion_tokens": 5, "prompt_tokens_details": Object{"cached_tokens": nil}}, 2, 0, 0, 1, 1},
		{"messages-read-write", "messages", Object{"input_tokens": 70, "output_tokens": 5, "cache_read_input_tokens": 20, "cache_creation_input_tokens": 10}, 1, 20, 100, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			up := cacheStatsUpstream(t, tc.protocol, tc.usage)
			h.configure(t, up.URL, modelFixture("m", tc.protocol))
			w := h.generate(t, tc.protocol, requestFixture(tc.protocol, "m"))
			requireStatus(t, w, 200)

			rows, err := h.s.DB.Query("SELECT cache_known,cache_tokens,input_tokens,write_tokens FROM requests")
			if err != nil {
				t.Fatal(err)
			}
			if rows[0].Int("cache_known") != tc.wantState || rows[0].Int("cache_tokens") != tc.wantCache {
				t.Fatalf("request cache accounting wrong: %+v", rows[0])
			}
			_, out := h.rpc(t, "model.stats", Object{"days": 1}, h.token)
			u := obj(obj(obj(out["data"])["models"])["m"])
			if int64(num(u, "cache_samples")) != tc.wantSamples || int64(num(u, "cache_input_tokens")) != tc.wantInput || num(u, "cache_null_rate") != tc.wantNullRate {
				t.Fatalf("cache stats wrong: %v", u)
			}
			if tc.wantInput > 0 && num(u, "cache_hit_rate") != float64(tc.wantCache)/float64(tc.wantInput) {
				t.Fatalf("cache hit rate wrong: %v", u)
			}
			if tc.wantSamples == 0 && u["cache_hit_rate"] != nil {
				t.Fatalf("missing cache field must remain unknown: %v", u)
			}
		})
	}
}

func TestRouteCacheStatsAndRollupRebuild(t *testing.T) {
	h := newHarness(t)
	up := cacheStatsUpstream(t, "chat", Object{"prompt_tokens": 100, "completion_tokens": 5, "prompt_tokens_details": Object{"cached_tokens": 25}})
	h.configure(t, up.URL, modelFixture("m", "chat"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "Auto", Strategy: "priority", Enabled: true, Candidates: []Candidate{{ModelID: "m", Weight: 1}}}}
	})
	requireStatus(t, h.generate(t, "chat", requestFixture("chat", "auto")), 200)

	_, out := h.rpc(t, "route.stats", Object{"minutes": 60}, h.token)
	u := obj(obj(obj(out["data"])["cache"])["auto"])
	if num(u, "cache_hit_rate") != 0.25 || num(u, "cache_samples") != 1 {
		t.Fatalf("route cache stats must use requested_model: %v", u)
	}
	if err := h.s.DB.Transaction(func(tx *sqlite.Tx) error { return rebuildRollup(tx, 0) }); err != nil {
		t.Fatal(err)
	}
	rows, err := h.s.DB.Query("SELECT cache_known_requests,cache_null_requests,cache_input_tokens,cache_hit_tokens FROM cache_hourly")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Int("cache_known_requests") != 1 || rows[0].Int("cache_input_tokens") != 100 || rows[0].Int("cache_hit_tokens") != 25 {
		t.Fatalf("rebuilt cache rollup wrong: %+v", rows)
	}
}

func TestReportedCostWithoutTokensDoesNotCreateCacheSample(t *testing.T) {
	h := newHarness(t)
	up := cacheStatsUpstream(t, "chat", Object{"cost": 0.1})
	m := modelFixture("m", "chat")
	h.configure(t, up.URL, m)
	h.change(t, func(c *Config) { c.Providers[0].Kind = "openrouter" })
	requireStatus(t, h.generate(t, "chat", requestFixture("chat", "m")), 200)
	rows, err := h.s.DB.Query("SELECT cost_known,cache_known FROM requests")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Int("cost_known") != 1 || rows[0].Int("cache_known") != 0 {
		t.Fatalf("reported cost must not imply cache coverage: %+v", rows[0])
	}
	_, out := h.rpc(t, "model.stats", Object{"days": 1}, h.token)
	if u := obj(obj(obj(out["data"])["models"])["m"]); num(u, "cache_samples") != 0 || u["cache_hit_rate"] != nil {
		t.Fatalf("cost-only usage must keep cache stats unknown: %v", u)
	}
}

func TestCacheStatsOldDatabaseMigration(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{
		"ALTER TABLE requests DROP COLUMN cache_known",
		"DROP TABLE cache_hourly",
	} {
		if err := h.s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.DB.Exec(`INSERT INTO usage_hourly VALUES (?,?,?,?,?,7,6,1,700,70,35,0,0,600,?)`, now()/3600000, "k", "old-route", "m", "p", now()); err != nil {
		t.Fatal(err)
	}
	if err := h.s.migrate(); err != nil {
		t.Fatal(err)
	}
	rows, err := h.s.DB.Query("SELECT requests,cache_tokens FROM usage_hourly")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Int("requests") != 7 || rows[0].Int("cache_tokens") != 35 {
		t.Fatalf("old rollup must be preserved and marked unknown: %+v", rows)
	}
	if cache, err := h.s.DB.Query("SELECT COUNT(*) n FROM cache_hourly"); err != nil || cache[0].Int("n") != 0 {
		t.Fatalf("old rows must not gain inferred cache coverage: %v %v", cache, err)
	}
	// 旧版仍使用 15 列无列名 INSERT；升级后必须继续可写，自动回滚才安全。
	if err := h.s.DB.Exec(`INSERT INTO usage_hourly VALUES (?,?,?,?,?,1,1,0,10,1,0,0,0,10,?)`, now()/3600000-1, "old", "rollback-route", "m", "p", now()-3600000); err != nil {
		t.Fatalf("old binary rollup insert is no longer compatible: %v", err)
	}
	cols, err := h.s.DB.Query("PRAGMA table_info(requests)")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cols {
		found = found || c.String("name") == "cache_known"
	}
	if !found {
		t.Fatal("old requests table did not gain cache_known")
	}
}
