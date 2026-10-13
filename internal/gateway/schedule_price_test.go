package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"prism-gateway/internal/webui"
)

func scheduleConfigRPC(t *testing.T, h *harness) Object {
	t.Helper()
	w, out := h.rpc(t, "schedule.get", Object{}, h.token)
	requireStatus(t, w, http.StatusOK)
	return obj(obj(out["data"])["config"])
}

func TestPriceScheduleDefaultAndLegacyMigration(t *testing.T) {
	h := newHarness(t)
	c := scheduleConfigRPC(t, h)
	if c["price_enabled"] != true || num(c, "price_hours") != 1 || num(c, "price_sync_version") != 1 {
		t.Fatalf("新配置应默认每小时同步价格: %v", c)
	}

	legacy := Object{"price_enabled": false, "price_hours": 24, "backup_keep": 19, "quota_enabled": false, "report_hour": 7}
	if err := h.s.DB.Exec("INSERT INTO meta VALUES ('schedule_config', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", raw(legacy)); err != nil {
		t.Fatal(err)
	}
	c = scheduleConfigRPC(t, h)
	if c["price_enabled"] != true || num(c, "price_hours") != 1 || num(c, "price_sync_version") != 1 ||
		num(c, "backup_keep") != 19 || c["quota_enabled"] != false || num(c, "report_hour") != 7 {
		t.Fatalf("旧配置只应迁移价格同步默认值: %v", c)
	}
}

func TestPriceScheduleSavePersistsAcrossRestart(t *testing.T) {
	h := newHarness(t)
	w, out := h.rpc(t, "schedule.save", Object{"price_enabled": false, "price_hours": 37, "price_sync_version": 99}, h.token)
	requireStatus(t, w, http.StatusOK)
	c := obj(obj(out["data"])["config"])
	if c["price_enabled"] != false || num(c, "price_hours") != 37 || num(c, "price_sync_version") != 1 {
		t.Fatalf("保存应保留显式设置并固定内部版本: %v", c)
	}

	s, err := OpenStore(strings.TrimSuffix(h.s.KeyPath, ".key"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := NewApp(ctx, s, webui.Handler())
	defer func() {
		cancel()
		a.Wait()
		s.DB.Close()
	}()
	c = scheduleConfigRPC(t, &harness{s: s, a: a, token: h.token, ctx: ctx})
	if c["price_enabled"] != false || num(c, "price_hours") != 37 || num(c, "price_sync_version") != 1 {
		t.Fatalf("重启 RPC 读取后设置丢失: %v", c)
	}
}

func TestPriceScheduleDueAndValidation(t *testing.T) {
	var priceTask scheduledTask
	for _, task := range scheduledTasks {
		if task.id == "price" {
			priceTask = task
			break
		}
	}
	if priceTask.due == nil {
		t.Fatal("缺少价格同步任务")
	}
	c := defaultSchedule
	nowAt := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if priceTask.due(c, nowAt.Add(-59*time.Minute), nowAt) || !priceTask.due(c, nowAt.Add(-time.Hour), nowAt) {
		t.Fatal("价格同步应每满一小时到期")
	}
	c.PriceHours = 2
	if priceTask.due(c, nowAt.Add(-119*time.Minute), nowAt) || !priceTask.due(c, nowAt.Add(-2*time.Hour), nowAt) {
		t.Fatal("价格同步应按配置间隔到期")
	}
	c.PriceEnabled = false
	if priceTask.due(c, time.Time{}, nowAt) {
		t.Fatal("关闭后不应到期")
	}

	h := newHarness(t)
	for _, hours := range []int{1, 168} {
		if w, _ := h.rpc(t, "schedule.save", Object{"price_hours": hours}, h.token); w.Code != http.StatusOK {
			t.Fatalf("price_hours=%d 应被接受，实得 %d", hours, w.Code)
		}
	}
	for _, hours := range []int{0, 169} {
		if w, _ := h.rpc(t, "schedule.save", Object{"price_hours": hours}, h.token); w.Code != http.StatusBadRequest {
			t.Fatalf("price_hours=%d 应被拒绝，实得 %d", hours, w.Code)
		}
	}
}
