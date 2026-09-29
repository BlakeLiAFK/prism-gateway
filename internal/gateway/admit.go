package gateway

// 请求准入与结算：准入时写入预留记录，结束时按实际用量改写；进程崩溃后预留原样保留。

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"prism-gateway/internal/sqlite"
)

// gate 检查冷却、并发与 RPM，被拒时记入拒绝统计。调用方持有 e.mu。
func (e *Engine) gate(h *health, s selection, t int64) (rerr error) {
	// 准入被拒计入内存，路由实时面板据此回答「是不是被并发 / 冷却挡住了」
	defer func() { h.reject(rerr, t) }()
	if h.Cooldown > t {
		return fail("UPSTREAM_COOLDOWN", "上游限流冷却中", 429)
	}
	if e.global >= e.store.Config().Settings.GlobalConcurrency || h.Active >= s.Model.Concurrency {
		return fail("CONCURRENCY_LIMIT", "并发已满，请稍后重试", 429)
	}
	recent := h.Recent[:0]
	for _, ts := range h.Recent {
		if ts > t-60000 {
			recent = append(recent, ts)
		}
	}
	h.Recent = recent
	if s.Model.RPM > 0 && len(h.Recent) >= s.Model.RPM {
		return fail("RPM_LIMIT", "本地 RPM 限额已满", 429)
	}
	return nil
}

// checkBudget 检查本地滚动预算（含本次预留）。汇总查询较慢，不能占着 e.mu；
// 调用方持有该模型的预算锁，保证检查与随后的预留写入之间没有别的请求插进来。
func (e *Engine) checkBudget(s selection, reserve int64) error {
	// 便宜的内存检查先行：并发已满、冷却中的候选不必再去查库
	e.mu.Lock()
	err := e.gate(e.state(s.Model.ID), s, now())
	e.mu.Unlock()
	if err != nil {
		return err
	}
	q, err := e.quota(s.Model.ID)
	if err != nil {
		return err
	}
	for _, v := range []struct {
		k string
		n float64
	}{{"used_5h", s.Model.Limit5h}, {"used_7d", s.Model.Limit7d}, {"used_30d", s.Model.Limit30d}} {
		if v.n > 0 && nano(num(q, v.k))+reserve > nano(v.n) {
			err = fail("LOCAL_QUOTA_LIMIT", "本地滚动预算不足（包含本次预留）；不是上游官方余额", 429)
			e.mu.Lock()
			e.state(s.Model.ID).reject(err, now())
			e.mu.Unlock()
			return err
		}
	}
	return nil
}

// release 归还 admit 已占用但没能落库的并发与 RPM 名额
func (e *Engine) release(model string, t int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.state(model)
	h.Active = max(0, h.Active-1)
	e.global = max(0, e.global-1)
	if i := slices.Index(h.Recent, t); i >= 0 {
		h.Recent = slices.Delete(h.Recent, i, i+1)
	}
}

// admit 先在内存里占位，再写预留记录。e.mu 只保护内存状态，
// 汇总查询与落库（fsync）都在锁外，否则所有模型的请求都要排队等它们。
func (e *Engine) admit(s selection, key Principal, reqID, requested, p, session string, from client) (string, error) {
	// 预留金额只取决于请求体，遍历大请求体不该发生在锁内
	reserve := reserveCost(s.Model, s.Body)
	if hasBudget(s.Model) {
		// 预算检查与预留写入必须原子，否则并发请求会各自看到同一份用量而一起放行；
		// 只需要在同一个模型内串行
		l, _ := e.budgetLocks.LoadOrStore(s.Model.ID, &sync.Mutex{})
		l.(*sync.Mutex).Lock()
		defer l.(*sync.Mutex).Unlock()
		if err := e.checkBudget(s, reserve); err != nil {
			return "", err
		}
	}
	t := now()
	e.mu.Lock()
	h := e.state(s.Model.ID)
	if err := e.gate(h, s, t); err != nil {
		e.mu.Unlock()
		return "", err
	}
	h.Active++
	e.global++
	h.Recent = append(h.Recent, t)
	e.mu.Unlock()
	id := randomID("att_")
	err := e.store.DB.Exec(`INSERT INTO requests(id,parent_id,key_id,requested_model,model_id,provider_id,protocol,upstream_protocol,session_id,status,started_at,cost_nano,cost_known,reason,is_demo,client_ip,user_agent,agent_role) VALUES (?,?,?,?,?,?,?,?,?,'running',?,?,?,?,?,?,?,?)`, id, reqID, key.ID, requested, s.Model.ID, s.Provider.ID, p, s.Model.Protocol, session, t, reserve, s.Model.PricingSet, s.Reason, s.Provider.Kind == "mock", from.IP, from.Agent, from.Role)
	if err != nil {
		e.release(s.Model.ID, t)
		return "", err
	}
	return id, nil
}

