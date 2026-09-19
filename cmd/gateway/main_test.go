//go:build linux || darwin || freebsd

package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func TestFlagGivenDistinguishesDefaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("listen", "127.0.0.1:8080", "")
	fs.String("db", "./data/gateway.db", "")
	if err := fs.Parse([]string{"--db", "./other.db"}); err != nil {
		t.Fatal(err)
	}
	// 只有显式写在命令行上的参数才算「给了」；默认值不算，
	// 否则数据库里的监听地址永远会被默认值盖掉
	if flagGiven(fs, "listen") {
		t.Fatal("未指定的参数不应判为已给出")
	}
	if !flagGiven(fs, "db") {
		t.Fatal("显式指定的参数应判为已给出")
	}
	if flagGiven(fs, "no-such-flag") {
		t.Fatal("不存在的参数不应判为已给出")
	}
}

func TestWriteAdminTokenIsOwnerOnly(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gateway.db")
	const token = "prism_admin_example_value_for_test"
	path, err := writeAdminToken(db, token)
	if err != nil {
		t.Fatal(err)
	}
	if path != db+".admin-token" {
		t.Fatalf("路径应挨着数据库: %s", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 令牌等同于完整管理权限，同组或其他用户都不该读到
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("权限应为 0600，实得 %04o", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != token {
		t.Fatalf("内容不符: %q", string(data))
	}
}
