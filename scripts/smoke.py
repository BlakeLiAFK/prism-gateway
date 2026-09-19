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
        command = [str(binary), '--db', str(directory / 'gateway.db'), '--listen', f'127.0.0.1:{port}', '--demo']
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
            token = re.search(r'prism_admin_[A-Za-z0-9_-]+', logpath.read_text()).group()

            def rpc(action, params=None):
                status, _, body = request('/api.json', {'action': action, 'params': params or {}}, {'Authorization': 'Bearer ' + token})
                obj = json.loads(body)
                assert status == 200 and obj['ok'], (action, status, obj)
                return obj['data']

            status, headers, body = request('/')
            assert status == 200 and b'assets/app.js' in body and headers['X-Frame-Options'] == 'DENY'
            status, _, body = request('/assets/app.js')
            assert status == 200 and b'providerEditor' in body
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
