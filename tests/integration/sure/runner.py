"""A bounded, reusable Rails runner for the synthetic Sure integration stack."""
import json
import queue
import subprocess
import threading
import time
from collections import deque


class SureRunner:
    def __init__(self, compose):
        self.compose = compose
        self.process = None
        self.boots = 0
        self.requests = 0
        self.seconds = 0

    def _start(self):
        self.responses = queue.Queue()
        container = subprocess.check_output(self.compose + ['ps', '-q', 'web'], text=True, timeout=15).strip()
        if not container:
            raise RuntimeError('Sure web container is not running')
        self.process = subprocess.Popen(
            [self.compose[0], 'exec', '-i', container, 'bin/rails', 'runner', '/integration/runner.rb'],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, bufsize=1,
        )
        self.boots += 1
        # A reader thread also handles Rails boot output without buffering or
        # select/readline races. EOF wakes the caller instead of hanging it.
        def read(stdout, responses):
            try:
                for line in stdout:
                    responses.put(line.rstrip())
            finally:
                responses.put(None)
        self.reader = threading.Thread(target=read, args=(self.process.stdout, self.responses), daemon=True)
        self.reader.start()
        self.ready = False

    def request(self, command, timeout=45):
        started = time.monotonic()
        recent = deque(maxlen=10)
        def next_line():
            line = self.responses.get(timeout=max(0, timeout - (time.monotonic() - started)))
            if line is None:
                raise RuntimeError('Rails runner exited')
            recent.append(line)
            return line
        try:
            if self.process is None:
                self._start()
            while not self.ready:
                self.ready = next_line() == 'E2E_READY'
            self.process.stdin.write(json.dumps({'command': command}) + '\n')
            self.process.stdin.flush()
            while True:
                line = next_line()
                if line.startswith('E2E_JSON:'):
                    result = json.loads(line.removeprefix('E2E_JSON:'))
                    if 'error' in result:
                        raise RuntimeError(result['error'])
                    return result
                if line.startswith('PASS '):
                    print(line, flush=True)
        except (queue.Empty, BrokenPipeError, RuntimeError) as error:
            self.close()
            raise RuntimeError(f'Sure runner {command} failed or timed out: {error}\n' + '\n'.join(recent)) from error
        finally:
            self.requests += 1
            self.seconds += time.monotonic() - started

    def close(self):
        if self.process is None:
            return
        process, self.process = self.process, None
        try:
            process.stdin.close()
        except BrokenPipeError:
            pass
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
        self.reader.join(timeout=5)
        process.stdout.close()
