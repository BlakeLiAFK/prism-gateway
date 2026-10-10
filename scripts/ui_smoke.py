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
         'requests', 'sessions', 'jobs', 'keys', 'audit', 'settings']


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
    # 带 UA 的演示请求：总览最近请求要显示来源 IP 与 UA
    key = rpc('apikey.create', {'name': 'smoke', 'allowed': []})['key']
    # 候选全部被淘汰的请求：没有到达任何模型，最近请求里要显示客户端请求名而不是空白。
    # 先发它，保证下面那条带 UA 的演示请求仍是最新一条。
    body = json.dumps({'model': 'demo-responses', 'max_tokens': 16, 'messages': [{'role': 'user', 'content': [
        {'type': 'document', 'source': {'type': 'base64', 'media_type': 'application/pdf', 'data': 'JVBERi0='}}]}]}).encode()
    req = urllib.request.Request(base + '/anthropic/v1/messages', data=body, headers={
        'Content-Type': 'application/json', 'x-api-key': key, 'anthropic-version': '2023-06-01'})
    try:
        urllib.request.urlopen(req, timeout=20)
        raise AssertionError('带文档的跨协议请求应返回 400')
    except urllib.error.HTTPError as e:
        assert e.code == 400, e.code
    body = json.dumps({'model': 'demo-auto', 'messages': [{'role': 'user', 'content': 'hi'}], 'max_tokens': 16}).encode()
    req = urllib.request.Request(base + '/openai/v1/chat/completions', data=body, headers={
        'Content-Type': 'application/json', 'Authorization': 'Bearer ' + key, 'User-Agent': 'claude-cli/2.1.3 (external, cli)'})
    with urllib.request.urlopen(req, timeout=20) as r:
        r.read()
    return rpc


