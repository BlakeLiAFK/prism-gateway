#!/usr/bin/env python3
"""Browser acceptance checks for request-list filters.

The script starts a real gateway and uses its mock provider to create request
records. Only the explicit network-failure check intercepts request.list.
"""
import argparse
import json
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from datetime import datetime, timedelta
from pathlib import Path

from ui_smoke import enable_demo, read_admin_token, wait_ready


DAY_MS = 24 * 60 * 60 * 1000


def seed_requests(base, rpc, count=35):
    key = rpc('apikey.create', {'name': 'request-filter-ui', 'allowed': []})['key']
    url = base + '/openai/v1/chat/completions'
    for index in range(count):
        body = json.dumps({
            'model': 'demo-auto',
            'messages': [{'role': 'user', 'content': f'filter fixture {index}'}],
            'max_tokens': 16,
        }).encode()
        request = urllib.request.Request(url, data=body, headers={
            'Content-Type': 'application/json',
            'Authorization': 'Bearer ' + key,
        })
        with urllib.request.urlopen(request, timeout=20) as response:
            response.read()


def login(page, base, token):
    page.goto(base + '/', wait_until='networkidle')
    page.fill('#login-form input[name="token"]', token)
    page.click('#login-form button[type="submit"]')
    page.wait_for_selector('.sidebar', timeout=15000)
    page.click('.nav-item[data-nav="requests"]')
    page.wait_for_selector('#request-filter', timeout=10000)


def local_datetime(value):
    return value.strftime('%Y-%m-%dT%H:%M')


def request_list_params(request):
    if not request.url.endswith('/api.json') or request.method != 'POST':
        return None
    payload = request.post_data_json
    if payload.get('action') != 'request.list':
        return None
    return payload.get('params', {})


def assert_filter_geometry(page, width):
    page.set_viewport_size({'width': width, 'height': 900})
    page.wait_for_timeout(150)
    violations = page.locator('#request-filter').evaluate("""form => {
        const outer=form.getBoundingClientRect(),tolerance=1;
        return [...form.querySelectorAll('fieldset,input')]
          .filter(element=>element.getClientRects().length)
          .map(element=>{
            const box=element.getBoundingClientRect();
            return {
              element:element.name||element.tagName.toLowerCase(),
              box:{left:box.left,right:box.right,top:box.top,bottom:box.bottom},
              outer:{left:outer.left,right:outer.right,top:outer.top,bottom:outer.bottom}
            };
          })
          .filter(({box,outer})=>box.left<outer.left-tolerance||box.right>outer.right+tolerance||
            box.top<outer.top-tolerance||box.bottom>outer.bottom+tolerance);
    }""")
    assert not violations, f'{width}px filter controls overflow: {violations}'


