#!/usr/bin/env python3
"""隔离验证发布成功、切换失败回滚和备份失败不替换。"""
import os
from pathlib import Path
import subprocess
import tempfile

SCRIPT = Path(__file__).resolve().parents[1] / 'deploy/activate-release.sh'

def executable(path, content):
    path.write_text('#!/bin/sh\n' + content)
    path.chmod(0o755)

def check(mode):
    with tempfile.TemporaryDirectory() as root:
        root = Path(root)
        binary = root / 'gateway'
        new = root / 'candidate'
        executable(binary, 'echo "Prism Gateway 1.0.0"\n')
        version = '1.0.0' if mode == 'same-version' else '1.1.0'
        executable(new, 'echo "Prism Gateway ' + version + '"\n')
        executable(root / 'backup', '[ "$CASE" != backup-fail ]\n')
        executable(root / 'systemctl', '''case "$1" in
is-active) [ "${3:-$2}" = prism-gateway@a ];;
stop) [ "$CASE" != stop-fail ] || [ "${3:-$2}" != prism-gateway@b ];;
start) [ "$CASE" != start-fail ] || [ "${3:-$2}" = prism-gateway@a ];;
*) exit 0;;
esac
''')
        executable(root / 'curl', '''v=$($PRISM_BIN -version | awk '{print $3}')
case "$CASE" in health-fail|stop-fail) [ "$v" = 1.0.0 ] || exit 22;; esac
printf '{"ok":true,"version":"%s"}' "$v"
''')
        env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                   PRISM_BIN=str(binary), PRISM_BACKUP_COMMAND=str(root / 'backup'),
                   PRISM_DEPLOY_LOCK=str(root / 'lock'), PRISM_HEALTH_TRIALS='1',
                   PRISM_HEALTH_DELAY='0', CASE=mode)
        result = subprocess.run(['sh', str(SCRIPT), str(new), version], env=env, capture_output=True, text=True)
        version = subprocess.check_output([str(binary), '-version'], text=True).strip()
        expected = '1.1.0' if mode == 'success' else '1.0.0'
        assert version.endswith(expected), (mode, version, result.stderr)
        assert (result.returncode == 0) == (mode == 'success'), (mode, result.stderr)
        assert not (root / 'lock').exists(), mode
        if mode in ('health-fail', 'start-fail', 'stop-fail'):
            assert '已恢复版本 1.0.0' in result.stderr, result.stderr
        print(mode + ': passed')

if __name__ == '__main__':
    for case in ('success', 'health-fail', 'start-fail', 'backup-fail', 'stop-fail', 'same-version'):
        check(case)
