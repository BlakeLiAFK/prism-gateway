//go:build linux || darwin || freebsd

package main

import (
	"path/filepath"
	"testing"
)

func TestCheckListen(t *testing.T) {
	cases := []struct {
		listen      string
		allowRemote bool
		local       bool
		wantErr     bool
	}{
		{"127.0.0.1:8080", false, true, false},
		{"localhost:8080", false, true, false},
		{"[::1]:8080", false, true, false},
		{"0.0.0.0:8080", false, false, true},      // 对外监听必须显式授权
		{"192.168.1.10:8080", false, false, true}, // 局域网同样要显式授权
		{"0.0.0.0:8080", true, false, false},
		{"127.0.0.1", false, false, true}, // 缺端口
		{"", false, false, true},
	}
	for _, c := range cases {
		local, err := checkListen(c.listen, c.allowRemote)
		if (err != nil) != c.wantErr {
			t.Fatalf("checkListen(%q,%v) err=%v，期望出错=%v", c.listen, c.allowRemote, err, c.wantErr)
		}
		if err == nil && local != c.local {
			t.Fatalf("checkListen(%q,%v) local=%v，期望 %v", c.listen, c.allowRemote, local, c.local)
		}
	}
}

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
