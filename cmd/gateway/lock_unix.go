//go:build linux || darwin || freebsd

package main

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

// lockWait 是等待旧进程释放锁的上限。部署交接时旧进程收到停止信号后立即关监听、放锁，
// 再在后台把在途请求处理完；新进程在这段时间内拿到锁接手，不必等旧请求全部结束
var lockWait = 60 * time.Second

func lockDatabase(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for waited := false; ; waited = true {
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("该 SQLite 已被其他 Prism 进程使用: %w", err)
		}
		if !waited {
			slog.Info("数据库正被另一个 Prism 进程使用，等待它释放（部署交接时属正常）")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var once sync.Once
	return func() { once.Do(func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }) }, nil
}
