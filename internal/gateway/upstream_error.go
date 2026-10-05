package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// upstreamStatusError 是上游返回非 2xx 的错误。Detail 只进请求记录供管理员排查，
// 上游错误正文不转发给客户端。
type upstreamStatusError struct {
	Status int
	Detail string
}

func (e *upstreamStatusError) Error() string { return fmt.Sprintf("upstream status %d", e.Status) }

func upstreamDetail(err error) string {
	var ue *upstreamStatusError
	if errors.As(err, &ue) {
		return ue.Detail
	}
	return ""
}

// 密钥形态：常见前缀的令牌和 Bearer 凭证。上游错误偶尔会回显请求里的片段，落库前统一抹掉。
var secretLike = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+|\b(?:sk|pk|prism|key|token)[-_][A-Za-z0-9_-]{8,}`)

const upstreamDetailRunes = 300

// upstreamErrorSummary 取出上游错误里人能读的那句话：优先 error.message，
// 其次 message / detail / error 字符串，都没有就用正文本身。折叠空白、脱敏、限长。
func upstreamErrorSummary(body []byte) string {
	text := strings.TrimSpace(string(body))
	var o Object
	if json.Unmarshal(body, &o) == nil {
		for _, s := range []string{str(obj(o["error"]), "message"), str(o, "message"), str(o, "detail"), str(o, "error")} {
			if s != "" {
				text = s
				break
			}
		}
	}
	text = secretLike.ReplaceAllString(strings.Join(strings.Fields(text), " "), "***")
	if r := []rune(text); len(r) > upstreamDetailRunes {
		text = string(r[:upstreamDetailRunes]) + "…"
	}
	return text
}
