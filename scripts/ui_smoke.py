#!/usr/bin/env python3
"""WebUI 冒烟测试：真实启动二进制，用浏览器走完所有页面与关键交互。

前端是内嵌的 ES Module，Go 编译期检查不到它，改错了只有点到才暴露。
这个脚本就是前端重构的安全网：拆分模块、改动渲染逻辑之后必须跑一遍。

用法:
    python3 scripts/ui_smoke.py --binary ./bin/prism-gateway

Playwright 未安装时跳过并返回 0，保证 make check 在没有浏览器的机器上仍可用：
    pip install playwright && python3 -m playwright install chromium
"""
import argparse
import json
import re
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

PAGES = ['overview', 'providers', 'models', 'routes', 'playground', 'usage',
         'requests', 'sessions', 'jobs', 'keys', 'settings']


def wait_ready(base, process, logpath):
    for _ in range(100):
        if process.poll() is not None:
            raise RuntimeError('网关已退出: ' + logpath.read_text())
        try:
            with urllib.request.urlopen(base + '/healthz', timeout=5) as r:
                if r.status == 200:
                    return
        except (OSError, urllib.error.URLError):
            time.sleep(.1)
    raise RuntimeError('网关 10 秒内未就绪')


def enable_demo(base, token):
    def rpc(action, params=None):
        body = json.dumps({'action': action, 'params': params or {}}).encode()
        req = urllib.request.Request(base + '/api.json', data=body, headers={
            'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token})
        with urllib.request.urlopen(req, timeout=20) as r:
            payload = json.loads(r.read())
        assert payload['ok'], (action, payload)
        return payload['data']
    rpc('demo.enable', {'version': rpc('config.get')['version']})


def run_ui_checks(page, base, token, results):
    errors = []

    def fail(message):
        # 断言失败时把已捕获的页面错误一并带出，否则只能看到症状看不到原因
        detail = ('\n页面错误:\n  ' + '\n  '.join(errors[:8])) if errors else '\n（无 console 错误）'
        raise AssertionError(message + detail)

    page.on('console', lambda m: errors.append(m.text) if m.type == 'error' else None)
    page.on('pageerror', lambda e: errors.append(str(e)))

    page.goto(base + '/', wait_until='networkidle')
    page.fill('#login-form input[name="token"]', token)
    page.click('#login-form button[type="submit"]')
    page.wait_for_selector('.sidebar', timeout=15000)
    results.append('login + shell render')

    for name in PAGES:
        page.click(f'.nav-item[data-nav="{name}"]')
        page.wait_for_timeout(250)
        if not page.locator('main').inner_text().strip():
            fail(f'{name} 页渲染为空')
        results.append(f'page: {name}')

    # 命令面板与主题切换是全局交互，容易在模块拆分时漏掉绑定
    page.keyboard.press('Meta+k')
    page.wait_for_timeout(250)
    if page.locator('dialog.palette-dialog').count() == 0:
        fail('命令面板未打开')
    page.keyboard.press('Escape')
    page.wait_for_timeout(150)
    results.append('command palette')

    before = page.evaluate("document.documentElement.dataset.theme || ''")
    page.click('[data-action="theme"]')
    page.wait_for_timeout(200)
    if page.evaluate("document.documentElement.dataset.theme || ''") == before:
        fail('主题未切换')
    page.click('[data-action="theme"]')
    results.append('theme toggle')

    # 编辑抽屉：拆分后最容易断的一类按钮
    page.click('.nav-item[data-nav="providers"]')
    page.wait_for_timeout(250)
    page.click('[data-action="add-provider"]')
    page.wait_for_timeout(300)
    if page.locator('dialog.drawer').count() == 0:
        fail('供应商编辑抽屉未打开')
    page.click('[data-action="close-dialog"]')
    page.wait_for_timeout(200)
    results.append('provider editor drawer')

    # 设置页往返：确认表单取值与提交链路完整
    page.click('.nav-item[data-nav="settings"]')
    page.wait_for_timeout(300)
    page.select_option('select[name="log_level"]', 'warn')
    page.click('#settings-form button[type="submit"]')
    page.wait_for_timeout(600)
    page.wait_for_timeout(400)
    if page.locator('select[name="log_level"]').input_value() != 'warn':
        fail('设置保存后未回显新值')
    results.append('settings save round trip')

    # 移动端宽度不得产生横向滚动
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(300)
    overflow = page.evaluate('document.documentElement.scrollWidth - document.documentElement.clientWidth')
    if overflow > 0:
        fail(f'390px 宽度下横向溢出 {overflow}px')
    results.append('responsive 390px')

    if errors:
        raise AssertionError('页面产生 console 错误: ' + ' | '.join(errors[:5]))
    results.append('zero console errors')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./bin/prism-gateway')
    parser.add_argument('--headed', action='store_true')
    args = parser.parse_args()
    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        print('playwright 未安装，跳过 WebUI 冒烟；'
              '安装：pip install playwright && python3 -m playwright install chromium')
        return 0
    binary = Path(args.binary).resolve()
    if not binary.is_file():
        raise SystemExit('先构建：make build')
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    base = f'http://127.0.0.1:{port}'
    results = []
    with tempfile.TemporaryDirectory(prefix='prism-ui-') as directory:
        directory = Path(directory)
        logpath = directory / 'server.log'
        log = logpath.open('w')
        process = subprocess.Popen(
            [str(binary), '--db', str(directory / 'gateway.db'), '--listen', f'127.0.0.1:{port}'],
            stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
        try:
            wait_ready(base, process, logpath)
            token = re.search(r'prism_admin_[A-Za-z0-9_-]+', logpath.read_text()).group()
            enable_demo(base, token)
            with sync_playwright() as p:
                browser = p.chromium.launch(headless=not args.headed)
                page = browser.new_page(viewport={'width': 1280, 'height': 900})
                try:
                    run_ui_checks(page, base, token, results)
                finally:
                    browser.close()
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    process.kill()
            log.close()
    print(json.dumps({'passed': results}, ensure_ascii=False, indent=2))
    return 0


if __name__ == '__main__':
    sys.exit(main())
