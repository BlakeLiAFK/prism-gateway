package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// upstreamStatusError 是上游返回非 2xx（或流首事件即错误）的失败。
// Detail 是限长摘要，进请求记录；Message 是脱敏后的上游原话，最后一个候选也失败时原样交给客户端：
// Claude Code 会从 400 的原话里识别不支持的字段、上下文超限等并自行调整，网关改写它就等于拿掉了这个能力。
type upstreamStatusError struct {
	Status  int
	Detail  string
	Message string
	Type    string
	Code    string
}

func (e *upstreamStatusError) Error() string { return fmt.Sprintf("upstream status %d", e.Status) }

func upstreamDetail(err error) string {
	var ue *upstreamStatusError
	if errors.As(err, &ue) {
		return ue.Detail
	}
	return ""
}

// 密钥形态：常见前缀的令牌和 Bearer 凭证。上游错误偶尔会回显请求里的片段，落库与透传前统一抹掉。
var secretLike = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+|\b(?:sk|pk|prism|key|token)[-_][A-Za-z0-9_-]{8,}`)

const (
	upstreamDetailRunes  = 300
	upstreamMessageRunes = 4000
)

// newUpstreamStatusError 解析上游错误正文：Anthropic 的 {type:error,error:{type,message}}、
// OpenAI 的 {error:{message,type,code}}、以及只有 message / detail 的变体，都取出人能读的那句话。
func newUpstreamStatusError(status int, body []byte) *upstreamStatusError {
	ue := &upstreamStatusError{Status: status, Detail: upstreamErrorSummary(body)}
	var o Object
	if json.Unmarshal(body, &o) == nil {
		if eo := obj(o["error"]); eo != nil {
			ue.Type = str(eo, "type")
			switch c := eo["code"].(type) {
			case string:
				ue.Code = c
			case float64:
				ue.Code = fmt.Sprint(int(c))
			}
		}
	}
	ue.Message = redact(upstreamErrorText(body), upstreamMessageRunes)
	if ue.Message == "" {
		ue.Message = fmt.Sprintf("上游返回 HTTP %d，没有错误正文", status)
	}
	return ue
}

// upstreamErrorText 取出上游错误里人能读的那句话：优先 error.message，
// 其次 message / detail / error 字符串，都没有就用正文本身。
func upstreamErrorText(body []byte) string {
	text := strings.TrimSpace(string(body))
	var o Object
	if json.Unmarshal(body, &o) == nil {
		for _, s := range []string{str(obj(o["error"]), "message"), str(o, "message"), str(o, "detail"), str(o, "error")} {
			if s != "" {
				return s
			}
		}
	}
	return text
}

// redact 折叠空白、抹掉令牌形态的内容并限长
func redact(text string, limit int) string {
	text = secretLike.ReplaceAllString(strings.Join(strings.Fields(text), " "), "***")
	if r := []rune(text); len(r) > limit {
		text = string(r[:limit]) + "…"
	}
	return text
}

// upstreamErrorSummary 是进请求记录的限长摘要
func upstreamErrorSummary(body []byte) string {
	return redact(upstreamErrorText(body), upstreamDetailRunes)
}

// anthropicErrorTypes 是 Messages 协议定义过的错误类型；上游是 Messages 原生时原样透传，否则按状态码映射
var anthropicErrorTypes = map[string]bool{"invalid_request_error": true, "authentication_error": true, "permission_error": true, "not_found_error": true,
	"request_too_large": true, "rate_limit_error": true, "api_error": true, "overloaded_error": true, "billing_error": true, "timeout_error": true}

func messagesErrorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 413:
		return "request_too_large"
	case status == 429:
		return "rate_limit_error"
	case status == 529:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	}
	return "invalid_request_error"
}

// upstreamErrorResponse 把最后一个候选的上游错误按客户端协议封装后原样交出：状态码与原话都不改写。
// 客户端据此做它自己的决定——重试、调参或放弃；网关只保证里面没有凭证。
func upstreamErrorResponse(w http.ResponseWriter, p string, ue *upstreamStatusError, id string) {
	status := ue.Status
	if status < 400 {
		status = 502
	}
	w.Header().Set("Content-Type", "application/json")
	setRequestID(w.Header(), id)
	w.WriteHeader(status)
	if p == "messages" {
		typ := ue.Type
		if !anthropicErrorTypes[typ] {
			typ = messagesErrorType(status)
		}
		writeJSON(w, Object{"type": "error", "error": Object{"type": typ, "message": ue.Message}, "request_id": id})
		return
	}
	typ := ue.Type
	if typ == "" {
		typ = "upstream_error"
	}
	var code any
	if ue.Code != "" {
		code = ue.Code
	}
	writeJSON(w, Object{"error": Object{"type": typ, "code": code, "message": ue.Message, "param": nil}, "request_id": id})
}

// writeJSON 不做 HTML 转义：错误原话里的 > 等字符要原样到达客户端，客户端按字面匹配它
func writeJSON(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// setRequestID 同时写 Anthropic SDK 读的 request-id 与 OpenAI SDK 读的 x-request-id
func setRequestID(h http.Header, id string) {
	h.Set("Request-Id", id)
	h.Set("X-Request-ID", id)
}
