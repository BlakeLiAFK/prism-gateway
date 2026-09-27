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
