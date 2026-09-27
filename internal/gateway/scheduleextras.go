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
	text := fmt.Sprintf("花费异常\n今日已记录花费 $%.2f，是前 7 个完整日均值 $%.2f 的 %.1f 倍（阈值 %.1f 倍）。",
		dollars(todayCost), dollars(int64(average)), float64(todayCost)/average, c.SpendMultiple)
	if todayUnpriced+pastUnpriced > 0 {
		text += fmt.Sprintf("\n今日 %d 次、前 7 日 %d 次成功请求价格未确认。", todayUnpriced, pastUnpriced)
	}
	text += "\n已记录花费可能含本地估算与异常预留，不代表供应商实际账单。"
	if err = a.notify(text); err != nil {
		return "", err
	}
	a.markWarned(key, today.AddDate(0, 0, 1).UnixMilli())
	return text, nil
}

// taskFailureRate 按精确分钟窗口统计已结束的真实请求，达到阈值后进入可配置冷却。
func (a *App) taskFailureRate(c scheduleConfig) (string, error) {
	end := now()
	rows, err := a.Store.DB.Query(`SELECT COUNT(*) total,
		COALESCE(SUM(status='error'),0) errors, COALESCE(SUM(status='unknown'),0) unknown
		FROM requests WHERE started_at>=? AND started_at<? AND status IN ('success','error','unknown') AND is_demo=0`,
		end-int64(c.FailureMinutes)*60000, end)
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
	text := fmt.Sprintf("失败率告警\n最近 %d 分钟 %d/%d 次失败（%.1f%%，阈值 %.1f%%）。\n错误 %d 次，状态未知 %d 次。",
		c.FailureMinutes, failed, total, rate, c.FailurePercent, errors, unknown)
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
	lines, keys := []string{}, []string{}
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
			line := fmt.Sprintf("「%s」%s $%.2f / $%.2f（%.1f%%）", key.String("name"), window.label, window.spend, window.limit, percent)
			if unpriced > 0 {
				line += fmt.Sprintf("；另有 %d 次成功请求价格未确认", unpriced)
			}
			line += "；汇总金额可能含本地估算与异常预留，明细仅覆盖保留期"
			lines, keys = append(lines, line), append(keys, warnKey)
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	text := "Key 预算预警\n" + strings.Join(lines, "\n")
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
