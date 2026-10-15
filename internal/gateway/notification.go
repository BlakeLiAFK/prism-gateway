package gateway

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// 动态字段保持单行，避免客户端输入伪造通知段落。
func noticeValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "未记录"
	}
	r := []rune(s)
	if len(r) > 240 {
		return string(r[:240]) + "..."
	}
	return s
}
func noticeName(name, id string) string {
	if name == "" || name == id {
		return noticeValue(id)
	}
	if id == "" {
		return noticeValue(name)
	}
	return noticeValue(name) + " (" + noticeValue(id) + ")"
}
func noticeTime(ms int64) string {
	if ms <= 0 {
		return "未提供"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05 UTC")
}
func notice(title string, blocks ...string) string {
	parts := []string{title}
	for _, b := range blocks {
		if strings.TrimSpace(b) != "" {
			parts = append(parts, b)
		}
	}
	return strings.Join(parts, "\n\n")
}
func noticeEnvelope(text string) string {
	return notice("Prism Gateway", text, "发送时间："+noticeTime(now()))
}

func (e *Engine) noticeSource(keyID string, from client) string {
	name := ""
	if rows, err := e.store.DB.Query("SELECT name FROM api_keys WHERE id=?", keyID); err == nil && len(rows) > 0 {
		name = rows[0].String("name")
	}
	s := "调用凭证：" + noticeName(name, keyID) + "\n来源 IP：" + noticeValue(from.IP) + "\n客户端 UA：" + noticeValue(from.Agent)
	if from.Role != "" {
		s += "\n客户端角色：" + noticeValue(from.Role)
	}
	return s
}
func (e *Engine) requestAlert(key, title, details, keyID, requested, reqID string, from client) {
	if e.OnAlert == nil {
		return
	}
	text := notice(title, details, "请求入口："+noticeValue(requested)+"\n"+e.noticeSource(keyID, from)+"\n请求 ID："+noticeValue(reqID), "定位：在请求日志中按请求 ID 查询；调用凭证名称不代表已核实的个人身份。")
	e.OnAlert("route", key, text)
}

// 给非 BMP 字符预留两个单位，分段后仍低于 Telegram 的长度上限。
func noticeChunks(text string) []string {
	const maxUnits = 3500
	text = strings.ToValidUTF8(text, "?")
	parts := []string{}
	for len(text) > 0 {
		units, end, lastLine := 0, 0, 0
		for i, r := range text {
			n := 1
			if r > 0xffff {
				n = 2
			}
			if units+n > maxUnits {
				break
			}
			units += n
			end = i + len(string(r))
			if r == '\n' {
				lastLine = end
			}
		}
		if end < len(text) && lastLine > 0 {
			end = lastLine
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	if len(parts) > 1 {
		for i := range parts {
			parts[i] = fmt.Sprintf("第 %d/%d 段\n\n", i+1, len(parts)) + parts[i]
		}
	}
	return parts
}

// 错误码保留用于检索，说明只来自固定文本，不回显上游错误正文。
func noticeError(code string) string {
	meaning := map[string]string{"UPSTREAM_400": "上游拒绝请求参数", "UPSTREAM_401": "上游认证失败", "UPSTREAM_403": "上游拒绝访问", "UPSTREAM_404": "上游模型或接口不存在", "UPSTREAM_429": "上游限流或额度不足", "UPSTREAM_500": "上游服务内部错误", "UPSTREAM_502": "上游服务异常", "UPSTREAM_503": "上游服务暂不可用", "UPSTREAM_504": "上游响应超时", "NO_CAPACITY": "候选容量不足", "NO_COMPATIBLE_MODEL": "没有兼容候选", "CLIENT_CANCELED": "客户端主动取消"}[code]
	if meaning == "" {
		return noticeValue(code)
	}
	return meaning + " (" + noticeValue(code) + ")"
}

func noticeModels(c Config, ids []string) string {
	seen := map[string]bool{}
	lines := []string{}
	total := 0
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		total++
		if len(lines) >= 5 {
			continue
		}
		label := noticeValue(id)
		if m, ok := c.model(id); ok {
			provider := noticeValue(m.ProviderID)
			if p, ok := c.provider(m.ProviderID); ok {
				provider = noticeName(p.Name, p.ID)
			}
			label = provider + " / " + noticeName(m.Name, m.ID)
		}
		lines = append(lines, label)
	}
	if total == 0 {
		return "候选来源：未记录"
	}
	if total > len(lines) {
		lines = append(lines, fmt.Sprintf("另有 %d 个候选，请查看请求日志", total-len(lines)))
	}
	return "候选供应商 / 模型：\n" + strings.Join(lines, "\n")
}

func noticeTimeIn(ms int64, zone *time.Location) string {
	if ms <= 0 {
		return "未提供"
	}
	return time.UnixMilli(ms).In(zone).Format("2006-01-02 15:04:05 -07:00") + " (" + zone.String() + ")"
}