def run_checks(page, artifacts):
    results = []
    calls = []
    browser_errors = []
    expected_503 = {'active': False}

    # 唯一允许的浏览器报错是脚本主动制造的单次 503。
    def capture_console(message):
        if message.type != 'error':
            return
        text = message.text
        if expected_503['active'] and ('503' in text or 'Failed to load resource' in text):
            return
        browser_errors.append('console: ' + text)

    page.on('console', capture_console)
    page.on('pageerror', lambda error: browser_errors.append('pageerror: ' + str(error)))
    page.on('request', lambda request: calls.append(params)
            if (params := request_list_params(request)) is not None else None)

    page.reload(wait_until='networkidle')
    page.wait_for_selector('#request-filter', timeout=10000)

    form = page.locator('#request-filter')
    ranges = form.locator('[name="range"] option').evaluate_all(
        'options => options.map(option => option.value)')
    assert ranges == ['24h', '7d', '30d', 'custom'], ranges
    assert 'all' not in ranges
    initial = calls[-1]
    assert 'all_time' not in initial
    assert 'from' not in initial and 'to' not in initial, initial
    initial_window = page.evaluate("""async () => {
        const {state} = await import('/assets/core.js');
        return [state.requestFilterApplied.from_ms, state.requestFilterApplied.to_ms];
    }""")
    assert initial_window[1] - initial_window[0] == DAY_MS, initial_window
    results.append('default window is exactly 24h and has no all-time option')

    # Draft values survive focus changes and do not affect requests before apply.
    before_calls = len(calls)
    form.locator('[name="q"]').fill('demo-auto')
    form.locator('[name="min_input_tokens"]').fill('0')
    form.locator('[name="status"]').focus()
    assert form.locator('[name="q"]').input_value() == 'demo-auto'
    assert len(calls) == before_calls
    results.append('unapplied draft survives blur without querying')

    # Submit by keyboard and verify all values arrive at the real management API.
    form.locator('[name="status"]').select_option('success')
    form.locator('[name="provider"]').select_option('local-demo')
    form.locator('[name="sort"]').select_option('cost_desc')
    form.locator('[name="max_input_tokens"]').fill('999999')
    form.locator('[name="min_output_tokens"]').fill('0')
    form.locator('[name="max_output_tokens"]').fill('999999')
    form.locator('[name="min_cost"]').fill('0.000000001')
    form.locator('[name="max_cost"]').fill('999.123456789')
    with page.expect_request(lambda request: request_list_params(request) is not None) as sent:
        form.locator('[name="max_cost"]').press('Enter')
    page.wait_for_load_state('networkidle')
    params = request_list_params(sent.value)
    expected = {
        'q': 'demo-auto', 'status': 'success', 'provider_id': 'local-demo',
        'sort': 'cost_desc', 'min_input_tokens': '0',
        'max_input_tokens': '999999', 'min_output_tokens': '0',
        'max_output_tokens': '999999', 'min_cost': '0.000000001',
        'max_cost': '999.123456789',
    }
    for key, value in expected.items():
        assert params.get(key) == value, (key, params)
    assert params['to'] - params['from'] == DAY_MS
    results.append('keyboard submit preserves exact combined API parameters')

    # Use a broad real-server query to exercise page 1 -> 2 -> 1.
    form.locator('[name="min_cost"]').fill('0')
    form.locator('[name="max_cost"]').fill('999')
    form.locator('button[type="submit"]').click()
    page.wait_for_timeout(350)
    anchor = (calls[-1]['from'], calls[-1]['to'])
    assert page.locator('[data-action="request-next"]').is_enabled()
    page.locator('[data-action="request-next"]').click()
    page.wait_for_timeout(350)
    assert calls[-1]['page'] == 2
    assert (calls[-1]['from'], calls[-1]['to']) == anchor
    page.locator('[data-action="request-prev"]').click()
    page.wait_for_timeout(350)
    assert calls[-1]['page'] == 1
    assert (calls[-1]['from'], calls[-1]['to']) == anchor
    results.append('real server filtering paginates with a stable time anchor')

    # Custom datetime-local values must be converted in the browser's timezone.
    start = datetime.now().replace(second=0, microsecond=0) - timedelta(days=2)
    end = start + timedelta(days=1, hours=2)
    form = page.locator('#request-filter')
    form.locator('[name="range"]').select_option('custom')
    assert form.locator('.request-custom-time').is_visible()
    form.locator('[name="from"]').fill(local_datetime(start))
    form.locator('[name="to"]').fill(local_datetime(end))
    form.locator('button[type="submit"]').click()
    page.wait_for_timeout(350)
    custom = calls[-1]
    expected_times = page.evaluate("""values => values.map(value => new Date(value).getTime())""",
                                   [local_datetime(start), local_datetime(end)])
    assert [custom['from'], custom['to']] == expected_times, custom
    assert custom['to'] - custom['from'] <= 30 * DAY_MS
    assert page.locator('.empty').is_visible()
    results.append('custom local time is exact and empty state renders')

    # Invalid ranges and values stay client-side and issue no management request.
    before_calls = len(calls)
    form = page.locator('#request-filter')
    too_early = end - timedelta(days=30, minutes=1)
    form.locator('[name="from"]').fill(local_datetime(too_early))
    form.locator('[name="to"]').fill(local_datetime(end))
    form.locator('button[type="submit"]').click()
    assert page.locator('#request-filter-error').is_visible()
    assert '30' in page.locator('#request-filter-error').inner_text()
    assert len(calls) == before_calls
    form.locator('[name="from"]').fill(local_datetime(start))
    form.locator('[name="min_cost"]').fill('1.1234567890')
    form.locator('button[type="submit"]').click()
    assert page.locator('#request-filter-error').is_visible()
    assert len(calls) == before_calls
    results.append('over-30d and malformed numeric input never query the server')

    # Clear returns to a server-issued 24h window and resets every field.
    form.locator('#request-filter-clear').click()
    page.wait_for_timeout(350)
    cleared = calls[-1]
    assert 'from' not in cleared and 'to' not in cleared
    assert cleared['page'] == 1
    assert form.locator('[name="range"]').input_value() == '24h'
    assert form.locator('[name="q"]').input_value() == ''
    assert form.locator('[name="sort"]').input_value() == 'newest'
    cleared_window = page.evaluate("""async () => {
        const {state} = await import('/assets/core.js');
        return [state.requestFilterApplied.from_ms, state.requestFilterApplied.to_ms];
    }""")
    assert cleared_window[1] - cleared_window[0] == DAY_MS
    results.append('clear resets filters and restores 24h')

    # Normal refresh moves the relative window rather than reusing the old anchor.
    old_to = cleared_window[1]
    page.wait_for_timeout(20)
    with page.expect_request(lambda request: request_list_params(request) is not None):
        page.locator('[data-action="refresh"]').click()
    page.wait_for_timeout(250)
    assert calls[-1]['to'] > old_to, (old_to, calls[-1])
    assert calls[-1]['to'] - calls[-1]['from'] == DAY_MS
    results.append('manual refresh advances a relative window')

    # One intercepted failure proves the form is reusable; retry itself is real.
    failed = {'done': False}

    def fail_once(route, request):
        params = request_list_params(request)
        if params is not None and not failed['done']:
            failed['done'] = True
            route.fulfill(status=503, content_type='application/json', body=json.dumps({
                'ok': False, 'error': {'code': 'TEST_UNAVAILABLE', 'message': 'temporary test failure'},
            }))
            return
        route.continue_()

    page.route('**/api.json', fail_once)
    form = page.locator('#request-filter')
    form.locator('[name="q"]').fill('demo-auto')
    expected_503['active'] = True
    form.locator('button[type="submit"]').click()
    page.wait_for_timeout(350)
    expected_503['active'] = False
    page.unroute('**/api.json', fail_once)
    assert failed['done']
    assert form.get_attribute('aria-busy') == 'false'
    form.locator('button[type="submit"]').click()
    page.wait_for_timeout(350)
    assert page.locator('table:not(.compact-table) tbody tr').count() > 0
    results.append('one intercepted network failure can be retried successfully')

    # 每个目标宽度都逐项检查控件矩形，避免卡片自身未溢出但金额输入框越界。
    for width in (1024, 1280, 1440, 390):
        assert_filter_geometry(page, width)
    results.append('all filter fieldsets and inputs stay inside the card at 390/1024/1280/1440px')

    assert not browser_errors, 'browser errors: ' + ' | '.join(browser_errors)
    results.append('zero unexpected console errors or page exceptions')

    page.set_viewport_size({'width': 1280, 'height': 900})
    page.locator('#toasts .toast button').evaluate_all(
        'buttons => buttons.forEach(button => button.click())')
    page.evaluate('scrollTo(0, 0)')
    if artifacts:
        artifacts.mkdir(parents=True, exist_ok=True)
        page.screenshot(path=str(artifacts / 'request-filters-desktop.png'), full_page=True)

    desktop = page.viewport_size
    page.set_viewport_size({'width': 390, 'height': 844})
    page.wait_for_timeout(250)
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1')
    page.locator('#request-filter-clear').focus()
    assert page.evaluate('document.activeElement.id') == 'request-filter-clear'
    page.evaluate('scrollTo(0, 0)')
    if artifacts:
        page.screenshot(path=str(artifacts / 'request-filters-mobile.png'), full_page=True)
    page.set_viewport_size(desktop)
    results.append('390px has no page overflow and controls accept keyboard focus')
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./bin/prism-gateway')
    parser.add_argument('--headed', action='store_true')
    parser.add_argument('--artifacts', type=Path)
    args = parser.parse_args()
    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        print('playwright is not installed; skipping request-filter UI acceptance')
        return 0
    binary = Path(args.binary).resolve()
    if not binary.is_file():
        raise SystemExit('build the gateway first: make build')
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    base = f'http://127.0.0.1:{port}'
    with tempfile.TemporaryDirectory(prefix='prism-request-filters-') as directory:
        directory = Path(directory)
        logpath = directory / 'server.log'
        with logpath.open('w') as log:
            process = subprocess.Popen(
                [str(binary), '--db', str(directory / 'gateway.db'),
                 '--listen', f'127.0.0.1:{port}'],
                stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
            try:
                wait_ready(base, process, logpath)
                token = read_admin_token(directory, logpath)
                rpc = enable_demo(base, token)
                seed_requests(base, rpc)
                with sync_playwright() as playwright:
                    browser = playwright.chromium.launch(headless=not args.headed)
                    page = browser.new_page(viewport={'width': 1280, 'height': 900})
                    try:
                        login(page, base, token)
                        results = run_checks(page, args.artifacts)
                    finally:
                        browser.close()
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=20)
                    except subprocess.TimeoutExpired:
                        process.kill()
    print(json.dumps({'passed': results}, indent=2))
    return 0


if __name__ == '__main__':
    sys.exit(main())
