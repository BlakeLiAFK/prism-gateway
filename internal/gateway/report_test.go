package gateway

import (
	"strings"
	"testing"
	"time"
)

// 日报优先显示后台配置的模型名称，配置中已删除的模型仍保留原始 ID。
func TestDailyReportModelNames(t *testing.T) {
	h := newHarness(t)
	h.change(t, func(c *Config) {
		m := modelFixture("model-a", "chat")
		m.Name = "模型甲"
		c.Providers = []Provider{{ID: "p_test", Name: "测试上游", Kind: "custom", BaseURL: "https://example.com", Auth: "none", Enabled: true, TimeoutSec: 30}}
		c.Models = []Model{m}
	})
	c := defaultSchedule
	nowAt := time.Date(2026, 9, 26, 9, 0, 0, 0, c.zone())
	yesterday := nowAt.AddDate(0, 0, -1).UnixMilli()
	insertUsage(t, h, "model-a", "p", "success", "", yesterday, 0, true, false)
	insertUsage(t, h, "removed-model", "p", "error", "", yesterday, 0, true, false)

	text, err := h.a.dailyReport(c, nowAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"模型甲 1 次", "removed-model 1 次", "失败最多：removed-model 1 次"} {
		if !strings.Contains(text, want) {
			t.Errorf("日报缺少「%s」:\n%s", want, text)
		}
	}
	if strings.Contains(text, "model-a") {
		t.Errorf("已配置名称的模型不应显示原始 ID:\n%s", text)
	}
}

func TestWeeklyDueAndReport(t *testing.T) {
	c := defaultSchedule
	zero := time.Unix(0, 0)
	c.WeeklyWeekday, c.WeeklyHour = 1, 9
	if weeklyDue(c, zero, time.Date(2026, 9, 21, 8, 0, 0, 0, c.zone())) {
		t.Fatal("首次启用时，本周设定时刻前不应补发上周周报")
	}
	if !weeklyDue(c, zero, time.Date(2026, 9, 21, 9, 0, 0, 0, c.zone())) {
		t.Fatal("首次启用时，到达本周设定时刻应运行一次")
	}
	c.WeeklyWeekday, c.WeeklyHour = 3, 9
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	if weeklyDue(c, at("2026-09-16T01:00:00Z"), at("2026-09-23T00:59:00Z")) {
		t.Fatal("设定时刻前不应运行")
	}
	if !weeklyDue(c, at("2026-09-16T01:00:00Z"), at("2026-09-23T01:00:00Z")) {
		t.Fatal("到达设定时刻应运行")
	}
	if !weeklyDue(c, at("2026-09-16T01:00:00Z"), at("2026-09-24T01:00:00Z")) {
		t.Fatal("错过设定时刻后应补跑")
	}
	if weeklyDue(c, at("2026-09-23T01:00:00Z"), at("2026-09-24T01:00:00Z")) {
		t.Fatal("本周已运行后不应重复")
	}

	h := newHarness(t)
	nowAt := time.Date(2026, 9, 23, 9, 0, 0, 0, c.zone())
	insertUsage(t, h, "a", "p", "success", "", time.Date(2026, 9, 14, 0, 0, 0, 0, c.zone()).UnixMilli(), 1e9, true, false)
	insertUsage(t, h, "b", "p", "error", "", time.Date(2026, 9, 20, 23, 0, 0, 0, c.zone()).UnixMilli(), 0, true, false)
	insertUsage(t, h, "c", "p", "success", "", time.Date(2026, 9, 21, 0, 0, 0, 0, c.zone()).UnixMilli(), 5e9, true, false)
	text, err := h.a.weeklyReport(c, nowAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"周报 2026-09-14 至 2026-09-20（UTC+8）", "请求 2 次", "估算花费 $1.00"} {
		if !strings.Contains(text, want) {
			t.Errorf("周报缺少「%s」:\n%s", want, text)
		}
	}

	w, out := h.rpc(t, "schedule.save", Object{"weekly_enabled": true, "weekly_weekday": 5, "weekly_hour": 18}, h.token)
	requireStatus(t, w, 200)
	saved := obj(obj(out["data"])["config"])
	if saved["weekly_enabled"] != true || num(saved, "weekly_weekday") != 5 || num(saved, "weekly_hour") != 18 {
		t.Fatalf("周报配置未持久化: %v", saved)
	}
	if w, _ = h.rpc(t, "schedule.save", Object{"weekly_weekday": 0}, h.token); w.Code != 400 {
		t.Fatalf("非法星期应被拒绝: %d", w.Code)
	}
}
