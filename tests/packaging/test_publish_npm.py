"""Recovery must never skip a conflicting immutable npm version."""
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('publish_npm', ROOT / 'scripts/publish-npm.py')
PUBLISH = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PUBLISH)


class PublishTests(unittest.TestCase):
    def test_release_channels_reject_noncanonical_tags(self):
        self.assertEqual(PUBLISH.dist_tag('v4.6.0'), 'latest')
        self.assertEqual(PUBLISH.dist_tag('v4.6.0-rc.1'), 'next')
        for tag in ('v04.6.0', 'v4.6.0-rc.01', 'v4.6.0-beta.1', 'main'):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                PUBLISH.dist_tag(tag)

    @unittest.skipUnless(shutil.which('npm'), 'npm required for packed recovery checks')
    def test_resume_verifies_real_tarball_and_fails_closed(self):
        with tempfile.TemporaryDirectory() as temporary:
            package = Path(temporary) / 'cli'
            package.mkdir()
            (package / 'package.json').write_text(json.dumps({'name': '@agentfeedback/recovery-fixture', 'version': '1.0.0'}))
            for state in ('identical', 'conflict', 'absent', 'unauthorized'):
                with self.subTest(state=state):
                    published = []
                    checksum = None

                    def run(command, **kwargs):
                        nonlocal checksum
                        if command[1] == 'pack':
                            result = subprocess.run(command, **kwargs)
                            data = json.loads(result.stdout)
                            entry = next(iter(data.values())) if isinstance(data, dict) else data[0]
                            checksum = hashlib.sha1((Path(command[4]) / entry['filename']).read_bytes()).hexdigest()
                            return result
                        if command[1] == 'view':
                            if state in ('identical', 'conflict'):
                                return subprocess.CompletedProcess(command, 0, json.dumps(checksum if state == 'identical' else '0' * 40))
                            return subprocess.CompletedProcess(command, 1, json.dumps({'error': {'code': 'E404' if state == 'absent' else 'E401'}}))
                        published.append(command)
                        self.assertTrue(Path(command[2]).is_file())
                        return subprocess.CompletedProcess(command, 0)

                    if state == 'conflict':
                        with self.assertRaisesRegex(ValueError, 'different package bytes'):
                            PUBLISH.publish(package, 'next', run)
                    elif state == 'unauthorized':
                        with self.assertRaisesRegex(RuntimeError, 'E401'):
                            PUBLISH.publish(package, 'next', run)
                    else:
                        PUBLISH.publish(package, 'next', run)
                    self.assertEqual(len(published), 1 if state == 'absent' else 0)
