package sqlite

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
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

func TestRowAccessorsAndErrors(t *testing.T) {
	d, e := Open(filepath.Join(t.TempDir(), "acc.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if Version() == "" {
		t.Fatal("SQLite 版本不应为空")
	}
	r := Row{"i": int64(7), "f": 2.5, "s": "x"}
	if r.Float("i") != 7 {
		t.Fatal("整数列应可按浮点读取")
	}
	if r.Float("f") != 2.5 {
		t.Fatal("浮点列读取错误")
	}
	if r.Float("missing") != 0 || r.Int("missing") != 0 || r.String("missing") != "" {
		t.Fatal("缺失列应返回零值而不是 panic")
	}
	if r.Int("s") != 0 {
		t.Fatal("类型不符时应返回零值")
	}
	// 语法错误必须带上 SQLite 自己的说明，便于定位
	err := d.Exec("SELECT FROM WHERE")
	if err == nil {
		t.Fatal("非法 SQL 应当报错")
	}
	if !strings.Contains(err.Error(), "sqlite") {
		t.Fatalf("错误信息应标明来源: %v", err)
	}
	if _, err = d.Query("SELECT * FROM no_such_table"); err == nil {
		t.Fatal("查询不存在的表应报错")
	}
	// 不支持的绑定类型要明确拒绝，不能悄悄写入错误的值
	if err = d.Exec("CREATE TABLE t (v)"); err != nil {
		t.Fatal(err)
	}
	if err = d.Exec("INSERT INTO t VALUES (?)", struct{ A int }{1}); err == nil {
		t.Fatal("不支持的参数类型应被拒绝")
	}
	if err = d.Exec("INSERT INTO t VALUES (?)", math.NaN()); err == nil {
		t.Fatal("NaN 应被拒绝")
	}
	if err = d.Exec("INSERT INTO t VALUES (?)", []byte{}); err != nil {
		t.Fatalf("空 blob 应可写入: %v", err)
	}
	if err = d.Exec("INSERT INTO t VALUES (?)", nil); err != nil {
		t.Fatalf("NULL 应可写入: %v", err)
	}
	rows, err := d.Query("SELECT v FROM t")
	if err != nil || len(rows) != 2 {
		t.Fatalf("应读回 2 行: %d %v", len(rows), err)
	}
	// 关闭后的操作必须拒绝而不是崩溃
	d2, _ := Open(filepath.Join(t.TempDir(), "closed.db"))
	d2.Close()
	if err = d2.Exec("SELECT 1"); err == nil {
		t.Fatal("已关闭的连接不应接受查询")
	}
	if err = d2.Close(); err != nil {
		t.Fatalf("重复关闭应幂等: %v", err)
	}
}
