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
    # 拖拽排序至少要两个路由才能验证
    cfg = rpc('config.get')
    first = cfg['routes'][0]
    second = dict(first, id='demo-second', name='Second route', sort=99)
    rpc('route.save', {'version': cfg['version'], 'id': 'demo-second', 'route': second})
    return rpc


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

    # 模型编辑器：确认新增的能力开关渲染出来且可读取
    page.click('.nav-item[data-nav="models"]')
    page.wait_for_timeout(250)
    page.evaluate("document.querySelector('[data-action=\"add-model\"], [data-action=\"edit-model\"]')?.click()")
    page.wait_for_timeout(400)
    if page.locator('input[name="drop_reasoning"]').count() == 0:
        fail('模型编辑器缺少 drop_reasoning 开关')
    if page.locator('input[name="drop_reasoning"]').is_checked():
        fail('drop_reasoning 默认必须关闭')
    page.evaluate("document.querySelector('[data-action=\"close-dialog\"]')?.click()")
    page.wait_for_timeout(200)
    results.append('model editor capability toggles')

    # 路由编辑器的候选搜索：模型多起来之后这是唯一能用的定位方式
    page.click('.nav-item[data-nav="routes"]')
    page.wait_for_timeout(250)
    page.evaluate("document.querySelector('[data-action=\"add-route\"]')?.click()")
    page.wait_for_timeout(400)
    if page.locator('#candidate-search').count() == 0:
        fail('路由编辑器缺少候选搜索框')
    total = page.locator('.candidate-row').count()
    if total == 0:
        fail('候选列表为空，无法验证搜索')
    # 先勾上第一个，确认它在搜索无匹配时依然可见
    page.locator('.candidate-row input[name="candidate"]').first.check()
    page.fill('#candidate-search', 'zzz-no-such-model-zzz')
    page.wait_for_timeout(250)
    visible = page.evaluate("[...document.querySelectorAll('.candidate-row')].filter(r=>r.offsetParent!==null).length")
    if visible != 1:
        dbg = page.evaluate("[...document.querySelectorAll('.candidate-row')].map(r=>[r.dataset.search,r.hidden,r.offsetParent!==null,r.querySelector('input[name=candidate]').checked])")
        fail(f'搜索无匹配时应只剩已勾选的 1 行，实得 {visible} :: {dbg}')
    page.fill('#candidate-search', '')
    page.wait_for_timeout(250)
    visible = page.evaluate("[...document.querySelectorAll('.candidate-row')].filter(r=>r.offsetParent!==null).length")
    if visible != total:
        fail(f'清空搜索应恢复全部 {total} 行，实得 {visible}')
    page.evaluate("document.querySelector('[data-action=\"close-dialog\"]')?.click()")
    page.wait_for_timeout(200)
    results.append('route candidate search')

    # 顺序由拖拽产生，不该再让人手填数字
    if page.locator('#f-sort').count() != 0:
        fail('路由编辑器不应再暴露「列表排序」输入框')
    page.evaluate("document.querySelector('[data-action=\"close-dialog\"]')?.click()")
    page.wait_for_timeout(200)
    order = page.evaluate("[...document.querySelectorAll('.route-select p.mono')].map(e=>e.textContent)")
    if len(order) < 2:
        fail(f'需要至少两个路由才能验证拖拽，实得 {order}')
    page.locator('.route-list button').nth(1).drag_to(page.locator('.route-list button').nth(0))
    page.wait_for_timeout(900)
    after = page.evaluate("[...document.querySelectorAll('.route-select p.mono')].map(e=>e.textContent)")
    if after[0] != order[1]:
        fail(f'拖拽后第一位应变成 {order[1]}，实得 {after}')
    # 真正落库了才算数：重新加载页面，顺序是从服务端重新拉回来的
    page.reload()
    page.wait_for_timeout(1200)
    page.click('.nav-item[data-nav="routes"]')
    page.wait_for_timeout(500)
    reloaded = page.evaluate("[...document.querySelectorAll('.route-select p.mono')].map(e=>e.textContent)")
    if reloaded != after:
        fail(f'拖拽结果没有落库: 拖后 {after}，重载后 {reloaded}')
    results.append('route drag reorder')

    # 调试台的 System One 面板：载荷形状和对话协议完全不同，必须单独验证
    page.click('.nav-item[data-nav="playground"]')
    page.wait_for_timeout(300)
    page.click('[data-action="play-protocol"][data-value="systemone"]')
    page.wait_for_timeout(350)
    if page.locator('#play-state').count() == 0:
        fail('System One 面板缺少 state 输入')
    if page.locator('#play-prompt').count() != 0:
        fail('切到 System One 后不应再显示对话协议的 Prompt 框')
    before = page.locator('.question-card').count()
    page.click('[data-action="add-question"]')
    page.wait_for_timeout(300)
    if page.locator('.question-card').count() != before + 1:
        fail('添加问题没有生效')
    # noul 是是非题，没有选项可填，选项框应当消失
    criteria_before = page.locator('[name="q-criteria"]').count()
    page.locator('[name="q-type"]').last.select_option('noul')
    page.wait_for_timeout(300)
    if page.locator('[name="q-criteria"]').count() != criteria_before - 1:
        fail('题型切到 noul 后选项框应当隐藏')
    page.click('[data-action="remove-question"]')
    page.wait_for_timeout(300)
    if page.locator('.question-card').count() != before:
        fail('删除问题没有生效')
    results.append('systemone playground editor')


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



def read_admin_token(directory, logpath):
    """令牌优先从 <db>.admin-token 读取。

    stdout 被收集时（这里就是重定向到文件）网关不再打印令牌，改为落盘，
    所以先看文件；老版本二进制仍然只打印，因此保留日志回退。
    """
    token_file = Path(directory) / 'gateway.db.admin-token'
    if token_file.exists():
        return token_file.read_text().strip()
    found = re.search(r'prism_admin_[A-Za-z0-9_-]+', Path(logpath).read_text())
    if not found:
        raise RuntimeError('既没有令牌文件也没有在输出里找到令牌')
    return found.group()

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
            token = read_admin_token(directory, logpath)
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
