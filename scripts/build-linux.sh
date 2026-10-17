#!/bin/sh
# 交叉编译 Linux 发布产物，默认 linux/amd64，产物完全静态。
#
# Linux 构建是 CGo 且静态链接 SQLite（见 internal/sqlite 的 -l:libsqlite3.a），
# 所以只设 GOOS/GOARCH 不够：还需要目标平台的 C 交叉工具链，以及为该目标编译的
# 静态 libsqlite3.a。这里用 musl 交叉工具链生成不依赖目标机 libc 的静态二进制，
# 不需要 Docker，也不需要登录构建机。SQLite 源码版本与校验和固定，构建可复现。
set -eu
cd "$(dirname "$0")/.."

TARGET_OS=${TARGET_OS:-linux}
TARGET_ARCH=${TARGET_ARCH:-amd64}
case "$TARGET_ARCH" in
amd64) MUSL=x86_64-linux-musl ;;
arm64) MUSL=aarch64-linux-musl ;;
*)
    echo "不支持的架构：${TARGET_ARCH}（可用 amd64、arm64）" >&2
    exit 1
    ;;
esac
CC=${CC:-$MUSL-gcc}
AR=${AR:-$MUSL-ar}
# 换 SQLite 版本时这两个值必须一起改，否则校验失败并中止。
SQLITE_DIR=sqlite-amalgamation-3450100
SQLITE_ZIP=$SQLITE_DIR.zip
SQLITE_URL=https://www.sqlite.org/2024/$SQLITE_ZIP
SQLITE_SHA256=5592243caf28b2cdef41e6ab58d25d653dfc53deded8450eb66072c929f030c4
CACHE=${PRISM_CROSS_CACHE:-${TMPDIR:-/tmp}/prism-cross-$TARGET_OS-$TARGET_ARCH}
# 产物名带上源码里的版本号，与 dist 里已有的历史产物一致；取不到就退回不带版本。
VERSION=$(sed -n 's/^const Version = "\([^"]*\)"$/\1/p' internal/gateway/types.go | head -1)
OUT=${OUT:-dist/prism-gateway-${VERSION:+$VERSION-}$TARGET_OS-$TARGET_ARCH}

for tool in "$CC" "$AR" unzip curl; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "缺少 ${tool}。macOS 可安装 musl 交叉工具链：brew install filosottile/musl-cross/musl-cross" >&2
        echo '也可用 CC / AR 指定其他能静态链接目标平台 SQLite 的交叉工具链。' >&2
        exit 1
    }
done

hashline() {
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1"; else sha256sum "$1"; fi
}

mkdir -p "$CACHE" "$(dirname "$OUT")"
if [ ! -f "$CACHE/$SQLITE_ZIP" ]; then
    echo "下载 SQLite 源码：$SQLITE_URL"
    curl -fsSL -o "$CACHE/$SQLITE_ZIP.part" "$SQLITE_URL"
    mv "$CACHE/$SQLITE_ZIP.part" "$CACHE/$SQLITE_ZIP"
fi
got=$(hashline "$CACHE/$SQLITE_ZIP" | awk '{print $1}')
[ "$got" = "$SQLITE_SHA256" ] || {
    echo "SQLite 源码校验和不匹配：$got" >&2
    exit 1
}
[ -f "$CACHE/$SQLITE_DIR/sqlite3.c" ] || (cd "$CACHE" && unzip -oq "$SQLITE_ZIP")

# 静态库按目标平台编译一次后复用；源码更新（换版本）时重新生成。
if [ ! -f "$CACHE/libsqlite3.a" ] || [ "$CACHE/$SQLITE_DIR/sqlite3.c" -nt "$CACHE/libsqlite3.a" ]; then
    echo '编译目标平台的静态 libsqlite3.a'
    "$CC" -O2 -c "$CACHE/$SQLITE_DIR/sqlite3.c" -o "$CACHE/sqlite3.o"
    rm -f "$CACHE/libsqlite3.a"
    "$AR" rcs "$CACHE/libsqlite3.a" "$CACHE/sqlite3.o"
fi

echo "构建 ${TARGET_OS}/${TARGET_ARCH}（静态链接 SQLite）"
CC="$CC" CGO_ENABLED=1 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" \
    CGO_CFLAGS="-I$CACHE/$SQLITE_DIR" CGO_LDFLAGS="-L$CACHE -static" \
    GOPROXY=off go build -trimpath -ldflags='-s -w -linkmode external -extldflags -static' -o "$OUT" ./cmd/gateway

# 静态链接是这条路径存在的理由：动态链接的产物换台机器就可能跑不起来，必须当场拦住。
if command -v file >/dev/null 2>&1; then
    file "$OUT"
    file "$OUT" | grep -q 'statically linked' || {
        echo '产物不是静态链接，请检查交叉工具链与 CGO_LDFLAGS' >&2
        exit 1
    }
fi
hashline "$OUT" >"$OUT.sha256"
printf '\n产物：%s\n校验和：%s\n' "$OUT" "$OUT.sha256"
