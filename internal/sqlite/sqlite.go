// Package sqlite is a small, serialized SQLite C API binding. No external Go
// modules, downloaded drivers, or database processes are needed.
package sqlite

/*
#cgo linux LDFLAGS: -l:libsqlite3.a -lm -ldl -lpthread
#cgo darwin LDFLAGS: -lsqlite3
#cgo windows LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>
static int bind_text_copy(sqlite3_stmt *s, int i, const char *v, int n) {
    return sqlite3_bind_text(s, i, v, n, SQLITE_TRANSIENT);
}
static int bind_blob_copy(sqlite3_stmt *s, int i, const void *v, int n) {
    return sqlite3_bind_blob(s, i, v, n, SQLITE_TRANSIENT);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"unsafe"
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

type DB struct {
	mu   sync.Mutex
	conn *C.sqlite3
}
type Tx struct{ db *DB }

func Version() string { return C.GoString(C.sqlite3_libversion()) }
func Open(path string) (*DB, error) {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	d := &DB{}
	if rc := C.sqlite3_open_v2(p, &d.conn, C.SQLITE_OPEN_READWRITE|C.SQLITE_OPEN_CREATE|C.SQLITE_OPEN_FULLMUTEX, nil); rc != C.SQLITE_OK {
		err := d.err(rc)
		if d.conn != nil {
			C.sqlite3_close_v2(d.conn)
		}
		return nil, err
	}
	C.sqlite3_busy_timeout(d.conn, 5000)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA trusted_schema=OFF"} {
		if _, err := d.Query(q); err != nil {
			d.Close()
			return nil, err
		}
	}
	return d, nil
}
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil
	}
	rc := C.sqlite3_close_v2(d.conn)
	if rc != C.SQLITE_OK {
		return d.err(rc)
	}
	d.conn = nil
	return nil
}
func (d *DB) err(rc C.int) error {
	if d.conn == nil {
		return errors.New("sqlite: closed")
	}
	return fmt.Errorf("sqlite (%d): %s", int(rc), C.GoString(C.sqlite3_errmsg(d.conn)))
}
func (d *DB) Query(sql string, args ...any) ([]Row, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.query(sql, args...)
}
func (d *DB) Exec(sql string, args ...any) error           { _, err := d.Query(sql, args...); return err }
func (t *Tx) Query(sql string, args ...any) ([]Row, error) { return t.db.query(sql, args...) }
func (t *Tx) Exec(sql string, args ...any) error           { _, err := t.Query(sql, args...); return err }
func (d *DB) Transaction(fn func(*Tx) error) (err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err = d.query("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if v := recover(); v != nil {
			d.query("ROLLBACK")
			panic(v)
		}
		if err != nil {
			d.query("ROLLBACK")
		}
	}()
	if err = fn(&Tx{d}); err != nil {
		return err
	}
	_, err = d.query("COMMIT")
	return err
}
func (d *DB) query(query string, args ...any) ([]Row, error) {
	if d.conn == nil {
		return nil, errors.New("sqlite: closed")
	}
	qs := C.CString(query)
	defer C.free(unsafe.Pointer(qs))
	var st *C.sqlite3_stmt
	var tail *C.char
	if rc := C.sqlite3_prepare_v2(d.conn, qs, -1, &st, &tail); rc != C.SQLITE_OK {
		return nil, d.err(rc)
	}
	// 一次只执行一条语句；剩余尾巴说明调用方拼接了多条 SQL，直接拒绝而不是静默丢弃
	if strings.TrimSpace(C.GoString(tail)) != "" {
		if st != nil {
			C.sqlite3_finalize(st)
		}
		return nil, errors.New("sqlite: 每次只允许一条语句")
	}
	if st == nil {
		return []Row{}, nil
	}
	defer C.sqlite3_finalize(st)
	if int(C.sqlite3_bind_parameter_count(st)) != len(args) {
		return nil, fmt.Errorf("sqlite: argument count mismatch")
	}
	for i, a := range args {
		var rc C.int
		idx := C.int(i + 1)
		switch v := a.(type) {
		case nil:
			rc = C.sqlite3_bind_null(st, idx)
		case string:
			b := C.CString(v)
			rc = C.bind_text_copy(st, idx, b, C.int(len(v)))
			C.free(unsafe.Pointer(b))
		case []byte:
			if len(v) == 0 {
				rc = C.sqlite3_bind_zeroblob(st, idx, 0)
			} else {
				p := C.CBytes(v)
				rc = C.bind_blob_copy(st, idx, p, C.int(len(v)))
				C.free(p)
			}
		case int:
			rc = C.sqlite3_bind_int64(st, idx, C.sqlite3_int64(v))
		case int64:
			rc = C.sqlite3_bind_int64(st, idx, C.sqlite3_int64(v))
		case bool:
			n := 0
			if v {
				n = 1
			}
			rc = C.sqlite3_bind_int(st, idx, C.int(n))
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, errors.New("sqlite: nonfinite number")
			}
			rc = C.sqlite3_bind_double(st, idx, C.double(v))
		default:
			return nil, fmt.Errorf("sqlite: unsupported argument %T", a)
		}
		if rc != C.SQLITE_OK {
			return nil, d.err(rc)
		}
	}
	rows := []Row{}
	for {
		rc := C.sqlite3_step(st)
		if rc == C.SQLITE_DONE {
			return rows, nil
		}
		if rc != C.SQLITE_ROW {
			return nil, d.err(rc)
		}
		row := Row{}
		n := int(C.sqlite3_column_count(st))
		for i := 0; i < n; i++ {
			ci := C.int(i)
			name := C.GoString(C.sqlite3_column_name(st, ci))
			switch C.sqlite3_column_type(st, ci) {
			case C.SQLITE_INTEGER:
				row[name] = int64(C.sqlite3_column_int64(st, ci))
			case C.SQLITE_FLOAT:
				row[name] = float64(C.sqlite3_column_double(st, ci))
			case C.SQLITE_TEXT:
				row[name] = C.GoStringN((*C.char)(unsafe.Pointer(C.sqlite3_column_text(st, ci))), C.sqlite3_column_bytes(st, ci))
			case C.SQLITE_BLOB:
				row[name] = C.GoBytes(C.sqlite3_column_blob(st, ci), C.sqlite3_column_bytes(st, ci))
			default:
				row[name] = nil
			}
		}
		rows = append(rows, row)
	}
}
