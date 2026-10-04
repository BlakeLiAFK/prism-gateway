package gateway

// 把引擎各处发生的事翻译成监控事件。这里只做格式化与发布，不碰任何业务状态。

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// watchAuditEvery 是审计表轮询间隔，测试里会缩短
var watchAuditEvery = time.Second

func watchModelName(m Model) string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// noteDone 记一次请求结束：成功、失败或客户端取消，并计入趋势
func (e *Engine) noteDone(s selection, requested string, r settled, u Usage, start int64) {
	name := watchModelName(s.Model)
	if requested != s.Model.ID {
		name = requested + " -> " + name
	}
	dur := fmtDur(now() - start)
	switch {
	case r.state == "success":
		mode := "native"
		if s.Cross {
			mode = "converted"
		}
		e.watch.publish("OK", fmt.Sprintf("%s  %s  %d tok  %s", name, dur, u.Output, mode))
		e.watch.countRequest(false)
	case r.code == "CLIENT_CANCELED":
		e.watch.publish("CXL", fmt.Sprintf("%s  client canceled  %s", name, dur))
	default:
		e.watch.publish("FAIL", fmt.Sprintf("%s  %s  %s", name, r.code, dur))
		e.watch.countRequest(true)
	}
}

// noteRejected 记一次没有任何候选能处理的请求（淘汰或准入被拒）
func (e *Engine) noteRejected(requested, code, detail string) {
	e.watch.publish("REJ", fmt.Sprintf("%s  %s  %s", requested, code, detail))
	e.watch.countRequest(true)
}

func (e *Engine) noteCooldown(id string, d time.Duration) {
	name := id
	if m, ok := e.store.Config().model(id); ok {
		name = watchModelName(m)
	}
	e.watch.publish("COOL", fmt.Sprintf("%s  cooldown %s", name, fmtDur(min(d, 24*time.Hour).Milliseconds())))
}

// pollAudit 把审计表里的新记录推给监控：配置变更与各种管理操作都写审计表，
// 按它推送就不必在每个操作里逐个接线。只在有人观看时查库；没人看时只推进游标，
// 所以有人连上时不会补推此前积压的记录。
func (a *App) pollAudit(ctx context.Context) {
	cursor := now()
	tick := time.NewTicker(watchAuditEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if a.Engine.watch.watchers() == 0 {
			cursor = now()
			continue
		}
		rows, err := a.Store.DB.Query("SELECT action,target,version,created_at FROM audit_logs WHERE created_at>? ORDER BY created_at", cursor)
		if err != nil {
			slog.Error("watch audit poll failed", "err", err)
			continue
		}
		for _, r := range rows {
			a.Engine.watch.publish("ADMIN", fmt.Sprintf("%s  %s  cfg v%d", r.String("action"), r.String("target"), r.Int("version")))
			cursor = max(cursor, r.Int("created_at"))
		}
	}
}
