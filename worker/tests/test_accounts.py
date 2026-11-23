import asyncio
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import AsyncMock, patch
from aiohttp.test_utils import make_mocked_request
import main
from tasks.common import TaskError


class AccountIsolationTest(unittest.IsolatedAsyncioTestCase):
    async def test_switch_closes_old_runtime_before_changing_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            seen = []
            async def close():
                seen.append((main.active_account, main._session_state_path()))
            with patch.object(main, 'PROFILE_DIR', root), patch.object(main, 'active_account', 'default'), patch.object(main, 'shutdown_browser', AsyncMock(side_effect=close)):
                await main._select_account('savings')
                self.assertEqual(seen, [('default', root / 'session-cookies.json')])
                self.assertEqual(main._session_state_path(), root / 'accounts/savings/session-cookies.json')
                await main._select_account('default')
                self.assertEqual(seen[-1], ('savings', root / 'accounts/savings/session-cookies.json'))

    async def test_cleanup_keeps_other_accounts_and_rejects_traversal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            saved = root / 'accounts/savings/credentials.enc'
            saved.parent.mkdir(parents=True)
            saved.write_text('encrypted')
            (root / 'old-cache').mkdir()
            with patch.object(main, 'PROFILE_DIR', root), patch.object(main, 'active_account', 'default'):
                main._prune_persistent_profile()
                self.assertEqual(saved.read_text(), 'encrypted')
                self.assertFalse((root / 'old-cache').exists())
                with self.assertRaises(TaskError):
                    await main._select_account('../savings')
                with self.assertRaises(Exception):
                    main._request_account(make_mocked_request('GET', '/tasks/check-session?account=..%2Fsavings'))

    async def test_serial_tasks_never_share_an_active_account(self):
        with tempfile.TemporaryDirectory() as directory:
            observed = []
            async def task(expected):
                observed.append(main.active_account)
                await asyncio.sleep(.005)
                self.assertEqual(main.active_account, expected)
                return {'valid': True}
            with patch.object(main, 'PROFILE_DIR', Path(directory)), patch.object(main, 'active_account', 'default'), patch.object(main, 'task_lock', asyncio.Lock()), patch.object(main, 'ensure_browser', AsyncMock()), patch.object(main, 'shutdown_browser', AsyncMock()), patch.object(main, '_schedule_idle_shutdown'):
                await asyncio.gather(main.run_task('first', lambda: task('first'), account='first'), main.run_task('second', lambda: task('second'), account='second'))
            self.assertEqual(observed, ['first', 'second'])
