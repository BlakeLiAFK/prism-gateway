package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestScheduledTaskFailureNoticeDoesNotExposeError(t *testing.T) {
	h := newHarness(t)
	msgs := webhookSink(t, h)
	secret := "https://user:password@example.com/path?key=secret"
	task := scheduledTask{
		id:   "private-task",
		name: "私有任务",
		due:  func(scheduleConfig, time.Time, time.Time) bool { return true },
		run:  func(*App, scheduleConfig) (string, error) { return "", errors.New(secret) },
	}

	run := h.a.runTask(task, defaultSchedule)
	got := msgs()
	if run.Status != "failed" || len(got) != 1 {
		t.Fatalf("失败任务应记录并推送一次: run=%v notices=%v", run, got)
	}
	if strings.Contains(got[0], secret) || strings.Contains(got[0], "password") || strings.Contains(got[0], "key=secret") {
		t.Fatalf("通知泄露了任务错误详情: %s", got[0])
	}
	for _, want := range []string{"定时任务失败", "任务：私有任务 (private-task)", "请在管理后台的任务记录中查看详情"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("失败通知缺少 %q: %s", want, got[0])
		}
	}
}
