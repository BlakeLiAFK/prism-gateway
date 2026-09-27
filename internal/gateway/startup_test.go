package gateway

import (
	"context"
	"path/filepath"
	"testing"
)

// 初始化必须在返回应用前完成，否则后台重置会清掉刚完成的任务记录。
func TestAppInitializesScheduleBeforeServing(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	st := scheduleState{Runs: map[string]taskRun{"quota": {At: now()}, "upstream": {At: now()}, "report": {At: now()}}, Warned: map[string]int64{}}
	if err := s.DB.Exec("INSERT INTO meta VALUES ('schedule_state', ?)", raw(st)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := NewApp(ctx, s, nil)
	defer func() { cancel(); a.Wait() }()
	loaded, err := a.readScheduleState()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Runs["quota"].At != 0 || loaded.Runs["upstream"].At != 0 || loaded.Runs["report"].At == 0 {
		t.Fatalf("启动状态未就绪: %+v", loaded.Runs)
	}
}
