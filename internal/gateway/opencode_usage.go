package gateway

import "fmt"

// Go 的兼容接口只提供套餐窗口，不返回现金余额或 token 总数。
func parseOpenCodeUsage(o Object) (string, []any) {
	usage := obj(o["usage"])
	headline, fields := "", []any{}
	for _, spec := range []struct{ key, label string }{
		{"rolling", "5 小时"}, {"weekly", "每周"}, {"monthly", "每月"},
	} {
		window := obj(usage[spec.key])
		percent, numeric := window["percent"].(float64)
		status := str(window, "status")
		known := numeric && percent >= 0 && percent <= 100 && (status == "ok" || status == "rate-limited")
		value := "待确认"
		if known {
			value = fmt.Sprintf("已用 %g%%", percent)
			if headline == "" {
				headline = spec.label + value
			}
			if status == "rate-limited" {
				value += " · 已达限额"
			}
		}
		switch reset := ccReset(window["resetsAt"]); {
		case reset > now():
			value += " · " + durationText(reset-now()) + "后重置"
		case reset > 0:
			value += " · 等待上游刷新"
		default:
			value += " · 重置时间待确认"
		}
		fields = append(fields, Object{"label": spec.label, "value": value})
	}
	fields = append(fields, Object{"label": "现金余额", "value": "接口未提供"})
	return headline, fields
}
