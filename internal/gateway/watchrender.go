package gateway

// 终端监控的画面渲染：全是纯函数，输入一份快照，输出若干行文本。
// 图表只用 ASCII 字符，栏目名用英文，这样在任何终端、任何字体下都能对齐；
// 只有事件行末尾的原因说明可能含中文，所以宽度一律按显示宽度（全角算两格）计算。

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type dashCand struct {
	Name             string
	Paused, Disabled bool
	Cooling          time.Duration
	Share            float64 // 本路由近 5 分钟请求中落在它身上的比例，0 到 1
	Active           int64
	RPM, Requests5m  int64
	TokS             float64
	TTFB             time.Duration
}

type dashRoute struct {
	ID, Strategy string
	RPM, Active  int64
	Cands        []dashCand
}

type dashView struct {
	Version   string
	Zone      *time.Location
	Now       time.Time
	Uptime    time.Duration
	ConfigVer int64
	Active    int64
	RPM       int64
	OKPct     float64 // 近 5 分钟成功率，没有样本时为负数
	TokS      float64
	Watchers  int
	TokenLeft time.Duration
	Routes    []dashRoute
	Req, Err  []int
	Events    []watchEvent
	W, H      int
	Color     bool
}

// cellWidth 是一个字符在终端里占的格数：东亚全角与宽字符占两格
func cellWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe6f, r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6:
		return 2
	}
	return 1
}

func textWidth(s string) int {
	n := 0
	for _, r := range s {
		n += cellWidth(r)
	}
	return n
}

// fitText 把文本截到不超过 w 格；被截断时以 ~ 结尾并刚好不超宽
func fitText(s string, w int) string {
	if textWidth(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		if used+cellWidth(r) > w-1 {
			break
		}
		b.WriteRune(r)
		used += cellWidth(r)
	}
	return b.String() + "~"
}

