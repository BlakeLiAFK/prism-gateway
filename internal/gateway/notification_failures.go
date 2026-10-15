package gateway

import (
	"fmt"
	"log/slog"
	"strings"
)

type failureSample struct {
	ID, Provider, Model, Code string
	At                        int64
}

// 只保留触发告警所需的五次尝试，不持有请求正文或凭证。
func (e *Engine) trackFailure(h *health, s selection, id, state, code string) {
	if state == "success" {
		h.Fails = 0
		h.FailureSamples = nil
		return
	}
	if code == "CLIENT_CANCELED" {
		return
	}
	h.Fails++
	if h.Fails <= failAlertAfter {
		h.FailureSamples = append(h.FailureSamples, failureSample{id, noticeName(s.Provider.Name, s.Provider.ID), noticeName(s.Model.Name, s.Model.ID), code, now()})
	}
	if h.Fails == failAlertAfter && e.OnAlert != nil {
		samples := append([]failureSample(nil), h.FailureSamples...)
		go func() { e.OnAlert("failures", "fail:"+s.Model.ID, e.failureNotice(samples)) }()
	}
}

func (e *Engine) failureNotice(samples []failureSample) string {
	if len(samples) == 0 {
		return "模型连续失败\n\n请求上下文未记录"
	}
	type source struct {
		text  string
		ids   []string
		count int
	}
	groups := []source{}
	index := map[string]int{}
	for _, sample := range samples {
		rows, err := e.store.DB.Query("SELECT key_id,client_ip,user_agent,agent_role,requested_model,parent_id FROM requests WHERE id=?", sample.ID)
		keyID, requested, reqID := "", "", sample.ID
		from := client{}
		if err != nil {
			slog.Warn("alert request context read failed", "request_id", sample.ID)
		}
		if len(rows) > 0 {
			r := rows[0]
			keyID, requested, reqID = r.String("key_id"), r.String("requested_model"), r.String("parent_id")
			from = client{IP: r.String("client_ip"), Agent: r.String("user_agent"), Role: r.String("agent_role")}
		}
		label := "供应商：" + sample.Provider + "\n模型：" + sample.Model + "\n请求入口：" + noticeValue(requested) + "\n" + e.noticeSource(keyID, from)
		key := raw([]string{sample.Provider, keyID, from.IP, from.Agent, from.Role, requested})
		n, ok := index[key]
		if !ok {
			n = len(groups)
			index[key] = n
			groups = append(groups, source{text: label})
		}
		groups[n].count++
		groups[n].ids = append(groups[n].ids, noticeValue(reqID))
	}
	last := samples[len(samples)-1]
	blocks := []string{fmt.Sprintf("连续 %d 次请求失败（按上游尝试计数）\n最近错误：%s\n时间范围：%s 至 %s", len(samples), noticeError(last.Code), noticeTime(samples[0].At), noticeTime(last.At))}
	for i, g := range groups {
		blocks = append(blocks, fmt.Sprintf("来源 %d：%d 次\n%s\n请求 ID：%s", i+1, g.count, g.text, strings.Join(g.ids, "、")))
	}
	blocks = append(blocks, "定位：按供应商和请求 ID 查看请求日志。以上来源分别计数，调用凭证名称不代表已核实的个人身份。")
	return notice("模型连续失败", blocks...)
}
