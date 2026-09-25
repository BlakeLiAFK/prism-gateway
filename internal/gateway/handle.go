// 对话请求主流程：选候选、转发、协议转换、失败切换。
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func pathFor(p string) string {
	switch p {
	case "chat":
		return "/chat/completions"
	case "responses":
		return "/responses"
	case "systemone":
		return "/systemone"
	default:
		return "/messages"
	}
}

func protocolError(w http.ResponseWriter, p string, status int, code, message, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", id)
	if status == 429 {
		w.Header().Set("Retry-After", "30")
	}
	w.WriteHeader(status)
	if p == "messages" {
		typ := "invalid_request_error"
		if status == 401 {
			typ = "authentication_error"
		} else if status == 429 {
			typ = "rate_limit_error"
		} else if status >= 500 {
			typ = "api_error"
		}
		json.NewEncoder(w).Encode(Object{"type": "error", "error": Object{"type": typ, "message": message}, "request_id": id})
	} else {
		json.NewEncoder(w).Encode(Object{"error": Object{"type": code, "code": code, "message": message, "param": nil}, "request_id": id})
	}
}

func errorParts(err error) (int, string, string) {
	var a *APIError
	if errors.As(err, &a) {
		return a.Status, a.Code, a.Message
	}
	return 500, "INTERNAL_ERROR", "内部操作失败，请检查服务端日志"
}

