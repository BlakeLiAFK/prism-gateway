package gateway

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
)

// 监听地址存在 SQLite 里，可以在管理后台修改并立即生效。
// 但「是否允许对外监听」仍由启动参数 --allow-remote 决定：
// 拿到管理令牌的人不应该因此就能把网关暴露到公网。
type Listener struct {
	mu          sync.Mutex
	srv         *http.Server
	ln          net.Listener
	allowRemote bool
	tlsConfig   *tls.Config
}

func NewListener(srv *http.Server, allowRemote bool, tlsConfig *tls.Config) *Listener {
	return &Listener{srv: srv, allowRemote: allowRemote, tlsConfig: tlsConfig}
}

func validateListenAddr(addr string) error {
	host, port, e := net.SplitHostPort(addr)
	if e != nil {
		return errors.New("监听地址必须是 host:port 形式")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return errors.New("监听端口必须是 1–65535")
	}
	if host != "" && host != "localhost" && net.ParseIP(host) == nil {
		return errors.New("监听地址必须是 IP 或 localhost，不解析域名")
	}
	return nil
}

// Loopback 供命令行判断是否需要提示 TLS 风险。
func Loopback(addr string) bool { return loopbackAddr(addr) }

func loopbackAddr(addr string) bool {
	host, _, e := net.SplitHostPort(addr)
	if e != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// sameListen 判断两个地址是否指向同一个监听点。
// ":8080"、"0.0.0.0:8080" 与 "[::]:8080" 都是通配监听，不该被当成不同地址
// 而触发一次注定失败的重新 bind。
func sameListen(a, b string) bool {
	ha, pa, e1 := net.SplitHostPort(a)
	hb, pb, e2 := net.SplitHostPort(b)
	if e1 != nil || e2 != nil || pa != pb {
		return false
	}
	norm := func(h string) string {
		if h == "" || h == "0.0.0.0" || h == "::" {
			return "*"
		}
		if ip := net.ParseIP(h); ip != nil {
			return ip.String()
		}
		return h
	}
	return norm(ha) == norm(hb)
}

// SameAddr 报告目标地址是否就是当前正在服务的地址。
func (l *Listener) SameAddr(addr string) bool { return sameListen(addr, l.Addr()) }

// Bind 校验地址并真正占用它，成功后由调用方交给 Adopt。
// 先拿到 listener 再写配置，配置与实际监听不会出现不一致。
func (l *Listener) Bind(addr string) (net.Listener, error) {
	if e := validateListenAddr(addr); e != nil {
		return nil, fail("INVALID_LISTEN", e.Error(), 400)
	}
	if !loopbackAddr(addr) && !l.allowRemote {
		return nil, fail("REMOTE_NOT_ALLOWED", "对外监听需要进程以 --allow-remote 启动；这是启动期的安全边界，不能在后台解除", 400)
	}
	ln, e := net.Listen("tcp", addr)
	if e != nil {
		return nil, fail("LISTEN_FAILED", "无法监听 "+addr+"："+e.Error(), 400)
	}
	if l.tlsConfig != nil {
		ln = tls.NewListener(ln, l.tlsConfig)
	}
	return ln, nil
}

// Adopt 接管新 listener 并关闭旧的；已建立的连接不受影响。此调用不会失败。
func (l *Listener) Adopt(ln net.Listener) {
	l.mu.Lock()
	old, srv := l.ln, l.srv
	l.ln = ln
	l.mu.Unlock()
	go func() {
		// listener 被替换或服务停止时返回的错误属于预期，不上报
		if e := srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) && !errors.Is(e, net.ErrClosed) {
			slog.Error("listener stopped", "addr", ln.Addr().String(), "err", e)
		}
	}()
	if old != nil {
		slog.Info("监听地址已切换", "from", old.Addr().String(), "to", ln.Addr().String())
		old.Close()
	}
}

func (l *Listener) Addr() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln == nil {
		return ""
	}
	return l.ln.Addr().String()
}
