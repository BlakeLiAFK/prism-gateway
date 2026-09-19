package gateway

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Prometheus 文本格式导出。默认关闭，需要在管理后台开启，且必须用管理员令牌抓取：
// 指标里有模型名、调用量与费用，属于内部经营信息，不能匿名暴露。
// 只导出聚合计数，不含 prompt、回答、密钥或会话内容。

func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

type metricRow struct {
	model, protocol, status        string
	count, duration, input, output int64
	cache, write, cost             int64
}

func (a *App) writeMetrics(w io.Writer) error {
	c := a.Store.Config()
	fmt.Fprintf(w, "# HELP prism_build_info 构建信息，值恒为 1\n# TYPE prism_build_info gauge\n")
	fmt.Fprintf(w, "prism_build_info{version=%q} 1\n", escapeLabel(Version))
	fmt.Fprintf(w, "# HELP prism_uptime_seconds 进程运行时长\n# TYPE prism_uptime_seconds gauge\n")
	fmt.Fprintf(w, "prism_uptime_seconds %d\n", (now()-a.Started)/1000)
	fmt.Fprintf(w, "# HELP prism_config_version 当前配置版本\n# TYPE prism_config_version gauge\n")
	fmt.Fprintf(w, "prism_config_version %d\n", c.Version)

	health := a.Engine.Health()
	fmt.Fprintf(w, "# HELP prism_active_requests 正在处理的上游请求数\n# TYPE prism_active_requests gauge\n")
	fmt.Fprintf(w, "prism_active_requests %v\n", health["active"])

	rows, err := a.Store.DB.Query(`SELECT model_id,protocol,status,COUNT(*) n,
		COALESCE(SUM(duration_ms),0) dur,COALESCE(SUM(input_tokens),0) it,
		COALESCE(SUM(output_tokens),0) ot,COALESCE(SUM(cache_tokens),0) ct,
		COALESCE(SUM(write_tokens),0) wt,COALESCE(SUM(cost_nano),0) cost
		FROM requests WHERE started_at>=? GROUP BY model_id,protocol,status`, now()-30*86400000)
	if err != nil {
		return err
	}
	out := make([]metricRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, metricRow{
			model: r.String("model_id"), protocol: r.String("protocol"), status: r.String("status"),
			count: r.Int("n"), duration: r.Int("dur"), input: r.Int("it"), output: r.Int("ot"),
			cache: r.Int("ct"), write: r.Int("wt"), cost: r.Int("cost"),
		})
	}
	// 固定顺序，方便 diff 与测试
	sort.Slice(out, func(i, j int) bool {
		if out[i].model != out[j].model {
			return out[i].model < out[j].model
		}
		if out[i].protocol != out[j].protocol {
			return out[i].protocol < out[j].protocol
		}
		return out[i].status < out[j].status
	})

	label := func(m metricRow) string {
		return fmt.Sprintf("model=%q,protocol=%q,status=%q",
			escapeLabel(m.model), escapeLabel(m.protocol), escapeLabel(m.status))
	}
	fmt.Fprintf(w, "# HELP prism_requests_total 近 30 天请求 attempt 数\n# TYPE prism_requests_total counter\n")
	for _, m := range out {
		fmt.Fprintf(w, "prism_requests_total{%s} %d\n", label(m), m.count)
	}
	fmt.Fprintf(w, "# HELP prism_request_duration_ms_total 近 30 天耗时累计\n# TYPE prism_request_duration_ms_total counter\n")
	for _, m := range out {
		fmt.Fprintf(w, "prism_request_duration_ms_total{%s} %d\n", label(m), m.duration)
	}
	fmt.Fprintf(w, "# HELP prism_tokens_total 近 30 天 token 用量\n# TYPE prism_tokens_total counter\n")
	for _, m := range out {
		base := fmt.Sprintf("model=%q,protocol=%q,status=%q", escapeLabel(m.model), escapeLabel(m.protocol), escapeLabel(m.status))
		for _, kv := range []struct {
			kind string
			v    int64
		}{{"cache", m.cache}, {"input", m.input}, {"output", m.output}, {"write", m.write}} {
			fmt.Fprintf(w, "prism_tokens_total{%s,kind=%q} %d\n", base, kv.kind, kv.v)
		}
	}
	fmt.Fprintf(w, "# HELP prism_cost_nano_total 本地费用估算累计，单位十亿分之一美元；不是供应商账单\n# TYPE prism_cost_nano_total counter\n")
	for _, m := range out {
		fmt.Fprintf(w, "prism_cost_nano_total{%s} %d\n", label(m), m.cost)
	}
	return nil
}
