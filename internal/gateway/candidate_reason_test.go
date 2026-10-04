package gateway

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// reasonRequest 描述一次请求带了哪些会触发候选淘汰的特征
type reasonRequest struct {
	tools, image, zdr bool
}

func (r reasonRequest) build(model string) Object {
	o := requestFixture("messages", model)
	if r.tools {
		o["tools"] = []any{Object{"name": "t", "description": "d", "input_schema": Object{"type": "object", "properties": Object{}}}}
	}
	if r.image {
		o["messages"] = []any{Object{"role": "user", "content": []any{
			Object{"type": "image", "source": Object{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}},
			Object{"type": "text", "text": "what is this"}}}}
	}
	if r.zdr {
		o["provider"] = Object{"zdr": true}
	}
	return o
}

// singleCandidateRoute 配置只含一个候选的路由 auto，让该候选的淘汰原因不被其他候选干扰
func singleCandidateRoute(t *testing.T, m Model, paused, providerOff bool) *harness {
	t.Helper()
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", m)
	h.change(t, func(c *Config) {
		c.Providers[0].Enabled = !providerOff
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: m.ID, Weight: 10, Disabled: paused}}}}
	})
	return h
}

// 管理员自己的操作（暂停、停用）必须压过所有按请求内容产生的原因，
// 否则 recordIncompatible 会把管理员暂停的路由误判成需要告警
func TestCandidateReasonAdminActionsTakePrecedence(t *testing.T) {
	noTools := func(m *Model) { m.Tools = false }
	noVision := func(m *Model) { m.Vision = false }
	noBoth := func(m *Model) { m.Tools, m.Vision = false, false }
	off := func(m *Model) { m.Enabled = false }
	offNoTools := func(m *Model) { m.Enabled, m.Tools = false, false }
	offNoVision := func(m *Model) { m.Enabled, m.Vision = false, false }
	unconfirmed := func(m *Model) { m.Protocols = []string{} }
	none := func(m *Model) {}
	cases := []struct {
		name        string
		model       func(*Model)
		paused      bool
		providerOff bool
		req         reasonRequest
		want        string
	}{
		// 暂停优先
		{"暂停且不支持工具", noTools, true, false, reasonRequest{tools: true}, "candidate_paused"},
		{"暂停且不支持图像", noVision, true, false, reasonRequest{image: true}, "candidate_paused"},
		{"暂停且工具与图像都不支持", noBoth, true, false, reasonRequest{tools: true, image: true}, "candidate_paused"},
		{"暂停且 zdr 无法满足", none, true, false, reasonRequest{zdr: true}, "candidate_paused"},
		{"暂停且原生协议未确认", unconfirmed, true, false, reasonRequest{}, "candidate_paused"},
		// 暂停与停用同时存在时报暂停
		{"暂停且模型停用", off, true, false, reasonRequest{}, "candidate_paused"},
		{"暂停且模型停用且不支持工具", offNoTools, true, false, reasonRequest{tools: true}, "candidate_paused"},
		{"暂停且供应商停用", none, true, true, reasonRequest{}, "candidate_paused"},
		// 停用压过按请求内容产生的原因
		{"模型停用且不支持工具", offNoTools, false, false, reasonRequest{tools: true}, "disabled"},
		{"模型停用且不支持图像", offNoVision, false, false, reasonRequest{image: true}, "disabled"},
		{"模型停用且 zdr 无法满足", off, false, false, reasonRequest{zdr: true}, "disabled"},
		{"模型停用且原生协议未确认", func(m *Model) { m.Enabled, m.Protocols = false, []string{} }, false, false, reasonRequest{}, "disabled"},
		{"供应商停用且不支持工具", noTools, false, true, reasonRequest{tools: true}, "disabled"},
		// 其余原因之间的先后保持不变：后写的覆盖先写的，即 vision > tools > zdr > 协议未确认
		{"不支持工具且 zdr 无法满足", noTools, false, false, reasonRequest{tools: true, zdr: true}, "tools_unsupported"},
		{"工具与图像都不支持", noBoth, false, false, reasonRequest{tools: true, image: true}, "vision_unsupported"},
		{"仅不支持工具", noTools, false, false, reasonRequest{tools: true}, "tools_unsupported"},
		{"仅不支持图像", noVision, false, false, reasonRequest{image: true}, "vision_unsupported"},
		{"协议未确认且不支持工具", func(m *Model) { m.Tools, m.Protocols = false, []string{} }, false, false, reasonRequest{tools: true}, "tools_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := modelFixture("b", "messages")
			tc.model(&m)
			h := singleCandidateRoute(t, m, tc.paused, tc.providerOff)
			selections, reasons, err := h.a.Engine.selections(h.s.Config(), tc.req.build("auto"), "messages", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(selections) != 0 || len(reasons) != 1 {
				t.Fatalf("唯一的候选应被淘汰: selections=%d reasons=%v", len(selections), reasons)
			}
			r := obj(reasons[0])
			if r["eligible"] != false || r["model_id"] != "b" {
				t.Fatalf("原因条目不对: %v", r)
			}
			if got := str(r, "reason"); got != tc.want {
				t.Fatalf("淘汰原因 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// zdr 的原因是一句说明而不是固定代号，单独断言它在没有管理员操作时仍然会出现
func TestCandidateReasonZDRUnchanged(t *testing.T) {
	h := singleCandidateRoute(t, modelFixture("b", "messages"), false, false)
	_, reasons, err := h.a.Engine.selections(h.s.Config(), reasonRequest{zdr: true}.build("auto"), "messages", "")
	if err != nil || len(reasons) != 1 {
		t.Fatalf("%v %v", reasons, err)
	}
	if got := str(obj(reasons[0]), "reason"); !strings.HasPrefix(got, "provider.zdr") {
		t.Fatalf("zdr 原因不对: %q", got)
	}
}

// 暂停一个不兼容的候选不能影响其他候选的筛选，reasons 仍按候选顺序逐个给出
func TestCandidateReasonKeepsOtherCandidatesAndOrder(t *testing.T) {
	h := newHarness(t)
	bare := modelFixture("bare", "messages")
	bare.Tools = false
	h.configure(t, "http://127.0.0.1:1", bare, modelFixture("good", "messages"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority",
			Candidates: []Candidate{{ModelID: "bare", Weight: 10, Disabled: true}, {ModelID: "good", Weight: 10}}}}
	})
	selections, reasons, err := h.a.Engine.selections(h.s.Config(), reasonRequest{tools: true}.build("auto"), "messages", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(selections) != 1 || selections[0].Model.ID != "good" {
		t.Fatalf("只有 good 应合格: %v", selections)
	}
	var got []string
	for _, v := range reasons {
		got = append(got, str(obj(v), "model_id")+"="+str(obj(v), "reason"))
	}
	if want := []string{"bare=candidate_paused", "good="}; !slices.Equal(got, want) {
		t.Fatalf("reasons = %v，期望 %v", got, want)
	}
}

// 端到端：管理员暂停了路由的全部候选，请求恰好带工具而模型不支持，
// 客户端照常收到 400，但这是管理员自己的操作，不应该向管理员告警
func TestPausedRouteWithToolRequestDoesNotAlert(t *testing.T) {
	bare := modelFixture("b", "messages")
	bare.Tools = false
	t.Run("全部暂停不告警", func(t *testing.T) {
		h := singleCandidateRoute(t, bare, true, false)
		alerts := make(chan string, 8)
		h.a.Engine.OnAlert = func(event, key, text string) { alerts <- text }
		w := h.generate(t, "messages", reasonRequest{tools: true}.build("auto"))
		requireStatus(t, w, 400)
		select {
		case got := <-alerts:
			t.Errorf("管理员自己暂停的路由不应告警: %s", got)
		case <-time.After(300 * time.Millisecond):
		}
		if body := w.Body.String(); !strings.Contains(body, "candidate_paused") || strings.Contains(body, "tools_unsupported") {
			t.Errorf("响应应说明候选已暂停: %s", body)
		}
		rows, err := h.s.DB.Query("SELECT reason FROM requests")
		if err != nil || len(rows) != 1 || !strings.Contains(rows[0].String("reason"), "candidate_paused") {
			t.Errorf("请求记录应留下暂停原因: %v %v", rows, err)
		}
	})
	// 对照：同样的请求落在没有暂停的候选上，是真正的不兼容，仍然要告警
	t.Run("未暂停仍告警", func(t *testing.T) {
		h := singleCandidateRoute(t, bare, false, false)
		alerts := make(chan string, 8)
		h.a.Engine.OnAlert = func(event, key, text string) { alerts <- text }
		requireStatus(t, h.generate(t, "messages", reasonRequest{tools: true}.build("auto")), 400)
		select {
		case got := <-alerts:
			if !strings.Contains(got, "tools_unsupported") {
				t.Fatalf("告警内容不对: %s", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("路由候选因请求内容被淘汰时应告警")
		}
	})
}
