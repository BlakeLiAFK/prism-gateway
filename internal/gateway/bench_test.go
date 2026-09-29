package gateway

// 热路径基准：选候选、准入与结算、预算查询。
// 用法：go test -run '^$' -bench . -benchmem ./internal/gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"prism-gateway/internal/sqlite"
)

// benchEngine 建一个带三个候选的路由：两个 chat（跨协议）加一个 messages（原生）
func benchEngine(b *testing.B, upstream string, mutate func(*Config)) *Engine {
	b.Helper()
	s, err := OpenStore(filepath.Join(b.TempDir(), "gateway.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.DB.Close() })
	_, err = s.Change(s.Config().Version, "bench", "bench", func(c *Config) error {
		c.Providers = []Provider{{ID: "p_test", Name: "bench", Kind: "custom", BaseURL: upstream, Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		c.Models = []Model{modelFixture("a", "chat"), modelFixture("b", "chat"), modelFixture("c", "messages")}
		// 并发上限放到最大，基准衡量的是锁与查询开销，不该被并发限流挡掉
		for i := range c.Models {
			c.Models[i].Concurrency = 128
		}
		c.Settings.GlobalConcurrency = 512
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority",
			Candidates: []Candidate{{ModelID: "a", Weight: 10}, {ModelID: "b", Weight: 10}, {ModelID: "c", Weight: 10}}}}
		if mutate != nil {
			mutate(c)
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	e := NewEngine(s)
	b.Cleanup(e.Close)
	return e
}

// benchBody 生成约 kb KB 的 messages 请求体
func benchBody(kb int) Object {
	msgs := []any{}
	for i := 0; i < kb/2; i++ {
		msgs = append(msgs, Object{"role": "user", "content": []any{Object{"type": "text", "text": strings.Repeat("x", 2000)}}})
	}
	return Object{"model": "auto", "messages": msgs, "max_tokens": 256}
}

// fakeUpstream 按路径返回最小的合法响应
func fakeUpstream(b *testing.B) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			io.WriteString(w, `{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		io.WriteString(w, `{"id":"x","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	b.Cleanup(srv.Close)
	return srv
}

// seedRequests 给模型预置 n 条已成功的历史请求（每条 1 纳美元），用来放大预算汇总查询的开销
func seedRequests(tb testing.TB, db *sqlite.DB, model string, n int) {
	tb.Helper()
	err := db.Transaction(func(t *sqlite.Tx) error {
		for i := 0; i < n; i++ {
			if err := t.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,reason,cost_nano) VALUES (?,?,?,?,?,?,?,?,?,'success',?,'seed',1)`,
				fmt.Sprint("seed", i), "p", "k", "auto", model, "p_test", "chat", "chat", "s", now()-int64(i)*1000); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkSelections 单独衡量选候选：请求体每个候选被复制一次
func BenchmarkSelections(b *testing.B) {
	for _, kb := range []int{64, 2048} {
		b.Run(fmt.Sprintf("%dKB", kb), func(b *testing.B) {
			e := benchEngine(b, "http://127.0.0.1:1", nil)
			body := benchBody(kb)
			c := e.store.Config()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := e.selections(c, body, "messages", ""); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkHandle 端到端：selections + admit + 上游往返 + finish，并发调用
func BenchmarkHandle(b *testing.B) {
	for _, kb := range []int{64, 2048} {
		b.Run(fmt.Sprintf("%dKB", kb), func(b *testing.B) {
			up := fakeUpstream(b)
			e := benchEngine(b, up.URL, nil)
			payload := raw(benchBody(kb))
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					r := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(payload))
					w := httptest.NewRecorder()
					e.Handle(w, r, "messages", Principal{ID: "bench-key"})
					if w.Code != 200 {
						b.Errorf("status=%d %s", w.Code, w.Body)
						return
					}
				}
			})
		})
	}
}

// BenchmarkAdmitBudget 衡量设了本地预算的模型：每次准入都要汇总 30 天窗口内的请求
func BenchmarkAdmitBudget(b *testing.B) {
	for _, rows := range []int{0, 50000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			e := benchEngine(b, "http://127.0.0.1:1", func(c *Config) {
				for i := range c.Models {
					c.Models[i].Limit30d = 1e6
					c.Models[i].PricingSet = true
				}
			})
			seedRequests(b, e.store.DB, "a", rows)
			c := e.store.Config()
			m, _ := c.model("a")
			pr, _ := c.provider("p_test")
			sel := selection{Model: m, Provider: pr, Body: Object{"max_tokens": 16}}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					id, err := e.admit(sel, Principal{ID: "k"}, "req", "auto", "chat", "", client{})
					if err != nil {
						b.Error(err)
						return
					}
					e.finish(sel, id, "", "k", Usage{Input: 1, Output: 1, Known: true}, 200, nil, now())
				}
			})
		})
	}
}

// BenchmarkAdmitMixed 一个带预算的模型（5 万行历史）持续被请求，同时衡量另一个无预算模型的准入耗时。
// 预算汇总占着全局锁时，无关模型的请求也会被拖慢。
func BenchmarkAdmitMixed(b *testing.B) {
	e := benchEngine(b, "http://127.0.0.1:1", func(c *Config) {
		c.Models[0].Limit30d = 1e6
		c.Models[0].PricingSet = true
	})
	seedRequests(b, e.store.DB, "a", 50000)
	c := e.store.Config()
	pick := func(id string) selection {
		m, _ := c.model(id)
		pr, _ := c.provider("p_test")
		return selection{Model: m, Provider: pr, Body: Object{"max_tokens": 16}}
	}
	budgeted, plain := pick("a"), pick("b")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if id, err := e.admit(budgeted, Principal{ID: "k"}, "req", "auto", "chat", "", client{}); err == nil {
				e.finish(budgeted, id, "", "k", Usage{Input: 1, Output: 1, Known: true}, 200, nil, now())
			}
			time.Sleep(20 * time.Millisecond) // 稳定流量，不是死循环独占连接
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, err := e.admit(plain, Principal{ID: "k"}, "req", "auto", "chat", "", client{})
		if err != nil {
			b.Fatal(err)
		}
		e.finish(plain, id, "", "k", Usage{Input: 1, Output: 1, Known: true}, 200, nil, now())
	}
	b.StopTimer()
	close(stop)
	<-done
}

// BenchmarkLoadDuringBudgetAdmit 衡量纯内存路径 load() 在预算模型准入期间的延迟：
// e.mu 被长操作占用时，选候选、Health 这类纯内存调用也会被挡住。
func BenchmarkLoadDuringBudgetAdmit(b *testing.B) {
	e := benchEngine(b, "http://127.0.0.1:1", func(c *Config) {
		c.Models[0].Limit30d = 1e6
		c.Models[0].PricingSet = true
	})
	seedRequests(b, e.store.DB, "a", 50000)
	c := e.store.Config()
	ma, _ := c.model("a")
	mb, _ := c.model("b")
	pr, _ := c.provider("p_test")
	sel := selection{Model: ma, Provider: pr, Body: Object{"max_tokens": 16}}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if id, err := e.admit(sel, Principal{ID: "k"}, "req", "auto", "chat", "", client{}); err == nil {
				e.finish(sel, id, "", "k", Usage{Input: 1, Output: 1, Known: true}, 200, nil, now())
			}
		}
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.load(mb)
	}
	b.StopTimer()
	close(stop)
	<-done
}

// BenchmarkAdmitFinish 无预算模型的准入 + 结算，并发调用：衡量每个请求的落库开销
func BenchmarkAdmitFinish(b *testing.B) {
	e := benchEngine(b, "http://127.0.0.1:1", nil)
	c := e.store.Config()
	m, _ := c.model("b")
	pr, _ := c.provider("p_test")
	sel := selection{Model: m, Provider: pr, Body: Object{"max_tokens": 16}}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id, err := e.admit(sel, Principal{ID: "k"}, "req", "auto", "chat", "sess", client{})
			if err != nil {
				b.Error(err)
				return
			}
			e.finish(sel, id, "sess", "k", Usage{Input: 1, Output: 1, Known: true}, 200, nil, now())
		}
	})
}
