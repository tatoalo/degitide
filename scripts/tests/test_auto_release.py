import importlib.util
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import call, patch


spec = importlib.util.spec_from_file_location('auto_release', Path(__file__).parents[1] / 'auto_release.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class VersionTests(unittest.TestCase):
    def test_numeric_order_and_prerelease_exclusion(self):
        tag = module.latest_stable_tag(['v0.1.9', 'v0.1.10', 'v1.0.0-rc.1', 'other'])
        self.assertEqual(tag, 'v0.1.10')
        self.assertEqual(module.next_patch(tag), 'v0.1.11')

    def test_manual_major_releases_set_the_baseline(self):
        self.assertEqual(module.next_patch(module.latest_stable_tag(['v0.9.9', 'v1.0.0'])), 'v1.0.1')

    def test_missing_stable_baseline_fails(self):
        with self.assertRaises(RuntimeError):
            module.latest_stable_tag(['v0.1.0-rc.1', 'v01.0.0'])


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.cwd = directory.name
        self.git('init', '-q')
        self.git('commit', '--allow-empty', '-qm', 'Previous release')
        self.previous = self.git('rev-parse', 'HEAD')
        self.git('tag', '-a', 'v0.1.4', '-m', 'Previous release')
        self.git('commit', '--allow-empty', '-qm', 'New feature')
        self.sha = self.git('rev-parse', 'HEAD')
        self.main = self.sha
        self.created = []
        self.start_patch('command', side_effect=self.command)
        self.start_patch('api', side_effect=self.api)
        self.publish = self.start_patch('ensure_published')

    def start_patch(self, name, **kwargs):
        patcher = patch.object(module, name, **kwargs)
        self.addCleanup(patcher.stop)
        return patcher.start()

    def git(self, *args):
        return self.command('git', '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
                            '-c', 'commit.gpgsign=false', '-c', 'tag.gpgsign=false', *args)

    def command(self, *args):
        return subprocess.check_output(args, cwd=self.cwd, text=True, stderr=subprocess.PIPE).strip()

    def api(self, path, **fields):
        if path == 'git/ref/heads/main':
            return {'object': {'sha': self.main}}
        self.assertEqual(path, 'git/refs')
        self.git('tag', fields['ref'].removeprefix('refs/tags/'), fields['sha'])
        self.created.append(fields)

    def test_release_targets_tested_commit_and_increments_once(self):
        module.release(self.sha)
        self.assertEqual(self.created, [{'ref': 'refs/tags/v0.1.5', 'sha': self.sha}])
        self.publish.assert_has_calls([call('v0.1.4', self.previous), call('v0.1.5', self.sha)])
        module.release(self.sha)
        self.assertEqual(len(self.created), 1)

    def test_retry_after_dispatch_failure_reuses_reserved_tag(self):
        self.publish.side_effect = [None, RuntimeError('Dispatch unavailable')]
        with self.assertRaises(RuntimeError):
            module.release(self.sha)
        self.publish.side_effect = None
        module.release(self.sha)
        self.assertEqual(len(self.created), 1)
        self.publish.assert_called_with('v0.1.5', self.sha)

    def test_stale_commit_is_skipped(self):
        self.main = self.previous
        module.release(self.sha)
        self.assertEqual(self.created, [])
        self.publish.assert_not_called()

    def test_skip_release_marker_does_not_create_tag(self):
        self.git('commit', '--amend', '--allow-empty', '-qm', 'Configure automation [skip release]')
        self.sha = self.main = self.git('rev-parse', 'HEAD')
        module.release(self.sha)
        self.assertEqual(self.created, [])
        self.publish.assert_not_called()

    def test_checkout_mismatch_is_rejected(self):
        with self.assertRaises(RuntimeError):
            module.release(self.previous)
        self.assertEqual(self.created, [])

    def test_advance_while_waiting_does_not_release_stale_commit(self):
        def advance(*args):
            self.main = self.previous
        self.publish.side_effect = advance
        module.release(self.sha)
        self.assertEqual(self.created, [])

    def test_failed_previous_release_blocks_next_version(self):
        self.publish.side_effect = RuntimeError('Previous publisher failed')
        with self.assertRaises(RuntimeError):
            module.release(self.sha)
        self.assertEqual(self.created, [])

    def test_unrelated_tag_history_is_rejected(self):
        self.git('checkout', '--orphan', 'unrelated')
        self.git('commit', '--allow-empty', '-qm', 'Unrelated history')
        self.git('tag', 'v1.0.0')
        self.git('checkout', '--detach', self.sha)
        with self.assertRaises(subprocess.CalledProcessError):
            module.release(self.sha)
        self.assertEqual(self.created, [])
        self.publish.assert_not_called()


class PublisherTests(unittest.TestCase):
    def setUp(self):
        self.run = {'id': 42, 'head_branch': 'v0.1.5', 'head_sha': 'a' * 40,
                    'event': 'workflow_dispatch', 'status': 'in_progress', 'conclusion': None,
                    'html_url': 'https://github.com/example/repo/actions/runs/42'}

    @patch.object(module.time, 'sleep')
    @patch.object(module, 'command')
    @patch.object(module, 'api')
    @patch.object(module, 'published', side_effect=[False, False, True])
    def test_dispatches_once_and_waits_for_visibility(self, published, api, command, sleep):
        api.return_value = {'workflow_runs': []}
        module.ensure_published('v0.1.5', 'a' * 40)
        command.assert_called_once_with('gh', 'workflow', 'run', module.WORKFLOW, '--ref', 'v0.1.5')

    @patch.object(module.time, 'sleep')
    @patch.object(module, 'command')
    @patch.object(module, 'api')
    @patch.object(module, 'published', side_effect=[False, True])
    def test_existing_run_is_reused(self, published, api, command, sleep):
        api.return_value = {'workflow_runs': [self.run]}
        module.ensure_published('v0.1.5', 'a' * 40)
        command.assert_not_called()

    @patch.object(module, 'command')
    @patch.object(module, 'api')
    @patch.object(module, 'published', return_value=False)
    def test_failed_run_requires_recovery_without_another_dispatch(self, published, api, command):
        self.run.update(status='completed', conclusion='failure')
        api.return_value = {'workflow_runs': [self.run]}
        with self.assertRaisesRegex(RuntimeError, 'Fix or rerun'):
            module.ensure_published('v0.1.5', 'a' * 40)
        command.assert_not_called()

    @patch.object(module.time, 'sleep')
    @patch.object(module, 'command')
    @patch.object(module, 'api')
    @patch.object(module, 'published', side_effect=[False, True])
    def test_other_refs_and_pr_runs_do_not_suppress_dispatch(self, published, api, command, sleep):
        api.return_value = {'workflow_runs': [dict(self.run, head_branch='main'),
                                              dict(self.run, event='pull_request')]}
        module.ensure_published('v0.1.5', 'a' * 40)
        command.assert_called_once()

    @patch.object(module, 'api')
    def test_draft_and_prerelease_are_not_published_stable_releases(self, api):
        for value in [None, {'draft': True, 'prerelease': False}, {'draft': False, 'prerelease': True}]:
            api.return_value = value
            self.assertFalse(module.published('v0.1.5'))


if __name__ == '__main__':
    unittest.main()
