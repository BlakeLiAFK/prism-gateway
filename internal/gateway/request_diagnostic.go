package gateway

import (
	"log/slog"
	"reflect"
	"slices"

	"prism-gateway/internal/sqlite"
)

type messageShape struct {
	Role         string   `json:"role"`
	ContentTypes []string `json:"content_types"`
	ContentCount int      `json:"content_count"`
	Truncated    bool     `json:"truncated"`
}

type requestShape struct {
	Messages     []messageShape `json:"messages"`
	MessageCount int            `json:"message_count"`
	Truncated    bool           `json:"truncated"`
}

// 只保留协议枚举，未知字符串可能包含正文或密钥，不能直接写入诊断。
func diagnosticEnum(v any, allowed ...string) string {
	if v == nil {
		return "missing_or_null"
	}
	if s, ok := v.(string); ok {
		if slices.Contains(allowed, s) {
			return s
		}
		return "unknown_string"
	}
	return "invalid_type"
}

func diagnosticShape(o Object) requestShape {
	ms := arr(o["messages"])
	r := requestShape{Messages: []messageShape{}, MessageCount: len(ms), Truncated: len(ms) > 128}
	for _, v := range ms[:min(len(ms), 128)] {
		m := obj(v)
		s := messageShape{Role: diagnosticEnum(m["role"], "user", "assistant", "system", "developer", "tool", "function"), ContentTypes: []string{}}
		switch c := m["content"].(type) {
		case string:
			s.ContentTypes, s.ContentCount = []string{"text"}, 1
		case []any:
			s.ContentCount, s.Truncated = len(c), len(c) > 16
			for _, b := range c[:min(len(c), 16)] {
				s.ContentTypes = append(s.ContentTypes, diagnosticEnum(obj(b)["type"], "text", "image", "document", "thinking", "redacted_thinking", "tool_use", "tool_result", "server_tool_use", "web_search_tool_result", "web_fetch_tool_result"))
			}
		default:
			s.ContentTypes = []string{"missing_or_invalid"}
		}
		r.Messages = append(r.Messages, s)
	}
	return r
}

func (e *Engine) recordRequestDiagnostic(id string, started int64, before, after Object) {
	until := e.store.Config().Settings.RequestDiagnosticsUntil
	if until <= now()/1000 || started < (until-900)*1000 || started >= until*1000 {
		return
	}
	b, a := diagnosticShape(before), diagnosticShape(after)
	detail := raw(Object{"before": b, "after": a, "captured_structure_changed": !reflect.DeepEqual(b, a)})
	// 限额与写入在同一事务内，避免并发超额；重启后仍按数据库记录计数。
	err := e.store.DB.Transaction(func(tx *sqlite.Tx) error {
		rows, err := tx.Query("SELECT COUNT(*) n FROM requests WHERE started_at>=? AND started_at<? AND diagnostic<>''", (until-900)*1000, until*1000)
		if err != nil {
			return err
		}
		if rows[0].Int("n") >= 20 {
			return nil
		}
		return tx.Exec("UPDATE requests SET diagnostic=? WHERE id=?", detail, id)
	})
	if err != nil {
		slog.Error("request diagnostic write failed", "request_id", id, "err", err)
	}
}
