package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func diagnosticHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"Invalid input at messages.1.role"}}`)
	}))
	t.Cleanup(up.Close)
	h.configure(t, up.URL, modelFixture("b", "messages"))
	return h
}

func TestRequestDiagnosticHandler(t *testing.T) {
	h := diagnosticHarness(t)
	h.change(t, func(c *Config) { c.Settings.RequestDiagnosticsUntil = now()/1000 + 900 })
	body := requestFixture("messages", "b")
	body["messages"] = []any{Object{"role": "user", "content": "secret-prompt"}, Object{"role": "assistant", "content": []any{Object{"type": "text", "text": "secret-answer"}}}}
	original := raw(body)
	w := h.generate(t, "messages", body)
	requireStatus(t, w, 400)
	if raw(body) != original || strings.Contains(w.Body.String(), "content_types") {
		t.Fatal("诊断不得修改原请求或泄露到调用方响应")
	}
	rows, err := h.s.DB.Query("SELECT id,diagnostic FROM requests")
	if err != nil || len(rows) != 1 {
		t.Fatalf("records: %v %v", rows, err)
	}
	d := rows[0].String("diagnostic")
	if strings.Contains(d, "secret-") || !strings.Contains(d, `"role":"assistant"`) || !strings.Contains(d, `"captured_structure_changed":false`) {
		t.Fatalf("脱敏结构不正确: %s", d)
	}
	w, res := h.rpc(t, "request.get", Object{"id": rows[0].String("id")}, h.token)
	requireStatus(t, w, 200)
	if str(obj(res["data"]), "diagnostic") != d {
		t.Fatal("管理员详情缺少诊断")
	}
	w, _ = h.rpc(t, "request.get", Object{"id": rows[0].String("id")}, h.createKey(t))
	if w.Code == 200 {
		t.Fatal("调用 Key 不得读取管理诊断")
	}
}

func TestRequestDiagnosticOffAndExpired(t *testing.T) {
	for _, until := range []int64{0, now()/1000 - 1} {
		h := diagnosticHarness(t)
		h.change(t, func(c *Config) { c.Settings.RequestDiagnosticsUntil = until })
		requireStatus(t, h.generate(t, "messages", requestFixture("messages", "b")), 400)
		rows, err := h.s.DB.Query("SELECT diagnostic FROM requests")
		if err != nil || len(rows) != 1 || rows[0].String("diagnostic") != "" {
			t.Fatalf("关闭或过期不应采样: %v %v", rows, err)
		}
	}
}

func TestRequestDiagnosticScope(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		status   int
		disable  bool
		enable   bool
	}{{"messages", 200, false, false}, {"messages", 429, false, false}, {"chat", 400, false, false}, {"messages", 400, true, false}, {"messages", 400, false, true}} {
		h := newHarness(t)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.disable {
				h.change(t, func(c *Config) { c.Settings.RequestDiagnosticsUntil = 0 })
			}
			if tc.enable {
				h.change(t, func(c *Config) { c.Settings.RequestDiagnosticsUntil = now()/1000 + 60 })
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, `{"error":{"message":"rejected"}}`)
		}))
		t.Cleanup(up.Close)
		h.configure(t, up.URL, modelFixture("b", tc.protocol))
		if !tc.enable {
			h.change(t, func(c *Config) { c.Settings.RequestDiagnosticsUntil = now()/1000 + 900 })
		}
		h.generate(t, tc.protocol, requestFixture(tc.protocol, "b"))
		rows, err := h.s.DB.Query("SELECT COUNT(*) n FROM requests WHERE diagnostic<>''")
		if err != nil || rows[0].Int("n") != 0 {
			t.Fatalf("不在范围内也采样了: %+v %v %v", tc, rows, err)
		}
	}
}

func TestRequestDiagnosticPersistentConcurrentLimit(t *testing.T) {
	h := diagnosticHarness(t)
	h.change(t, func(c *Config) { c.Settings.RequestDiagnosticsUntil = now()/1000 + 900 })
	for i := 0; i < 21; i++ {
		requireStatus(t, h.generate(t, "messages", requestFixture("messages", "b")), 400)
	}
	rows, err := h.s.DB.Query("SELECT id FROM requests WHERE diagnostic='' LIMIT 1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("第 21 条必须不采样: %v %v", rows, err)
	}
	id := rows[0].String("id")
	// 重建引擎不应重置持久计数；并发尝试也不得越过上限。
	reopened, err := OpenStore(strings.TrimSuffix(h.s.KeyPath, ".key"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	e := NewEngine(reopened)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.recordRequestDiagnostic(id, now(), Object{}, Object{})
		}()
	}
	wg.Wait()
	rows, err = h.s.DB.Query("SELECT COUNT(*) n FROM requests WHERE diagnostic<>''")
	if err != nil || rows[0].Int("n") != 20 {
		t.Fatalf("限额必须保持 20: %v %v", rows, err)
	}
}

func TestDiagnosticShapeRedactionAndBounds(t *testing.T) {
	messages := []any{Object{"role": "secret-role", "content": []any{Object{"type": "secret-type", "input": "secret-arg", "content": "secret-result"}}}, Object{"role": 12}, Object{"content": "secret-text"}}
	for i := 0; i < 140; i++ {
		blocks := make([]any, 20)
		messages = append(messages, Object{"role": "user", "content": blocks})
	}
	shape := diagnosticShape(Object{"messages": messages})
	data := raw(shape)
	if strings.Contains(data, "secret-") || !shape.Truncated || len(shape.Messages) != 128 || !shape.Messages[3].Truncated || len(shape.Messages[3].ContentTypes) != 16 {
		t.Fatal("脱敏或结构大小限制失效")
	}
	if shape.Messages[0].Role != "unknown_string" || shape.Messages[1].Role != "invalid_type" || shape.Messages[2].Role != "missing_or_null" {
		t.Fatal("角色异常分类错误")
	}
	if diagnosticEnum("tool_use", "tool_use") != "tool_use" {
		t.Fatal("合法枚举被丢弃")
	}
}

func TestRequestDiagnosticSettingsValidation(t *testing.T) {
	h := newHarness(t)
	for _, until := range []int64{-1, now()/1000 + 1800} {
		w, _ := h.rpc(t, "settings.update", Object{"version": h.s.Config().Version, "settings": Object{"request_diagnostics_until": until}}, h.token)
		if w.Code == 200 {
			t.Fatal("无效期限不应保存")
		}
	}
	until := now()/1000 + 900
	w, _ := h.rpc(t, "settings.update", Object{"version": h.s.Config().Version, "settings": Object{"request_diagnostics_until": until}}, h.token)
	requireStatus(t, w, 200)
	if h.s.Config().Settings.RequestDiagnosticsUntil != until {
		t.Fatal("期限没有进入配置快照")
	}
	rows, err := h.s.DB.Query("SELECT key FROM settings WHERE key='request_diagnostics_until'")
	if err != nil || len(rows) != 0 {
		t.Fatalf("临时状态不应阻止旧版本加载 settings: %v %v", rows, err)
	}
	if err := h.s.load(); err != nil || h.s.Config().Settings.RequestDiagnosticsUntil != until {
		t.Fatalf("临时诊断状态没有持久化: %v", err)
	}
}

func TestRequestDiagnosticCapturesChangedStructure(t *testing.T) {
	h := diagnosticHarness(t)
	h.change(t, func(c *Config) { c.Settings.RequestDiagnosticsUntil = now()/1000 + 900 })
	requireStatus(t, h.generate(t, "messages", requestFixture("messages", "b")), 400)
	rows, err := h.s.DB.Query("SELECT id FROM requests LIMIT 1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("record: %v %v", rows, err)
	}
	b := Object{"messages": []any{Object{"role": "user", "content": "private"}}}
	a := Object{"messages": []any{Object{"role": "assistant", "content": "private"}}}
	h.a.Engine.recordRequestDiagnostic(rows[0].String("id"), now(), b, a)
	rows, err = h.s.DB.Query("SELECT diagnostic FROM requests LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(rows[0].String("diagnostic")), &d); err != nil || d["captured_structure_changed"] != true || reflect.DeepEqual(d["before"], d["after"]) {
		t.Fatalf("结构变更未标明: %v %v", d, err)
	}
}