func (e *Engine) Handle(w http.ResponseWriter, r *http.Request, p string, key Principal) {
	from := clientOf(r)
	id := randomID("req_")
	w.Header().Set("X-Request-ID", id)
	c := e.store.Config()
	r.Body = http.MaxBytesReader(w, r.Body, int64(c.Settings.MaxBodyMB)<<20)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		protocolError(w, p, 413, "BODY_TOO_LARGE", "请求体过大", id)
		return
	}
	var o Object
	if err = json.Unmarshal(b, &o); err != nil || o == nil {
		protocolError(w, p, 400, "INVALID_JSON", "无效的 JSON 对象", id)
		return
	}
	requested := str(o, "model")
	if requested == "" {
		protocolError(w, p, 400, "MODEL_REQUIRED", "必须指定 model", id)
		return
	}
	if !key.allows(requested) {
		protocolError(w, p, 403, "MODEL_FORBIDDEN", "此 API Key 无权调用该模型或路由", id)
		return
	}
	rawSession := ""
	for _, h := range []string{"x-prism-session", "x-opencode-session", "x-claude-code-session-id", "x-codex-session-id", "session_id", "x-session-id"} {
		if v := r.Header.Get(h); v != "" && len(v) <= 512 {
			rawSession = v
			break
		}
	}
	if rawSession == "" {
		rawSession = randomID("conv_")
	}
	// Claude Code 的子代理与主线程共用会话 ID，但提示词前缀不同、不共享上游缓存，
	// 按 agent-id 分开亲和，多个子代理才能分散到不同候选
	sessionKey := rawSession
	if a := r.Header.Get("x-claude-code-agent-id"); a != "" && len(a) <= 128 {
		sessionKey += "#" + a
	}
	session := "ses_" + digest(key.ID + ":" + sessionKey)[:32]
	w.Header().Set("X-Prism-Session", rawSession)
	w.Header().Set("X-Prism-Config-Version", fmt.Sprint(c.Version))
	if p == "systemone" {
		if er := validateSystemOne(o); er != nil {
			protocolError(w, p, 400, "INVALID_REQUEST", er.Error(), id)
			return
		}
	}
	selections, whys, err := e.selections(c, o, p, session)
	if err != nil {
		status, code, msg := errorParts(err)
		protocolError(w, p, status, code, msg, id)
		return
	}
	if len(selections) == 0 {
		// 每个候选为什么被排除是算出来了的，不告诉调用方等于让人去猜
		detail := ""
		for _, v := range whys {
			e := obj(v)
			if r := str(e, "reason"); r != "" {
				if detail != "" {
					detail += "；"
				}
				detail += str(e, "model_id") + ": " + r
			}
		}
		if detail == "" {
			detail = "候选池为空或全部被禁用"
		}
		protocolError(w, p, 400, "NO_COMPATIBLE_MODEL", "没有兼容此请求的候选模型（"+detail+"）", id)
		return
	}
	// Requests with server-managed state or built-in tools must never be replayed.
	safeFallback := o["previous_response_id"] == nil && o["conversation"] == nil && !boolean(o, "background")
	for _, t := range arr(o["tools"]) {
		tt := str(obj(t), "type")
		if tt != "" && tt != "function" {
			safeFallback = false
		}
	}
	lastErr := fail("NO_CAPACITY", "所有候选模型当前均不可用", 429)
	for _, s := range selections {
		att, er := e.admit(s, key, id, requested, p, session, from)
		if er != nil {
			lastErr = er
			continue
		}
		start := now()
		u := Usage{}
		status := 200
		upstreamStatus := 0
		var runErr error
		func() {
			defer func() { e.finish(s, att, session, key.ID, u, status, runErr, start) }()
			w.Header().Set("X-Prism-Model", s.Model.ID)
			w.Header().Set("X-Prism-Provider", s.Provider.ID)
			w.Header().Set("X-Prism-Upstream-Model", s.Model.Upstream)
			mode := "native"
			if s.Cross {
				mode = "converted"
			}
			w.Header().Set("X-Prism-Protocol-Mode", mode)
			if len(s.Ignored) > 0 {
				// 这些请求字段被接受但没有传给上游。忽略可以，不说不行。
				w.Header().Set("X-Prism-Ignored", strings.Join(s.Ignored, ","))
			}
			if s.Provider.Kind == "mock" && p == "systemone" {
				w.Header().Set("X-Prism-Demo", "true")
				res := demoSystemOne(o, s.Model.Upstream)
				extractUsage(obj(res["usage"]), "systemone", &u)
				w.Header().Set("Content-Type", "application/json")
				runErr = json.NewEncoder(w).Encode(res)
				return
			}
			if s.Provider.Kind == "mock" {
				w.Header().Set("X-Prism-Demo", "true")
				co := demoCompletion(o, p)
				u = co.Usage
				if boolean(o, "stream") {
					w.Header().Set("Content-Type", "text/event-stream")
					em := newEmitter(w, p, s.Model.ID, id)
					em.usage = u
					for i, b := range co.Blocks {
						if er = em.start(i, b); er != nil {
							runErr = er
							break
						}
						text := b.Text
						if b.Kind == "tool" {
							text = b.Arguments
						}
						runes := []rune(text)
						for n := 0; n < len(runes); n += 8 {
							select {
							case <-r.Context().Done():
								runErr = r.Context().Err()
								status = 499
								return
							default:
							}
							if er = em.delta(i, string(runes[n:min(n+8, len(runes))])); er != nil {
								runErr = er
								status = 499
								return
							}
							time.Sleep(8 * time.Millisecond)
						}
					}
					runErr = em.finish()
				} else {
					w.Header().Set("Content-Type", "application/json")
					runErr = json.NewEncoder(w).Encode(completionObject(co, p, s.Model.ID, id))
				}
				return
			}
			target := strings.TrimRight(s.Provider.BaseURL, "/") + pathFor(s.Model.Protocol)
			req, er := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewBufferString(raw(s.Body)))
			if er != nil {
				runErr = er
				status = 502
				return
			}
			requestHeaders(req, r, s.Provider, s.Model.Protocol, rawSession)
			res, er := e.client(s.Provider).Do(req)
			if er != nil {
				runErr = er
				status = 502
				if r.Context().Err() != nil {
					status = 499
				}
				return
			}
			defer res.Body.Close()
			status = res.StatusCode
			upstreamStatus = res.StatusCode
			if status < 200 || status >= 300 {
				io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
				runErr = fmt.Errorf("upstream status %d", status)
				// 被拒的响应往往才带着 Retry-After 和剩余额度，这里同样要留存
				e.recordLimits(s.Model.ID, res.Header)
				if retryable(status) {
					e.cooldown(s.Model.ID, res.Header.Get("Retry-After"))
					if status == 429 && e.OnRateLimited != nil {
						go e.OnRateLimited(s.Provider)
					}
				} else if exhausted(status) {
					e.cooldownFor(s.Model.ID, exhaustedCooldown)
				}
				return
			}
			e.recordTTFB(s.Model.ID, now()-start)
			e.recordLimits(s.Model.ID, res.Header)
			for _, h := range []string{"retry-after", "anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-tokens-remaining", "x-ratelimit-remaining-requests", "x-ratelimit-remaining-tokens"} {
				if v := res.Header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
			if boolean(o, "stream") {
				if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
					status = 502
					runErr = errors.New("expected SSE content type")
					return
				}
				if s.Cross && s.Model.DropReasoning {
					// 流式没法等到发现推理内容再补响应头，所以开关开启时先声明：
					// 本次跨协议响应会丢弃推理内容（如果上游返回了的话）
					w.Header().Set("X-Prism-Dropped", "reasoning")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache, no-transform")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(200)
				if s.Cross {
					runErr = convertedStream(w, res.Body, s.Model.Protocol, p, s.Model.ID, id, &u, s.Model.DropReasoning)
				} else {
					runErr = nativeStream(w, res.Body, p, &u)
				}
				if runErr != nil {
					status = 502
					if r.Context().Err() != nil {
						status = 499
					} else {
						streamError(w, p, id)
					}
				}
				return
			}
			data, er := io.ReadAll(io.LimitReader(res.Body, (16<<20)+1))
			if er != nil || len(data) > 16<<20 {
				status = 502
				runErr = errors.New("invalid or oversized upstream body")
				return
			}
			var ro Object
			if er = json.Unmarshal(data, &ro); er != nil || ro == nil {
				status = 502
				runErr = errors.New("invalid upstream JSON")
				return
			}
			if ro["error"] != nil {
				status = 502
				runErr = errors.New("upstream error object")
				return
			}
			extractUsage(obj(ro["usage"]), s.Model.Protocol, &u)
			if !hasVisibleOutput(ro, s.Model.Protocol) {
				// 上游 200 了却没给出任何可用内容。静默把空回答转出去，调用方只会
				// 以为模型答不上来；实际是这个候选本次不可用，该换下一个。
				if truncatedStop(ro, s.Model.Protocol) {
					runErr = fail("EMPTY_OUTPUT_TRUNCATED", "上游在触及输出上限前没有产出正文（推理模型常见：输出预算被思考占满）", 502)
				} else {
					runErr = fail("EMPTY_OUTPUT", "上游返回了空内容", 502)
				}
				return
			}
			if s.Cross {
				co, er := decodeCompletion(ro, s.Model.Protocol)
				if er != nil && s.Model.DropReasoning && stripReasoning(ro, s.Model.Protocol) {
					// 管理员为这个模型显式接受了丢弃推理内容。剥离后重试，
					// 并在响应头标注——丢弃可以被接受，静默不行。
					if co, er = decodeCompletion(ro, s.Model.Protocol); er == nil {
						w.Header().Set("X-Prism-Dropped", "reasoning")
					}
				}
				if er != nil {
					// 上游是通的，也确实返回了内容；问题出在响应里有跨协议表达不了的
					// 部分（典型是带签名的推理）。必须如实说明，否则调用方只会去
					// 排查根本没出问题的上游。
					status = 502
					runErr = fail("UPSTREAM_INCOMPATIBLE",
						"上游响应包含无法跨协议转换的内容（"+er.Error()+"）；请对该模型改用其原生协议调用",
						502)
					return
				}
				data = []byte(raw(completionObject(co, p, s.Model.ID, id)))
			}
			w.Header().Set("Content-Type", "application/json")
			_, runErr = w.Write(data)
			if runErr != nil {
				status = 499
			}
		}()
		if runErr == nil {
			return
		}
		if boolean(o, "stream") && w.Header().Get("Content-Type") == "text/event-stream" {
			return
		}
		if retryable(status) && safeFallback {
			lastErr = fail("UPSTREAM_RATE_LIMIT", "上游拒绝请求，已尝试可用候选", 429)
			continue
		}
		if exhausted(status) && safeFallback {
			lastErr = fail("UPSTREAM_EXHAUSTED", "上游额度或权限不可用，已尝试可用候选", 402)
			continue
		}
		if gatewayFailure(upstreamStatus) && safeFallback {
			lastErr = fail("UPSTREAM_GATEWAY_ERROR", "上游网关错误，请求未到达模型，已尝试可用候选", 502)
			continue
		}
		if emptyOutput(runErr) && safeFallback {
			// 换一个候选才有意义：同一个模型再来一次大概率还是同样的结果
			lastErr = fail("EMPTY_OUTPUT", "候选模型没有返回任何可用内容，已尝试可用候选", 502)
			var ae *APIError
			if n := requestedMaxTokens(o); n > 0 && errors.As(runErr, &ae) && ae.Code == "EMPTY_OUTPUT_TRUNCATED" {
				// 仍然换候选：路由里可能有不思考的模型，同样的上限也能答出来
				lastErr = fail("EMPTY_OUTPUT_TRUNCATED", fmt.Sprintf("max_tokens=%d 太小：推理模型的思考用完了输出预算，没来得及输出正文，请调大 max_tokens", n), 502)
			}
			continue
		}
		if status == 499 {
			return
		}
		outStatus := status
		if outStatus < 400 || outStatus >= 500 {
			outStatus = 502
		}
		// 网关自己构造的错误带着准确的原因，不要被笼统的「上游请求失败」盖掉。
		// 上游返回的错误正文仍然不转发。
		var detailed *APIError
		if errors.As(runErr, &detailed) {
			protocolError(w, p, detailed.Status, detailed.Code, detailed.Message, id)
			return
		}
		protocolError(w, p, outStatus, "UPSTREAM_ERROR", fmt.Sprintf("上游请求失败（HTTP %d）；请求未被自动重放。", status), id)
		return
	}
	status, code, msg := errorParts(lastErr)
	protocolError(w, p, status, code, msg, id)
}

