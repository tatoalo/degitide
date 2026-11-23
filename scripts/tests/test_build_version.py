import importlib.util
import subprocess
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('build_version', Path(__file__).parents[1] / 'build_version.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class BuildVersionTests(unittest.TestCase):
    def test_release_development_and_untagged_history(self):
        with tempfile.TemporaryDirectory() as directory:
            def git(*args):
                subprocess.run(['git', '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
                                '-c', 'commit.gpgsign=false', *args], cwd=directory, check=True, capture_output=True)
            git('init')
            git('commit', '--allow-empty', '-m', 'Initial')
            self.assertEqual(module.build_version(directory), 'dev.1')
            git('tag', 'v0.1.0')
            self.assertEqual(module.build_version(directory), 'v0.1.0')
            git('commit', '--allow-empty', '-m', 'Development')
            self.assertEqual(module.build_version(directory), 'v0.1.0-dev.1')
            git('tag', 'v0.2.0-rc.1')
            self.assertEqual(module.build_version(directory), 'v0.2.0-rc.1')
            git('commit', '--allow-empty', '-m', 'Development after prerelease')
            self.assertEqual(module.build_version(directory), 'v0.2.0-rc.1-dev.1')
