package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
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

func main() {
	db := flag.String("db", "./data/gateway.db", "SQLite 数据库路径；同目录 .key 为凭证加密主密钥")
	listen := flag.String("listen", "127.0.0.1:8080", "监听地址")
	demo := flag.Bool("demo", false, "添加明确标记的本地演示 Provider（不调用云端）")
	reset := flag.Bool("reset-admin", false, "轮换管理员令牌并注销全部管理会话")
	version := flag.Bool("version", false, "显示版本")
	allowRemote := flag.Bool("allow-remote", false, "确认允许非 loopback 监听；公网必须使用 TLS")
	cert := flag.String("tls-cert", "", "TLS 证书文件（可选）")
	tlsKey := flag.String("tls-key", "", "TLS 私钥文件（可选）")
	flag.Parse()
	if *version {
		fmt.Println("Prism Gateway " + gateway.Version)
		return
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatal(err)
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !local && !*allowRemote {
		log.Fatal("非 loopback 监听需要显式 --allow-remote；请启用 TLS 或置于可信 HTTPS 反向代理后")
	}
	if (*cert == "") != (*tlsKey == "") {
		log.Fatal("--tls-cert 与 --tls-key 必须一起提供")
	}
	if err = os.MkdirAll(filepath.Dir(*db), 0700); err != nil {
		log.Fatal(err)
	}
	unlock, err := lockDatabase(*db + ".lock")
	if err != nil {
		log.Fatal(err)
	}
	defer unlock()
	s, err := gateway.OpenStore(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer s.DB.Close()
	token, err := s.AdminToken(*reset)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	app := gateway.NewApp(ctx, s, webui.Handler())
	if *demo {
		if _, err = app.EnableDemo(s.Config().Version); err != nil {
			log.Fatal(err)
		}
	}
	server := &http.Server{Addr: *listen, Handler: app, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	fmt.Printf("\n  PRISM GATEWAY  v%s\n  %s://%s\n  管理入口: POST /api.json\n  数据库: %s\n", gateway.Version, scheme, *listen, *db)
	if token != "" {
		fmt.Printf("\n  首次/新管理员令牌（只显示一次，请妥善保存）：\n\n  %s\n\n", token)
	} else {
		fmt.Println("  使用已保存的管理员令牌登录；遗失时停止服务再运行 --reset-admin")
	}
	if !local && *cert == "" {
		fmt.Println("  警告：当前监听未启用 TLS。只在可信网络或 HTTPS 代理后使用；不要直接公开。")
	}
	if *demo {
		fmt.Println("  本地演示已启用：不是真实 AI，不产生云端费用。")
	}
	fmt.Println("  不需要 YAML / npm / 独立前端服务。按 Ctrl+C 停止。\n" + strings.Repeat("─", 62))
	done := make(chan error, 1)
	go func() {
		if *cert != "" {
			done <- server.ListenAndServeTLS(*cert, *tlsKey)
		} else {
			done <- server.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-done:
		if err != http.ErrServerClosed {
			log.Printf("server: %v", err)
		}
		cancel()
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	if err = server.Shutdown(shutCtx); err != nil {
		server.Close()
	}
	cancel()
	app.Wait()
	log.Println("Prism 已安全停止")
}