// settled 是一次请求结束后的记账结论
type settled struct {
	state, code, mode string
	value             int64
	known             bool
	cacheKnown        int
}

// settle 依据用量与状态算出记账结论，只读入参，不碰任何共享状态
func settle(s selection, u Usage, status int, err error) settled {
	r := settled{state: "success", mode: "reported_tokens", value: cost(s.Model, u), known: u.Known && s.Model.PricingSet}
	reported := isOpenRouter(s.Provider) && u.ReportedCostKnown
	if reported {
		r.mode = "reported_cost"
		r.value = u.ReportedCostNano
		r.known = true
	}
	if err != nil || status >= 400 {
		r.state = "error"
		r.code = fmt.Sprintf("UPSTREAM_%d", status)
		// 带自有错误码的失败（空回答、跨协议不兼容）记它自己的码：
		// 记成 UPSTREAM_502 会把「上游坏了」和「上游好好的但内容用不了」混为一谈
		var ae *APIError
		if errors.As(err, &ae) && ae.Code != "" {
			r.code = ae.Code
		}
		if status == 499 {
			r.code = "CLIENT_CANCELED"
		}
	}
	if !u.Known && !reported {
		r.mode = "unknown"
		r.known = false
	}
	// OpenRouter 已报告金额时，即使后续转换或客户端写入失败也保留实际费用。
	// 只有 token 用量时仍要求上游成功，避免把失败响应里的模糊字段当成账单。
	billed := reported || status == 200 && u.Known
	if (r.state == "error" || (!u.Known && !reported)) && !billed {
		if status == 429 || status == 503 || (status >= 400 && status < 500 && status != 499) {
			r.value = 0
			r.mode = "rejected"
		} else {
			r.mode = "reserved_unknown"
			r.state = "unknown"
			r.value = reserveCost(s.Model, s.Body)
			r.known = false
		}
	}
	if u.CacheKnown {
		r.cacheKnown = 1
	} else if u.CacheNull {
		r.cacheKnown = 2
	}
	return r
}

// finish 先在 e.mu 内更新内存状态，再把落库合并成一个事务：
// 原来是 UPDATE、两次汇总、会话亲和各自提交，每次提交一次 fsync，而且全程占着 e.mu。
func (e *Engine) finish(s selection, id, session, keyID string, u Usage, status int, err error, start int64) {
	r := settle(s, u, status, err)
	t := now()
	writeSession := session != "" && r.state == "success"
	e.mu.Lock()
	h := e.state(s.Model.ID)
	h.Active = max(0, h.Active-1)
	e.global = max(0, e.global-1)
	h.LastStatus = status
	h.LatencyMS = t - start
	e.trackFailure(h, s.Model, r.state, r.code)
	keep := writeSession && e.keepPin(s)
	e.mu.Unlock()

	var soft []error
	dberr := e.store.DB.Transaction(func(tx *sqlite.Tx) error {
		if err := tx.Exec(`UPDATE requests SET status=?,http_status=?,duration_ms=?,input_tokens=?,output_tokens=?,cache_tokens=?,write_tokens=?,cache_known=?,cost_nano=?,cost_known=?,usage_mode=?,error_code=? WHERE id=?`, r.state, status, t-start, u.Input, u.Output, u.Cache, u.Write, r.cacheKnown, r.value, r.known, r.mode, r.code, id); err != nil {
			return err
		}
		// 汇总与会话亲和单条失败只记日志、不回滚：它们只影响统计与路由，不能连累记账
		if err := rollupInto(tx, id); err != nil {
			soft = append(soft, fmt.Errorf("usage rollup: %w", err))
		}
		if writeSession {
			// 冲突分支里的列要用本表限定。曾经误写成 requests.requests（另一张表），
			// 导致整条语句编译失败、会话亲和记录一条都没写进去，而错误被丢弃因此无人察觉。
			q := `INSERT INTO sessions VALUES (?,?,?,?,?,1) ON CONFLICT(id) DO UPDATE SET model_id=excluded.model_id,provider_id=excluded.provider_id,updated_at=excluded.updated_at,requests=sessions.requests+1`
			if keep {
				// 原模型只是临时故障：保留绑定、只续期，冷却结束后回到原模型
				q = `INSERT INTO sessions VALUES (?,?,?,?,?,1) ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at,requests=sessions.requests+1`
			}
			if err := tx.Exec(q, session, keyID, s.Model.ID, s.Provider.ID, t); err != nil {
				soft = append(soft, fmt.Errorf("session affinity: %w", err))
			}
		}
		return nil
	})
	if dberr != nil {
		slog.Error("request accounting write failed", "request_id", id, "model", s.Model.ID, "err", dberr)
	}
	// 事务已结束、数据库锁已释放，再输出日志
	for _, err := range soft {
		slog.Error("request auxiliary write failed", "request_id", id, "err", err)
	}
}
