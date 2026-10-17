package sqlite

// 在线备份与还原，都走驱动暴露的 SQLite 在线备份接口（sqlite3_backup），
// 在一个读事务 / 写事务内完成，不需要停库，也不经过临时文件。
//   - Snapshot：线上库 → 内存库 → 序列化字节（远程备份直接压缩上传，本机磁盘零额外占用）
//   - Load：序列化字节 → 内存库（还原前在内存里校验）
//   - RestoreFrom：内存库 → 线上库（整体替换内容，其他连接随后看到新内容，WAL 模式不变）

import (
	"context"
	"errors"

	modernc "modernc.org/sqlite"
)

// 驱动把底层连接交给 sql.Conn.Raw，这几个接口靠方法集断言取用；
// 纯 Go 驱动没有导出连接类型，只能用接口名匹配。
type backuper interface {
	NewBackup(dstURI string) (*modernc.Backup, error)
}

type serializer interface {
	Serialize() ([]byte, error)
}

type deserializer interface {
	Deserialize([]byte) error
}

// raw 独占底层驱动连接交给 fn。调用期间 d 上不能再执行语句，fn 内也不要回头调用 d。
func (d *DB) raw(fn func(any) error) error {
	conn, err := d.db.Conn(context.Background())
	if err != nil {
		return wrap(err)
	}
	defer conn.Close()
	return conn.Raw(fn)
}

// copyInto 把 src 的内容整体复制到 dstURI 指向的库。n=-1 一次复制全部页面，
// 返回值 true 表示还有剩余页面，正常情况下一次即可完成。
func copyInto(src any, dstURI string) error {
	b, ok := src.(backuper)
	if !ok {
		return errors.New("sqlite: 驱动不支持在线备份")
	}
	bk, err := b.NewBackup(dstURI)
	if err != nil {
		return wrap(err)
	}
	for {
		done, err := bk.Step(-1)
		if err != nil {
			bk.Finish()
			return wrap(err)
		}
		if !done {
			return wrap(bk.Finish())
		}
	}
}

// Snapshot 生成当前库的一致性快照并交给 fn。字节是一份独立的 Go 切片，
// fn 返回后依然有效；峰值内存约为数据库大小。
func (d *DB) Snapshot(fn func([]byte) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errClosed
	}
	return d.raw(func(dc any) error {
		b, ok := dc.(backuper)
		if !ok {
			return errors.New("sqlite: 驱动不支持在线备份")
		}
		// 先备份到内存库再序列化：WAL 模式下的库直接序列化拿不到已提交内容。
		bk, err := b.NewBackup(":memory:")
		if err != nil {
			return wrap(err)
		}
		for {
			done, err := bk.Step(-1)
			if err != nil {
				bk.Finish()
				return wrap(err)
			}
			if !done {
				break
			}
		}
		// Commit 交出内存库的连接（Finish 会直接关掉它，那样就没法再序列化）
		dst, err := bk.Commit()
		if err != nil {
			return wrap(err)
		}
		defer dst.Close()
		ser, ok := dst.(serializer)
		if !ok {
			return errors.New("sqlite: 驱动不支持序列化")
		}
		buf, err := ser.Serialize()
		if err != nil {
			return wrap(err)
		}
		return fn(buf)
	})
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
	// 文件头第 18、19 字节标记 WAL 模式；内存库不支持 WAL，改回传统日志格式才能读。
	// 驱动会自己复制一份缓冲区，这里仍然复制，避免改动调用方传进来的切片。
	buf := make([]byte, len(b))
	copy(buf, b)
	buf[18], buf[19] = 1, 1
	if err = mem.raw(func(dc any) error {
		des, ok := dc.(deserializer)
		if !ok {
			return errors.New("sqlite: 驱动不支持反序列化")
		}
		return wrap(des.Deserialize(buf))
	}); err != nil {
		mem.Close()
		return nil, err
	}
	return mem, nil
}

// RestoreFrom 用 src 的全部内容替换当前库，在单个写事务内完成
func (d *DB) RestoreFrom(src *DB) error {
	if d.path == "" {
		return errors.New("sqlite: 内存库不能作为还原目标")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	src.mu.Lock()
	defer src.mu.Unlock()
	if d.closed || src.closed {
		return errClosed
	}
	// 目的端由驱动按路径另开一条连接写入，本进程里已打开的其他连接随后能看到新内容
	return src.raw(func(dc any) error { return copyInto(dc, d.path) })
}
