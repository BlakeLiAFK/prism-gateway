package gateway

// 上游限额响应头的采集与解析。
// 各家给的头名字、单位、重置时间格式都不一样，这里先原样留存一份用于观察，
// 再尽力解析出结构化的剩余量与重置时刻——解析不出来不影响留存。

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// rateLimit 是从一族响应头里归并出来的一条限额：
// scope 说明限的是什么（请求数 / token / 额度），reset 是绝对毫秒时间戳。
type rateLimit struct {
	Scope     string
	Remaining float64
	HasRemain bool
	Limit     float64
	HasLimit  bool
	Reset     int64
}

// limitHeaders 挑出与限额有关的响应头，原样返回（名字统一小写）。
// 阶段一要的就是这份原始数据：先看清各家到底给什么，再谈怎么用。
func limitHeaders(h http.Header) Object {
	out := Object{}
	for name, vs := range h {
		l := strings.ToLower(name)
		if !strings.Contains(l, "ratelimit") && !strings.Contains(l, "rate-limit") &&
			!strings.Contains(l, "x-quota") && l != "retry-after" {
			continue
		}
		if len(vs) > 0 && len(vs[0]) <= 200 {
			out[l] = vs[0]
		}
	}
	return out
}

// parseLimits 把原始头归并成按 scope 分组的限额。
// 同一族头的名字里同时带着 scope（requests / tokens）和角色（limit / remaining / reset），
// 例如 anthropic-ratelimit-requests-remaining、x-ratelimit-reset-tokens。
func parseLimits(raw Object) map[string]rateLimit {
	out := map[string]rateLimit{}
	for name, v := range raw {
		s, _ := v.(string)
		if s == "" || name == "retry-after" {
			continue
		}
		scope := "unknown"
		switch {
		case strings.Contains(name, "request"):
			scope = "requests"
		case strings.Contains(name, "token"):
			scope = "tokens"
		case strings.Contains(name, "credit"), strings.Contains(name, "cost"):
			scope = "credits"
		}
		l := out[scope]
		l.Scope = scope
		switch {
		case strings.Contains(name, "remaining"):
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				l.Remaining, l.HasRemain = n, true
			}
		case strings.Contains(name, "reset"):
			if t := parseReset(s); t > 0 {
				l.Reset = t
			}
		case strings.Contains(name, "limit"):
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				l.Limit, l.HasLimit = n, true
			}
		}
		out[scope] = l
	}
	return out
}

// parseReset 把五花八门的重置时间统一成绝对毫秒时间戳，解析不了返回 0。
// 见过的写法：毫秒时间戳（OpenRouter）、RFC3339（Anthropic）、
// Go duration（OpenAI 的 "6m0s"）、以及单纯的「还有多少秒」。
func parseReset(s string) int64 {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		switch {
		case n > 1e12:
			return int64(n) // 毫秒时间戳
		case n > 1e9:
			return int64(n) * 1000 // 秒时间戳
		case n >= 0:
			return now() + int64(n*1000) // 还有多少秒
		}
		return 0
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now() + d.Milliseconds()
	}
	for _, layout := range []string{time.RFC3339, time.RFC1123, http.TimeFormat} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// exhaustedUntil 找出「上游明说额度已经用完」的重置时刻。
// 只认 remaining == 0 这种事实陈述：剩余比例低于某个阈值是需要上下文才能解读的
// 推测，各家单位又不一致，据此拉黑候选很容易误伤。
func exhaustedUntil(raw Object) int64 {
	var until int64
	for _, l := range parseLimits(raw) {
		if l.HasRemain && l.Remaining == 0 && l.Reset > now() {
			if l.Reset > until {
				until = l.Reset
			}
		}
	}
	return until
}
