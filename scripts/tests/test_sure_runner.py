import importlib.util
import subprocess
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('sure_runner', Path(__file__).parents[2] / 'tests/integration/sure/runner.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

# Real pipes and child lifecycle, without Docker or Rails. Reject input sent
# before readiness so the test covers the startup handshake as well as reuse.
CHILD = '''
import json, select, sys, time
time.sleep(.02)
if select.select([sys.stdin], [], [], 0)[0]:
    raise SystemExit('Request arrived before readiness')
print('Boot log', flush=True)
print('E2E_READY', flush=True)
count = 0
for line in sys.stdin:
    command = json.loads(line)['command']
    if command == 'exit':
        break
    count += 1
    result = {'error': 'Fixture failure'} if command == 'fail' else {'count': count}
    print('E2E_JSON:' + json.dumps(result), flush=True)
'''


class SureRunnerTests(unittest.TestCase):
    def setUp(self):
        popen = subprocess.Popen
        self.lookup = patch.object(module.subprocess, 'check_output', return_value='synthetic-container\n')
        self.launch = patch.object(module.subprocess, 'Popen', side_effect=lambda _args, **kwargs: popen(
            [sys.executable, '-u', '-c', CHILD], **kwargs))
        self.lookup.start()
        self.launch.start()
        self.runner = module.SureRunner(['docker', 'compose'])
        self.addCleanup(self.lookup.stop)
        self.addCleanup(self.launch.stop)
        self.addCleanup(self.runner.close)

    def test_waits_for_readiness_reuses_process_and_restarts_after_close(self):
        self.assertEqual(self.runner.request('snapshot'), {'count': 1})
        self.assertEqual(self.runner.request('snapshot'), {'count': 2})
        self.assertEqual(self.runner.boots, 1)
        self.runner.close()
        self.assertEqual(self.runner.request('snapshot'), {'count': 1})
        self.assertEqual(self.runner.boots, 2)

    def test_reports_child_errors_and_eof_without_replaying_commands(self):
        for command, message in [('fail', 'Fixture failure'), ('exit', 'Rails runner exited')]:
            with self.assertRaisesRegex(RuntimeError, message):
                self.runner.request(command, timeout=2)
            self.assertIsNone(self.runner.process)
