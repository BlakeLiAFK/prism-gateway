#!/bin/sh
# 交叉编译 Linux 发布产物，默认 linux/amd64，产物完全静态。
#
# SQLite 走纯 Go 实现（见 internal/sqlite），所以不需要目标平台的 C 工具链，
# 也不需要 Docker：一条 GOOS/GOARCH 的 go build 就够，产物默认静态链接。
set -eu
cd "$(dirname "$0")/.."

TARGET_OS=${TARGET_OS:-linux}
TARGET_ARCH=${TARGET_ARCH:-amd64}
# 产物名带上源码里的版本号，与 dist 里已有的历史产物一致；取不到就退回不带版本。
VERSION=$(sed -n 's/^const Version = "\([^"]*\)"$/\1/p' internal/gateway/types.go | head -1)
OUT=${OUT:-dist/prism-gateway-${VERSION:+$VERSION-}$TARGET_OS-$TARGET_ARCH}

echo "构建 ${TARGET_OS}/${TARGET_ARCH}（纯 Go，无 CGo）"
mkdir -p "$(dirname "$OUT")"
CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" \
    go build -trimpath -ldflags='-s -w' -o "$OUT" ./cmd/gateway

# 静态链接是这条路径存在的理由：动态链接的产物换台机器就可能跑不起来，必须当场拦住。
if command -v file >/dev/null 2>&1; then
    file "$OUT"
    file "$OUT" | grep -q 'statically linked' || {
        echo '产物不是静态链接，请检查 CGO_ENABLED 是否为 0' >&2
        exit 1
    }
fi

if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$OUT" >"$OUT.sha256"
else
    sha256sum "$OUT" >"$OUT.sha256"
fi
printf '\n产物：%s\n校验和：%s\n' "$OUT" "$OUT.sha256"
