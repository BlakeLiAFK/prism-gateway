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

// 损坏的调度状态必须保留原值，避免启动时用空状态覆盖并丢失冷却记录。
func TestAppPreservesInvalidScheduleState(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	const invalid = `{"runs":`
	if err = s.DB.Exec("INSERT INTO meta VALUES ('schedule_state', ?)", invalid); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := NewApp(ctx, s, nil)
	cancel()
	a.Wait()
	rows, err := s.DB.Query("SELECT value FROM meta WHERE key='schedule_state'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].String("value") != invalid {
		t.Fatalf("损坏状态不应被覆盖: %v", rows)
	}
}

// 并发上限取消后，老库里残留的 global_concurrency 必须在迁移阶段清掉：
// settings 表只增不删，而 loadSettings 是严格解码，留着这个键会让老库直接启动失败。
func TestMigrationDropsObsoleteGlobalConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("INSERT INTO settings VALUES ('global_concurrency','512')"); err != nil {
		t.Fatal(err)
	}
	s.DB.Close()

	s, err = OpenStore(path)
	if err != nil {
		t.Fatalf("带残留设置的旧库应能正常启动: %v", err)
	}
	defer s.DB.Close()
	rows, err := s.DB.Query("SELECT key FROM settings WHERE key='global_concurrency'")
	if err != nil || len(rows) != 0 {
		t.Fatalf("迁移应删掉残留的 global_concurrency: %v %v", rows, err)
	}
	if got := s.Config().Settings.AppName; got != defaults().Settings.AppName {
		t.Fatalf("其余设置应回到默认值，实得 %q", got)
	}
}
