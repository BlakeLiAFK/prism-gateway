#!/bin/sh
# 在目标主机交接已上传的二进制；健康检查失败时恢复原版本。
set -eu
NEW=${1:?需要新二进制路径}
VERSION=${2:?需要预期版本}
BIN=${PRISM_BIN:-/usr/local/bin/prism-gateway}
PREFIX=${PRISM_SERVICE_PREFIX:-prism-gateway}
HEALTH=${PRISM_HEALTH_URL:-http://127.0.0.1:8091/healthz}
LOCK=${PRISM_DEPLOY_LOCK:-/run/prism-deploy.lock}
BACKUP=${PRISM_BACKUP_COMMAND:-/usr/local/bin/prism-backup.sh}
TRIALS=${PRISM_HEALTH_TRIALS:-60}
DELAY=${PRISM_HEALTH_DELAY:-1}
case "$VERSION" in ''|*[!0-9.]*) echo '版本号格式无效' >&2; exit 2;; esac
[ -x "$NEW" ] && [ -x "$BIN" ]
[ "$("$NEW" -version | awk '{print $3}')" = "$VERSION" ]
mkdir "$LOCK" || { echo '另一个部署正在执行或锁需要检查' >&2; exit 1; }
changed=0
health() {
    count=0
    while [ "$count" -lt "$TRIALS" ]; do
        if curl -fsS --max-time 2 "$HEALTH" 2>/dev/null | grep -Fq "\"version\":\"$1\""; then return 0; fi
        count=$((count + 1))
        sleep "$DELAY"
    done
    return 1
}
finish() {
    result=$?
    trap - EXIT HUP INT TERM
    if [ "$changed" = 1 ]; then
        echo '新版本未通过验证，正在回滚' >&2
        systemctl stop "$NEXT" || echo '停止新实例失败，继续恢复旧二进制与实例' >&2
        restored=0
        if cp -p "$SAVED" "$BIN.rollback" && mv -f "$BIN.rollback" "$BIN"; then
            restored=1
            systemctl start "$CUR" || echo '启动旧实例命令失败，继续检查健康状态' >&2
        fi
        if [ "$restored" = 1 ] && health "$OLD"; then
            systemctl enable "$CUR" >/dev/null && systemctl disable "$NEXT" >/dev/null || result=1
            echo "已恢复版本 $OLD" >&2
        else
            echo '回滚未恢复健康，请检查服务状态与保留的旧二进制' >&2
        fi
        result=1
    fi
    rm -f "$BIN.new" "$BIN.rollback"
    rmdir "$LOCK"
    exit "$result"
}
trap finish EXIT
trap 'exit 130' HUP INT TERM
if systemctl is-active --quiet "$PREFIX@a"; then CUR="$PREFIX@a"; NEXT="$PREFIX@b"
elif systemctl is-active --quiet "$PREFIX@b"; then CUR="$PREFIX@b"; NEXT="$PREFIX@a"
else echo '没有健康的当前实例，拒绝交接' >&2; exit 1; fi
if systemctl is-active --quiet "$NEXT"; then echo '两个实例同时活动，拒绝交接' >&2; exit 1; fi
[ "$(systemctl is-active "$NEXT" || true)" != deactivating ] || { echo '备用实例仍在排空，请稍后重试' >&2; exit 1; }
OLD=$("$BIN" -version | awk '{print $3}')
case "$OLD" in ''|*[!0-9.]*) echo '旧版本号无效' >&2; exit 1;; esac
[ "$VERSION" != "$OLD" ] || { echo '拒绝同版本交接，请使用新的版本号' >&2; exit 1; }
SAVED="$BIN.$OLD"
health "$OLD" || { echo '当前实例健康检查失败' >&2; exit 1; }
"$BACKUP"
cp -p "$BIN" "$SAVED"
install -m 0755 "$NEW" "$BIN.new"
# 标记必须早于替换，任何后续失败都回滚。
changed=1
mv -f "$BIN.new" "$BIN"
systemctl start --no-block "$NEXT"
systemctl stop --no-block "$CUR"
health "$VERSION" || { echo '新版本健康检查失败' >&2; exit 1; }
systemctl enable "$NEXT" >/dev/null
systemctl disable "$CUR" >/dev/null
changed=0
echo "已交接 $OLD -> $VERSION"
