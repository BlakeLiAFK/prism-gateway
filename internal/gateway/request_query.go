package gateway

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const requestDayMS int64 = 24 * 60 * 60 * 1000
const knownRequestCost = "(cost_known=1 AND usage_mode IN ('reported_tokens','" + repricedMode + "','reported_cost','rejected'))"

type requestQuery struct {
	where, order string
	args         []any
	from, to     int64
}

func (a *App) requests(p Object) (any, error) {
	q, err := buildRequestQuery(p, now())
	if err != nil {
		return nil, fail("INVALID_PARAMS", err.Error(), 400)
	}
	page := clamp(int(num(p, "page")), 1, 10000)
	limit := 30
	if p["page_size"] != nil {
		limit = clamp(int(num(p, "page_size")), 1, 100)
	}
	count, err := a.Store.DB.Query("SELECT COUNT(*) n FROM requests"+q.where, q.args...)
	if err != nil {
		return nil, err
	}
	args := append(q.args, limit, (page-1)*limit)
	rows, err := a.Store.DB.Query("SELECT * FROM requests"+q.where+" ORDER BY "+q.order+" LIMIT ? OFFSET ?", args...)
	return Object{"items": rows, "page": page, "page_size": limit, "total": count[0].Int("n"), "from": q.from, "to": q.to}, err
}

func buildRequestQuery(p Object, timestamp int64) (requestQuery, error) {
	// 毫秒精度下包含刚写入的当前毫秒，仍以半开区间保持整24小时。
	q := requestQuery{from: timestamp + 1 - requestDayMS, to: timestamp + 1}
	if err := q.timeRange(p); err != nil {
		return q, err
	}
	q.where = " WHERE started_at>=? AND started_at<?"
	q.args = []any{q.from, q.to}
	for _, key := range []string{"q", "status", "provider_id"} {
		if p[key] == nil {
			continue
		}
		v, ok := p[key].(string)
		if !ok || len(v) > 512 {
			return q, fmt.Errorf("%s 必须是至多512字节的文本", key)
		}
		if v == "" {
			continue
		}
		if key == "q" {
			q.where += " AND (model_id LIKE ? OR requested_model LIKE ? OR parent_id LIKE ? OR client_ip LIKE ?)"
			q.args = append(q.args, "%"+v+"%", "%"+v+"%", "%"+v+"%", "%"+v+"%")
		} else {
			q.where += " AND " + key + "=?"
			q.args = append(q.args, v)
		}
	}
	for _, column := range []string{"input_tokens", "output_tokens", "cost"} {
		if err := q.numericRange(p, column); err != nil {
			return q, err
		}
	}
	err := q.sort(p)
	return q, err
}

func (q *requestQuery) timeRange(p Object) error {
	if v, exists := p["all_time"]; exists && v != false {
		return fmt.Errorf("不允许查询全部时间，单次最多30天")
	}
	if p["from"] != nil || p["to"] != nil {
		for _, key := range []string{"from", "to"} {
			if _, text := p[key].(string); text {
				return fmt.Errorf("%s 必须是整数毫秒数值，不能是文本", key)
			}
		}
		var err error
		if q.from, err = requestInteger(p["from"]); err != nil {
			return fmt.Errorf("起始时间必须是非负整数毫秒，且与结束时间同时提供")
		}
		if q.to, err = requestInteger(p["to"]); err != nil {
			return fmt.Errorf("结束时间必须是非负整数毫秒，且与起始时间同时提供")
		}
	}
	if q.from >= q.to || q.to-q.from > 30*requestDayMS {
		return fmt.Errorf("起始时间必须早于结束时间，时间跨度最多30天")
	}
	return nil
}

func knownRequestTokens(column string) string {
	// 历史记录未单独保存 Token 已知性；仅报告金额时不能把默认的零猜成真实用量。
	return "(usage_mode IN ('reported_tokens','" + repricedMode + "') OR (usage_mode='reported_cost' AND " + column + ">0))"
}

func (q *requestQuery) numericRange(p Object, field string) error {
	column, known, parse := field, knownRequestTokens(field), requestInteger
	if field == "cost" {
		column, known, parse = "cost_nano", knownRequestCost, requestCost
	}
	var values [2]int64
	var present [2]bool
	for i, prefix := range []string{"min_", "max_"} {
		v := p[prefix+field]
		if v == nil || v == "" {
			continue
		}
		n, err := parse(v)
		if err != nil {
			return fmt.Errorf("%s%s: %w", prefix, field, err)
		}
		values[i], present[i] = n, true
		q.where += " AND " + column + []string{">=?", "<=?"}[i]
		q.args = append(q.args, n)
	}
	if present[0] && present[1] && values[0] > values[1] {
		return fmt.Errorf("%s 最小值不能大于最大值", field)
	}
	if present[0] || present[1] {
		q.where += " AND " + known
	}
	return nil
}

func (q *requestQuery) sort(p Object) error {
	sort := "newest"
	if p["sort"] != nil {
		var ok bool
		if sort, ok = p["sort"].(string); !ok {
			return fmt.Errorf("排序必须是文本")
		}
	}
	q.order = "started_at DESC, id DESC"
	column, known := "", ""
	switch sort {
	case "", "newest":
		return nil
	case "input_desc":
		column, known = "input_tokens", knownRequestTokens("input_tokens")
	case "output_desc":
		column, known = "output_tokens", knownRequestTokens("output_tokens")
	case "cost_desc":
		column, known = "cost_nano", knownRequestCost
	default:
		return fmt.Errorf("排序必须是 newest / input_desc / output_desc / cost_desc")
	}
	q.order = "CASE WHEN " + known + " THEN 0 ELSE 1 END, CASE WHEN " + known + " THEN " + column + " END DESC, " + q.order
	return nil
}

func requestInteger(v any) (int64, error) {
	var s string
	switch n := v.(type) {
	case string:
		s = n
	case json.Number:
		s = string(n)
	case int:
		s = strconv.Itoa(n)
	case int64:
		s = strconv.FormatInt(n, 10)
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return 0, fmt.Errorf("必须是非负安全整数")
		}
		s = strconv.FormatFloat(n, 'f', 0, 64)
	default:
		return 0, fmt.Errorf("必须是非负安全整数")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if !decimalDigits(s) || err != nil || n > 9007199254740991 {
		return 0, fmt.Errorf("必须是0至9007199254740991之间的整数")
	}
	return n, nil
}

func decimalDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) == -1
}

func requestCost(v any) (int64, error) {
	s, ok := v.(string)
	if !ok || len(s) > 32 {
		return 0, fmt.Errorf("USD金额必须是十进制字符串，至多9位小数")
	}
	whole, fraction, dot := strings.Cut(s, ".")
	if !decimalDigits(whole) || dot && (!decimalDigits(fraction) || len(fraction) > 9) {
		return 0, fmt.Errorf("USD金额必须非负，至多9位小数")
	}
	// 直接补齐十进制小数转为纳美元，不经过浮点数，保留筛选边界的精度。
	n, err := strconv.ParseInt(whole+fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("USD金额超出可表示范围")
	}
	return n, nil
}
