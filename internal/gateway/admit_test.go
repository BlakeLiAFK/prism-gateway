package gateway

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// 结算合并成一个事务后，会话亲和这类辅助写入失败不能连累记账：
// 请求记录必须照常更新，用量汇总照常累加。
func TestFinishKeepsAccountingWhenSessionWriteFails(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", modelFixture("m", "chat"))
	c := h.s.Config()
	m, _ := c.model("m")
	pr, _ := c.provider("p_test")
	sel := selection{Model: m, Provider: pr, Body: Object{"max_tokens": 16}}
	e := h.a.Engine

	id, err := e.admit(sel, Principal{ID: "k"}, "req", "m", "chat", "s1", client{})
	if err != nil {
		t.Fatal(err)
	}
	// 让会话亲和的 upsert 必然失败
	if err := h.s.DB.Exec("DROP TABLE sessions"); err != nil {
		t.Fatal(err)
	}
	e.finish(sel, id, "s1", "k", Usage{Input: 7, Output: 3, Known: true}, 200, nil, now())

	rows, err := h.s.DB.Query("SELECT status,input_tokens,output_tokens FROM requests WHERE id=?", id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("查询请求记录: %v %v", rows, err)
	}
	if rows[0].String("status") != "success" || rows[0].Int("input_tokens") != 7 || rows[0].Int("output_tokens") != 3 {
		t.Fatalf("记账不应被会话写入失败回滚: %v", rows[0])
	}
	sum, err := h.s.DB.Query("SELECT COALESCE(SUM(requests),0) n FROM usage_hourly")
	if err != nil || sum[0].Int("n") != 1 {
		t.Fatalf("用量汇总应累加 1 条: %v %v", sum, err)
	}
	if e.global != 0 {
		t.Fatalf("并发名额应已归还，实得 %d", e.global)
	}
}

// 预留记录写不进库时，admit 已在内存里占的并发与 RPM 名额必须归还，
// 否则每次写库失败都会永久漏掉一个名额，直到并发被耗尽。
func TestAdmitReleasesSlotWhenInsertFails(t *testing.T) {
	for _, budget := range []bool{false, true} {
		t.Run(fmt.Sprint("budget=", budget), func(t *testing.T) {
			h := newHarness(t)
			m := modelFixture("m", "chat")
			m.RPM = 10
			if budget {
				m.PricingSet, m.Limit30d = true, 1000
			}
			h.configure(t, "http://127.0.0.1:1", m)
			// 换成同名视图：对视图 INSERT 必然失败，SUM 查询仍可用
			for _, q := range []string{"ALTER TABLE requests RENAME TO requests_real", "CREATE VIEW requests AS SELECT * FROM requests_real"} {
				if err := h.s.DB.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			c := h.s.Config()
			mm, _ := c.model("m")
			pr, _ := c.provider("p_test")
			sel := selection{Model: mm, Provider: pr, Body: Object{"max_tokens": 16}}
			e := h.a.Engine
			for i := 0; i < 5; i++ {
				if _, err := e.admit(sel, Principal{ID: "k"}, "req", "m", "chat", "", client{}); err == nil {
					t.Fatal("写库失败时 admit 应返回错误")
				}
			}
			e.mu.Lock()
			st := e.state("m")
			active, global, recent := st.Active, e.global, len(st.Recent)
			e.mu.Unlock()
			if active != 0 || global != 0 || recent != 0 {
				t.Fatalf("名额应全部归还，实得 active=%d global=%d rpm记录=%d", active, global, recent)
			}
		})
	}
}

// 非预算模型的并发上限：只占位不结算，恰好放行上限个，其余被拒，计数与放行数一致。
func TestAdmitNeverExceedsModelConcurrency(t *testing.T) {
	h := newHarness(t)
	m := modelFixture("m", "chat")
	m.Concurrency = 3
	h.configure(t, "http://127.0.0.1:1", m)
	h.change(t, func(c *Config) { c.Settings.GlobalConcurrency = 64 })
	c := h.s.Config()
	mm, _ := c.model("m")
	pr, _ := c.provider("p_test")
	sel := selection{Model: mm, Provider: pr, Body: Object{"max_tokens": 16}}
	e := h.a.Engine

	var ok, full atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.admit(sel, Principal{ID: "k"}, "req", "m", "chat", "", client{})
			switch {
			case err == nil:
				ok.Add(1)
			case strings.Contains(err.Error(), "并发已满"):
				full.Add(1)
			default:
				t.Errorf("意外错误: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 3 || full.Load() != 29 {
		t.Fatalf("应放行 3 个、拒绝 29 个，实得 ok=%d full=%d", ok.Load(), full.Load())
	}
	e.mu.Lock()
	active, global := e.state("m").Active, e.global
	e.mu.Unlock()
	if active != 3 || global != 3 {
		t.Fatalf("计数应为 3，实得 active=%d global=%d", active, global)
	}
}
