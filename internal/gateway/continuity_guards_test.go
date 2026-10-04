package gateway

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const pdfData = "JVBERi0xLjQKJSVFT0YK"

// documentHistoryRequest 的形态取自真实 Claude Code 会话：Read 读 PDF 之后，
// tool_result 里是一段文本说明加一个 document 块；用户也可能直接贴一个文档。
func documentHistoryRequest(model string) Object {
	pdf := func() Object {
		return Object{"type": "document", "source": Object{"type": "base64", "media_type": "application/pdf", "data": pdfData}}
	}
	return Object{"model": model, "max_tokens": 256,
		"tools": []any{Object{"name": "Read", "description": "read a file", "input_schema": Object{"type": "object", "properties": Object{"path": Object{"type": "string"}}}}},
		"messages": []any{
			Object{"role": "user", "content": "read doc.pdf"},
			Object{"role": "assistant", "content": []any{Object{"type": "tool_use", "id": "call_1", "name": "Read", "input": Object{"path": "doc.pdf"}}}},
			Object{"role": "user", "content": []any{Object{"type": "tool_result", "tool_use_id": "call_1", "content": []any{
				Object{"type": "text", "text": "PDF file read: doc.pdf (601 bytes)"}, pdf()}}}},
			Object{"role": "assistant", "content": "the code is PRISM-7711"},
			Object{"role": "user", "content": []any{pdf(), Object{"type": "text", "text": "compare with this one"}}},
		}}
}

func documentModel(id, protocol string, documents, reasoning bool) Model {
	m := modelFixture(id, protocol)
	m.DropDocuments, m.DropReasoning = documents, reasoning
	return m
}

func TestDropDocumentsReplacesHistoryDocuments(t *testing.T) {
	h := newHarness(t)
	up, sent := historyUpstream(t)
	h.configure(t, up.URL, documentModel("b", "chat", true, false))
	w := h.generate(t, "messages", documentHistoryRequest("b"))
	requireStatus(t, w, 200)
	if got := w.Header().Get("X-Prism-Dropped"); got != "documents" {
		t.Fatalf("文档未送达必须披露，X-Prism-Dropped=%q", got)
	}
	body := sent("chat")
	if strings.Contains(body, pdfData) || strings.Contains(body, "application/pdf\",\"data") {
		t.Fatalf("文档内容被发给了跨协议上游: %s", body)
	}
	// 占位说明要让模型知道这里原本有文档、且没有送达；其余对话内容不能丢
	for _, kept := range []string{"document omitted", "PDF file read: doc.pdf", "call_1", "PRISM-7711", "compare with this one"} {
		if !strings.Contains(body, kept) {
			t.Fatalf("上游应当收到 %q: %s", kept, body)
		}
	}
	if strings.Count(body, "document omitted") != 2 {
		t.Fatalf("两处文档都应留下占位: %s", body)
	}
	roles := []string{}
	for _, m := range arr(parse(t, body)["messages"]) {
		roles = append(roles, str(obj(m), "role"))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,assistant,user" {
		t.Fatalf("消息序列不对: %v", roles)
	}
}

func TestHistoryDocumentsRejectedWithoutOptIn(t *testing.T) {
	h := newHarness(t)
	up, sent := historyUpstream(t)
	// drop_reasoning 开着也不行：两个开关各管各的，不能互相扩权
	h.configure(t, up.URL, documentModel("b", "chat", false, true))
	w := h.generate(t, "messages", documentHistoryRequest("b"))
	requireStatus(t, w, 400)
	if w.Header().Get("X-Prism-Dropped") != "" || sent("chat") != "" {
		t.Fatal("未开启 drop_documents 时不得替换文档，也不得把请求发给上游")
	}
}

// 两个开关同时生效时，后写的披露不能盖掉先写的；流式路径会在流开始前再声明一次 reasoning
func TestDroppedHeaderListsEveryKind(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := newHarness(t)
		up, _ := historyUpstream(t)
		h.configure(t, up.URL, documentModel("b", "chat", true, true))
		body := documentHistoryRequest("b")
		msgs := arr(body["messages"])
		msgs[3] = Object{"role": "assistant", "content": []any{
			Object{"type": "thinking", "thinking": "private plan", "signature": "sig-a"}, Object{"type": "text", "text": "the code is PRISM-7711"}}}
		body["stream"] = stream
		w := h.generate(t, "messages", body)
		requireStatus(t, w, 200)
		got := strings.Split(w.Header().Get("X-Prism-Dropped"), ",")
		slices.Sort(got)
		if strings.Join(got, ",") != "documents,reasoning" {
			t.Fatalf("stream=%t 披露不完整: %q", stream, w.Header().Get("X-Prism-Dropped"))
		}
	}
}

func TestDropDocumentsDoesNotMutateRequest(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", documentModel("b", "chat", true, false), modelFixture("a", "messages"))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority",
			Candidates: []Candidate{{ModelID: "b", Weight: 10}, {ModelID: "a", Weight: 10}}}}
	})
	o := documentHistoryRequest("auto")
	want := raw(o["messages"])
	selections, _, err := h.a.Engine.selections(h.s.Config(), o, "messages", "")
	if err != nil || len(selections) != 2 {
		t.Fatalf("两个候选都应合格: %d %v", len(selections), err)
	}
	if got := raw(o["messages"]); got != want {
		t.Fatalf("原请求被改动: %s", got)
	}
	for _, s := range selections {
		switch s.Model.ID {
		case "a":
			if got := raw(s.Body["messages"]); got != want || len(s.Dropped) != 0 {
				t.Fatalf("原生候选的请求体被改动: dropped=%v %s", s.Dropped, got)
			}
		case "b":
			if strings.Contains(raw(s.Body), pdfData) || !slices.Equal(s.Dropped, []string{"documents"}) {
				t.Fatalf("跨协议候选应替换文档并记录: dropped=%v", s.Dropped)
			}
		}
	}
}

