package gateway

// 定时任务的具体实现。返回值是本次运行的结论；间隔任务无事可做时返回空串，不写入任务列表。
// 推送都经 notify 走「告警推送」里配置的通道，去重键记在 schedule_state，推送成功后才记。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// taskBackup 生成一份自动快照，只轮转自动备份，手动备份不受影响
func (a *App) taskBackup(c scheduleConfig) (string, error) {
	path, size, err := a.Store.Backup("auto-")
	if err != nil {
		return "", err
	}
	removed, err := pruneBackups(a.Store.backupDir(), "gateway-auto-", c.BackupKeep)
	if err != nil {
		return "", fmt.Errorf("已生成 %s，但清理旧备份失败：%w", filepath.Base(path), err)
	}
	return fmt.Sprintf("已生成 %s（%.1f MB），清理旧的自动备份 %d 份", filepath.Base(path), float64(size)/(1<<20), removed), nil
}

// pruneBackups 按文件名（含时间戳）倒序保留最新 keep 份
func pruneBackups(dir, prefix string, keep int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	removed := 0
	for _, n := range names[min(keep, len(names)):] {
		if err = os.Remove(filepath.Join(dir, n)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// taskQuota 查 Command Code / Z.AI 的窗口额度，用量达到阈值时推送；同一窗口同一重置周期只推一次
func (a *App) taskQuota(c scheduleConfig) (string, error) {
	lines, keys, resets := []string{}, []string{}, []int64{}
	for _, p := range a.Store.Config().Providers {
		if !p.Enabled || (p.Kind != "commandcode" && p.Kind != "zai") {
			continue
		}
		u := a.providerUsage(a.Context, p)
		for _, v := range arr(u["windows"]) {
			w := obj(v)
			pct, reset := num(w, "percent"), int64(num(w, "reset_at"))
			if pct < float64(c.QuotaPercent) {
				continue
			}
			key := fmt.Sprintf("quota:%s:%s:%d", p.ID, str(w, "label"), reset)
			if a.warned(key) {
				continue
			}
			line := fmt.Sprintf("%s 的%s窗口已用 %.0f%%", p.Name, str(w, "label"), pct)
			if reset > now() {
				line += "，" + durationText(reset-now()) + "后重置"
			}
			lines, keys, resets = append(lines, line), append(keys, key), append(resets, reset)
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	if err := a.notify("额度预警\n" + strings.Join(lines, "\n")); err != nil {
		return "", err
	}
	for i, k := range keys {
		// 拿不到重置时刻时，一周内不再重复提醒
		a.markWarned(k, max(resets[i], now()+7*86400000))
	}
	return "已推送：" + strings.Join(lines, "；"), nil
}

// taskReport 汇总昨天（按设定时区）的用量推送出去
func (a *App) taskReport(c scheduleConfig) (string, error) {
	text, err := a.dailyReport(c, time.Now())
	if err != nil {
		return "", err
	}
	if err = a.notify(text); err != nil {
		return "", err
	}
	return text, nil
}

func (a *App) dailyReport(c scheduleConfig, t time.Time) (string, error) {
	local := t.In(c.zone())
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.zone())
	from, to := today.AddDate(0, 0, -1).UnixMilli()/3600000, today.UnixMilli()/3600000
	q := func(sql string) ([]Object, error) {
		rows, err := a.Store.DB.Query(sql, from, to)
		out := make([]Object, len(rows))
		for i, r := range rows {
			out[i] = Object(r)
		}
		return out, err
	}
	sum, err := q(`SELECT COALESCE(SUM(requests),0) r, COALESCE(SUM(success),0) s, COALESCE(SUM(errors),0) e, COALESCE(SUM(cost_nano),0) c,
		COALESCE(SUM(input_tokens+output_tokens),0) t, COALESCE(SUM(unpriced),0) u FROM usage_hourly WHERE hour>=? AND hour<?`)
	if err != nil {
		return "", err
	}
	s := sum[0]
	lines := []string{fmt.Sprintf("日报 %s（%s）", today.AddDate(0, 0, -1).Format("2006-01-02"), c.zone())}
	if num(s, "r") == 0 {
		return strings.Join(append(lines, "昨日没有请求。"), "\n"), nil
	}
	lines = append(lines, fmt.Sprintf("请求 %.0f 次，成功率 %.1f%%，失败 %.0f 次", num(s, "r"), num(s, "s")/num(s, "r")*100, num(s, "e")),
		fmt.Sprintf("Token %s，估算花费 $%.2f", compactCount(num(s, "t")), dollars(int64(num(s, "c")))))
	if u := num(s, "u"); u > 0 {
		lines[len(lines)-1] += fmt.Sprintf("（%.0f 次未计价）", u)
	}
	sections := []struct{ title, sql, format string }{
		{"常用模型", `SELECT model_id n, SUM(requests) v FROM usage_hourly WHERE hour>=? AND hour<? AND model_id!='' GROUP BY model_id ORDER BY v DESC LIMIT 3`, "%s %.0f 次"},
		{"花费最多的 Key", `SELECT COALESCE(k.name, u.key_id) n, SUM(u.cost_nano)/1e9 v FROM usage_hourly u LEFT JOIN api_keys k ON k.id=u.key_id
			WHERE u.hour>=? AND u.hour<? GROUP BY u.key_id HAVING v>0 ORDER BY v DESC LIMIT 3`, "%s $%.2f"},
		{"失败最多", `SELECT model_id n, SUM(errors) v FROM usage_hourly WHERE hour>=? AND hour<? AND model_id!='' GROUP BY model_id HAVING v>0 ORDER BY v DESC LIMIT 3`, "%s %.0f 次"},
	}
	for _, sec := range sections {
		rows, err := q(sec.sql)
		if err != nil {
			return "", err
		}
		items := []string{}
		for _, r := range rows {
			items = append(items, fmt.Sprintf(sec.format, str(r, "n"), num(r, "v")))
		}
		if len(items) > 0 {
			lines = append(lines, sec.title+"："+strings.Join(items, " · "))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// compactCount 把 12345678 写成 12.3M
func compactCount(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.1fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fK", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

// taskExpiry 提醒 N 天内到期的 Key；同一个 Key 的同一个到期时间只提醒一次
func (a *App) taskExpiry(c scheduleConfig) (string, error) {
	rows, err := a.Store.DB.Query("SELECT id, name, expires_at FROM api_keys WHERE enabled=1 AND expires_at>? AND expires_at<=? ORDER BY expires_at",
		now(), now()+int64(c.ExpiryDays)*86400000)
	if err != nil {
		return "", err
	}
	lines, keys, untils := []string{}, []string{}, []int64{}
	for _, r := range rows {
		at := r.Int("expires_at")
		key := fmt.Sprintf("expiry:%s:%d", r.String("id"), at)
		if a.warned(key) {
			continue
		}
		lines = append(lines, fmt.Sprintf("「%s」将于 %s 到期（还剩 %s）", r.String("name"),
			time.UnixMilli(at).In(c.zone()).Format("01-02 15:04"), durationText(at-now())))
		keys, untils = append(keys, key), append(untils, at)
	}
	if len(lines) == 0 {
		return "", nil
	}
	if err = a.notify("网关 Key 即将到期\n" + strings.Join(lines, "\n")); err != nil {
		return "", err
	}
	for i, k := range keys {
		a.markWarned(k, untils[i])
	}
	return "已提醒：" + strings.Join(lines, "；"), nil
}

// taskMaintain 更新查询规划统计，并把 WAL 合并回主库后截断
func (a *App) taskMaintain(scheduleConfig) (string, error) {
	if err := a.Store.DB.Exec("PRAGMA optimize"); err != nil {
		return "", err
	}
	rows, err := a.Store.DB.Query("PRAGMA wal_checkpoint(TRUNCATE)")
	if err != nil {
		return "", err
	}
	// 返回 busy / log / checkpointed：busy=1 表示有读者占用，本次没能截断，下次再来
	if len(rows) > 0 && rows[0].Int("busy") != 0 {
		return "已更新查询统计；WAL 正被读取，本次未能截断", nil
	}
	return "已更新查询统计，WAL 已合并并截断", nil
}

// taskUpstream 对照上游模型列表；每条发现推送成功后记入去重，推送失败的下次还会再推。
// 同一条下架提醒 30 天后重复一次；价格变动按「模型 + 上游价格」去重，上游再改价会再提醒。
func (a *App) taskUpstream(scheduleConfig) (string, error) {
	lines, keys := []string{}, []string{}
	for _, f := range a.checkUpstreamModels(a.Context) {
		if !a.warned(f.key) {
			lines, keys = append(lines, f.text), append(keys, f.key)
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	if err := a.notify("上游模型变动\n" + strings.Join(lines, "\n")); err != nil {
		// 清单已刷新，只是没推出去；界面上照样能看到
		return "", fmt.Errorf("发现 %d 项变动，推送失败：%w", len(lines), err)
	}
	for _, k := range keys {
		a.markWarned(k, now()+30*86400000)
	}
	return "已推送：" + strings.Join(lines, "；"), nil
}
