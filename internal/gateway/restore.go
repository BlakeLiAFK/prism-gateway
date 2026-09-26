package gateway

// 在线还原：把本机或 R2 上的备份载入内存库，在内存里完成校验、迁移、写回需保留的状态，
// 再用 sqlite3_backup 一次性写入线上库（单个写事务），最后重载配置、清空缓存。不替换文件、不需要重启：
// 交接部署时旧进程可能还开着库，替换文件会让它关闭时按路径删掉新库的 WAL。
// 还原前先在本机生成一份安全快照（gateway-before-restore-*.db），还原错了可以用它再还原回来。
// 在途请求结束时的写入找不到对应记录，按设计丢弃；备份里未结束的请求按重启处理，保留预留额度。

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"prism-gateway/internal/sqlite"
)

// 还原时沿用当前值的状态：管理员令牌与会话（否则令牌变回旧的，管理员会被锁在外面）、
// R2 配置（还原旧库后仍能找到远程备份）、定时任务状态（避免日报等重复推送）
var preservedMeta = []string{"admin_digest", "r2_config", "schedule_state"}

func init() {
	consoleActions["backup.restore"] = func(a *App, _ string, p Object) (any, error) { return a.restoreBackup(p) }
}

func (a *App) restoreBackup(p Object) (any, error) {
	if !boolean(p, "confirm") {
		return nil, fail("INVALID_PARAMS", "还原会用备份覆盖当前全部数据，需传 confirm: true", 400)
	}
	source, name := str(p, "source"), str(p, "name")
	data, err := a.fetchBackup(source, name)
	if err != nil {
		return nil, err
	}
	src, err := sqlite.Load(data)
	data = nil
	if err != nil {
		return nil, fail("RESTORE_INVALID", "备份无法读取："+err.Error(), 400)
	}
	defer src.Close()
	if err = a.prepareRestore(src); err != nil {
		return nil, fail("RESTORE_INVALID", "备份无法使用："+err.Error(), 400)
	}
	// 与定时任务互斥：不能一边还原一边写定时任务状态或上传备份
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	safety, _, err := a.Store.Backup("before-restore-")
	if err != nil {
		return nil, fmt.Errorf("还原前的安全快照生成失败，已中止还原：%w", err)
	}
	if err = a.carryState(src); err != nil {
		return nil, err
	}
	if err = a.Store.DB.RestoreFrom(src); err != nil {
		return nil, fmt.Errorf("写入失败，数据库未改动：%w", err)
	}
	if err = a.Store.load(); err != nil {
		return nil, fmt.Errorf("已还原但重载配置失败，请用安全快照 %s 再次还原：%w", filepath.Base(safety), err)
	}
	a.afterRestore()
	a.audit("backup.restore", source+":"+name)
	return Object{"restored_from": source + ":" + name, "safety_backup": filepath.Base(safety), "config_version": a.Store.Config().Version}, nil
}

// fetchBackup 读出备份的原始数据库字节：本机备份直接读文件，R2 备份下载后在内存里解压
func (a *App) fetchBackup(source, name string) ([]byte, error) {
	switch source {
	case "local":
		if name != filepath.Base(name) || !strings.HasPrefix(name, "gateway-") || !strings.HasSuffix(name, ".db") {
			return nil, fail("INVALID_PARAMS", "本机备份文件名不正确", 400)
		}
		data, err := os.ReadFile(filepath.Join(a.Store.backupDir(), name))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fail("NOT_FOUND", "本机备份不存在："+name, 404)
		}
		return data, err
	case "remote":
		cl, prefix, err := a.r2Client()
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(name, prefix+"gateway-") || !strings.HasSuffix(name, remoteObjectSuffix) {
			return nil, fail("INVALID_PARAMS", "只能还原网关上传的远程备份", 400)
		}
		ctx, cancel := context.WithTimeout(a.Context, 10*time.Minute)
		defer cancel()
		gz, err := cl.get(ctx, name)
		if err != nil {
			return nil, fail("R2_FAILED", "下载备份失败："+err.Error(), 502)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			return nil, fail("RESTORE_INVALID", "备份不是有效的 gzip 文件", 400)
		}
		return io.ReadAll(io.LimitReader(zr, 4<<30))
	}
	return nil, fail("INVALID_PARAMS", "source 只能是 local 或 remote", 400)
}

// prepareRestore 在内存库上校验并迁移：完整性、是否 Prism 数据库、schema 版本、
// 能否用当前主密钥解密全部上游凭证并载入配置。全部通过后才会动线上库。
func (a *App) prepareRestore(src *sqlite.DB) error {
	rows, err := src.Query("PRAGMA integrity_check")
	if err != nil || len(rows) == 0 || rows[0].String("integrity_check") != "ok" {
		return fmt.Errorf("完整性检查未通过")
	}
	if rows, err = src.Query("SELECT value FROM meta WHERE key='schema_version'"); err != nil || len(rows) != 1 {
		return fmt.Errorf("不是 Prism Gateway 的数据库")
	}
	tmp := &Store{DB: src, aead: a.Store.aead, KeyPath: a.Store.KeyPath}
	if err = tmp.migrate(); err != nil {
		return err
	}
	return tmp.load()
}

// carryState 把需沿用的当前状态写进待还原的内存库
func (a *App) carryState(src *sqlite.DB) error {
	for _, k := range preservedMeta {
		rows, err := a.Store.DB.Query("SELECT value FROM meta WHERE key=?", k)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			err = src.Exec("DELETE FROM meta WHERE key=?", k)
		} else {
			err = src.Exec("INSERT INTO meta VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, rows[0].String("value"))
		}
		if err != nil {
			return err
		}
	}
	sessions, err := a.Store.DB.Query("SELECT digest, csrf, expires_at FROM admin_sessions")
	if err != nil {
		return err
	}
	if err = src.Exec("DELETE FROM admin_sessions"); err != nil {
		return err
	}
	for _, s := range sessions {
		if err = src.Exec("INSERT INTO admin_sessions VALUES (?,?,?)", s.String("digest"), s.String("csrf"), s.Int("expires_at")); err != nil {
			return err
		}
	}
	return nil
}

// afterRestore 清掉由旧数据算出的内存缓存
func (a *App) afterRestore() {
	a.Engine.forgetKeys()
	a.Engine.keyMu.Lock()
	a.Engine.spend = map[string]keySpendEntry{}
	a.Engine.keyMu.Unlock()
	a.statsMu.Lock()
	a.statsCache = nil
	a.statsMu.Unlock()
}
