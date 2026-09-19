#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || { echo 'Go 1.23+ is required.' >&2; exit 1; }
command -v cc >/dev/null 2>&1 || { echo 'A C compiler is required. macOS: xcode-select --install; Ubuntu: apt install build-essential libsqlite3-dev' >&2; exit 1; }
printf '\nGo: '; go version
printf 'OS: '; go env GOOS GOARCH
printf '\nChecking the CGo / SQLite binding...\n'
if ! GOPROXY=off CGO_ENABLED=1 go test ./internal/sqlite -run TestSQLite -count=1; then
  printf '\nSQLite headers / libraries are required.\nmacOS: finish Xcode Command Line Tools installation.\nUbuntu: sudo apt install build-essential libsqlite3-dev\nWindows: use WSL2 Ubuntu.\n' >&2
  exit 1
fi
printf '\nReady. Run: CGO_ENABLED=1 go build -o prism-gateway ./cmd/gateway\n'
