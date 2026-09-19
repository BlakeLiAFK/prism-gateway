#!/usr/bin/env python3
"""并发与长稳压测。

用本地演示模型施压：不花钱、不依赖外部服务、结果可重复。
验证的是网关自身在持续负载下的表现——吞吐、延迟分布、并发计数器是否
完全释放、内存是否稳定——而不是某个上游有多快。

用法:
    python3 scripts/loadtest.py --binary ./bin/prism-gateway
    python3 scripts/loadtest.py --binary ./bin/prism-gateway --concurrency 64 --duration 300
"""
import argparse
import json
import os
import re
import socket
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def rss_kb(pid):
    try:
        out = subprocess.run(['ps', '-o', 'rss=', '-p', str(pid)],
                             capture_output=True, text=True).stdout.strip()
        return int(out) if out else 0
    except Exception:
        return 0


class Runner:
    def __init__(self, base, key, stream_ratio):
        self.base, self.key, self.stream_ratio = base, key, stream_ratio
        self.lock = threading.Lock()
        self.latencies = []
        self.codes = {}
        self.stop = threading.Event()

    def record(self, code, elapsed):
        with self.lock:
            self.codes[code] = self.codes.get(code, 0) + 1
            self.latencies.append(elapsed)

    def one(self, stream):
        payload = {'model': 'demo-chat', 'max_tokens': 64,
                   'messages': [{'role': 'user', 'content': 'load test'}]}
        if stream:
            payload['stream'] = True
        req = urllib.request.Request(
            self.base + '/openai/v1/chat/completions',
            data=json.dumps(payload).encode(),
            headers={'Content-Type': 'application/json',
                     'Authorization': 'Bearer ' + self.key})
        started = time.perf_counter()
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                r.read()
                code = r.status
        except urllib.error.HTTPError as e:
            e.read()
            code = e.code
        except Exception as e:
            code = type(e).__name__
        self.record(code, time.perf_counter() - started)

    def worker(self, seed):
        n = seed
        while not self.stop.is_set():
            n += 1
            self.one(self.stream_ratio and n % self.stream_ratio == 0)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--binary', default='./bin/prism-gateway')
    ap.add_argument('--concurrency', type=int, default=32)
    ap.add_argument('--duration', type=int, default=60, help='秒')
    ap.add_argument('--stream-ratio', type=int, default=4, help='每 N 个请求走一次流式，0 表示不用流式')
    args = ap.parse_args()

    binary = Path(args.binary).resolve()
    if not binary.is_file():
        raise SystemExit('先构建：make build')
    port = free_port()
    base = f'http://127.0.0.1:{port}'

    with tempfile.TemporaryDirectory(prefix='prism-load-') as directory:
        directory = Path(directory)
        log = (directory / 'server.log').open('w')
        proc = subprocess.Popen([str(binary), '--db', str(directory / 'gateway.db'),
                                 '--listen', f'127.0.0.1:{port}'],
                                stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
        try:
            for _ in range(100):
                try:
                    with urllib.request.urlopen(base + '/healthz', timeout=5):
                        break
                except Exception:
                    time.sleep(.1)
            else:
                raise SystemExit('网关未就绪')

            token_file = directory / 'gateway.db.admin-token'
            token = (token_file.read_text().strip() if token_file.exists()
                     else re.search(r'prism_admin_[A-Za-z0-9_-]+',
                                    (directory / 'server.log').read_text()).group())

            def rpc(action, params=None):
                req = urllib.request.Request(
                    base + '/api.json',
                    data=json.dumps({'action': action, 'params': params or {}}).encode(),
                    headers={'Content-Type': 'application/json',
                             'Authorization': 'Bearer ' + token})
                with urllib.request.urlopen(req, timeout=30) as r:
                    d = json.load(r)
                assert d['ok'], (action, d)
                return d['data']

            rpc('demo.enable', {'version': rpc('config.get')['version']})
            # 放开本地限流：这里要压的是网关本身，不是限流器
            cfg = rpc('config.get')
            rpc('settings.update', {'version': cfg['version'],
                                    'settings': {'global_concurrency': 512}})
            for m in rpc('config.get')['models']:
                if m['id'] == 'demo-chat':
                    m2 = dict(m, concurrency=128, rpm=0)
                    rpc('model.save', {'version': rpc('config.get')['version'],
                                       'id': m['id'], 'model': m2})
            key = rpc('apikey.create', {'name': 'loadtest', 'allowed': []})['key']

            runner = Runner(base, key, args.stream_ratio)
            samples = []
            threads = [threading.Thread(target=runner.worker, args=(i,), daemon=True)
                       for i in range(args.concurrency)]
            begin = time.perf_counter()
            for t in threads:
                t.start()
            while time.perf_counter() - begin < args.duration:
                time.sleep(5)
                info = rpc('system.info')
                samples.append({'t': round(time.perf_counter() - begin),
                                'rss_kb': rss_kb(proc.pid),
                                'active': info['runtime']['active'],
                                'done': len(runner.latencies)})
            runner.stop.set()
            for t in threads:
                t.join(timeout=30)
            elapsed = time.perf_counter() - begin

            time.sleep(1)
            final = rpc('system.info')
            lat = sorted(runner.latencies)
            def pct(p):
                return lat[min(len(lat) - 1, int(len(lat) * p))] * 1000 if lat else 0
            db_bytes = (directory / 'gateway.db').stat().st_size
            ok = sum(v for k, v in runner.codes.items() if k == 200)
            result = {
                'concurrency': args.concurrency,
                'duration_s': round(elapsed, 1),
                'requests': len(lat),
                'qps': round(len(lat) / elapsed, 1),
                'success_rate': round(ok / len(lat) * 100, 2) if lat else 0,
                'status_codes': {str(k): v for k, v in sorted(runner.codes.items(), key=str)},
                'latency_ms': {'p50': round(pct(.50), 1), 'p95': round(pct(.95), 1),
                               'p99': round(pct(.99), 1), 'max': round(lat[-1] * 1000, 1) if lat else 0},
                'rss_kb': {'first': samples[0]['rss_kb'] if samples else 0,
                           'last': samples[-1]['rss_kb'] if samples else 0,
                           'peak': max((s['rss_kb'] for s in samples), default=0)},
                'active_after_drain': final['runtime']['active'],
                'db_bytes': db_bytes,
                'samples': samples,
            }
            print(json.dumps(result, ensure_ascii=False, indent=2))

            problems = []
            if result['active_after_drain'] != 0:
                problems.append('并发计数器未归零：%s' % result['active_after_drain'])
            if result['success_rate'] < 99:
                problems.append('成功率偏低：%s%%' % result['success_rate'])
            if len(samples) >= 3:
                head = samples[0]['rss_kb'] or 1
                growth = (samples[-1]['rss_kb'] - head) / head
                if growth > 0.5:
                    problems.append('内存增长 %.0f%%，疑似泄漏' % (growth * 100))
            if problems:
                print('\n问题：', '；'.join(problems), file=sys.stderr)
                return 1
            print('\n通过：无泄漏迹象，计数器已完全释放')
            return 0
        finally:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    proc.kill()
            log.close()


if __name__ == '__main__':
    sys.exit(main())
