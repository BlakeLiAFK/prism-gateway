#!/usr/bin/env python3
"""Start an isolated gateway and verify embedded UI + RPC + model APIs.
Python standard library only. Never uses real provider credentials or charges.
Usage: python3 scripts/smoke.py --binary ./prism-gateway
"""
import argparse
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request



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
    args = parser.parse_args()
    binary = Path(args.binary).resolve()
    if not binary.is_file():
        raise SystemExit('Build first: make build (or specify --binary ./prism-gateway)')
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    base = f'http://127.0.0.1:{port}'
    results = []
    with tempfile.TemporaryDirectory(prefix='prism-smoke-') as directory:
        directory = Path(directory)
        logpath = directory / 'server.log'
        log = logpath.open('w')
        command = [str(binary), '--db', str(directory / 'gateway.db'), '--listen', f'127.0.0.1:{port}']
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)

        def request(path, data=None, headers=None):
            h = dict(headers or {})
            payload = None if data is None else json.dumps(data).encode()
            if payload is not None:
                h['Content-Type'] = 'application/json'
            req = urllib.request.Request(base + path, data=payload, headers=h)
            try:
                response = urllib.request.urlopen(req, timeout=20)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, response.headers, response.read()

        def wait_ready():
            for _ in range(100):
                if process.poll() is not None:
                    raise RuntimeError('Gateway exited: ' + logpath.read_text())
                try:
                    status, _, _ = request('/api.json', {'action': 'auth.status', 'params': {}})
                    if status == 200:
                        return
                except (OSError, urllib.error.URLError):
                    pass
                time.sleep(.1)
            raise RuntimeError('Gateway did not become ready within 10 seconds')

        try:
            wait_ready()
            token = read_admin_token(directory, logpath)

            def rpc(action, params=None):
                status, _, body = request('/api.json', {'action': action, 'params': params or {}}, {'Authorization': 'Bearer ' + token})
                obj = json.loads(body)
                assert status == 200 and obj['ok'], (action, status, obj)
                return obj['data']

            rpc('demo.enable', {'version': rpc('config.get')['version']})
            status, headers, body = request('/')
            assert status == 200 and b'assets/app.js' in body and headers['X-Frame-Options'] == 'DENY'
            # 前端是多模块 ES Module：入口能加载但某个被 import 的模块 404，
            # 页面会静默空白，所以逐个确认可服务
            for asset, marker in (('app.js', b'export function renderPage'),
                                  ('core.js', b'export const state'),
                                  ('ui.js', b'export function btn'),
                                  ('views.js', b'export function settings')):
                status, _, body = request('/assets/' + asset)
                assert status == 200 and marker in body, asset
            status, _, _ = request('/api/v1/providers')
            assert status == 404
            results.append('embedded UI + single management endpoint')
            config = rpc('config.get')
            assert len(config['models']) == 3
            key_data = rpc('apikey.create', {'name': 'isolated smoke', 'allowed': []})
            key = key_data['key']
            assert key not in json.dumps(rpc('apikey.list'))
            auth = {'Authorization': 'Bearer ' + key, 'X-Prism-Session': 'smoke-conversation'}
            endpoints = {
                'chat': '/openai/v1/chat/completions',
                'responses': '/openai/v1/responses',
                'messages': '/anthropic/v1/messages',
            }
            for protocol, endpoint in endpoints.items():
                payload = {'model': 'demo-' + protocol}
                if protocol == 'responses':
                    payload.update(input='Hello', max_output_tokens=256)
                else:
                    payload.update(messages=[{'role': 'user', 'content': 'Hello'}], max_tokens=256)
                status, headers, body = request(endpoint, payload, auth)
                assert status == 200 and headers['X-Prism-Demo'] == 'true'
                assert '本地演示' in json.dumps(json.loads(body), ensure_ascii=False)
                payload['stream'] = True
                status, headers, body = request(endpoint, payload, auth)
                terminal = {'chat': b'[DONE]', 'messages': b'message_stop', 'responses': b'response.completed'}[protocol]
                assert status == 200 and 'text/event-stream' in headers['Content-Type'] and terminal in body
                results.append(protocol + ': JSON + SSE')
            status, headers, body = request('/anthropic/v1/messages/count_tokens', {'model': 'demo-messages', 'messages': [{'role': 'user', 'content': 'hello'}]}, auth)
            assert status == 200 and headers['X-Prism-Token-Count-Mode'] == 'estimated'
            assert json.loads(body)['input_tokens'] > 0
            results.append('explicit estimated token count')
            for endpoint in ['/openai/v1/models', '/anthropic/v1/models']:
                status, _, body = request(endpoint, headers=auth)
                assert status == 200 and len(json.loads(body)['data']) == 4
            results.append('OpenAI + Anthropic model catalogs')
            summary = rpc('dashboard.get')['summary']
            assert summary['requests'] == 6 and summary['demo_requests'] == 6
            assert summary['cost_nano'] == 0
            rpc('settings.update', {'version': config['version'], 'settings': {'app_name': 'Persisted smoke'}})
            process.terminate()
            process.wait(timeout=20)
            process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
            wait_ready()
            assert rpc('config.get')['settings']['app_name'] == 'Persisted smoke'
            assert rpc('dashboard.get')['summary']['requests'] == 6
            results.append('restart: config + credential + usage persistence')
            rpc('apikey.revoke', {'id': key_data['id']})
            status, _, _ = request('/openai/v1/models', headers=auth)
            assert status == 401
            results.append('key revoke enforcement')
            status, _, body = request('/healthz')
            probe = json.loads(body)
            assert status == 200 and probe['ok'] is True and 'config_version' not in probe
            status, _, _ = request('/healthz', {'action': 'auth.status'})
            assert status == 405
            results.append('public health probe')
            exported = rpc('config.export')
            assert exported['format'] == 'prism.config/1'
            dumped = json.dumps(exported)
            assert key not in dumped and token not in dumped
            version = rpc('config.get')['version']
            rpc('settings.update', {'version': version, 'settings': {'app_name': 'Overwritten'}})
            assert rpc('config.get')['settings']['app_name'] == 'Overwritten'
            rpc('config.import', {'version': rpc('config.get')['version'], 'config': exported['config']})
            restored = rpc('config.get')
            assert restored['settings']['app_name'] == 'Persisted smoke'
            assert len(restored['models']) == 3
            results.append('config export/import round trip')
            version = rpc('config.get')['version']
            rpc('settings.update', {'version': version, 'settings': {'log_level': 'debug', 'log_format': 'json'}})
            assert rpc('config.get')['settings']['log_level'] == 'debug'
            status, _, body = request('/api.json', {'action': 'settings.update', 'params': {'version': rpc('config.get')['version'], 'settings': {'log_level': 'verbose'}}}, {'Authorization': 'Bearer ' + token})
            assert status == 400 and not json.loads(body)['ok']
            rpc('settings.update', {'version': rpc('config.get')['version'], 'settings': {'log_level': 'info', 'log_format': 'text'}})
            results.append('runtime log level switching')
            with socket.socket() as probe:
                probe.bind(('127.0.0.1', 0))
                new_port = probe.getsockname()[1]
            rpc('settings.update', {'version': rpc('config.get')['version'], 'settings': {'listen': f'127.0.0.1:{new_port}'}})
            moved = f'http://127.0.0.1:{new_port}'
            with urllib.request.urlopen(moved + '/healthz', timeout=10) as response:
                assert json.loads(response.read())['ok'] is True
            try:
                urllib.request.urlopen(base + '/healthz', timeout=3)
                raise AssertionError('旧监听地址应已关闭')
            except (urllib.error.URLError, OSError):
                pass
            base = moved
            status, _, body = request('/api.json', {'action': 'settings.update', 'params': {'version': rpc('config.get')['version'], 'settings': {'listen': '0.0.0.0:9'}}}, {'Authorization': 'Bearer ' + token})
            assert status == 400 and json.loads(body)['error']['code'] == 'REMOTE_NOT_ALLOWED'
            with urllib.request.urlopen(base + '/healthz', timeout=5) as response:
                assert json.loads(response.read())['ok'] is True
            results.append('listen hot switch + remote guard')
            status, _, _ = request('/metrics', headers={'Authorization': 'Bearer ' + token})
            assert status == 404, '指标端点默认必须关闭'
            rpc('settings.update', {'version': rpc('config.get')['version'], 'settings': {'metrics_enabled': True}})
            status, _, _ = request('/metrics')
            assert status == 401, '开启后匿名抓取必须被拒绝'
            status, headers, body = request('/metrics', headers={'Authorization': 'Bearer ' + token})
            assert status == 200 and 'text/plain' in headers['Content-Type']
            text = body.decode()
            assert 'prism_build_info{version=' in text and 'prism_requests_total{' in text
            assert token not in text and key not in text
            results.append('prometheus metrics gate')
            snapshot = rpc('backup.create')
            assert snapshot['bytes'] > 0 and os.path.isfile(snapshot['path'])
            assert len(rpc('backup.list')) == 1
            with open(snapshot['path'], 'rb') as handle:
                content = handle.read()
            # 库里只有前缀（用于界面识别）与 SHA-256，完整密钥不可恢复
            assert key.encode() not in content, '备份中出现了完整调用密钥'
            assert token.encode() not in content, '备份中出现了管理员令牌'
            results.append('online backup snapshot')
            info = rpc('system.info')
            print(json.dumps({'passed': results, 'runtime': info}, ensure_ascii=False, indent=2))
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            log.close()


if __name__ == '__main__':
    main()
