package gateway

// 定时任务：统一调度自动备份、额度预警、每日日报、Key 到期提醒、数据库维护与上游检查。
// 配置存 meta.schedule_config，界面修改后下一分钟生效；运行记录与推送去重键存 meta.schedule_state，
// 交接部署重启后每日任务不会重跑、同一件事不会重复推送。
// 任务在调度协程里串行执行，失败只记录不重试，等下一个周期；只有产生动作或失败的运行才写入 jobs 表，
// 否则每 10 分钟一次的额度检查会把模型同步等任务挤出任务列表。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type scheduleConfig struct {
	UTCOffset       int  `json:"utc_offset"`
	BackupEnabled   bool `json:"backup_enabled"`
	BackupHour      int  `json:"backup_hour"`
	BackupKeep      int  `json:"backup_keep"`
	QuotaEnabled    bool `json:"quota_enabled"`
	QuotaPercent    int  `json:"quota_percent"`
	QuotaMinutes    int  `json:"quota_minutes"`
	ReportEnabled   bool `json:"report_enabled"`
	ReportHour      int  `json:"report_hour"`
	ExpiryEnabled   bool `json:"expiry_enabled"`
	ExpiryDays      int  `json:"expiry_days"`
	MaintainEnabled bool `json:"maintain_enabled"`
	MaintainHour    int  `json:"maintain_hour"`
	UpstreamEnabled bool `json:"upstream_enabled"`
	UpstreamHours   int  `json:"upstream_hours"`
}

// 自动备份与每日日报默认关闭，由管理员自己开启
var defaultSchedule = scheduleConfig{UTCOffset: 8, BackupHour: 4, BackupKeep: 7, QuotaEnabled: true, QuotaPercent: 80, QuotaMinutes: 10,
	ReportHour: 9, ExpiryEnabled: true, ExpiryDays: 3, MaintainEnabled: true, MaintainHour: 5, UpstreamEnabled: true, UpstreamHours: 6}

type taskRun struct {
	At     int64  `json:"at"`
	Status string `json:"status"` // succeeded | failed | skipped（推送通道未启用）
	Result string `json:"result"`
}

type scheduleState struct {
	Runs   map[string]taskRun `json:"runs"`
	Warned map[string]int64   `json:"warned"` // 推送去重键 → 失效时刻
}

type scheduler struct {
	mu    sync.Mutex // 串行化所有任务运行与状态读写，手动运行不会与调度撞车
	state scheduleState
}

type scheduledTask struct {
	id, name string
	due      func(c scheduleConfig, last, t time.Time) bool
	run      func(a *App, c scheduleConfig) (string, error)
}

var scheduledTasks = []scheduledTask{
	{"backup", "自动备份", func(c scheduleConfig, last, t time.Time) bool {
		return c.BackupEnabled && dailyDue(c, c.BackupHour, last, t)
	}, (*App).taskBackup},
	{"quota", "额度预警", func(c scheduleConfig, last, t time.Time) bool {
		return c.QuotaEnabled && t.Sub(last) >= time.Duration(c.QuotaMinutes)*time.Minute
	}, (*App).taskQuota},
	{"report", "每日日报", func(c scheduleConfig, last, t time.Time) bool {
		return c.ReportEnabled && dailyDue(c, c.ReportHour, last, t)
	}, (*App).taskReport},
	{"expiry", "Key 到期提醒", func(c scheduleConfig, last, t time.Time) bool { return c.ExpiryEnabled && t.Sub(last) >= time.Hour }, (*App).taskExpiry},
	{"maintain", "数据库维护", func(c scheduleConfig, last, t time.Time) bool {
		return c.MaintainEnabled && dailyDue(c, c.MaintainHour, last, t)
	}, (*App).taskMaintain},
	{"upstream", "上游检查", func(c scheduleConfig, last, t time.Time) bool {
		return c.UpstreamEnabled && t.Sub(last) >= time.Duration(c.UpstreamHours)*time.Hour
	}, (*App).taskUpstream},
}

// 这些任务的结论只在内存里（如下架清单），进程启动后要先跑一次，不沿用上次运行时间
var runOnStart = map[string]bool{"quota": true, "upstream": true}

