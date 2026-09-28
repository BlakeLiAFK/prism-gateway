package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 上游列表使用显式 OpenRouter 类型，测试服务器只负责返回模型数据。
const freeListJSON = `{"data":[
 {"id":"v/new-a:free","name":"New A","created":300,"context_length":262144,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools","reasoning"]},
 {"id":"v/new-b:free","name":"New B","created":400,"context_length":1000000,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
 {"id":"v/lib:free","name":"Lib","created":200,"context_length":262144,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
 {"id":"v/back:free","name":"Back","created":100,"context_length":262144,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
 {"id":"v/small:free","context_length":65536,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
 {"id":"v/notools:free","context_length":262144,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["reasoning"]},
 {"id":"v/excl:free","context_length":262144,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
 {"id":"v/paid","context_length":262144,"pricing":{"prompt":"0.000001","completion":"0.000002"},"supported_parameters":["tools"]}]}`

func freeHarness(t *testing.T) (*harness, *atomic.Bool) {
	var broken atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, freeListJSON)
	}))
	t.Cleanup(up.Close)
	h := newHarness(t)
	orm := func(id, upstream string, enabled bool) Model {
		m := modelFixture(id, "chat")
		m.ProviderID, m.Upstream, m.Enabled = "p_or", upstream, enabled
		return m
	}
	h.change(t, func(c *Config) {
		c.Providers = []Provider{{ID: "p_or", Name: "OpenRouter", Kind: "openrouter", BaseURL: up.URL, Auth: "none", Enabled: true, AllowPrivate: true, TimeoutSec: 30}}
		c.Models = []Model{
			orm("lite-old", "v/old:free", true),    // 在路由里、上游已下架 → 停用
			orm("lite-back", "v/back:free", false), // 在路由里、停用、上游又出现 → 重新启用
			orm("lib-model", "v/lib:free", false),  // 模型库有、不在路由 → 启用并加入
			orm("lite-excl", "v/excl:free", false), // 排除 → 不动
			orm("paid-model", "v/paid", true),      // 收费模型 → 不动
		}
		c.Routes = []Route{{ID: "lite", Name: "lite", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "lite-old", Weight: 30}, {ModelID: "lite-back", Weight: 20}, {ModelID: "lite-excl", Weight: 10}, {ModelID: "paid-model", Weight: 10}}}}
	})
	w, _ := h.rpc(t, "schedule.save", Object{"free_enabled": true, "free_exclude": " v/excl:free "}, h.token)
	requireStatus(t, w, 200)
	return h, &broken
}

func TestFreeModelsAutoMaintain(t *testing.T) {
	h, broken := freeHarness(t)
	r := runScheduled(t, h, "free")
	if str(r, "status") != "succeeded" {
		t.Fatalf("运行失败: %v", r)
	}
	c := h.s.Config()
	route, _ := c.route("lite")
	order := []string{}
	for _, cd := range route.Candidates {
		order = append(order, cd.ModelID)
	}
	// 新加入的排在末尾，按上线时间新的在前：new-b(400) > new-a(300) > lib(200)
	if want := "lite-old,lite-back,lite-excl,paid-model,lite-new-b,lite-new-a,lib-model"; strings.Join(order, ",") != want {
		t.Fatalf("路由候选不对:\n got %s\nwant %s", strings.Join(order, ","), want)
	}
	for id, want := range map[string]bool{"lite-old": false, "lite-back": true, "lib-model": true, "lite-new-a": true, "lite-new-b": true, "lite-excl": false, "paid-model": true} {
		if m, ok := c.model(id); !ok || m.Enabled != want {
			t.Errorf("%s 启用状态应为 %v: %+v", id, want, m)
		}
	}
	if m, _ := c.model("lite-new-a"); !m.PricingSet || m.InputPrice != 0 || m.DropReasoning || m.Context != 262144 {
		t.Errorf("新模型应确认零价格、默认不丢弃推理、上下文取自上游: %+v", m)
	}
	for _, id := range []string{"lite-small", "lite-notools"} {
		if _, ok := c.model(id); ok {
			t.Errorf("%s 不符合条件，不应加入", id)
		}
	}
	res := str(r, "result")
	for _, want := range []string{"加入并启用：lite-new-b、lite-new-a、lib-model", "重新启用：lite-back", "停用（上游已下架）：lite-old", "推送未发出"} {
		if !strings.Contains(res, want) {
			t.Errorf("结果缺少「%s」: %s", want, res)
		}
	}
	// 没有改动：不产生新配置版本，结果为空
	v := h.s.Config().Version
	if r = runScheduled(t, h, "free"); str(r, "status") != "succeeded" || str(r, "result") != "" || h.s.Config().Version != v {
		t.Fatalf("无改动时不应产生新版本: %v v%d→v%d", r, v, h.s.Config().Version)
	}
	// 上游列表拉取失败：什么都不动
	broken.Store(true)
	if r = runScheduled(t, h, "free"); str(r, "status") != "failed" || h.s.Config().Version != v {
		t.Fatalf("拉取失败应记为失败且不改配置: %v", r)
	}
}

// 「丢弃推理内容」选项后开也生效：已在路由里启用的免费模型在下一次运行时补上
func TestFreeModelsDropReasoningOptIn(t *testing.T) {
	h, _ := freeHarness(t)
	runScheduled(t, h, "free")
	if m, _ := h.s.Config().model("lite-new-a"); m.DropReasoning {
		t.Fatal("默认不应丢弃推理内容")
	}
	h.rpc(t, "schedule.save", Object{"free_drop_reasoning": true}, h.token)
	r := runScheduled(t, h, "free")
	for _, id := range []string{"lite-new-a", "lite-back", "lib-model"} {
		if m, _ := h.s.Config().model(id); !m.DropReasoning {
			t.Errorf("开启选项后 %s 应丢弃推理内容", id)
		}
	}
	if m, _ := h.s.Config().model("paid-model"); m.DropReasoning {
		t.Error("收费模型不应被改动")
	}
	if !strings.Contains(str(r, "result"), "开启丢弃推理内容：") {
		t.Errorf("结果应列出补上选项的模型: %v", r)
	}
}
