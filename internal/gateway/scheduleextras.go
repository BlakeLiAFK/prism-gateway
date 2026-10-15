package gateway

import (
	"fmt"
	"strings"
	"time"
)

// taskSpendAnomaly 比较今日累计已知花费与前七个完整自然日的日均值。
func (a *App) taskSpendAnomaly(c scheduleConfig) (string, error) {
	local := time.Now().In(c.zone())
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.zone())
	read := func(from, to time.Time) (int64, int64, error) {
		rows, err := a.Store.DB.Query(`SELECT
			COALESCE(SUM(cost_nano),0) cost, COALESCE(SUM(unpriced),0) unpriced
			FROM usage_hourly WHERE hour>=? AND hour<?`, from.UnixMilli()/3600000, to.UnixMilli()/3600000)
		if err != nil {
			return 0, 0, err
		}
		return rows[0].Int("cost"), rows[0].Int("unpriced"), nil
	}
	// 当前小时已有完成请求时也要计入，因此上界取下一整点。
	todayCost, todayUnpriced, err := read(today, local.Truncate(time.Hour).Add(time.Hour))
	if err != nil {
		return "", err
	}
	pastCost, pastUnpriced, err := read(today.AddDate(0, 0, -7), today)
	if err != nil {
		return "", err
	}
	average := float64(pastCost) / 7
	if average <= 0 || float64(todayCost) < c.SpendMinimum*1e9 || float64(todayCost) < average*c.SpendMultiple {
		return "", nil
	}
	key := "spend:" + today.Format("2006-01-02")
	if a.warned(key) {
		return "", nil
	}
	lines := []string{
		fmt.Sprintf("统计日期：%s（%s）", today.Format("2006-01-02"), c.zone()),
		fmt.Sprintf("今日已记录花费：$%.2f", dollars(todayCost)),
		fmt.Sprintf("前 7 个完整自然日日均：$%.2f", dollars(int64(average))),
		fmt.Sprintf("当前倍数：%.1f 倍", float64(todayCost)/average),
		fmt.Sprintf("预警阈值：%.1f 倍，且今日不少于 $%.2f", c.SpendMultiple, c.SpendMinimum),
	}
	if todayUnpriced+pastUnpriced > 0 {
		lines = append(lines, fmt.Sprintf("价格未确认：今日 %d 次，前 7 日 %d 次成功请求", todayUnpriced, pastUnpriced))
	}
	lines = append(lines, "统计口径：已结束请求；金额可能含本地估算与异常预留，不代表供应商账单")
	text := notice("花费异常", strings.Join(lines, "\n"))
	if err = a.notify(text); err != nil {
		return "", err
	}
	a.markWarned(key, today.AddDate(0, 0, 1).UnixMilli())
	return text, nil
}

// taskFailureRate 按精确分钟窗口统计已结束的真实请求，达到阈值后进入可配置冷却。
// usage_mode='incompatible' 的记录没有到达任何模型（直接按模型调用、候选全部暂停等刻意不告警的 400），
// 不能算作上游失败，否则这些请求会绕过各自的告警规则、反过来推高失败率。
func (a *App) taskFailureRate(c scheduleConfig) (string, error) {
	end := now()
	start := end - int64(c.FailureMinutes)*60000
	rows, err := a.Store.DB.Query(`SELECT COUNT(*) total,
		COALESCE(SUM(status='error'),0) errors, COALESCE(SUM(status='unknown'),0) unknown
		FROM requests WHERE started_at>=? AND started_at<? AND status IN ('success','error','unknown') AND is_demo=0 AND usage_mode!='incompatible'`,
		start, end)
	if err != nil {
		return "", err
	}
	total, errors, unknown := rows[0].Int("total"), rows[0].Int("errors"), rows[0].Int("unknown")
	if total < int64(c.FailureMinSamples) {
		return "", nil
	}
	failed := errors + unknown
	rate := float64(failed) / float64(total) * 100
	if rate < c.FailurePercent || a.warned("failure-rate") {
		return "", nil
	}
	lines := []string{
		fmt.Sprintf("统计窗口：最近 %d 分钟", c.FailureMinutes),
		"开始时间：" + noticeTimeIn(start, c.zone()),
		"结束时间：" + noticeTimeIn(end, c.zone()),
		fmt.Sprintf("失败：%d / %d 次（%.1f%%）", failed, total, rate),
		fmt.Sprintf("预警阈值：%.1f%%", c.FailurePercent),
		fmt.Sprintf("失败构成：错误 %d 次，状态未知 %d 次", errors, unknown),
		"统计口径：已结束的真实请求，不包含演示请求和未进入上游的兼容性拒绝",
	}
	breakdown, err := a.Store.DB.Query(`SELECT provider_id p, model_id m, COUNT(*) n
		FROM requests WHERE started_at>=? AND started_at<? AND status IN ('error','unknown') AND is_demo=0 AND usage_mode!='incompatible'
		GROUP BY provider_id,model_id ORDER BY n DESC LIMIT 3`, start, end)
	if err != nil {
		return "", err
	}
	blocks := []string{strings.Join(lines, "\n")}
	if len(breakdown) > 0 {
		cfg := a.Store.Config()
		items := []string{"主要失败来源"}
		for _, row := range breakdown {
			provider, model := row.String("p"), row.String("m")
			if p, ok := cfg.provider(provider); ok {
				provider = noticeName(p.Name, p.ID)
			}
			if m, ok := cfg.model(model); ok {
				model = noticeName(m.Name, m.ID)
			}
			items = append(items, fmt.Sprintf("%s / %s：%d 次", noticeValue(provider), noticeValue(model), row.Int("n")))
		}
		blocks = append(blocks, strings.Join(items, "\n"))
	}
	text := notice("失败率告警", blocks...)
	if err = a.notify(text); err != nil {
		return "", err
	}
	a.markWarned("failure-rate", end+int64(c.FailureCooldownMinutes)*60000)
	return text, nil
}

