import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('build_changelog', Path(__file__).parents[1] / 'build_changelog.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ChangelogTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name)
        self.git('init', '-b', 'main')

    def git(self, *args):
        return subprocess.check_output(['git', '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
                                        '-c', 'commit.gpgsign=false', *args], cwd=self.path, text=True,
                                       stderr=subprocess.PIPE).strip()

    def commit(self, title, tag=None):
        self.git('commit', '--allow-empty', '-m', title)
        if tag:
            self.git('tag', tag)

    def build(self, version, **kwargs):
        return module.build_changelog(version, self.path, **kwargs)

    def test_each_release_contains_only_its_changes_and_preserves_history(self):
        self.commit('Initial account setup (#1)', 'v0.1.9')
        self.commit('Show live syncing (#2)', 'v0.1.10')
        self.commit('Generate release notes (#3) [skip ci]', 'v0.1.11')
        data = self.build('v0.1.11')
        self.assertEqual(data['version'], 'v0.1.11')
        self.assertEqual(data['releases'], [
            {'version': 'v0.1.11', 'changes': ['Generate release notes (#3)']},
            {'version': 'v0.1.10', 'changes': ['Show live syncing (#2)']},
            {'version': 'v0.1.9', 'changes': ['Initial account setup (#1)']},
        ])
        self.assertEqual(self.build('v0.1.11', limit=1)['releases'], data['releases'][:1])
        # A rebuild of an older tag must never contain a later release.
        self.git('checkout', 'v0.1.10')
        self.assertEqual([r['version'] for r in self.build('v0.1.10')['releases']], ['v0.1.10', 'v0.1.9'])

    def test_development_pr_and_prerelease_builds_use_the_previous_stable_boundary(self):
        self.commit('Initial', 'v1.0.0')
        self.commit('Feature A', 'v1.1.0-rc.1')
        self.commit('Feature B')
        for version in ('dev', 'v1.0.0-dev.2', 'v0.0.0-pr.52', 'v1.1.0-rc.2'):
            self.assertEqual(self.build(version)['releases'][0],
                             {'version': version, 'changes': ['Feature B', 'Feature A']})
        self.git('tag', 'v1.1.0')
        self.assertEqual(self.build('v1.1.0-rc.2')['releases'][0]['changes'], ['Feature B', 'Feature A'])
        self.assertNotIn('v1.1.0', [r['version'] for r in self.build('v1.1.0-rc.2')['releases']])
        self.git('checkout', 'v1.0.0')
        self.assertEqual(self.build('dev')['releases'][0]['version'], 'v1.0.0')

    def test_unmerged_tags_and_feature_commits_are_not_duplicated(self):
        self.commit('Initial', 'v1.0.0')
        self.git('checkout', '-b', 'unrelated')
        self.commit('Unreleased branch', 'v9.0.0')
        self.git('checkout', 'main')
        self.git('checkout', '-b', 'feature')
        self.commit('Implementation detail')
        self.git('checkout', 'main')
        self.git('merge', '--no-ff', 'feature', '-m', 'Merge pull request #2 from feature\n\nA useful feature')
        self.assertEqual(self.build('v1.1.0')['releases'][0]['changes'], ['A useful feature'])
        self.assertNotIn('v9.0.0', [r['version'] for r in self.build('v1.1.0')['releases']])

    def test_shallow_history_fails_instead_of_shipping_incomplete_notes(self):
        self.commit('Initial', 'v1.0.0')
        self.commit('Next', 'v1.1.0')
        clone = self.path / 'shallow'
        self.git('clone', '--depth=1', self.path.as_uri(), str(clone))
        with self.assertRaisesRegex(ValueError, 'full Git history'):
            module.build_changelog('v1.1.0', clone)

    def test_generator_writes_json_with_literal_title_text(self):
        title = 'Support <holdings> & "history" (#4)'
        self.commit(title, 'v1.0.0')
        output = self.path / 'generated/changelog.json'
        subprocess.run(['python3', str(Path(module.__file__).resolve()), 'v1.0.0', '--output', str(output)],
                       cwd=self.path, check=True, capture_output=True)
        self.assertEqual(json.loads(output.read_text())['releases'][0]['changes'], [title])