def run_ui_checks(page, base, token, results, artifacts=None):
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

    recent = page.locator('.compact-table tbody tr').first
    recent.wait_for(timeout=5000)
    text = recent.inner_text()
    if '127.0.0.1' not in text or 'Claude Code' not in text or 'claude-cli/2.1.3' not in text:
        fail(f'总览最近请求缺少 IP 或 UA: {text!r}')
    if recent.bounding_box()['height'] > 56:
        fail(f'最近请求行高应紧凑，实得 {recent.bounding_box()["height"]}')
    # 这条被淘汰的请求总览会计入、用量页不计入，总览要用一行字说明差异，并给出准确条数
    note = page.locator('#unreached-note')
    if note.count() != 1 or not note.is_visible() or '有 1 条请求没有到达任何模型' not in note.inner_text():
        fail(f'总览应说明有 1 条请求没有到达任何模型: {note.count()} {note.inner_text() if note.count() else None!r}')
    nbox = note.bounding_box()
    if nbox['x'] + nbox['width'] > page.viewport_size['width'] + 1:
        fail(f'总览说明超出视口: {nbox}')
    if artifacts:
        note.screenshot(path=str(artifacts / 'unreached-note.png'))
    rejected = page.locator('.compact-table tbody tr', has_text='Demo / Responses')
    if rejected.count() == 0:
        fail('被全部候选淘汰的请求应以客户端请求名出现在最近请求里')
    # 点开详情：管理员要在这里看到淘汰原因，没有上游的字段不能显示成半截或「待确认」
    rejected.first.click()
    page.wait_for_selector('.detail-grid')
    detail = page.locator('.detail-grid').inner_text()
    if '跨协议不支持内容类型 document' not in detail or 'NO_COMPATIBLE_MODEL' not in detail or '未到达任何模型' not in detail:
        fail(f'请求详情缺少淘汰原因: {detail!r}')
    if '→' in detail or '待确认' in detail:
        fail(f'没有上游的请求不应显示协议转换或待确认的估算价值: {detail!r}')
    if artifacts:
        page.locator('dialog').screenshot(path=str(artifacts / 'incompatible-detail.png'))
    page.click('[data-action="close-dialog"]')
    page.wait_for_timeout(200)
    diagnostic = page.evaluate("""async () => {
        const {requestDetail} = await import('/assets/views.js');
        const {showDialog} = await import('/assets/ui.js');
        const shape = {
            before:{messages:[{role:'user',content_types:['text'],content_count:1,truncated:false}],message_count:1,truncated:false},
            after:{messages:[{role:'assistant',content_types:['text'],content_count:1,truncated:false}],message_count:1,truncated:false},
            captured_structure_changed:true
        };
        showDialog('请求诊断','smoke',requestDetail({diagnostic:JSON.stringify(shape)}),null,'',true);
        const block = document.querySelector('.request-diagnostic');
        return {text:block?.innerText || '', images:block?.querySelectorAll('img').length || 0};
    }""")
    if '"captured_structure_changed": true' not in diagnostic['text'] or '"content_types"' not in diagnostic['text']:
        fail(f'请求详情没有可读展示脱敏诊断: {diagnostic}')
    if diagnostic['images'] != 0:
        fail(f'请求诊断内容没有正确转义: {diagnostic}')
    if artifacts:
        page.locator('.request-diagnostic').screenshot(path=str(artifacts / 'diagnostic-detail-desktop.png'))
    desktop = page.viewport_size
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(250)
    dialog_box = page.locator('dialog').bounding_box()
    if (not dialog_box or dialog_box['x'] + dialog_box['width'] > 391 or
            page.evaluate('document.documentElement.scrollWidth > innerWidth + 1')):
        fail(f'390px 下请求诊断详情横向溢出: {dialog_box}')
    if artifacts:
        page.locator('.request-diagnostic').screenshot(path=str(artifacts / 'diagnostic-detail-mobile.png'))
    page.set_viewport_size(desktop)
    page.click('[data-action="close-dialog"]')
    page.wait_for_timeout(200)
    results.append('request detail shows escaped diagnostic JSON')
    # 没有到达任何模型的请求既没有花费也没有待确认的用量，请求列表的估算价值列必须显示「—」。
    # 用总览行的 data-id 定位同一条记录，不依赖文本匹配；
    # 断言渲染后的可见文本与可见性，而不是只看 HTML 源码。
    rejected_id = rejected.first.get_attribute('data-id')
    page.click('.nav-item[data-nav="requests"]')
    # 总览的紧凑表格里同一条记录也带 data-id，必须排除它，否则会在列表页渲染完成前就命中总览行
    row = page.locator(f'table:not(.compact-table) tr[data-id="{rejected_id}"]')
    row.wait_for(state='visible', timeout=5000)
    if 'Demo / Responses' not in row.inner_text():
        fail(f'请求列表里被淘汰的请求应显示客户端请求名: {row.inner_text()!r}')
    # 按表头定位列，避免以后增删列时下标悄悄错位
    cost_col = row.locator('xpath=ancestor::table//thead//button[starts-with(@aria-label, "估算价值")]').evaluate('el => el.closest("th").cellIndex')
    cost_cell = row.locator('td').nth(cost_col)
    if not cost_cell.is_visible():
        fail('请求列表里被淘汰请求的估算价值单元格不可见')
    cost_text = cost_cell.inner_text().strip()
    if cost_text != '—':
        fail(f'没有花费的请求在请求列表里估算价值应显示「—」，实得 {cost_text!r}')
    results.append('request list: incompatible row cost is dash')
    # 回到总览：后面的检查继续基于总览最近请求卡片
    page.click('.nav-item[data-nav="overview"]')
    page.wait_for_selector('.compact-table .ua-cell')
    ua = page.locator('.compact-table .ua-cell').first.bounding_box()
    card = page.locator('.card:has(.compact-table)').bounding_box()
    if ua['x'] + 20 > card['x'] + card['width']:
        fail(f'最近请求的 UA 列被挤出卡片: ua={ua} card={card}')
    if artifacts:
        page.locator('.card:has(.compact-table)').screenshot(path=str(artifacts / 'recent-desktop.png'))
    results.append('overview recent shows ip and ua')

    # 共用 Token 格式化与总览按钮：大数默认使用 B，精确显示在切页后保留。
    page.evaluate("""async () => {
        const {state,formatTokens} = await import('/assets/core.js');
        const {renderPage} = await import('/assets/app.js');
        for (const [value,want] of [[999,'999'],[1000,'1.0K'],[999950,'1.00M'],[1000000,'1.00M'],[999995000,'1.00B'],[6840810000,'6.84B']]) {
            if (formatTokens(value) !== want) throw new Error(`Token 格式错误: ${value}`);
        }
        state.data.summary.input_tokens = 6840810000;
        state.data.summary.output_tokens = 0;
        renderPage(false);
    }""")
    token_button = page.locator('[data-action="toggle-token-unit"]')
    if token_button.inner_text() != '6.84B':
        fail('总览 Token 默认单位不是 B')
    token_button.click()
    if page.locator('[data-action="toggle-token-unit"]').inner_text() != '6,840,810,000':
        fail('点击后未显示精确 Token 数量')
    desktop_size = page.viewport_size
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(250)
    if page.evaluate('document.documentElement.scrollWidth > innerWidth'):
        fail('390px 下精确 Token 数量造成横向溢出')
    if artifacts:
        page.locator('.stat:has([data-action="toggle-token-unit"])').screenshot(path=str(artifacts / 'token-exact-mobile.png'))
    page.set_viewport_size(desktop_size)
    if artifacts:
        page.screenshot(path=str(artifacts / 'token-exact-desktop.png'))
    page.locator('[data-action="toggle-token-unit"]').focus()
    page.keyboard.press('Enter')
    if page.locator('[data-action="toggle-token-unit"]').inner_text() != '6.84B':
        fail('键盘未切回自动单位')
    page.locator('[data-action="toggle-token-unit"]').click()
    page.click('.nav-item[data-nav="usage"]')
    page.wait_for_selector('.usage-card-head')
    page.evaluate("""async () => {
        const {state} = await import('/assets/core.js');
        const {renderPage} = await import('/assets/app.js');
        if (state.tokenDisplay !== 'exact') throw new Error('切页丢失 Token 单位');
        state.data.top_models = [{model_id:'demo',requests:1,tokens:6840810000,cost_nano:0,latency_ms:0}];
        renderPage(false);
        if (!document.querySelector('main').innerText.includes('6,840,810,000'))
            throw new Error('用量页没有沿用精确 Token 单位');
    }""")
    page.click('.nav-item[data-nav="overview"]')
    page.wait_for_selector('[data-action="toggle-token-unit"]')
    page.locator('[data-action="toggle-token-unit"]').click()
    results.append('token units: B, exact, keyboard, cross-page')

    for name in PAGES:
        page.click(f'.nav-item[data-nav="{name}"]')
        page.wait_for_timeout(250)
        if not page.locator('main').inner_text().strip():
            fail(f'{name} 页渲染为空')
        results.append(f'page: {name}')

    # 指标网格只能包含六个指标，不能出现模板残留文本。
    page.click('.nav-item[data-nav="routes"]')
    page.wait_for_selector('.live-grid')
    page.evaluate("""async () => {
        const {state} = await import('/assets/core.js');
        const {routeLiveBar} = await import('/assets/routes.js');
        const original = state.live;
        try {
            for (const live of [null, {routes:{check:{tok_s:0}}}, {routes:{check:{live_tok_s:12,tok_s:8}}}]) {
                state.live = live;
                const host = document.createElement('div');
                host.innerHTML = routeLiveBar('check');
                const grid = host.querySelector('.live-grid');
                const stray = [...grid.childNodes].filter(n => n.nodeType === Node.TEXT_NODE && n.textContent.trim());
                if (stray.length || grid.querySelectorAll('.live-item').length !== 6)
                    throw new Error('路由实时指标含多余文本或指标缺失');
            }
        } finally { state.live = original; }
    }""")
    if artifacts:
        page.screenshot(path=str(artifacts / 'routes-desktop.png'), full_page=True)
    results.append('route live metrics clean in all states')

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
    page.click('[data-action="add-model"]')
    page.wait_for_timeout(400)
    for capability in ('tools', 'vision'):
        if page.locator(f'select[name="{capability}"]').input_value() != 'unknown':
            fail(f'新增模型 {capability} 应默认交给上游判断')
    if page.locator('input[name="drop_reasoning"]').count() == 0:
        fail('模型编辑器缺少 drop_reasoning 开关')
    if page.locator('input[name="drop_reasoning"]').is_checked():
        fail('drop_reasoning 默认必须关闭')
    if page.locator('input[name="drop_documents"]').count() == 0:
        fail('模型编辑器缺少 drop_documents 开关')
    if page.locator('input[name="drop_documents"]').is_checked():
        fail('drop_documents 默认必须关闭')
    page.evaluate("document.querySelector('[data-action=\"close-dialog\"]')?.click()")
    page.wait_for_timeout(200)
    results.append('model editor capability toggles')
    page.locator('[data-action="edit-model"]').first.click()
    page.wait_for_selector('input[name="price_locked"]')
    page.check('input[name="price_locked"]')
    page.check('input[name="native_protocol"][value="responses"]')
    page.check('input[name="protocols_locked"]')
    page.select_option('select[name="vision"]', 'unknown')
    page.locator('dialog button[type="submit"]').click()
    page.wait_for_timeout(400)
    page.locator('[data-action="edit-model"]').first.click()
    page.wait_for_selector('input[name="price_locked"]')
    if not page.locator('input[name="price_locked"]').is_checked():
        fail('模型价格锁保存未生效')
    if not page.locator('input[name="native_protocol"][value="responses"]').is_checked():
        fail('模型多协议保存未生效')
    if not page.locator('input[name="protocols_locked"]').is_checked():
        fail('模型协议锁保存未生效')
    if page.locator('select[name="vision"]').input_value() != 'unknown':
        fail('模型未知能力状态保存未生效')
    if artifacts:
        page.screenshot(path=str(artifacts / 'model-capabilities-desktop.png'))
        desktop = page.viewport_size
        page.set_viewport_size({'width': 390, 'height': 844})
        page.screenshot(path=str(artifacts / 'model-capabilities-mobile.png'))
        if page.evaluate('document.documentElement.scrollWidth > innerWidth'):
            fail('模型能力编辑器在移动端横向溢出')
        page.set_viewport_size(desktop)
    page.click('[data-action="close-dialog"]')
    results.append('model price lock persists')
    results.append('model native protocols and unknown capability persist')

    model_id = page.locator('[data-action="edit-model"]').first.get_attribute('data-id')
    page.evaluate("""async id => {
        const {rpc} = await import('/assets/api.js');
        const c = await rpc('config.get');
        await rpc('model.save', {version:c.version, id, model:{supported_protocols:[]}});
    }""", model_id)
    page.reload(wait_until='networkidle')
    page.click('.nav-item[data-nav="models"]')
    page.locator(f'[data-action="edit-model"][data-id="{model_id}"]').click()
    page.wait_for_selector('input[name="name"]')
    page.fill('input[name="name"]', '协议未确认模型')
    page.locator('dialog button[type="submit"]').click()
    page.wait_for_timeout(300)
    protocols = page.evaluate("""async id => {
        const {rpc} = await import('/assets/api.js');
        return (await rpc('config.get')).models.find(m=>m.id===id).supported_protocols;
    }""", model_id)
    if protocols != []:
        fail(f'修改名称不能启用未确认协议: {protocols}')
    # 恢复演示模型，后续交互继续使用原来的路由。
    page.locator(f'[data-action="edit-model"][data-id="{model_id}"]').click()
    page.check('input[name="native_protocol"][value="chat"]')
    page.locator('dialog button[type="submit"]').click()
    page.wait_for_timeout(300)
    results.append('editing unrelated fields preserves empty protocols')


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

    # 会话切入提示：演示路由的候选原生协议各不相同，未开开关的候选必须有可见提示；
    # 在模型上开启两个开关后提示消失。断言渲染结果而不是 HTML 文本。
    hint_state = """id => {
        const cards = [...document.querySelectorAll('.candidate')];
        const card = id ? cards.find(c => c.querySelector('small .mono')?.textContent === id) : cards.find(c => c.querySelector('.switch-hint'));
        const hint = card?.querySelector('.switch-hint');
        if (!hint) return {present: false};
        const box = hint.getBoundingClientRect();
        return {present: true, visible: hint.offsetParent !== null && box.width > 0 && box.height > 0,
                right: box.right, text: hint.innerText, title: hint.title,
                id: card.querySelector('small .mono').textContent};
    }"""
    set_switches = """async ([id, on]) => {
        const {rpc} = await import('/assets/api.js');
        const c = await rpc('config.get');
        await rpc('model.save', {version: c.version, id, model: {drop_reasoning: on, drop_documents: on}});
    }"""

    def open_demo_route():
        page.reload(wait_until='networkidle')
        page.click('.nav-item[data-nav="routes"]')
        page.wait_for_timeout(400)
        page.locator('.route-select', has_text='demo-auto').first.click()
        page.wait_for_timeout(400)

    open_demo_route()
    hint = page.evaluate(hint_state, None)
    if not hint.get('visible') or '推理历史' not in hint.get('text', ''):
        fail(f'跨协议候选缺少可见的会话切入提示: {hint}')
    if '需跨协议' not in hint.get('title', ''):
        fail(f'提示应说明原因: {hint}')
    hinted = hint['id']
    size = page.viewport_size
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(250)
    narrow = page.evaluate(hint_state, hinted)
    if not narrow.get('visible') or narrow.get('right', 999) > 390:
        fail(f'390px 下会话切入提示不可见或超出视口: {narrow}')
    if artifacts:
        page.locator('.route-canvas').screenshot(path=str(artifacts / 'switch-hint-mobile.png'))
    page.set_viewport_size(size)
    page.wait_for_timeout(250)
    if artifacts:
        page.locator('.route-canvas').screenshot(path=str(artifacts / 'switch-hint.png'))
    page.evaluate(set_switches, [hinted, True])
    open_demo_route()
    if page.evaluate(hint_state, hinted).get('present'):
        fail('模型开启两个开关后，会话切入提示应消失')
    page.evaluate(set_switches, [hinted, False])
    page.reload(wait_until='networkidle')
    page.click('.nav-item[data-nav="routes"]')
    page.wait_for_timeout(500)
    results.append('route session switch hint')

    # 候选暂停开关：落库、重载可见、编辑路由后不被重置
    paused = "document.querySelector('.candidate [data-action=\"toggle-candidate\"]').getAttribute('aria-checked')"
    page.locator('.candidate [data-action="toggle-candidate"]').first.click()
    page.wait_for_timeout(800)
    page.reload()
    page.wait_for_timeout(1200)
    page.click('.nav-item[data-nav="routes"]')
    page.wait_for_timeout(500)
    first = page.locator('.candidate').first
    if page.evaluate(paused) != 'false' or '已暂停' not in first.inner_text() or 'muted' not in first.get_attribute('class'):
        fail('暂停候选后重载，开关、标记或置灰状态不对')
    if artifacts:
        page.locator('.route-canvas').screenshot(path=str(artifacts / 'candidate-paused.png'))
    # 390px 下开关必须完整可见，不能被挤出卡片
    size = page.viewport_size
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(250)
    box = page.locator('.candidate [data-action="toggle-candidate"]').first.bounding_box()
    if not box or box['x'] + box['width'] > 390:
        fail(f'390px 下候选开关超出视口: {box}')
    if artifacts:
        page.locator('.route-canvas').screenshot(path=str(artifacts / 'candidate-paused-mobile.png'))
    page.set_viewport_size(size)
    page.wait_for_timeout(250)
    page.locator('[data-action="edit-route"]').first.click()
    page.wait_for_selector('dialog button[type="submit"]')
    page.locator('dialog button[type="submit"]').click()
    page.wait_for_timeout(800)
    if page.evaluate(paused) != 'false':
        fail('编辑并保存路由后，候选的暂停状态被重置')
    page.locator('.candidate [data-action="toggle-candidate"]').first.click()
    page.wait_for_timeout(800)

    # 路由分组：编辑器里添加一组、把候选放进第 2 组；保存后画布分两节，重载仍是两节；删掉一组后回到单节
    visible_groups = "[...document.querySelectorAll('.route-group')].filter(g => g.offsetParent !== null).length"
    page.locator('[data-action="edit-route"]').first.click()
    page.wait_for_selector('dialog #group-table')
    page.click('#group-add')
    if page.locator('#group-table .group-row').count() != 2:
        fail('添加一组后组表应有 2 行')
    shown = page.evaluate("[...document.querySelectorAll('.candidate-row .group-select')].filter(s => s.offsetParent !== null).length")
    if shown == 0:
        fail('有多个组时，候选行的「所属组」下拉必须可见')
    moved = page.evaluate("""() => {
        const row = [...document.querySelectorAll('.candidate-row')].find(r => r.querySelector('input[name=candidate]').checked);
        const sel = row.querySelector('.group-select');
        sel.value = sel.options[1].value;
        return row.querySelector('input[name=candidate]').value;
    }""")
    if artifacts:
        page.locator('dialog').screenshot(path=str(artifacts / 'route-editor-groups.png'))
    page.locator('dialog button[type="submit"]').click()
    page.wait_for_timeout(900)
    if page.evaluate(visible_groups) != 2:
        fail('保存两组路由后，画布上应能看到两节')
    in_second = page.evaluate("id => [...document.querySelectorAll('.route-group')][1]?.innerText.includes(id)", moved)
    head_visible = page.evaluate("[...document.querySelectorAll('.route-group-head')].filter(h => h.offsetParent !== null).length")
    if not in_second or head_visible != 2:
        fail(f'候选应出现在第 2 节，且两节都有可见的组头: in_second={in_second} heads={head_visible}')
    if artifacts:
        page.locator('.route-canvas').screenshot(path=str(artifacts / 'route-groups.png'))
    page.reload(wait_until='networkidle')
    page.click('.nav-item[data-nav="routes"]')
    page.wait_for_timeout(600)
    if page.evaluate(visible_groups) != 2:
        fail('重载后两组路由应仍是两节')
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(250)
    overflow = page.evaluate("document.documentElement.scrollWidth > window.innerWidth + 1")
    if overflow:
        fail('390px 下分组画布不应出现横向滚动')
    page.set_viewport_size({'width': 1280, 'height': 900})
    page.wait_for_timeout(250)
    page.locator('[data-action="edit-route"]').first.click()
    page.wait_for_selector('dialog #group-table')
    page.locator('#group-table .group-row').last.locator('[data-grp="del"]').click()
    if page.locator('#group-table .group-row').count() != 1:
        fail('删除后组表应剩 1 行')
    page.locator('dialog button[type="submit"]').click()
    page.wait_for_timeout(900)
    single = page.evaluate("[...document.querySelectorAll('.route-group-head')].filter(h => h.offsetParent !== null).length")
    if page.evaluate(visible_groups) != 1 or single != 0:
        fail('删到只剩一组后，画布应回到不带组头的单节')
    results.append('route groups add move reload delete')
    if page.evaluate(paused) != 'true':
        fail('再次点击开关应恢复候选')
    results.append('route candidate pause toggle')

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
    diagnostics = page.locator('input[name="request_diagnostics"]')
    if diagnostics.count() != 1 or diagnostics.is_checked():
        fail('临时请求结构诊断应存在且默认关闭')
    copy = page.locator('.setting-row', has=diagnostics).inner_text()
    for text in ('Messages 上游 400', '15 分钟', '最多 20 条', '不保存正文、工具参数或工具结果'):
        if text not in copy:
            fail(f'临时请求结构诊断说明缺少 {text!r}: {copy!r}')
    if artifacts:
        page.locator('.setting-row', has=diagnostics).screenshot(path=str(artifacts / 'request-diagnostics-settings-desktop.png'))
    desktop = page.viewport_size
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(250)
    setting_box = page.locator('.setting-row', has=diagnostics).bounding_box()
    if (not setting_box or setting_box['x'] + setting_box['width'] > 391 or
            page.evaluate('document.documentElement.scrollWidth > innerWidth + 1')):
        fail(f'390px 下请求诊断设置横向溢出: {setting_box}')
    if artifacts:
        page.locator('.setting-row', has=diagnostics).screenshot(path=str(artifacts / 'request-diagnostics-settings-mobile.png'))
    page.set_viewport_size(desktop)
    page.wait_for_timeout(250)
    diagnostics.check()
    page.click('#settings-form button[type="submit"]')
    page.wait_for_timeout(600)
    first_until = page.evaluate("""async () => {
        const {rpc} = await import('/assets/api.js');
        return (await rpc('config.get')).settings.request_diagnostics_until;
    }""")
    now = int(time.time())
    if not now + 890 <= first_until <= now + 910:
        fail(f'开启诊断应设置约 15 分钟有效期，实得 {first_until - now} 秒')
    if not page.locator('input[name="request_diagnostics"]').is_checked():
        fail('开启诊断后设置页没有回显开启状态')
    page.click('#settings-form button[type="submit"]')
    page.wait_for_timeout(600)
    kept_until = page.evaluate("""async () => {
        const {rpc} = await import('/assets/api.js');
        return (await rpc('config.get')).settings.request_diagnostics_until;
    }""")
    if kept_until != first_until:
        fail(f'普通保存不应延长未过期诊断: {first_until} -> {kept_until}')
    page.locator('input[name="request_diagnostics"]').uncheck()
    page.click('#settings-form button[type="submit"]')
    page.wait_for_timeout(600)
    disabled_until = page.evaluate("""async () => {
        const {rpc} = await import('/assets/api.js');
        return (await rpc('config.get')).settings.request_diagnostics_until;
    }""")
    if disabled_until != 0:
        fail(f'关闭诊断应清零有效期，实得 {disabled_until}')
    page.evaluate("""async () => {
        const {state} = await import('/assets/core.js');
        const {renderPage} = await import('/assets/app.js');
        state.config.settings.request_diagnostics_until = 1;
        renderPage(false);
    }""")
    if page.locator('input[name="request_diagnostics"]').is_checked():
        fail('已过期的请求结构诊断应显示为关闭')
    page.locator('input[name="request_diagnostics"]').check()
    page.click('#settings-form button[type="submit"]')
    page.wait_for_timeout(600)
    if not page.locator('input[name="request_diagnostics"]').is_checked():
        fail('已过期的请求结构诊断应能重新开启')
    page.locator('input[name="request_diagnostics"]').uncheck()
    page.click('#settings-form button[type="submit"]')
    page.wait_for_timeout(600)
    results.append('request diagnostics enable preserve expire reopen disable')
    page.select_option('select[name="log_level"]', 'warn')
    page.click('#settings-form button[type="submit"]')
    page.wait_for_timeout(600)
    page.wait_for_timeout(400)
    if page.locator('select[name="log_level"]').input_value() != 'warn':
        fail('设置保存后未回显新值')
    results.append('settings save round trip')

    # 终端监控：界面生成的命令里的令牌必须真的能拉流，撤销后立刻失效。
    # 用 urllib 而不是执行命令本身，避免在测试里依赖 shell 与 curl 的版本。
    page.click('[data-action="settings-tab"][data-value="watch"]')
    page.wait_for_selector('#watch-card [data-action="watch-create"]')
    if page.locator('#watch-days').input_value() != '7':
        fail('监控令牌默认有效期应为 7 天')
    page.select_option('#watch-mode', 'log')
    page.fill('#watch-name', 'smoke')
    if artifacts:
        page.locator('#watch-card').screenshot(path=str(artifacts / 'watch-card.png'))
    page.click('[data-action="watch-create"]')
    page.wait_for_selector('dialog .secret-box')
    if artifacts:
        page.locator('dialog').screenshot(path=str(artifacts / 'watch-dialog.png'))
    # 390px 下令牌是一长串不可断行的字符，命令框必须在对话框里换行，不能撑出视口或产生横向滚动
    desktop = page.viewport_size
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(300)
    box = page.locator('dialog .secret-box').bounding_box()
    dlg = page.locator('dialog').bounding_box()
    overflow = page.evaluate("() => document.querySelector('dialog .secret-box').scrollWidth - document.querySelector('dialog .secret-box').clientWidth")
    if not box or box['x'] + box['width'] > 390 + 1 or overflow > 1 or dlg['x'] + dlg['width'] > 390 + 1:
        fail(f'390px 下监控命令框超出视口或需要横向滚动: box={box} dialog={dlg} overflow={overflow}')
    copy_box = page.locator('dialog [data-action="copy-secret"]').bounding_box()
    if not copy_box or copy_box['x'] + copy_box['width'] > 390:
        fail(f'390px 下复制按钮超出视口: {copy_box}')
    if artifacts:
        page.locator('dialog').screenshot(path=str(artifacts / 'watch-dialog-mobile.png'))
    page.set_viewport_size(desktop)
    page.wait_for_timeout(300)
    command = page.locator('dialog .secret-box').inner_text()
    for part in ('watch.stream', 'Authorization: Bearer prism_watch_', base + '/api.json', 'while :; do curl -sN --fail-with-body', '-eq 22'):
        if part not in command:
            fail(f'生成的命令缺少 {part!r}: {command!r}')
    token = re.search(r'prism_watch_[A-Za-z0-9_-]+', command).group(0)

    def open_stream():
        request = urllib.request.Request(base + '/api.json', data=json.dumps({'action': 'watch.stream', 'params': {'mode': 'log'}}).encode(),
                                         headers={'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token})
        return urllib.request.urlopen(request, timeout=10)

    with open_stream() as stream:
        first = stream.readline().decode()
        if 'prism gateway' not in first:
            fail(f'监控流第一行不对: {first!r}')
        if not stream.headers.get('Content-Type', '').startswith('text/plain'):
            fail(f'监控流应是纯文本: {stream.headers.get("Content-Type")}')
    page.click('[data-action="close-dialog"]')
    page.wait_for_selector('#watch-card [data-action="watch-revoke"]')
    row = page.locator('#watch-card tbody tr', has_text='smoke')
    if row.count() != 1 or not row.first.is_visible():
        fail('令牌列表里应当看到刚生成的令牌')
    row.first.locator('[data-action="watch-revoke"]').click()
    page.click('.confirm-dialog [data-yes]')
    # 页面的 CSP 禁止 eval，不能用字符串形式的 wait_for_function
    page.locator('#watch-card tbody tr').first.wait_for(state='detached', timeout=5000)
    try:
        open_stream().close()
        fail('撤销后的令牌不应再能拉流')
    except urllib.error.HTTPError as e:
        if e.code != 401:
            fail(f'撤销后应返回 401，实得 {e.code}')
    results.append('terminal watch token create stream revoke')

    # 新定时任务参数必须经过界面保存、刷新后仍保持。
    page.click('[data-action="settings-tab"][data-value="schedule"]')
    page.wait_for_selector('[name="weekly_hour"]')
    fields = {'weekly_hour': '10', 'weekly_weekday': '2', 'price_hours': '12',
              'spend_multiple': '3', 'spend_minimum': '20', 'failure_minutes': '10',
              'failure_min_samples': '25', 'failure_percent': '30',
              'failure_cooldown_minutes': '90', 'key_budget_percent': '85',
              'key_budget_cooldown_minutes': '720'}
    for name, value in fields.items():
        page.fill(f'#schedule-card [name="{name}"]', value)
    toggles = ('weekly_enabled', 'price_enabled', 'spend_enabled', 'failure_enabled', 'key_budget_enabled')
    for name in toggles:
        page.check(f'#schedule-card [name="{name}"]')
    page.click('[data-action="schedule-save"]')
    page.wait_for_timeout(400)
    page.reload(wait_until='networkidle')
    page.click('[data-action="settings-tab"][data-value="schedule"]')
    page.wait_for_selector('[name="weekly_hour"]')
    for name, value in fields.items():
        if page.locator(f'#schedule-card [name="{name}"]').input_value() != value:
            fail(f'定时任务字段 {name} 未持久化')
    for name in toggles:
        if not page.locator(f'#schedule-card [name="{name}"]').is_checked():
            fail(f'定时任务开关 {name} 未持久化')
        page.uncheck(f'#schedule-card [name="{name}"]')
    page.click('[data-action="schedule-save"]')
    page.wait_for_timeout(300)
    if artifacts:
        page.screenshot(path=str(artifacts / 'schedule-desktop.png'), full_page=True)
    results.append('new scheduled task settings persist')

    # 审计页筛选与清空，确认真实接口与事件绑定。
    page.click('.nav-item[data-nav="audit"]')
    page.wait_for_selector('#audit-filter')
    page.fill('#audit-filter input[name="action"]', 'settings.update')
    page.click('#audit-filter button[type="submit"]')
    page.wait_for_timeout(350)
    rows = page.locator('main tbody tr')
    if rows.count() == 0 or any('settings.update' not in row.inner_text() for row in rows.all()):
        fail('审计操作筛选未生效')
    page.fill('#audit-filter input[name="q"]', 'no-such-audit-record')
    page.click('#audit-filter button[type="submit"]')
    page.wait_for_timeout(350)
    if page.locator('main tbody tr').count() != 0:
        fail('审计空结果筛选未生效')
    page.click('[data-action="audit-clear"]')
    page.wait_for_timeout(350)
    if page.locator('main tbody tr').count() == 0:
        fail('审计清除筛选未恢复记录')
    results.append('audit filter and reset')
    if artifacts:
        page.screenshot(path=str(artifacts / 'audit-desktop.png'), full_page=True)

    # 移动端宽度不得产生横向滚动
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(300)
    overflow = page.evaluate('document.documentElement.scrollWidth - document.documentElement.clientWidth')
    if overflow > 0:
        fail(f'390px 宽度下横向溢出 {overflow}px')
    results.append('responsive 390px')
    if artifacts:
        page.screenshot(path=str(artifacts / 'audit-mobile.png'), full_page=True)
    for target in ('settings', 'models', 'routes'):
        page.goto(base + '/#' + target, wait_until='networkidle')
        page.wait_for_timeout(400)
        if target == 'settings':
            page.click('[data-action="settings-tab"][data-value="schedule"]')
            page.wait_for_selector('[name="weekly_hour"]')
        overflow = page.evaluate('document.documentElement.scrollWidth - document.documentElement.clientWidth')
        if overflow > 0:
            fail(f'{target} 在390px横向溢出 {overflow}px')
        if artifacts:
            page.screenshot(path=str(artifacts / (target + '-mobile.png')), full_page=True)
    page.emulate_media(reduced_motion='reduce')
    page.goto(base + '/#audit', wait_until='networkidle')
    page.wait_for_selector('#audit-filter')
    page.locator('#audit-filter input[name="q"]').focus()
    page.keyboard.press('Tab')
    if page.evaluate('document.activeElement.name') != 'action':
        fail('审计筛选键盘顺序错误')
    results.append('mobile pages and reduced-motion keyboard navigation')


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
    parser.add_argument('--artifacts', type=Path)
    args = parser.parse_args()
    if args.artifacts:
        args.artifacts.mkdir(parents=True, exist_ok=True)
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
                    run_ui_checks(page, base, token, results, args.artifacts)
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
