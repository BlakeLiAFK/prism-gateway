package gateway

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

func stripANSI(s string) string { return ansiPattern.ReplaceAllString(s, "") }

func TestWatchTextWidthCountsWideCharsAsTwo(t *testing.T) {
	cases := map[string]int{"abc": 3, "": 0, "中文": 4, "a中b": 4, "ｆｕｌｌ": 8}
	for s, want := range cases {
		if got := textWidth(s); got != want {
			t.Errorf("textWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestWatchFitTextNeverExceedsWidth(t *testing.T) {
	for _, s := range []string{"short", strings.Repeat("a", 200), strings.Repeat("中", 80), "ab中cd中ef"} {
		for _, w := range []int{1, 2, 3, 7, 10, 40} {
			if got := fitText(s, w); textWidth(got) > w {
				t.Errorf("fitText(%q, %d) = %q，宽度 %d 超出", s, w, got, textWidth(got))
			}
		}
	}
	if got := fitText("hello world", 8); got != "hello w~" {
		t.Errorf("截断应以 ~ 结尾并刚好占满: %q", got)
	}
	if got := fitText("hi", 8); got != "hi" {
		t.Errorf("不超宽不应改动: %q", got)
	}
}

func TestWatchBarAndSpark(t *testing.T) {
	if got := bar(0.5, 10); got != "[#####.....]" {
		t.Errorf("bar = %q", got)
	}
	if got := bar(0, 10); got != "[..........]" {
		t.Errorf("bar(0) = %q", got)
	}
	if got := bar(1.7, 4); got != "[####]" || bar(-1, 4) != "[....]" {
		t.Errorf("越界应被钳制: %q %q", bar(1.7, 4), bar(-1, 4))
	}
	s, peak := spark([]int{0, 1, 5, 10})
	if peak != 10 || len([]rune(s)) != 4 || s[0] != ' ' || s[3] != '@' {
		t.Errorf("spark = %q peak=%d", s, peak)
	}
	if s, peak := spark([]int{0, 0, 0}); s != "   " || peak != 0 {
		t.Errorf("全零应全空: %q %d", s, peak)
	}
}

func TestWatchDurationFormats(t *testing.T) {
	cases := map[int64]string{0: "0ms", 450: "450ms", 6200: "6.2s", 59900: "59.9s", 123000: "2m03s", 3600000: "60m00s"}
	for ms, want := range cases {
		if got := fmtDur(ms); got != want {
			t.Errorf("fmtDur(%d) = %q, want %q", ms, got, want)
		}
	}
	up := map[time.Duration]string{45 * time.Second: "45s", 13 * time.Minute: "13m", 2*time.Hour + 13*time.Minute: "2h13m", 52 * time.Hour: "2d4h"}
	for d, want := range up {
		if got := fmtUptime(d); got != want {
			t.Errorf("fmtUptime(%v) = %q, want %q", d, got, want)
		}
	}
}

func sampleView(w, h int, color bool) dashView {
	zone := time.FixedZone("UTC+8", 8*3600)
	return dashView{
		Version: "1.34.6", Zone: zone, Now: time.Date(2026, 10, 4, 12, 40, 7, 0, zone), Uptime: 2*time.Hour + 13*time.Minute,
		ConfigVer: 1472, Active: 3, Limit: 64, RPM: 42, OKPct: 98.2, TokS: 312.4, Watchers: 1,
		Routes: []dashRoute{{ID: "auto", Strategy: "weighted", RPM: 12, Active: 2, Capacity: 30, Cands: []dashCand{
			{Name: "Space Bunny Alpha", Share: 0.82, Active: 2, Conc: 10, RPM: 9, TokS: 38, TTFB: 7300 * time.Millisecond, Requests5m: 45},
			{Name: "DeepSeek V4.1 Flash", Share: 0.18, Active: 1, Conc: 20, RPM: 3, TokS: 51, TTFB: 4100 * time.Millisecond, Requests5m: 10},
			{Name: "GLM 5.3 Flash", Paused: true},
		}}},
		Req: []int{0, 1, 3, 8, 2, 0, 5, 9, 1, 0}, Err: []int{0, 0, 1, 0, 0, 0, 0, 2, 0, 0},
		Events: []watchEvent{
			{At: time.Date(2026, 10, 4, 12, 40, 5, 0, zone).UnixMilli(), Kind: "OK", Text: "auto -> Space Bunny Alpha  6.2s  812 tok  native"},
			{At: time.Date(2026, 10, 4, 12, 40, 1, 0, zone).UnixMilli(), Kind: "REJ", Text: "auto  NO_COMPATIBLE_MODEL  跨协议不支持内容类型 document"},
		},
		W: w, H: h, Color: color,
	}
}

// 每一行都不能超出终端宽度，否则折行会把整幅画面撑乱
func TestWatchDashboardFitsTheTerminal(t *testing.T) {
	for _, w := range []int{60, 80, 100, 160} {
		for _, color := range []bool{false, true} {
			lines := renderDashboard(sampleView(w, 40, color))
			for i, l := range lines {
				if tw := textWidth(stripANSI(l)); tw > w {
					t.Fatalf("w=%d color=%t 第 %d 行宽度 %d: %q", w, color, i+1, tw, stripANSI(l))
				}
			}
		}
	}
}

func TestWatchDashboardContent(t *testing.T) {
	text := stripANSI(strings.Join(renderDashboard(sampleView(100, 40, false)), "\n"))
	for _, want := range []string{
		"PRISM GATEWAY 1.34.6", "up 2h13m", "12:40:07", "UTC+8", "cfg v1472",
		"active 3/64", "rpm 42", "98.2%", "312.4 tok/s",
		"ROUTE auto", "weighted", "Space Bunny Alpha", "82%", "2/10", "ttfb 7.3s", "paused",
		"[########..]", "EVENTS", "12:40:05", "OK", "REJ", "NO_COMPATIBLE_MODEL",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("画面缺少 %q\n%s", want, text)
		}
	}
	if strings.Contains(stripANSI(strings.Join(renderDashboard(sampleView(100, 40, false)), "")), "\x1b") {
		t.Error("去掉转义序列之后不应残留 ESC")
	}
}

// 颜色只是点缀：关闭时一个转义序列都不能有，管道、重定向、纯文本终端才不会出现乱码
func TestWatchDashboardColorOff(t *testing.T) {
	for _, l := range renderDashboard(sampleView(100, 40, false)) {
		if strings.Contains(l, "\x1b") {
			t.Fatalf("关闭彩色时不应有转义序列: %q", l)
		}
	}
	colored := strings.Join(renderDashboard(sampleView(100, 40, true)), "\n")
	if !strings.Contains(colored, "\x1b[") {
		t.Error("开启彩色应当有转义序列")
	}
}

func TestWatchDashboardRespectsHeight(t *testing.T) {
	v := sampleView(100, 16, false)
	for i := 0; i < 30; i++ {
		v.Events = append(v.Events, watchEvent{At: v.Events[0].At + int64(i), Kind: "OK", Text: "e"})
	}
	if n := len(renderDashboard(v)); n > 16 {
		t.Fatalf("画面 %d 行超出终端高度 16", n)
	}
	// 路由很多时折叠空闲候选，仍不超高
	v = sampleView(100, 24, false)
	for i := 0; i < 18; i++ {
		v.Routes[0].Cands = append(v.Routes[0].Cands, dashCand{Name: "idle model"})
	}
	lines := renderDashboard(v)
	if len(lines) > 24 {
		t.Fatalf("画面 %d 行超出终端高度 24", len(lines))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "idle") {
		t.Error("折叠的空闲候选应有一行提示")
	}
}

func TestWatchEventLineFormats(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	ev := watchEvent{At: time.Date(2026, 10, 4, 12, 40, 5, 0, zone).UnixMilli(), Kind: "FAIL", Text: strings.Repeat("长", 80)}
	l := eventLine(ev, zone, 60, false)
	if !strings.HasPrefix(l, "12:40:05  FAIL ") {
		t.Errorf("行首格式: %q", l)
	}
	if textWidth(l) > 60 {
		t.Errorf("宽度 %d 超出", textWidth(l))
	}
	if c := eventLine(ev, zone, 60, true); !strings.Contains(c, "\x1b[31m") {
		t.Errorf("失败事件应为红色: %q", c)
	}
}

func TestWatchDashboardEmptyState(t *testing.T) {
	lines := renderDashboard(dashView{Version: "1", Zone: time.UTC, Now: time.Now(), W: 80, H: 30})
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "no enabled routes") || !strings.Contains(text, "no events yet") {
		t.Errorf("没有路由和事件时应有明确的空状态提示:\n%s", text)
	}
}
