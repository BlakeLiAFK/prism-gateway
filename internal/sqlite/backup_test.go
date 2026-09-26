package sqlite

import (
	"path/filepath"
	"testing"
)

// 快照 → 改动线上库 → 从快照还原：线上库回到快照时的内容，另一个连接也看得到，WAL 模式不变
func TestSnapshotRestoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	live, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	other, err := Open(path) // 模拟交接部署时同时开着库的另一个进程
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	live.Exec("CREATE TABLE x (n INTEGER)")
	live.Exec("INSERT INTO x VALUES (1)")
	var snap []byte
	if err = live.Snapshot(func(b []byte) error { snap = append([]byte{}, b...); return nil }); err != nil {
		t.Fatal(err)
	}
	live.Exec("INSERT INTO x VALUES (2)")
	live.Exec("CREATE TABLE y (s TEXT)")

	src, err := Load(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if r, err := src.Query("SELECT COUNT(*) n FROM x"); err != nil || r[0].Int("n") != 1 {
		t.Fatalf("内存库应能读出快照内容: %v %v", r, err)
	}
	if err = live.RestoreFrom(src); err != nil {
		t.Fatal(err)
	}
	for name, db := range map[string]*DB{"live": live, "other": other} {
		if r, err := db.Query("SELECT COUNT(*) n FROM x"); err != nil || r[0].Int("n") != 1 {
			t.Fatalf("%s 应看到还原后的 1 行: %v %v", name, r, err)
		}
		if _, err := db.Query("SELECT * FROM y"); err == nil {
			t.Fatalf("%s 不应再有快照之后建的表", name)
		}
	}
	if r, _ := live.Query("PRAGMA journal_mode"); r[0].String("journal_mode") != "wal" {
		t.Fatalf("还原后应仍是 WAL 模式: %v", r)
	}
	if err = other.Exec("INSERT INTO x VALUES (3)"); err != nil {
		t.Fatalf("还原后另一连接应能继续写入: %v", err)
	}
	if r, _ := live.Query("PRAGMA integrity_check"); r[0].String("integrity_check") != "ok" {
		t.Fatalf("完整性检查失败: %v", r)
	}
	if _, err = Load([]byte("not a database")); err == nil {
		t.Fatal("非 SQLite 内容应被拒绝")
	}
}
