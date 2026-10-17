package sqlite

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

// 语句扫描：字面量、注释、触发器体里的分号都不是语句分隔符，
// 字面量、注释里的问号也不是占位符。
func TestStatementScanning(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Exec("CREATE TABLE t (s TEXT, n INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err = d.Exec(`CREATE TABLE "semi;col" (a TEXT)`); err != nil {
		t.Fatalf("带分号的引号标识符应被接受: %v", err)
	}
	if err = d.Exec(`CREATE TABLE [br;col] (a TEXT)`); err != nil {
		t.Fatalf("带分号的方括号标识符应被接受: %v", err)
	}
	// 触发器体是唯一允许在语句内部出现分号的写法，必须整体接受
	if err = d.Exec(`CREATE TRIGGER guard BEFORE DELETE ON t WHEN OLD.n = 9 BEGIN SELECT RAISE(ABORT, '禁止删除'); END`); err != nil {
		t.Fatalf("触发器应被接受: %v", err)
	}
	if err = d.Exec(`CREATE TABLE u (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err = d.Exec(`CREATE TRIGGER counted AFTER INSERT ON u BEGIN UPDATE u SET n = CASE WHEN new.n > 0 THEN 1 ELSE 2 END; END`); err != nil {
		t.Fatalf("带 CASE 的触发器应被接受: %v", err)
	}
	if err = d.Exec("INSERT INTO u VALUES (5)"); err != nil {
		t.Fatal(err)
	}
	if r, e := d.Query("SELECT n FROM u"); e != nil || r[0].Int("n") != 1 {
		t.Fatalf("带 CASE 的触发器应真的生效: %v %v", r, e)
	}
	if err = d.Exec("INSERT INTO t VALUES ('x', 9)"); err != nil {
		t.Fatal(err)
	}
	if err = d.Exec("DELETE FROM t WHERE n = 9"); err == nil {
		t.Fatal("触发器应真的生效，说明语句被完整执行")
	}

	ok := []struct {
		q    string
		args []any
	}{
		{"INSERT INTO t VALUES ('?', 1)", nil},               // 字面量里的问号不是占位符
		{"INSERT INTO t VALUES ('a;b', 2)", nil},             // 字面量里的分号不是分隔符
		{"INSERT INTO t VALUES ('it''s;?', 3)", nil},         // 成对引号是转义，不结束字面量
		{"UPDATE t SET n = 4 -- 注释里的 ? 和 ;\n", nil},          // 行注释
		{"UPDATE t SET n = 5 /* 注释里的 ? 和 ; */ WHERE 1", nil}, // 块注释
		{"SELECT 1;", nil}, // 结尾的单个分号允许
		{"SELECT 1; -- 只有注释", nil},
		{"SELECT 1; /* 只有注释 */", nil},
		{"SELECT ? AS v", []any{7}},
		{"SELECT ?1 AS v", []any{8}}, // 序号占位符不参与个数校验，交给驱动
	}
	for _, c := range ok {
		if err = d.Exec(c.q, c.args...); err != nil {
			t.Fatalf("%q 应被接受: %v", c.q, err)
		}
	}
	if r, err := d.Query("SELECT ?1 AS v", 8); err != nil || r[0].Int("v") != 8 {
		t.Fatalf("序号占位符应能绑定: %v %v", r, err)
	}
	// 具名占位符不参与个数校验：报出来的应当是驱动的绑定错误，不是本包的个数错误
	if err = d.Exec("SELECT :a AS v"); err == nil || strings.Contains(err.Error(), "参数个数") {
		t.Fatalf("具名占位符应交由驱动判断: %v", err)
	}

	bad := []struct {
		q    string
		args []any
	}{
		{"DELETE FROM t; DROP TABLE t", nil},                 // 多条语句
		{"BEGIN; DELETE FROM t", nil},                        // 开头的 BEGIN 是事务语句，不是块
		{"SELECT CASE WHEN 1 THEN 1 END; DROP TABLE t", nil}, // CASE 结束后仍是多条
		{"CREATE TRIGGER g2 AFTER INSERT ON t BEGIN SELECT 1; END; DROP TABLE t", nil},
		{"INSERT INTO t VALUES (?)", nil},         // 参数少于占位符
		{"INSERT INTO t VALUES (?, ?)", []any{1}}, // 参数少于占位符
		{"SELECT 1", []any{1}},                    // 参数多于占位符
		{"SELECT 1", []any{1, 2}},
		{"SELECT 'abc", nil}, // 引号没闭合，交给 SQLite 报错
		{"DELETE FROM t; /* 注释之后还有语句 */ DROP TABLE t", nil},
	}
	before, err := d.Query("SELECT COUNT(*) n FROM t")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range bad {
		if err = d.Exec(c.q, c.args...); err == nil {
			t.Fatalf("%q 应被拒绝", c.q)
		}
	}
	after, err := d.Query("SELECT COUNT(*) n FROM t")
	if err != nil || after[0].Int("n") != before[0].Int("n") {
		t.Fatalf("被拒绝的语句不应留下痕迹: %v → %v %v", before, after, err)
	}
}

// 绑定参数的类型边界：能精确表达的接受，其余明确拒绝。
func TestBindArgumentTypes(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "bind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Exec("CREATE TABLE v (x)"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []any{nil, "", "s", []byte("b"), int(1), int64(2), float32(1.5), float64(2.5), true, false} {
		if err = d.Exec("INSERT INTO v VALUES (?)", a); err != nil {
			t.Fatalf("%T 应可写入: %v", a, err)
		}
	}
	for _, a := range []any{struct{ A int }{1}, uint(1), math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err = d.Exec("INSERT INTO v VALUES (?)", a); err == nil {
			t.Fatalf("%T 应被拒绝", a)
		}
	}
	rows, err := d.Query("SELECT x FROM v")
	if err != nil || len(rows) != 10 {
		t.Fatalf("应读回 10 行: %d %v", len(rows), err)
	}
	if rows[0].String("x") != "" || rows[0].Int("x") != 0 {
		t.Fatalf("NULL 应读回零值: %v", rows[0])
	}
	if rows[4].Int("x") != 1 || rows[6].Float("x") != 1.5 || rows[8].Int("x") != 1 || rows[9].Int("x") != 0 {
		t.Fatalf("绑定值应保真: %v", rows[4:])
	}
}

// 事务内的查询与 panic：panic 必须继续向上传播，事务整体回滚。
func TestTransactionQueryAndPanic(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "tx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Exec("CREATE TABLE t (s TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err = d.Transaction(func(tx *Tx) error {
		if r, e := tx.Query("SELECT ? AS v", 1); e != nil || r[0].Int("v") != 1 {
			t.Fatalf("事务内查询应可用: %v %v", r, e)
		}
		return tx.Exec("INSERT INTO t VALUES ('committed')")
	}); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic 应继续向上传播")
			}
		}()
		d.Transaction(func(tx *Tx) error {
			tx.Exec("INSERT INTO t VALUES ('rolled back')")
			panic("boom")
		})
	}()
	rows, err := d.Query("SELECT s FROM t")
	if err != nil || len(rows) != 1 || rows[0].String("s") != "committed" {
		t.Fatalf("panic 后事务应整体回滚: %v %v", rows, err)
	}
}

// 打开失败、内存库与已关闭连接的错误边界。
func TestOpenAndClosedEdges(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "no/such/dir/x.db")); err == nil {
		t.Fatal("路径不可用时应报错")
	}
	d, err := Open(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Exec("CREATE TABLE t (x)"); err != nil {
		t.Fatal(err)
	}
	mem, err := openMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	if err = mem.RestoreFrom(mem); err == nil {
		t.Fatal("内存库不能作为还原目标")
	}
	gone, err := Open(filepath.Join(t.TempDir(), "gone.db"))
	if err != nil {
		t.Fatal(err)
	}
	gone.Close()
	if err = d.RestoreFrom(gone); err == nil {
		t.Fatal("已关闭的源库不能用于还原")
	}
	if err = gone.Snapshot(func([]byte) error { return nil }); err == nil {
		t.Fatal("已关闭的连接不应接受快照")
	}
	if _, err = gone.Query("SELECT 1"); err == nil {
		t.Fatal("已关闭的连接不应接受查询")
	}
	if err = gone.Transaction(func(*Tx) error { return nil }); err == nil {
		t.Fatal("已关闭的连接不应开启事务")
	}
	if err = d.Exec("SELECT 1; DELETE FROM t"); err == nil {
		t.Fatal("多条语句必须被拒绝")
	}
	if _, err = Load(nil); err == nil {
		t.Fatal("空内容应被拒绝")
	}
}
