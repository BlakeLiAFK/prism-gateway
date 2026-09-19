#!/bin/sh
# 覆盖率门禁。没有 CI 的情况下，这是唯一阻止覆盖率悄悄滑落的机制。
# 阈值留了几个点的余量，避免正常改动造成的抖动误报。
set -e
PROFILE="${TMPDIR:-/tmp}/prism-cover.out"
CGO_ENABLED=1 go test -count=1 -coverprofile="$PROFILE" ./... >/dev/null

fail=0
check() {
    pkg="$1"; want="$2"
    got=$(CGO_ENABLED=1 go test -count=1 -cover "$pkg" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="coverage:") printf "%.1f", substr($(i+1),1,length($(i+1))-1)}')
    [ -z "$got" ] && got=0
    if [ "$(echo "$got < $want" | bc -l)" = "1" ]; then
        echo "  FAIL  $pkg  $got% < $want%"
        fail=1
    else
        echo "  ok    $pkg  $got% (>= $want%)"
    fi
}

echo "覆盖率门禁："
check ./internal/gateway 78
check ./internal/sqlite 75
check ./internal/webui 60

total=$(go tool cover -func="$PROFILE" | awk '/^total:/ {print substr($3,1,length($3)-1)}')
if [ "$(echo "$total < 75" | bc -l)" = "1" ]; then
    echo "  FAIL  总体      $total% < 75%"
    fail=1
else
    echo "  ok    总体      $total% (>= 75%)"
fi

[ "$fail" = "0" ] || { echo "覆盖率低于阈值，提交被拒绝。补测试，或在确有理由时调整 scripts/cover.sh"; exit 1; }
