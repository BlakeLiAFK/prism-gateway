package sqlite

// 在线备份与还原：都基于 sqlite3_backup，在一个读事务 / 写事务内完成，不需要停库，也不经过临时文件。
//   - Snapshot：线上库 → 内存库 → 序列化字节（远程备份直接压缩上传，本机磁盘零额外占用）
//   - Load：序列化字节 → 内存库（还原前在内存里校验）
//   - RestoreFrom：内存库 → 线上库（整体替换内容，其他连接随后看到新内容，WAL 模式不变）

/*
#include <sqlite3.h>
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"errors"
	"unsafe"
)

var mainName = C.CString("main")

func openMemory() (*DB, error) {
	p := C.CString(":memory:")
	defer C.free(unsafe.Pointer(p))
	d := &DB{}
	if rc := C.sqlite3_open_v2(p, &d.conn, C.SQLITE_OPEN_READWRITE|C.SQLITE_OPEN_CREATE|C.SQLITE_OPEN_FULLMUTEX, nil); rc != C.SQLITE_OK {
		err := d.err(rc)
		if d.conn != nil {
			C.sqlite3_close_v2(d.conn)
		}
		return nil, err
	}
	return d, nil
}

// copyDB 把 src 的 main 库整体复制到 dst；一次 step 完成，期间两边都持有连接锁
func copyDB(dst, src *DB) error {
	dst.mu.Lock()
	defer dst.mu.Unlock()
	src.mu.Lock()
	defer src.mu.Unlock()
	if dst.conn == nil || src.conn == nil {
		return errors.New("sqlite: closed")
	}
	b := C.sqlite3_backup_init(dst.conn, mainName, src.conn, mainName)
	if b == nil {
		return dst.err(C.sqlite3_errcode(dst.conn))
	}
	rc := C.sqlite3_backup_step(b, -1)
	if frc := C.sqlite3_backup_finish(b); rc == C.SQLITE_DONE && frc != C.SQLITE_OK {
		return dst.err(frc)
	}
	if rc != C.SQLITE_DONE {
		return dst.err(rc)
	}
	return nil
}

// Snapshot 生成当前库的一致性快照并交给 fn。字节直接指向 SQLite 分配的内存，
// fn 返回后即释放，fn 不得保留引用。峰值内存约为数据库大小。
func (d *DB) Snapshot(fn func([]byte) error) error {
	mem, err := openMemory()
	if err != nil {
		return err
	}
	defer mem.Close()
	if err = copyDB(mem, d); err != nil {
		return err
	}
	var size C.sqlite3_int64
	p := C.sqlite3_serialize(mem.conn, mainName, &size, 0)
	if p == nil {
		return errors.New("sqlite: serialize failed")
	}
	defer C.sqlite3_free(unsafe.Pointer(p))
	return fn(unsafe.Slice((*byte)(unsafe.Pointer(p)), int(size)))
}

// Load 把序列化的库载入一个新的内存库。调用方负责 Close。
func Load(b []byte) (*DB, error) {
	if len(b) < 100 || string(b[:16]) != "SQLite format 3\x00" {
		return nil, errors.New("sqlite: 不是 SQLite 数据库文件")
	}
	mem, err := openMemory()
	if err != nil {
		return nil, err
	}
	buf := C.sqlite3_malloc64(C.sqlite3_uint64(len(b)))
	if buf == nil {
		mem.Close()
		return nil, errors.New("sqlite: out of memory")
	}
	dst := unsafe.Slice((*byte)(buf), len(b))
	copy(dst, b)
	// 文件头第 18、19 字节标记 WAL 模式；内存库不支持 WAL，改回传统日志格式才能读
	dst[18], dst[19] = 1, 1
	rc := C.sqlite3_deserialize(mem.conn, mainName, (*C.uchar)(buf), C.sqlite3_int64(len(b)), C.sqlite3_int64(len(b)),
		C.SQLITE_DESERIALIZE_FREEONCLOSE|C.SQLITE_DESERIALIZE_RESIZEABLE)
	if rc != C.SQLITE_OK {
		err = mem.err(rc)
		mem.Close()
		return nil, err
	}
	return mem, nil
}

// RestoreFrom 用 src 的全部内容替换当前库，在单个写事务内完成
func (d *DB) RestoreFrom(src *DB) error { return copyDB(d, src) }
