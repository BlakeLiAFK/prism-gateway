package sqlite

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSQLite(t *testing.T) {
	d, e := Open(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if e = d.Exec("CREATE TABLE x (s TEXT, n INTEGER)"); e != nil {
		t.Fatal(e)
	}
	if e = d.Exec("INSERT INTO x VALUES (?,?)", "中文\x00尾", 42); e != nil {
		t.Fatal(e)
	}
	r, e := d.Query("SELECT * FROM x")
	if e != nil || len(r) != 1 || r[0].String("s") != "中文\x00尾" || r[0].Int("n") != 42 {
		t.Fatal(r, e)
	}
	d.Transaction(func(tx *Tx) error { tx.Exec("DELETE FROM x"); return errors.New("rollback") })
	r, _ = d.Query("SELECT * FROM x")
	if len(r) != 1 {
		t.Fatal("rollback failed")
	}
	// 多条语句必须整体失败，不能只执行第一条后静默丢弃其余
	if e = d.Exec("DELETE FROM x; DROP TABLE x"); e == nil {
		t.Fatal("multi-statement SQL must be rejected")
	}
	if r, _ = d.Query("SELECT * FROM x"); len(r) != 1 {
		t.Fatal("rejected statement must not take effect")
	}
	if e = d.Exec("SELECT 1", "extra"); e == nil {
		t.Fatal("argument count mismatch must be rejected")
	}
}
