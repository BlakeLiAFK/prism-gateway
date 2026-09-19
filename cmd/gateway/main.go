package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"prism-gateway/internal/gateway"
	"prism-gateway/internal/webui"
	"strings"
	"syscall"
	"time"
)

// checkTLS 要求证书与私钥成对出现，避免只配一半就以明文启动。
func checkTLS(cert, key string) error {
	if (cert == "") != (key == "") {
		return errors.New("--tls-cert 与 --tls-key 必须一起提供")
	}
	return nil
}

func fatal(v any) {
	slog.Error("startup failed", "err", v)
	os.Exit(1)
}

// flagGiven 区分「用户显式指定」与「仅仅是默认值」。
func flagGiven(name string) bool {
	given := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

func main() {
	db := flag.String("db", "./data/gateway.db", "SQLite 数据库路径；同目录 .key 为凭证加密主密钥")
	listen := flag.String("listen", "", "临时覆盖监听地址，用于救援；常规修改请在管理后台进行")
	reset := flag.Bool("reset-admin", false, "轮换管理员令牌并注销全部管理会话")
	version := flag.Bool("version", false, "显示版本")
	allowRemote := flag.Bool("allow-remote", false, "允许把监听地址设为非 loopback；公网必须使用 TLS")
	cert := flag.String("tls-cert", "", "TLS 证书文件（可选）")
	tlsKey := flag.String("tls-key", "", "TLS 私钥文件（可选）")
	flag.Parse()
	gateway.SetupLogging(os.Stderr)
	if *version {
		fmt.Println("Prism Gateway " + gateway.Version)
		return
	}
	if err := checkTLS(*cert, *tlsKey); err != nil {
		fatal(err)
	}
	var tlsConfig *tls.Config
	if *cert != "" {
		pair, err := tls.LoadX509KeyPair(*cert, *tlsKey)
		if err != nil {
			fatal(err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}
	if err := os.MkdirAll(filepath.Dir(*db), 0700); err != nil {
		fatal(err)
	}
	unlock, err := lockDatabase(*db + ".lock")
	if err != nil {
		fatal(err)
	}
	defer unlock()
	s, err := gateway.OpenStore(*db)
	if err != nil {
		fatal(err)
	}
	defer s.DB.Close()
	token, err := s.AdminToken(*reset)
	if err != nil {
		fatal(err)
	}
	// 监听地址来自数据库；--listen 只是本次启动的临时覆盖，不写回配置
	addr := s.Config().Settings.Listen
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	if flagGiven("listen") {
		addr = *listen
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	app := gateway.NewApp(ctx, s, webui.Handler())
	server := &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	app.Listen = gateway.NewListener(server, *allowRemote, tlsConfig)
	ln, err := app.Listen.Bind(addr)
	if err != nil {
		fatal(err)
	}
	app.Listen.Adopt(ln)
	scheme := "http"
	if tlsConfig != nil {
		scheme = "https"
	}
	fmt.Printf("\n  PRISM GATEWAY  v%s\n  %s://%s\n  管理入口: POST /api.json\n  数据库: %s\n", gateway.Version, scheme, addr, *db)
	if token != "" {
		fmt.Printf("\n  首次/新管理员令牌（只显示一次，请妥善保存）：\n\n  %s\n\n", token)
	} else {
		fmt.Println("  使用已保存的管理员令牌登录；遗失时停止服务再运行 --reset-admin")
	}
	if !gateway.Loopback(addr) && tlsConfig == nil {
		fmt.Println("  警告：当前监听未启用 TLS。只在可信网络或 HTTPS 代理后使用；不要直接公开。")
	}
	fmt.Println("  监听地址、日志与其余运行设置都可在管理后台修改。按 Ctrl+C 停止。\n" + strings.Repeat("─", 62))
	<-ctx.Done()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	if err = server.Shutdown(shutCtx); err != nil {
		server.Close()
	}
	cancel()
	app.Wait()
	slog.Info("Prism 已安全停止")
}
