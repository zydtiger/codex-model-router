#!/usr/bin/env python3
"""Exercise the router with a real Codex app-server and loopback mock inference.

Run on macOS with --codex pointing to the desktop's bundled executable.
No account, external inference, or production configuration is used.
"""
import argparse
import contextlib
import http.server
import json
import os
from pathlib import Path
import queue
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--codex', required=True)
    parser.add_argument('--router', required=True)
    args = parser.parse_args()
    args.codex = str(Path(args.codex).resolve())
    args.router = str(Path(args.router).resolve())
    requests = []
    errors = []
    auto_tool_sent = False
    preturn_sent = False
    compact_count = 0

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            nonlocal auto_tool_sent, preturn_sent, compact_count
            try:
                data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                requests.append(data)
                text = json.dumps(data.get('input', []))
                compact = 'Create a concise context checkpoint' in text
                is_auto = 'AUTO-CHECK' in text
                assert 'compaction_trigger' not in text
                assert 'cmr.compaction.' not in text
                if compact:
                    compact_count += 1
                    assert data['tools'] == [] and data['tool_choice'] == 'none'
                    assert 'cedar-742' in text
                    item = message('Checkpoint: preserve cedar-742. Continue the pending task.')
                elif is_auto and not auto_tool_sent:
                    auto_tool_sent = True
                    item = {'id': 'fc_probe', 'type': 'function_call', 'name': 'exec_command',
                            'call_id': 'call_probe', 'arguments': json.dumps({'cmd': "printf 'cedar-742'"}),
                            'status': 'completed'}
                else:
                    item = message('Secret checkpoint fact: cedar-742.')
                large_usage = item['type'] == 'function_call' or ('PRETURN-CHECK' in text and not preturn_sent and not compact)
                if 'PRETURN-CHECK' in text and not compact:
                    preturn_sent = True
                usage = {'input_tokens': 60000 if large_usage else 100,
                         'output_tokens': 20, 'total_tokens': 60020 if large_usage else 120}
                response = {'id': f'resp_{len(requests)}', 'object': 'response', 'status': 'completed',
                            'model': 'gpt-5.4', 'output': [item], 'usage': usage}
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                events = [('response.created', {'response': {**response, 'status': 'in_progress', 'output': []}}),
                          ('response.output_item.added', {'output_index': 0, 'item': item}),
                          ('response.output_item.done', {'output_index': 0, 'item': item}),
                          ('response.completed', {'response': response})]
                for n, (kind, fields) in enumerate(events):
                    self.wfile.write(('data: ' + json.dumps({'type': kind, 'sequence_number': n, **fields}) + '\n\n').encode())
                self.wfile.flush()
            except Exception as exc:
                errors.append(repr(exc))
                self.close_connection = True

    def message(text):
        return {'type': 'message', 'id': f'msg_{len(requests)}', 'role': 'assistant', 'status': 'completed',
                'content': [{'type': 'output_text', 'text': text, 'annotations': []}]}

    with tempfile.TemporaryDirectory(prefix='cmr-compaction-') as directory, contextlib.ExitStack() as stack:
        root = Path(directory)
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        stack.callback(server.server_close)
        stack.callback(server.shutdown)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        config = {'listen': {'host': '127.0.0.1', 'port': port}, 'routes': [{
            'name': 'mock', 'base_url': f'http://127.0.0.1:{server.server_port}/v1',
            'models': ['gpt-5.4'], 'compaction': {'adapter': 'text_summary'},
            'reasoning': {'adapter': 'sglang_chat_template', 'chat_template_kwargs': {'enable_thinking': False}}}]}
        (root / 'router.json').write_text(json.dumps(config))
        (root / 'auth.json').write_text(json.dumps({'OPENAI_API_KEY': 'mock-test'}))
        (root / 'config.toml').write_text(
            f'openai_base_url = "http://127.0.0.1:{port}/v1"\nmodel = "gpt-5.4"\n'
            'model_context_window = 100000\nmodel_auto_compact_token_limit = 55000\n'
            '[features]\nremote_compaction_v2 = true\n')
        processes = []

        def stop(process):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)

        def cleanup():
            for process in reversed(processes):
                stop(process)
        stack.callback(cleanup)

        def start_router():
            log = stack.enter_context((root / f'router-{len(processes)}.log').open('w'))
            process = subprocess.Popen([args.router, 'serve', '--config', str(root / 'router.json')], stdout=log, stderr=log)
            processes.append(process)
            for _ in range(100):
                try:
                    with urllib.request.urlopen(f'http://127.0.0.1:{port}/healthz', timeout=1):
                        return process
                except OSError:
                    time.sleep(.05)
            raise RuntimeError('router failed to start')

        class Client:
            def __init__(self):
                self.queue = queue.Queue()
                self.seq = 0
                self.events = []
                log = stack.enter_context((root / f'codex-{len(processes)}.log').open('w'))
                self.process = subprocess.Popen([args.codex, 'app-server'], stdin=subprocess.PIPE,
                    stdout=subprocess.PIPE, stderr=log, text=True,
                    env={**os.environ, 'CODEX_HOME': str(root), 'OPENAI_API_KEY': 'mock-test'})
                processes.append(self.process)
                threading.Thread(target=self.read, daemon=True).start()
                self.call('initialize', {'clientInfo': {'name': 'compaction-test', 'version': '1'},
                                        'capabilities': {'experimentalApi': True}})

            def read(self):
                for line in self.process.stdout:
                    try:
                        self.queue.put(json.loads(line))
                    except ValueError:
                        pass

            def call(self, method, params):
                self.seq += 1
                request_id = self.seq
                self.process.stdin.write(json.dumps({'id': request_id, 'method': method, 'params': params}) + '\n')
                self.process.stdin.flush()
                while True:
                    event = self.queue.get(timeout=60)
                    if event.get('id') == request_id:
                        if 'error' in event:
                            raise RuntimeError(event)
                        return event.get('result')
                    self.events.append(event)

            def done(self):
                while True:
                    event = self.events.pop(0) if self.events else self.queue.get(timeout=60)
                    if event.get('method') == 'turn/completed':
                        turn = event['params']['turn']
                        assert turn['status'] == 'completed', turn.get('error')
                        return

            def turn(self, tid, text):
                self.call('turn/start', {'threadId': tid, 'input': [{'type': 'text', 'text': text}]})
                self.done()

            def compact(self, tid):
                self.call('thread/compact/start', {'threadId': tid})
                self.done()

        router = start_router()
        client = Client()
        tid = client.call('thread/start', {'cwd': str(root), 'model': 'gpt-5.4',
            'approvalPolicy': 'never', 'sandbox': 'danger-full-access'})['thread']['id']
        client.turn(tid, 'Remember a checkpoint fact.')
        client.compact(tid)
        client.turn(tid, 'Continue from the checkpoint.')
        assert_checkpoint(requests[-1])
        client.compact(tid)
        # Restart both processes: checkpoint recovery must not rely on memory.
        stop(client.process)
        stop(router)
        router = start_router()
        client = Client()
        client.call('thread/resume', {'threadId': tid, 'cwd': str(root)})
        client.turn(tid, 'Continue after restart.')
        assert_checkpoint(requests[-1])
        fork = client.call('thread/fork', {'threadId': tid})['thread']['id']
        client.turn(fork, 'Continue in the fork.')
        assert any('Context checkpoint' in json.dumps(i) for i in requests[-1]['input'])
        before = compact_count
        auto = client.call('thread/start', {'cwd': str(root), 'model': 'gpt-5.4',
            'approvalPolicy': 'never', 'sandbox': 'danger-full-access'})['thread']['id']
        client.turn(auto, 'AUTO-CHECK: run a command then continue.')
        assert auto_tool_sent and compact_count > before, 'automatic compaction was not exercised'
        assert_checkpoint(requests[-1])
        assert not any(i.get('type') == 'function_call_output' for i in requests[-1]['input'])
        midturn_count = compact_count - before
        pre = client.call('thread/start', {'cwd': str(root), 'model': 'gpt-5.4',
            'approvalPolicy': 'never', 'sandbox': 'danger-full-access'})['thread']['id']
        client.turn(pre, 'PRETURN-CHECK: remember a checkpoint fact.')
        pre_count = compact_count
        client.turn(pre, 'Continue with the previous fact.')
        assert compact_count > pre_count, 'pre-turn compaction was not exercised'
        assert_checkpoint(requests[-1])
        assert not errors, errors
        print(json.dumps({'manual_compactions': before, 'midturn_auto_compactions': midturn_count, 'preturn_auto_compactions': compact_count - pre_count,
                          'resume_after_restart': True, 'fork': True, 'old_tool_history_removed': True,
                          'checkpoint_fact_retained': True, 'upstream_requests': len(requests)}))


def assert_checkpoint(request):
    text = json.dumps(request['input'])
    assert 'Context checkpoint' in text and 'cedar-742' in text
    assert not any(i.get('role') == 'assistant' for i in request['input']), 'old assistant history retained'


if __name__ == '__main__':
    main()
