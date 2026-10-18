package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// coolAfterFailure 按上游失败的性质给候选冷却。网关不替客户端重放，但要让客户端按自己规则发起的
// 重试落到别的候选上，所以凡是「这个候选此刻不可用」的失败都要冷却一会儿；
// 400 是请求本身被拒，与候选健康无关，不冷却。
func (e *Engine) coolAfterFailure(id string, pr Provider, status int, retryAfter string) {
	switch {
	case retryable(status):
		e.cooldownBecause(id, retryDuration(retryAfter), fmt.Sprintf("上游限流（HTTP %d）", status))
		if status == 429 && e.OnRateLimited != nil {
			go e.OnRateLimited(pr)
		}
	case exhausted(status):
		e.cooldownBecause(id, exhaustedCooldown, fmt.Sprintf("上游额度、权限不足或模型不存在（HTTP %d）", status))
	case status >= 500:
		e.cooldownBecause(id, failureCooldown, fmt.Sprintf("上游故障（HTTP %d）", status))
	}
}

// retryHints 在所有候选都被准入拒绝时，用客户端认识的头说明该不该等、等多久：
// Claude Code 与 OpenAI SDK 都读 Retry-After，并把 x-should-retry: false 当作立刻放弃。
// 冷却剩余超过一分钟（额度用尽、模型下架）时让客户端直接报错，而不是每 30 秒撞一次。
func (e *Engine) retryHints(h http.Header, selections []selection, lastErr error) {
	_, code, _ := errorParts(lastErr)
	switch code {
	case "UPSTREAM_COOLDOWN":
		var remaining int64
		t := now()
		e.mu.Lock()
		for _, s := range selections {
			if left := e.state(s.Model.ID).Cooldown - t; left > 0 && (remaining == 0 || left < remaining) {
				remaining = left
			}
		}
		e.mu.Unlock()
		if remaining <= 0 {
			return
		}
		h.Set("Retry-After", fmt.Sprint(max(1, (remaining+999)/1000)))
		if remaining > 60000 {
			h.Set("x-should-retry", "false")
		}
	case "RPM_LIMIT":
		h.Set("Retry-After", "10")
	case "LOCAL_QUOTA_LIMIT":
		// 本地滚动预算几分钟内不会变，重试没有意义
		h.Set("Retry-After", "60")
		h.Set("x-should-retry", "false")
	}
}

// peekSSE 读到上游流的第一个有效事件再返回，这样上游「200 之后立刻报错」还能当成普通失败处理：
// 这时还没向客户端写过一个字节，换候选或原样回错都安全。注释行（如 OpenRouter 的 PROCESSING）跳过。
// 返回的 reader 把已读内容接回去，后续按原流处理。
func peekSSE(r io.Reader, protocol string) (io.Reader, error) {
	br := bufio.NewReader(r)
	var buf bytes.Buffer
	var event string
	var data []string
	for buf.Len() < 1<<20 {
		line, err := br.ReadString('\n')
		buf.WriteString(line)
		if err != nil && line == "" {
			if buf.Len() == 0 {
				return nil, errors.New("upstream closed the stream before the first event")
			}
			return nil, errors.New("upstream stream ended inside the first event")
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			if event == "" && len(data) == 0 {
				continue
			}
			if ue := streamErrorFrame(event, strings.Join(data, "\n"), protocol); ue != nil {
				return nil, ue
			}
			return io.MultiReader(&buf, br), nil
		case strings.HasPrefix(trimmed, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
		}
	}
	return nil, errors.New("upstream first SSE event exceeds 1 MiB")
}

// streamErrorFrame 判断首事件是不是错误：Messages / Responses 用 event: error 或 type=error，
// Chat（含 OpenRouter）把 {"error":{...}} 放在 data 里。状态码取上游给的数字 code，没有就按错误类型推断。
func streamErrorFrame(event, data, protocol string) *upstreamStatusError {
	var o Object
	if data != "" && strings.TrimSpace(data) != "[DONE]" {
		json.Unmarshal([]byte(data), &o)
	}
	isErr := event == "error" || str(o, "type") == "error" || (protocol == "chat" && o != nil && o["error"] != nil)
	if !isErr {
		return nil
	}
	status := 502
	if c, ok := obj(o["error"])["code"].(float64); ok && c >= 400 && c < 600 {
		status = int(c)
	} else if typ := str(obj(o["error"]), "type"); strings.Contains(typ, "overloaded") {
		status = 529
	} else if strings.Contains(typ, "rate_limit") {
		status = 429
	}
	ue := newUpstreamStatusError(status, []byte(data))
	if data == "" {
		ue.Message = "上游在流的第一个事件就报错，没有错误正文"
		ue.Detail = ue.Message
	}
	return ue
}

func retryDuration(retryAfter string) time.Duration {
	d := 30 * time.Second
	if n, er := time.ParseDuration(retryAfter + "s"); er == nil && n > 0 {
		d = n
	} else if t, er := http.ParseTime(retryAfter); er == nil && t.After(time.Now()) {
		d = time.Until(t)
	}
	return d
}