// taskKeyBudget 复用 Key 限额的滚动 24 小时 / 30 天口径，在达到上限前提醒。
func (a *App) taskKeyBudget(c scheduleConfig) (string, error) {
	rows, err := a.Store.DB.Query(`SELECT id,name,limit_day,limit_month FROM api_keys
		WHERE enabled=1 AND (limit_day>0 OR limit_month>0) ORDER BY name`)
	if err != nil {
		return "", err
	}
	blocks, keys := []string{}, []string{}
	for _, key := range rows {
		id := key.String("id")
		day, month, err := a.Engine.keySpend(id)
		if err != nil {
			return "", err
		}
		for _, window := range []struct {
			label string
			days  int
			spend float64
			limit float64
		}{{"近 24 小时", 1, day, key.Float("limit_day")}, {"近 30 天", 30, month, key.Float("limit_month")}} {
			if window.limit <= 0 {
				continue
			}
			percent := window.spend / window.limit * 100
			warnKey := fmt.Sprintf("key-budget:%s:%dd:%g:%d", id, window.days, window.limit, c.KeyBudgetPercent)
			if percent < float64(c.KeyBudgetPercent) || a.warned(warnKey) {
				continue
			}
			unpriced, err := a.keyBudgetUnpriced(id, window.days)
			if err != nil {
				return "", err
			}
			lines := []string{
				"网关 Key：" + noticeName(key.String("name"), id),
				"统计窗口：" + window.label,
				fmt.Sprintf("已记录花费：$%.2f", window.spend),
				fmt.Sprintf("预算上限：$%.2f", window.limit),
				fmt.Sprintf("使用比例：%.1f%%（预警阈值 %d%%）", percent, c.KeyBudgetPercent),
			}
			if unpriced > 0 {
				lines = append(lines, fmt.Sprintf("价格未确认：%d 次成功请求", unpriced))
			}
			lines = append(lines, "统计口径：滚动窗口；金额可能含本地估算与异常预留，明细仅覆盖保留期")
			blocks, keys = append(blocks, strings.Join(lines, "\n")), append(keys, warnKey)
		}
	}
	if len(blocks) == 0 {
		return "", nil
	}
	text := notice("Key 预算预警", blocks...)
	if err = a.notify(text); err != nil {
		return "", err
	}
	for _, key := range keys {
		a.markWarned(key, now()+int64(c.KeyBudgetCooldownMinutes)*60000)
	}
	return text, nil
}

func (a *App) keyBudgetUnpriced(id string, days int) (int64, error) {
	rows, err := a.Store.DB.Query(`SELECT COALESCE(SUM(unpriced),0) unpriced
		FROM usage_hourly WHERE key_id=? AND hour>=?`, id, sinceHour(days))
	if err != nil {
		return 0, err
	}
	return rows[0].Int("unpriced"), nil
}
