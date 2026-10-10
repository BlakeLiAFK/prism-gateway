// 对话请求主流程：选候选、转发、协议转换、失败切换。
package gateway

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// passHeaders 是从上游响应原样透传给调用方的限额头
var passHeaders = []string{"retry-after", "anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-tokens-remaining", "x-ratelimit-remaining-requests", "x-ratelimit-remaining-tokens"}

// attemptHeaders 是每次候选尝试可能写入的响应头。换候选重试时必须先清掉，
// 否则失败候选的声明（忽略了哪些字段、上游的限额）会挂在下一个候选的响应上。
var attemptHeaders = append([]string{"X-Prism-Ignored", "X-Prism-Dropped", "X-Prism-Demo", "X-Prism-Adapted"}, passHeaders...)

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
	setRequestID(w.Header(), id)
	if status == 429 && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "30")
	}
	w.WriteHeader(status)
	if p == "messages" {
		writeJSON(w, Object{"type": "error", "error": Object{"type": messagesErrorType(status), "message": message}, "request_id": id})
	} else {
		writeJSON(w, Object{"error": Object{"type": code, "code": code, "message": message, "param": nil}, "request_id": id})
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
	if err := e.checkKey(key); err != nil {
		_, code, msg := errorParts(err)
		protocolError(w, p, 429, code, msg, randomID("req_"))
		return
	}
	from := clientOf(r)
	id := randomID("req_")
	setRequestID(w.Header(), id)
	c := e.store.Config()
	r.Body = http.MaxBytesReader(w, r.Body, int64(c.Settings.MaxBodyMB)<<20)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		protocolError(w, p, 413, "BODY_TOO_LARGE", "请求体过大", id)
		return
	}
	o, err := decodeBody(b)
	if err != nil || o == nil {
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
	// 候选本来就是空的时候没有东西可筛：真实原因是候选被淘汰，交给下面的 NO_COMPATIBLE_MODEL，
	// 否则带 x-cmd-zdr 的请求会收到一个误导性的 ZDR 错误，也不会留下记录
	if err == nil && len(selections) > 0 {
		selections, err = constrainSelections(selections, r.Header)
	}
	if err != nil {
		status, code, msg := errorParts(err)
		if code == "ZDR_UNSUPPORTED" {
			// 候选是被这个请求头筛光的：客户端响应照旧，但同样没有候选能服务它，必须留下记录
			e.recordIncompatible(c, key, id, requested, p, session, from, whys, code, msg)
		}
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
		e.recordIncompatible(c, key, id, requested, p, session, from, whys, "NO_COMPATIBLE_MODEL", detail)
		protocolError(w, p, 400, "NO_COMPATIBLE_MODEL", "没有兼容此请求的候选模型（"+detail+"）", id)
		return
	}
	// Requests with server-managed state or built-in tools must never be replayed.
	safeFallback := o["previous_response_id"] == nil && o["conversation"] == nil && !boolean(o, "background") && !boolean(o, "store")
	for _, t := range arr(o["tools"]) {
		tt := str(obj(t), "type")
		if tt != "" && tt != "function" {
			safeFallback = false
		}
	}
	lastErr := fail("NO_CAPACITY", "所有候选模型当前均不可用", 429)
	admitted := false
	for _, s := range selections {
		att, er := e.admit(s, key, id, requested, p, session, from)
		if er != nil {
			lastErr = er
			continue
		}
		admitted = true
		start := now()
		u := Usage{tap: func(n int) { e.addStreamed(s.Model.ID, n) }}
		status := 200
		var runErr error
		func() {
			defer func() {
				r := e.finish(s, att, session, key.ID, u, status, runErr, start)
				e.noteDone(s, requested, r, u, start)
				// 落到后面的组意味着前面的组都挂了、可能正在用更贵的模型：按路由限频推一次告警。
				// 只在前面的组确实被试过时才告警：前面的组被管理员暂停或停用、为空，或本来就接不了这类请求
				// （如 auto 权限模式的分类器请求）时，候选根本没进入 selections，落到后面的组是预期行为，不是故障
				if s.Group > selections[0].Group && s.RouteID != "" && r.state == "success" && e.OnAlert != nil {
					go e.OnAlert("route", "route-group:"+s.RouteID, fmt.Sprintf("路由 %s 已落到第 %d 组「%s」（模型 %s）：前面的组全部不可用，或都接不了这次请求。", s.RouteID, s.Group+1, s.GroupName, cmp.Or(s.Model.Name, s.Model.ID)))
				}
			}()
			w.Header().Set("X-Prism-Model", s.Model.ID)
			w.Header().Set("X-Prism-Provider", s.Provider.ID)
			w.Header().Set("X-Prism-Upstream-Model", s.Model.Upstream)
			mode := "native"
			if s.Cross {
				mode = "converted"
			}
			w.Header().Set("X-Prism-Protocol-Mode", mode)
			// 换候选重试时，上一个候选留下的忽略字段声明不能带到这一个响应里
			for _, h := range attemptHeaders {
				w.Header().Del(h)
			}
			if s.ToolImagesAdapted {
				w.Header().Set("X-Prism-Adapted", "tool-result-images")
			}
			// 请求历史里有内容没有发给这个上游，与丢弃响应推理同样要披露
			for _, kind := range s.Dropped {
				announceDropped(w.Header(), kind)
			}
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
				} else {
					// 连不上或超时：一个字节都没收到，短冷却让客户端的重试落到别的候选
					e.cooldownFor(s.Model.ID, failureCooldown)
				}
				return
			}
			defer res.Body.Close()
			status = res.StatusCode
			if status < 200 || status >= 300 {
				body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
				runErr = newUpstreamStatusError(status, body)
				if status == http.StatusBadRequest && p == "messages" && s.Model.Protocol == "messages" && c.Settings.RequestDiagnosticsUntil > start/1000 {
					e.recordRequestDiagnostic(att, start, o, s.Body)
				}
				// 被拒的响应往往才带着 Retry-After 和剩余额度，这里同样要留存
				e.recordLimits(s.Model.ID, res.Header)
				e.coolAfterFailure(s.Model.ID, s.Provider, status, res.Header.Get("Retry-After"))
				return
			}
			e.recordTTFB(s.Model.ID, now()-start)
			e.recordLimits(s.Model.ID, res.Header)
			for _, h := range passHeaders {
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
				// 先读到第一个事件再向客户端提交 200：上游「200 之后立刻报错」在这里还能当成普通失败处理
				var upstream io.Reader
				if upstream, runErr = peekSSE(res.Body, s.Model.Protocol); runErr != nil {
					var ue *upstreamStatusError
					if errors.As(runErr, &ue) {
						status = ue.Status
						e.coolAfterFailure(s.Model.ID, s.Provider, status, "")
					} else {
						status = 502
						e.cooldownFor(s.Model.ID, failureCooldown)
					}
					return
				}
				if s.Cross && s.Model.DropReasoning {
					// 流式没法等到发现推理内容再补响应头，所以开关开启时先声明：
					// 本次跨协议响应会丢弃推理内容（如果上游返回了的话）
					announceDropped(w.Header(), "reasoning")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache, no-transform")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(200)
				if s.Cross {
					runErr = convertedStream(w, upstream, s.Model.Protocol, p, s.Model.ID, id, &u, s.Model.DropReasoning)
				} else {
					runErr = nativeStream(w, upstream, p, &u)
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
			pending := !s.Cross && s.Model.Protocol == "responses" && str(ro, "id") != "" &&
				(str(ro, "status") == "queued" || str(ro, "status") == "in_progress")
			if !pending && !hasVisibleOutput(ro, s.Model.Protocol) && !truncatedStop(ro, s.Model.Protocol) {
				runErr = fail("EMPTY_OUTPUT", "上游返回了空内容", 502)
				return
			}
			if s.Cross {
				co, er := decodeCompletion(ro, s.Model.Protocol)
				if er != nil && s.Model.DropReasoning && stripReasoning(ro, s.Model.Protocol) {
					// 管理员为这个模型显式接受了丢弃推理内容。剥离后重试，
					// 并在响应头标注——丢弃可以被接受，静默不行。
					if co, er = decodeCompletion(ro, s.Model.Protocol); er == nil {
						announceDropped(w.Header(), "reasoning")
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
		if emptyOutput(runErr) && safeFallback {
			// 换一个候选才有意义：同一个模型再来一次大概率还是同样的结果
			lastErr = fail("EMPTY_OUTPUT", "候选模型没有返回任何可用内容，已尝试可用候选", 502)
			continue
		}
		var ue *upstreamStatusError
		if errors.As(runErr, &ue) && ue.Status == 400 && safeFallback {
			// 上游明确拒绝了请求、没有生成任何内容，是确定性失败：换下一个候选是安全的。
			// 全部候选都拒绝时，把最后一个上游的原话交给客户端——Claude Code 会从中识别不支持的字段并自行调整
			lastErr = ue
			continue
		}
		if status == 499 {
			return
		}
		// 网关自己构造的错误带着准确的原因，不要被笼统的「上游请求失败」盖掉
		var detailed *APIError
		if errors.As(runErr, &detailed) {
			protocolError(w, p, detailed.Status, detailed.Code, detailed.Message, id)
			return
		}
		if ue != nil {
			upstreamErrorResponse(w, p, ue, id)
			return
		}
		protocolError(w, p, 502, "UPSTREAM_ERROR", fmt.Sprintf("上游请求失败（HTTP %d）；请求未被自动重放。", status), id)
		return
	}
	if !admitted && len(selections) > 0 {
		e.recordRejected(selections[0], key, id, requested, p, session, from, lastErr)
	}
	// 所有候选都在同一请求里被上游拒绝（400 换候选用尽）：交出最后一个上游的原话
	var ue *upstreamStatusError
	if errors.As(lastErr, &ue) {
		upstreamErrorResponse(w, p, ue, id)
		return
	}
	if !admitted {
		e.retryHints(w.Header(), selections, lastErr)
	}
	status, code, msg := errorParts(lastErr)
	// 路由的全部候选都因容量或上游问题失败时告警；空回答这类请求本身的问题不算。
	// 客户端多半经别名访问，要按别名解析后的路由判断，限频键也用路由 ID
	if route, alias, isRoute := c.resolveRoute(requested); isRoute && e.OnAlert != nil && !strings.HasPrefix(code, "EMPTY_OUTPUT") {
		go e.OnAlert("route", "route:"+route.ID, fmt.Sprintf("路由 %s 的全部候选都不可用（%s），客户端%s收到 HTTP %d。", route.ID, code, viaAlias(alias), status))
	}
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

// emptyOutput 判断这次失败是不是「上游通了但没给出正文」。
// 只在非流式路径产生：流式一旦开始写就不能回退换候选了。
func emptyOutput(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == "EMPTY_OUTPUT"
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
