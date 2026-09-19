package gateway

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// 日志级别与格式都存在 SQLite 的 settings 里，由管理后台随时调整，
// 不设启动参数——排障不应该需要重启进程。
var (
	logMu     sync.Mutex
	logLevel            = new(slog.LevelVar)
	logWriter io.Writer = os.Stderr
)

// SetupLogging 安装全局 slog handler，进程启动时调用一次。
func SetupLogging(w io.Writer) {
	logMu.Lock()
	logWriter = w
	logMu.Unlock()
	applyLogSettings(Settings{})
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// applyLogSettings 在配置加载和每次变更后调用，让后台修改立即生效。
func applyLogSettings(s Settings) {
	logLevel.Set(parseLevel(s.LogLevel))
	logMu.Lock()
	w := logWriter
	logMu.Unlock()
	opts := &slog.HandlerOptions{Level: logLevel}
	if strings.EqualFold(s.LogFormat, "json") {
		slog.SetDefault(slog.New(slog.NewJSONHandler(w, opts)))
	} else {
		slog.SetDefault(slog.New(slog.NewTextHandler(w, opts)))
	}
}