func init() {
	consoleActions["schedule.get"] = func(a *App, _ string, _ Object) (any, error) { return a.scheduleView() }
	consoleActions["schedule.save"] = func(a *App, _ string, p Object) (any, error) { return a.saveSchedule(p) }
	consoleActions["schedule.run"] = func(a *App, _ string, p Object) (any, error) {
		for _, t := range scheduledTasks {
			if t.id == str(p, "task") {
				c, err := a.loadSchedule()
				if err != nil {
					return nil, err
				}
				a.audit("schedule.run", t.id)
				return a.runTask(t, c), nil
			}
		}
		return nil, fail("INVALID_PARAMS", "未知的定时任务", 400)
	}
}

func (c scheduleConfig) zone() *time.Location {
	return time.FixedZone(fmt.Sprintf("UTC%+d", c.UTCOffset), c.UTCOffset*3600)
}

// dailyDue：本地时间已过设定时刻，且今天还没跑过。进程在设定时刻停着也会在启动后补跑。
func dailyDue(c scheduleConfig, hour int, last, t time.Time) bool {
	t, last = t.In(c.zone()), last.In(c.zone())
	return t.Hour() >= hour && (last.YearDay() != t.YearDay() || last.Year() != t.Year())
}

func (c scheduleConfig) validate() error {
	hours := []int{c.BackupHour, c.ReportHour, c.MaintainHour}
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fail("INVALID_PARAMS", "执行时刻应在 0–23 点之间", 400)
		}
	}
	switch {
	case c.UTCOffset < -12 || c.UTCOffset > 14:
		return fail("INVALID_PARAMS", "时区偏移应在 -12 到 +14 之间", 400)
	case c.BackupKeep < 1 || c.BackupKeep > 365:
		return fail("INVALID_PARAMS", "备份保留份数应在 1–365 之间", 400)
	case c.QuotaPercent < 1 || c.QuotaPercent > 100:
		return fail("INVALID_PARAMS", "额度预警阈值应在 1–100% 之间", 400)
	case c.QuotaMinutes < 5 || c.QuotaMinutes > 1440:
		return fail("INVALID_PARAMS", "额度检查间隔应在 5–1440 分钟之间", 400)
	case c.ExpiryDays < 1 || c.ExpiryDays > 90:
		return fail("INVALID_PARAMS", "到期提醒应提前 1–90 天", 400)
	case c.UpstreamHours < 1 || c.UpstreamHours > 168:
		return fail("INVALID_PARAMS", "上游检查间隔应在 1–168 小时之间", 400)
	}
	return nil
}

func (a *App) loadSchedule() (scheduleConfig, error) {
	c := defaultSchedule
	rows, err := a.Store.DB.Query("SELECT value FROM meta WHERE key='schedule_config'")
	if err != nil || len(rows) == 0 {
		return c, err
	}
	return c, json.Unmarshal([]byte(rows[0].String("value")), &c)
}

func (a *App) saveSchedule(p Object) (any, error) {
	c, err := a.loadSchedule()
	if err != nil {
		return nil, err
	}
	// 在现有配置上覆盖提交的字段；类型不对（如时刻传了字符串）直接拒绝
	if err = json.Unmarshal([]byte(raw(p)), &c); err != nil {
		return nil, fail("INVALID_PARAMS", "定时任务参数格式不正确", 400)
	}
	if err = c.validate(); err != nil {
		return nil, err
	}
	if err = a.Store.DB.Exec("INSERT INTO meta VALUES ('schedule_config', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", raw(c)); err != nil {
		return nil, err
	}
	a.audit("schedule.save", "")
	return a.scheduleView()
}

// scheduleView 返回配置、每项最近一次运行结果，以及推送通道是否可用
func (a *App) scheduleView() (any, error) {
	c, err := a.loadSchedule()
	if err != nil {
		return nil, err
	}
	ac, err := a.loadAlert()
	if err != nil {
		return nil, err
	}
	// 不拿调度锁：任务可能正在跑网络请求，读库里的状态即可
	st, err := a.readScheduleState()
	if err != nil {
		return nil, err
	}
	return Object{"config": c, "runs": st.Runs, "channel": ac.Enabled && ac.Secret != "", "zone": c.zone().String()}, nil
}