// 候选全部被淘汰的请求以前不留任何痕迹，管理员只能等自己撞上
func TestIncompatibleRequestIsRecordedAndAlerted(t *testing.T) {
	h := newHarness(t)
	alerts := make(chan string, 8)
	h.a.Engine.OnAlert = func(event, key, text string) { alerts <- event + "|" + key + "|" + text }
	h.configure(t, "http://127.0.0.1:1", dropModel("b", "chat", false))
	h.change(t, func(c *Config) {
		c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "b", Weight: 10}}}}
	})
	w := h.generate(t, "messages", reasoningHistoryRequest("auto"))
	requireStatus(t, w, 400)

	rows, err := h.s.DB.Query("SELECT * FROM requests")
	if err != nil || len(rows) != 1 {
		t.Fatalf("应当留下一条请求记录: %d %v", len(rows), err)
	}
	r := rows[0]
	if r.String("status") != "error" || r.Int("http_status") != 400 || r.String("error_code") != "NO_COMPATIBLE_MODEL" ||
		r.String("requested_model") != "auto" || r.String("model_id") != "" || !strings.Contains(r.String("reason"), "内容类型 thinking") {
		t.Fatalf("请求记录内容不对: %v", r)
	}
	if r.String("parent_id") != w.Header().Get("X-Request-ID") {
		t.Fatal("记录应能按响应里的请求 ID 查到")
	}
	// 没有实际模型的记录只能按客户端请求名搜到
	_, list := h.rpc(t, "request.list", Object{"q": "auto"}, h.token)
	if items := arr(obj(list["data"])["items"]); len(items) != 1 {
		t.Fatalf("请求记录列表应能看到它: %v", list)
	}
	// 没有到达任何模型，不计入用量汇总，否则统计里会多出一个空白模型
	if usage, err := h.s.DB.Query("SELECT * FROM usage_hourly"); err != nil || len(usage) != 0 {
		t.Fatalf("不应计入用量汇总: %d %v", len(usage), err)
	}
	// 总览与路由占比按模型分组，空模型不能变成一行空白，也不能稀释各候选的流量占比
	_, dash := h.rpc(t, "dashboard.get", Object{}, h.token)
	d := obj(dash["data"])
	if recent := arr(d["recent"]); len(recent) != 1 || str(obj(recent[0]), "requested_model") != "auto" {
		t.Fatalf("最近请求应带客户端请求名，界面才有内容可显示: %v", d["recent"])
	}
	for _, v := range arr(d["top"]) {
		if str(obj(v), "model_id") == "" {
			t.Fatalf("模型排行不应出现空白模型: %v", d["top"])
		}
	}
	_, stats := h.rpc(t, "route.stats", Object{}, h.token)
	if share := obj(obj(obj(stats["data"])["share"])["auto"]); len(share) != 0 {
		t.Fatalf("路由流量占比不应计入没有到达任何模型的请求: %v", share)
	}
	select {
	case got := <-alerts:
		if !strings.HasPrefix(got, "route|") || !strings.Contains(got, "auto") || !strings.Contains(got, "内容类型 thinking") {
			t.Fatalf("告警内容不对: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("路由候选全部被淘汰时应告警")
	}
}

func TestIncompatibleRequestAlertOnlyWhenUseful(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		paused bool
	}{
		{"直接按模型调用是请求自己的问题", "b", false},
		{"全部暂停是管理员自己的操作", "auto", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			alerts := make(chan string, 8)
			h.a.Engine.OnAlert = func(event, key, text string) { alerts <- text }
			h.configure(t, "http://127.0.0.1:1", dropModel("b", "chat", false))
			h.change(t, func(c *Config) {
				c.Routes = []Route{{ID: "auto", Name: "auto", Enabled: true, Strategy: "priority", Candidates: []Candidate{{ModelID: "b", Weight: 10, Disabled: tc.paused}}}}
			})
			requireStatus(t, h.generate(t, "messages", reasoningHistoryRequest(tc.model)), 400)
			if rows, err := h.s.DB.Query("SELECT * FROM requests"); err != nil || len(rows) != 1 {
				t.Fatalf("仍应留下请求记录: %d %v", len(rows), err)
			}
			select {
			case got := <-alerts:
				t.Fatalf("不应告警: %s", got)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

func TestIncompatibleReasonIsBounded(t *testing.T) {
	h := newHarness(t)
	h.configure(t, "http://127.0.0.1:1", dropModel("b", "chat", false))
	body := requestFixture("messages", "b")
	body["messages"] = []any{Object{"role": "user", "content": []any{Object{"type": strings.Repeat("x", 5000)}}}}
	requireStatus(t, h.generate(t, "messages", body), 400)
	rows, err := h.s.DB.Query("SELECT reason FROM requests")
	if err != nil || len(rows) != 1 {
		t.Fatalf("应当留下一条请求记录: %d %v", len(rows), err)
	}
	if n := len([]rune(rows[0].String("reason"))); n > 600 {
		t.Fatalf("客户端可控的字符串不能原样长篇落库: %d", n)
	}
}
