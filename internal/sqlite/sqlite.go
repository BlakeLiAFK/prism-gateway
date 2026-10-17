// Package sqlite is a small, serialized SQLite binding over the pure Go driver
// modernc.org/sqlite. It needs no CGo, no C compiler and no system SQLite
// library: the same source builds and cross-compiles anywhere Go runs.
// 占位符一律用 ?：驱动不按位置绑定 :name / @name / $name 这类具名占位符。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"
)

type Row map[string]any

func (r Row) String(k string) string { v, _ := r[k].(string); return v }
func (r Row) Int(k string) int64     { v, _ := r[k].(int64); return v }
func (r Row) Float(k string) float64 {
	if v, ok := r[k].(float64); ok {
		return v
	}
	return float64(r.Int(k))
}

// errClosed 与旧实现的报错保持一致，调用方只关心「关闭后拒绝服务」这一点。
var errClosed = errors.New("sqlite: closed")

// DB 串行使用一条连接：database/sql 只开一条，语句由 mu 排队。
// 事务、会话亲和、PRAGMA 都因此与旧版 C 绑定表现一致。
type DB struct {
	mu     sync.Mutex
	db     *sql.DB
	path   string // 备份与还原需要按路径重新打开同一个库；内存库为空
	closed bool
}

type Tx struct{ tx *sql.Tx }

// dsn 生成文件库的连接串。PRAGMA 写在连接串里，每条新建连接都会自动执行，
// 不会出现池里换了连接就丢掉 foreign_keys 的情况。
func dsn(path string) string {
	v := url.Values{}
	for _, p := range []string{
		"journal_mode(WAL)",
		"synchronous(FULL)",
		"foreign_keys(ON)",
		"trusted_schema(OFF)",
		"busy_timeout(5000)",
	} {
		v.Add("_pragma", p)
	}
	// 事务一律 BEGIN IMMEDIATE，与旧实现的写事务语义相同
	v.Set("_txlock", "immediate")
	return "file:" + path + "?" + v.Encode()
}

func Open(path string) (*DB, error) {
	d, err := open(dsn(path))
	if err != nil {
		return nil, err
	}
	d.path = path
	return d, nil
}

// openMemory 打开一个内存库：没有 PRAGMA，也没有可重开路径，只用于承载快照。
func openMemory() (*DB, error) { return open(":memory:") }

func open(name string) (*DB, error) {
	conn, err := sql.Open("sqlite", name)
	if err != nil {
		return nil, wrap(err)
	}
	// 单连接：串行语义与旧实现一致，也让 Raw 取底层连接时不会和别的语句抢池子
	conn.SetMaxOpenConns(1)
	d := &DB{db: conn}
	// 立刻建一条连接：路径不可写、PRAGMA 被拒都在 Open 阶段暴露，不拖到第一条语句
	c, err := conn.Conn(context.Background())
	if err != nil {
		conn.Close()
		return nil, wrap(err)
	}
	c.Close()
	return d, nil
}

func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	if err := d.db.Close(); err != nil {
		return wrap(err)
	}
	return nil
}

// wrap 统一带上来源前缀；调用方与测试都靠这个前缀区分「数据库报错」。
func wrap(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("sqlite: %w", err)
}

func (d *DB) Query(sqlText string, args ...any) ([]Row, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.query(sqlText, args...)
}

func (d *DB) Exec(sqlText string, args ...any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errClosed
	}
	if err := checkArgs(sqlText, len(args)); err != nil {
		return err
	}
	bound, err := bindArgs(args)
	if err != nil {
		return err
	}
	if _, err = d.db.ExecContext(context.Background(), sqlText, bound...); err != nil {
		return wrap(err)
	}
	return nil
}

func (t *Tx) Query(sqlText string, args ...any) ([]Row, error) {
	return queryTx(t.tx, sqlText, args...)
}

func (t *Tx) Exec(sqlText string, args ...any) error {
	_, err := queryTx(t.tx, sqlText, args...)
	return err
}

func (d *DB) Transaction(fn func(*Tx) error) (err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errClosed
	}
	tx, err := d.db.BeginTx(context.Background(), nil)
	if err != nil {
		return wrap(err)
	}
	defer func() {
		if v := recover(); v != nil {
			tx.Rollback()
			panic(v)
		}
		if err != nil {
			tx.Rollback()
		}
	}()
	if err = fn(&Tx{tx: tx}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return wrap(err)
	}
	return nil
}

func (d *DB) query(sqlText string, args ...any) ([]Row, error) {
	if d.closed {
		return nil, errClosed
	}
	if err := checkArgs(sqlText, len(args)); err != nil {
		return nil, err
	}
	bound, err := bindArgs(args)
	if err != nil {
		return nil, err
	}
	return scanRows(d.db.QueryContext(context.Background(), sqlText, bound...))
}

// queryTx 供事务内使用：语句走事务自己的连接，不会串到池子里的另一条连接上。
func queryTx(tx *sql.Tx, sqlText string, args ...any) ([]Row, error) {
	if tx == nil {
		return nil, errClosed
	}
	if err := checkArgs(sqlText, len(args)); err != nil {
		return nil, err
	}
	bound, err := bindArgs(args)
	if err != nil {
		return nil, err
	}
	return scanRows(tx.QueryContext(context.Background(), sqlText, bound...))
}

// scanRows 把结果按列名收成 Row。列值沿用 SQLite 的存储类别：
// INTEGER→int64、REAL→float64、TEXT→string、BLOB→[]byte、NULL→nil。
// 本仓库的表只用 INTEGER / TEXT / REAL，不涉及驱动对 DATE / DATETIME 声明列的时间转换。
func scanRows(rows *sql.Rows, err error) ([]Row, error) {
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, wrap(err)
	}
	out := []Row{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			return nil, wrap(err)
		}
		row := Row{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, wrap(err)
	}
	return out, nil
}