func demoCompletion(o Object, p string) Completion {
	input := estimateInput(o)
	text := "这是 Prism Gateway 的本地演示响应，不是真实 AI 推理，也不会调用任何云端服务。\n\n连接、鉴权、协议适配、路由选择和请求统计链路均已运行。添加真实 Provider 与 API Key 后即可处理实际代码任务。"
	if p == "responses" {
		text += "\n\n当前入口：OpenAI Responses。"
	} else if p == "messages" {
		text += "\n\n当前入口：Anthropic Messages。"
	} else {
		text += "\n\n当前入口：OpenAI Chat Completions。"
	}
	return Completion{Blocks: []Block{{Kind: "text", Text: text}}, Usage: Usage{Input: input, Output: int64(len([]rune(text))), Known: true}, Stop: "stop"}
}

func (e *Engine) CountTokens(w http.ResponseWriter, r *http.Request, key Principal) {
	id := randomID("req_")
	var o Object
	r.Body = http.MaxBytesReader(w, r.Body, int64(e.store.Config().Settings.MaxBodyMB)<<20)
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		protocolError(w, "messages", 400, "INVALID_JSON", "JSON 无效", id)
		return
	}
	if !key.allows(str(o, "model")) {
		protocolError(w, "messages", 403, "MODEL_FORBIDDEN", "模型未授权", id)
		return
	}
	c := e.store.Config()
	cp := Object{}
	for k, v := range o {
		cp[k] = v
	}
	cp["max_tokens"] = 1
	sel, _, err := e.selections(c, cp, "messages", "")
	if err != nil || len(sel) == 0 {
		protocolError(w, "messages", 400, "NO_COMPATIBLE_MODEL", "没有兼容模型", id)
		return
	}
	s := sel[0]
	if s.Model.NativeCount && s.Model.Protocol == "messages" && s.Provider.Kind != "mock" {
		body := Object{}
		for k, v := range o {
			body[k] = v
		}
		body["model"] = s.Model.Upstream
		req, _ := http.NewRequestWithContext(r.Context(), "POST", strings.TrimRight(s.Provider.BaseURL, "/")+"/messages/count_tokens", bytes.NewBufferString(raw(body)))
		requestHeaders(req, r, s.Provider, "messages", "")
		res, err := e.client(s.Provider).Do(req)
		if err != nil {
			protocolError(w, "messages", 502, "UPSTREAM_ERROR", "计数上游连接失败", id)
			return
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err == nil && res.StatusCode == 200 && json.Valid(data) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Prism-Token-Count-Mode", "provider")
			w.Write(data)
			return
		}
		protocolError(w, "messages", 502, "UPSTREAM_ERROR", "上游计数失败；没有静默替换为估算值", id)
		return
	}
	if !c.Settings.AllowEstimatedCount {
		protocolError(w, "messages", 501, "TOKEN_COUNT_UNSUPPORTED", "此模型没有原生计数接口；本地估算已关闭", id)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Prism-Token-Count-Mode", "estimated")
	json.NewEncoder(w).Encode(Object{"input_tokens": estimateInput(o)})
}

// emptyOutput 判断这次失败是不是「上游通了但没给出正文」。
// 只在非流式路径产生：流式一旦开始写就不能回退换候选了。
func emptyOutput(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Code == "EMPTY_OUTPUT" || ae.Code == "EMPTY_OUTPUT_TRUNCATED")
}

// recordLimits 留存上游这次声明的限额，并在它明说额度已经归零时
// 直接冷却到重置时刻——不必再拿下一个请求去撞一次 429。
func (e *Engine) recordLimits(model string, h http.Header) {
	raw := limitHeaders(h)
	if len(raw) == 0 {
		return
	}
	e.mu.Lock()
	st := e.state(model)
	st.Limits = raw
	st.LimitsAt = now()
	if until := exhaustedUntil(raw); until > st.Cooldown {
		st.Cooldown = min(until, now()+int64(24*time.Hour/time.Millisecond))
	}
	e.mu.Unlock()
}