func (a *App) readScheduleState() (scheduleState, error) {
	st := scheduleState{Runs: map[string]taskRun{}, Warned: map[string]int64{}}
	rows, err := a.Store.DB.Query("SELECT value FROM meta WHERE key='schedule_state'")
	if err != nil || len(rows) == 0 {
		return st, err
	}
	err = json.Unmarshal([]byte(rows[0].String("value")), &st)
	if st.Runs == nil {
		st.Runs = map[string]taskRun{}
	}
	if st.Warned == nil {
		st.Warned = map[string]int64{}
	}
	return st, err
}

// loadScheduleState 需持有 a.sched.mu；每次从库里读，状态以数据库为准
func (a *App) loadScheduleState() (err error) {
	a.sched.state, err = a.readScheduleState()
	return err
}

// saveScheduleState 需持有 a.sched.mu；顺带清掉已失效的去重键
func (a *App) saveScheduleState() {
	for k, until := range a.sched.state.Warned {
		if until < now() {
			delete(a.sched.state.Warned, k)
		}
	}
	if err := a.Store.DB.Exec("INSERT INTO meta VALUES ('schedule_state', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", raw(a.sched.state)); err != nil {
		slog.Error("schedule state write failed", "err", err)
	}
}

// warned / markWarned 在任务运行期间调用（已持有 a.sched.mu）
func (a *App) warned(key string) bool { return a.sched.state.Warned[key] > now() }
func (a *App) markWarned(key string, until int64) {
	a.sched.state.Warned[key] = until
}

// runSchedule 每分钟检查一次到期任务
func (a *App) runSchedule(ctx context.Context) {
	a.resetOnStart()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.scheduleTick(time.Now())
		}
	}
}

// resetOnStart 清掉只在内存里保存结论的任务的上次运行时间，让它们启动后第一轮就跑
func (a *App) resetOnStart() {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	if err := a.loadScheduleState(); err != nil {
		slog.Error("schedule state load failed", "err", err)
	}
	for id := range runOnStart {
		delete(a.sched.state.Runs, id)
	}
	// 必须落库：每次运行任务都会从库里重读状态，只删内存的话会被同一轮里先跑的任务读回来
	a.saveScheduleState()
}

func (a *App) scheduleTick(t time.Time) {
	c, err := a.loadSchedule()
	if err != nil {
		slog.Error("schedule config load failed", "err", err)
		return
	}
	for _, task := range scheduledTasks {
		a.sched.mu.Lock()
		last := time.UnixMilli(a.sched.state.Runs[task.id].At)
		a.sched.mu.Unlock()
		if task.due(c, last, t) {
			a.runTask(task, c)
		}
	}
}

// runTask 执行一次任务并记录结果；失败时尝试推送一条通知（推送本身失败只记日志）
func (a *App) runTask(task scheduledTask, c scheduleConfig) taskRun {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	if err := a.loadScheduleState(); err != nil {
		return taskRun{At: now(), Status: "failed", Result: err.Error()}
	}
	result, err := task.run(a, c)
	run := taskRun{At: now(), Status: "succeeded", Result: result}
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == "ALERT_DISABLED" {
		// 有发现但推送通道没开：结论照样留在卡片上，不算失败、不写任务列表，
		// 否则每 10 分钟一条失败记录会把任务列表淹没
		run.Status, run.Result = "skipped", err.Error()
	} else if err != nil {
		run.Status, run.Result = "failed", err.Error()
		slog.Warn("scheduled task failed", "task", task.id, "err", err)
		if task.id != "report" && task.id != "quota" && task.id != "expiry" {
			if e := a.notify(task.name + "失败：" + err.Error()); e != nil {
				slog.Debug("schedule failure notice not sent", "err", e)
			}
		}
	}
	a.sched.state.Runs[task.id] = run
	a.saveScheduleState()
	if run.Status == "failed" || (run.Status == "succeeded" && result != "") {
		a.recordJob("schedule."+task.id, run)
	}
	return run
}

func (a *App) recordJob(action string, run taskRun) {
	res, msg := any(nil), any(nil)
	if run.Status == "failed" {
		msg = run.Result
	} else {
		res = run.Result
	}
	if err := a.Store.DB.Exec("INSERT INTO jobs(id,action,status,result,error,created_at,updated_at) VALUES (?,?,?,?,?,?,?)",
		randomID("job_"), action, run.Status, res, msg, run.At, run.At); err != nil {
		slog.Error("schedule job record failed", "action", action, "err", err)
	}
}
