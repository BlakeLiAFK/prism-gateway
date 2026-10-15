package gateway

import (
	"fmt"
	"strconv"
	"strings"
)

// Console 的套餐额度以微美分计价，不是账户现金余额。
func parseOpenCodeUsage(o Object) (string, []any) {
	meters := obj(obj(o["access"])["meters"])
	headline, fields := "", []any{}
	for _, spec := range []struct{ key, label string }{{"fiveHour", "5 小时"}, {"week", "每周"}, {"month", "每月"}} {
		meter := obj(meters[spec.key])
		used, usedOK := openCodeAmount(meter["usedMicroCents"])
		limit, limitOK := openCodeAmount(meter["limitMicroCents"])
		known := usedOK && limitOK && limit > 0
		value := "待确认"
		if known {
			value = "已用 " + openCodePercent(used, limit)
			if headline == "" {
				headline = spec.label + value
			}
			value += " · " + openCodeDollars(used) + " / " + openCodeDollars(limit)
			if used >= limit {
				value += " · 已达限额"
			}
		}
		field := Object{"label": spec.label}
		reset, start := ccReset(meter["resetsAt"]), ccReset(meter["startsAt"])
		switch {
		case !known:
			value += " · 重置时间待确认"
		case spec.key == "fiveHour" && used == 0 && (start <= 0 || start > now() || reset <= start):
			// 空窗口的 now+5h 参考值没有固定起点，不能当作已启动的倒计时。
			value += " · 窗口尚未开始或起点待确认"
		case reset > now():
			field["reset_at"] = reset
		case reset > 0:
			value += " · 等待上游刷新"
		default:
			value += " · 重置时间待确认"
		}
		field["value"] = value
		fields = append(fields, field)
	}
	fields = append(fields, Object{"label": "现金余额", "value": "接口未提供"})
	return headline, fields
}

func openCodeAmount(v any) (int64, bool) {
	s, ok := v.(string)
	if !ok || s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func openCodeDollars(n int64) string {
	fraction := strings.TrimRight(fmt.Sprintf("%08d", n%100000000), "0")
	for len(fraction) < 2 {
		fraction += "0"
	}
	return fmt.Sprintf("$%d.%s", n/100000000, fraction)
}

func openCodePercent(used, limit int64) string {
	percent := float64(used) / float64(limit) * 100
	switch {
	case used >= limit:
		return "100.00%"
	case used > 0 && percent < 0.01:
		return "<0.01%"
	case percent >= 99.995:
		return ">99.99%"
	default:
		return fmt.Sprintf("%.2f%%", percent)
	}
}