// bindArgs 只接受 SQLite 能准确表达的类型；其余一律拒绝，避免把值悄悄写坏。
// 旧实现同样拒绝非有限浮点与未知类型，这里保持同样的边界。
func bindArgs(args []any) ([]any, error) {
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case nil, string, []byte, int64, float64:
			out[i] = v
		case int:
			out[i] = int64(v)
		case bool:
			n := int64(0)
			if v {
				n = 1
			}
			out[i] = n
		case float32:
			out[i] = float64(v)
		default:
			return nil, fmt.Errorf("sqlite: unsupported argument %T", a)
		}
		if f, ok := out[i].(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
			return nil, errors.New("sqlite: nonfinite number")
		}
	}
	return out, nil
}

// checkArgs 在把语句交给驱动之前做两项把关，旧绑定这两项都由 SQLite 帮我们把关：
//   - 一次只允许一条语句。驱动会顺序执行多条，否则 "DELETE FROM x; DROP TABLE x"
//     会先执行前半句再报错，等于静默做了一半。
//   - 参数个数必须和占位符个数一致。驱动对多余的参数直接忽略，缺的参数按 NULL 绑定，
//     SQL 拼错时会静默按错的方式执行。
func checkArgs(q string, n int) error {
	params, err := scanSQL(q)
	if err != nil {
		return err
	}
	if params >= 0 && params != n {
		return fmt.Errorf("sqlite: 参数个数与占位符不符（%d 个参数，%d 个占位符）", n, params)
	}
	return nil
}

// scanSQL 词法扫一遍语句：数出占位符个数，并确认只有一条语句。
// 返回 -1 表示占位符个数没法靠文本可靠统计（?NNN 序号或具名占位符），此时不校验个数。
func scanSQL(q string) (int, error) {
	params, known := 0, true
	seen, depth := 0, 0 // seen：已经扫过的词数；depth：BEGIN / CASE 的嵌套层数
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j, ok := skipQuoted(q, i, c)
			if !ok {
				return 0, nil // 引号没闭合，交给 SQLite 报语法错误
			}
			i, seen = j+1, seen+1
		case c == '[':
			j := strings.IndexByte(q[i:], ']')
			if j < 0 {
				return 0, nil
			}
			i, seen = i+j+1, seen+1
		case c == '-' && strings.HasPrefix(q[i:], "--"):
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				return 0, nil
			}
			i += j
		case c == '/' && strings.HasPrefix(q[i:], "/*"):
			j := strings.Index(q[i+2:], "*/")
			if j < 0 {
				return 0, nil
			}
			i += j + 4
		case isNameByte(c):
			j := i
			for j < len(q) && isNameByte(q[j]) {
				j++
			}
			// CASE ... END 与触发器体（... BEGIN ... END）内部的分号不是语句分隔符；
			// 开头的 BEGIN 是事务语句，不是块的开始。
			switch strings.ToUpper(q[i:j]) {
			case "CASE":
				depth++
			case "BEGIN":
				if seen > 0 {
					depth++
				}
			case "END":
				if depth > 0 {
					depth--
				}
			}
			i, seen = j, seen+1
		case c == '?':
			i++
			if i < len(q) && q[i] >= '0' && q[i] <= '9' { // ?NNN 显式序号
				known = false
				for i < len(q) && q[i] >= '0' && q[i] <= '9' {
					i++
				}
				continue
			}
			params++
		case c == ':' || c == '@' || c == '$':
			i++
			if i < len(q) && isNameByte(q[i]) {
				known = false
				for i < len(q) && isNameByte(q[i]) {
					i++
				}
			}
		case c == ';':
			if depth == 0 && hasMore(q[i+1:]) {
				return 0, errors.New("sqlite: 每次只允许一条语句")
			}
			i++
		default:
			i++
		}
	}
	if !known {
		return -1, nil
	}
	return params, nil
}

// isNameByte 判断是否标识符字符（SQLite 的标识符与具名占位符都用这几个字符）。
func isNameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// skipQuoted 返回闭合引号的下标；SQL 里成对的引号是转义，不结束字面量。
func skipQuoted(q string, i int, quote byte) (int, bool) {
	for j := i + 1; j < len(q); j++ {
		if q[j] != quote {
			continue
		}
		if j+1 < len(q) && q[j+1] == quote {
			j++
			continue
		}
		return j, true
	}
	return 0, false
}

// hasMore 判断分号之后是否还有语句：空白与注释不算。
func hasMore(q string) bool {
	for i := 0; i < len(q); i++ {
		switch c := q[i]; c {
		case ' ', '\t', '\r', '\n':
		case '-':
			if i+1 < len(q) && q[i+1] == '-' {
				j := strings.IndexByte(q[i:], '\n')
				if j < 0 {
					return false
				}
				i += j
				continue
			}
			return true
		case '/':
			if i+1 < len(q) && q[i+1] == '*' {
				j := strings.Index(q[i+2:], "*/")
				if j < 0 {
					return false
				}
				i += j + 3
				continue
			}
			return true
		default:
			return true
		}
	}
	return false
}

var (
	versionOnce sync.Once
	versionText string
)

// Version 返回底层 SQLite 版本。纯 Go 实现不提供版本常量，问一次库再缓存。
func Version() string {
	versionOnce.Do(func() {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			return
		}
		defer db.Close()
		if err = db.QueryRow("SELECT sqlite_version()").Scan(&versionText); err != nil {
			versionText = ""
		}
	})
	return versionText
}
