//go:build linux || darwin || freebsd

package main

import (
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	old := lockWait
	lockWait = 300 * time.Millisecond
	defer func() { lockWait = old }()
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

// 部署交接：新进程先起来等锁，旧进程放锁后在等待期内接手
func TestLockDatabaseWaitsForHandoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.lock")
	unlock, err := lockDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, unlock)
	start := time.Now()
	unlock2, err := lockDatabase(path)
	if err != nil || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("应等旧进程放锁后接手: %v %v", err, time.Since(start))
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

// --reset-admin 曾经在轮换之后继续进入服务循环，于是
// 「停服务 → 轮换 → 启动」这套标准操作会卡在中间那一步：
// 轮换命令不退出，systemctl start 永远等不到执行，
// 留下一个不受管理、还占着数据库锁的游离进程。
func TestResetAdminExitsInsteadOfServing(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "gateway.db")
	bin := filepath.Join(dir, "prism-gateway")
	build := exec.Command("go", "build", "-o", bin, "prism-gateway/cmd/gateway")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("构建失败，跳过: %v %s", err, out)
	}

	// 先建库，拿到初始令牌
	first := exec.Command(bin, "--db", db, "--reset-admin")
	out, err := first.CombinedOutput()
	if err != nil {
		t.Fatalf("首次轮换应当正常退出: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "管理员令牌已写入") {
		t.Fatalf("应当告知令牌去向: %s", out)
	}
	tokenPath := db + ".admin-token"
	before, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}

	// 再轮换一次：必须换出新令牌，并且同样自行退出
	done := make(chan error, 1)
	second := exec.Command(bin, "--db", db, "--reset-admin")
	go func() { _, e := second.CombinedOutput(); done <- e }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("二次轮换应当正常退出: %v", err)
		}
	case <-time.After(20 * time.Second):
		second.Process.Kill()
		t.Fatal("--reset-admin 没有退出，说明它又进了服务循环")
	}
	after, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("轮换后令牌应当变化")
	}
}
