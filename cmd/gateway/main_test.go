//go:build linux || darwin || freebsd

package main

import (
	"path/filepath"
	"testing"
)

func TestCheckTLS(t *testing.T) {
	if err := checkTLS("", ""); err != nil {
		t.Fatalf("两者都为空应允许（明文监听）: %v", err)
	}
	if err := checkTLS("cert.pem", "key.pem"); err != nil {
		t.Fatalf("成对提供应允许: %v", err)
	}
	if checkTLS("cert.pem", "") == nil {
		t.Fatal("只给证书必须报错，否则会静默以明文启动")
	}
	if checkTLS("", "key.pem") == nil {
		t.Fatal("只给私钥必须报错")
	}
}

func TestLockDatabaseRejectsSecondProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.lock")
	unlock, err := lockDatabase(path)
	if err != nil {
		t.Fatalf("首次加锁应成功: %v", err)
	}
	if _, err = lockDatabase(path); err == nil {
		t.Fatal("同一数据库被第二个进程持有时必须拒绝")
	}
	unlock()
	unlock2, err := lockDatabase(path)
	if err != nil {
		t.Fatalf("释放后应可重新加锁: %v", err)
	}
	unlock2()
}