func padRight(s string, w int) string {
	if gap := w - textWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// bar 画一条 n 格的进度条，frac 越界或不是数时钳制到 0 到 1
func bar(frac float64, n int) string {
	if math.IsNaN(frac) {
		frac = 0
	}
	filled := int(math.Round(math.Max(0, math.Min(1, frac)) * float64(n)))
	return "[" + strings.Repeat("#", filled) + strings.Repeat(".", n-filled) + "]"
}

// spark 把一组计数画成一行字符，返回字符与峰值。有值的格子至少画一个点，不会被误认为 0
func spark(vals []int) (string, int) {
	const levels = " .:-=+*#%@"
	peak := 0
	for _, v := range vals {
		peak = max(peak, v)
	}
	out := make([]byte, len(vals))
	for i, v := range vals {
		switch {
		case v <= 0 || peak == 0:
			out[i] = ' '
		default:
			out[i] = levels[max(1, v*(len(levels)-1)/peak)]
		}
	}
	return string(out), peak
}

func fmtDur(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dm%02ds", ms/60000, ms/1000%60)
}

func fmtUptime(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

// paint 给文本加 ANSI 颜色；关闭彩色时原样返回，管道和纯文本终端不会出现乱码
func paint(color bool, code, s string) string {
	if !color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

var kindColor = map[string]string{
	"OK": "32", "FAIL": "31", "REJ": "31", "CXL": "2", "COOL": "33", "ALERT": "1;33",
	"ADMIN": "36", "JOB": "36", "SYS": "2",
}

// eventLine 是一条事件的一行文本：时间、类型、说明。说明按剩余宽度截断
func eventLine(ev watchEvent, zone *time.Location, width int, color bool) string {
	kind := fmt.Sprintf("%-5s", ev.Kind)
	text := fitText(ev.Text, max(width-16, 1))
	return time.UnixMilli(ev.At).In(zone).Format("15:04:05") + "  " + paint(color, kindColor[ev.Kind], kind) + " " + text
}

func candLine(i int, c dashCand, nameW, w int) string {
	head := fmt.Sprintf("  %02d %s ", i+1, padRight(fitText(c.Name, nameW), nameW))
	switch {
	case c.Paused:
		return head + "paused"
	case c.Disabled:
		return head + "disabled"
	case c.Cooling > 0:
		return head + "cooling " + fmtDur(c.Cooling.Milliseconds())
	}
	return fitText(head+fmt.Sprintf("%s %3d%%  active %d  rpm %d  %.1f tok/s  ttfb %s",
		bar(c.Share, 10), int(math.Round(c.Share*100)), c.Active, c.RPM, c.TokS, fmtDur(c.TTFB.Milliseconds())), w)
}

func busy(c dashCand) bool { return c.Active > 0 || c.RPM > 0 || c.Requests5m > 0 || c.Cooling > 0 }

// routeLines 画每条启用路由和它的候选。候选很多时只画有动静的，其余折成一行
func routeLines(routes []dashRoute, w int, color bool) []string {
	if len(routes) == 0 {
		return []string{"  no enabled routes"}
	}
	nameW := clamp(w-64, 10, 30)
	var out []string
	for _, r := range routes {
		out = append(out, paint(color, "1", fitText(fmt.Sprintf("ROUTE %s  %s  rpm %d  active %d", r.ID, r.Strategy, r.RPM, r.Active), w)))
		hidden := 0
		for i, c := range r.Cands {
			if len(r.Cands) > 6 && !busy(c) {
				hidden++
				continue
			}
			line := candLine(i, c, nameW, w)
			if c.Paused || c.Disabled {
				line = paint(color, "2", line)
			} else if c.Cooling > 0 {
				line = paint(color, "33", line)
			}
			out = append(out, line)
		}
		if hidden > 0 {
			out = append(out, fitText(fmt.Sprintf("      .. +%d idle or paused", hidden), w))
		}
	}
	return out
}

// tokenNote 提醒监控令牌还剩多久到期，到期前几天就能看到，不会突然断流
func tokenNote(left time.Duration) string {
	if left <= 0 {
		return ""
	}
	return "   token " + fmtUptime(left)
}

// renderDashboard 画一整幅仪表盘，行数不超过 v.H，每行不超过 v.W 格
func renderDashboard(v dashView) []string {
	w := v.W
	zone := v.Zone
	if zone == nil {
		zone = time.UTC
	}
	ok := "-"
	if v.OKPct >= 0 {
		ok = fmt.Sprintf("%.1f%%", v.OKPct)
	}
	head := []string{
		paint(v.Color, "1;36", fitText(fmt.Sprintf("PRISM GATEWAY %s   up %s   %s %s   cfg v%d   watchers %d",
			v.Version, fmtUptime(v.Uptime), v.Now.In(zone).Format("15:04:05"), zone, v.ConfigVer, v.Watchers), w)),
		strings.Repeat("-", w),
		fitText(fmt.Sprintf("active %d   rpm %d   ok(5m) %s   out %.1f tok/s%s", v.Active, v.RPM, ok, v.TokS, tokenNote(v.TokenLeft)), w),
		"",
	}
	reqs, peak := spark(v.Req)
	errs, _ := spark(v.Err)
	errTotal := 0
	for _, n := range v.Err {
		errTotal += n
	}
	tail := []string{"",
		fitText(fmt.Sprintf("rpm  [%s]  peak %d/min  last %dm", reqs, peak, len(v.Req)), w),
		fitText(fmt.Sprintf("err  [%s]  total %d", errs, errTotal), w)}

	routes := routeLines(v.Routes, w, v.Color)
	if room := max(v.H-len(head)-len(tail)-5, 2); len(routes) > room {
		more := len(routes) - (room - 1)
		routes = append(routes[:room-1], fitText(fmt.Sprintf("  .. %d more lines, enlarge the terminal", more), w))
	}

	k := clamp(v.H-len(head)-len(routes)-len(tail)-2, 3, 40)
	events := v.Events
	if len(events) > k {
		events = events[len(events)-k:]
	}
	lines := append(append(append(head, routes...), tail...), "", paint(v.Color, "1", "EVENTS  (newest last)"))
	if len(events) == 0 {
		lines = append(lines, "  no events yet")
	}
	for _, ev := range events {
		lines = append(lines, eventLine(ev, zone, w, v.Color))
	}
	return lines
}
