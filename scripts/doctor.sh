#!/bin/sh
# 构建环境自检。纯 Go 实现不需要 C 编译器，也不需要系统 SQLite。
set -eu
cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || { echo 'Go 1.25+ is required.' >&2; exit 1; }
printf '\nGo: '; go version
printf 'OS: '; go env GOOS GOARCH
printf '\nChecking the SQLite driver...\n'
if ! CGO_ENABLED=0 go test ./internal/sqlite -run TestSQLite -count=1; then
  printf '\nSQLite 驱动不可用。依赖模块需要能下载（首次构建联网），\n离线环境请先执行 go mod download。\n' >&2
  exit 1
fi
printf '\nReady. Run: go build -o prism-gateway ./cmd/gateway\n'
