package gateway

import (
	"fmt"
	"time"
)

// taskSpendAnomaly 比较今日累计已知花费与前七个完整自然日的日均值。
func (a *App) taskSpendAnomaly(c scheduleConfig) (string, error) {
	local := time.Now().In(c.zone())
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.zone())
	read := func(from, to time.Time) (int64, int64, error) {
		rows, err := a.Store.DB.Query(`SELECT
			COALESCE(SUM(CASE WHEN status!='running' AND cost_known=1 AND is_demo=0 THEN cost_nano ELSE 0 END),0) cost,
			COALESCE(SUM(CASE WHEN status IN ('success','unknown') AND cost_known=0 AND is_demo=0 THEN 1 ELSE 0 END),0) unknown
			FROM requests WHERE started_at>=? AND started_at<?`, from.UnixMilli(), to.UnixMilli())
		if err != nil {
			return 0, 0, err
		}
		return rows[0].Int("cost"), rows[0].Int("unknown"), nil
	}
	todayCost, todayUnknown, err := read(today, local)
	if err != nil {
		return "", err
	}
	pastCost, pastUnknown, err := read(today.AddDate(0, 0, -7), today)
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
	text := fmt.Sprintf("花费异常\n今日已知花费 $%.2f，是前 7 个完整日均值 $%.2f 的 %.1f 倍（阈值 %.1f 倍）。",
		dollars(todayCost), dollars(int64(average)), float64(todayCost)/average, c.SpendMultiple)
	if todayUnknown+pastUnknown > 0 {
		text += fmt.Sprintf("\n今日 %d 次、前 7 日 %d 次费用未知，实际花费可能更高。", todayUnknown, pastUnknown)
	} else {
		text += "\n今日与前 7 日均无未知费用。"
	}
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
