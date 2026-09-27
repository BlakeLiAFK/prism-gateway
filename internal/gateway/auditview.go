package gateway

import "strings"

func init() {
	consoleActions["audit.search"] = func(a *App, _ string, p Object) (any, error) {
		return a.auditSearch(p)
	}
}

func (a *App) auditSearch(p Object) (any, error) {
	page := clamp(int(num(p, "page")), 1, 10000)
	limit := clamp(int(num(p, "page_size")), 1, 100)
	if p["page_size"] == nil {
		limit = 30
	}

	q := strings.TrimSpace(str(p, "q"))
	action := strings.TrimSpace(str(p, "action"))
	target := strings.TrimSpace(str(p, "target"))
	if len(q) > 120 || len(action) > 120 || len(target) > 240 {
		return nil, fail("INVALID_PARAMS", "审计筛选条件过长", 400)
	}
	from, to := int64(num(p, "from")), int64(num(p, "to"))
	const maxTime = int64(253402300799999)
	if from < 0 || to < 0 || from > maxTime || to > maxTime || from > 0 && to > 0 && from > to {
		return nil, fail("INVALID_PARAMS", "审计时间范围无效", 400)
	}

	where := ` WHERE (?='' OR action LIKE ? OR target LIKE ?)
		AND (?='' OR action LIKE ?)
		AND (?='' OR target LIKE ?)
		AND (?=0 OR created_at>=?)
		AND (?=0 OR created_at<=?)`
	args := []any{q, "%" + q + "%", "%" + q + "%", action, "%" + action + "%", target, "%" + target + "%", from, from, to, to}
	count, err := a.Store.DB.Query("SELECT COUNT(*) n FROM audit_logs"+where, args...)
	if err != nil {
		return nil, err
	}
	total := count[0].Int("n")
	args = append(args, limit, (page-1)*limit)
	rows, err := a.Store.DB.Query("SELECT * FROM audit_logs"+where+" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	return Object{"items": rows, "page": page, "page_size": limit, "total": total, "pages": (total + int64(limit) - 1) / int64(limit)}, nil
}
