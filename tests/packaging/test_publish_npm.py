"""Recovery must never skip a conflicting immutable npm version."""
import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

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
            for state in ('identical', 'recompressed', 'conflict', 'absent', 'unauthorized'):
                with self.subTest(state=state):
                    published = []
                    checksum = None
                    payload = None

                    def run(command, **kwargs):
                        nonlocal checksum, payload
                        if command[1] == 'pack':
                            result = subprocess.run(command, **kwargs)
                            data = json.loads(result.stdout)
                            entry = next(iter(data.values())) if isinstance(data, dict) else data[0]
                            payload = (Path(command[4]) / entry['filename']).read_bytes()
                            if state in ('recompressed', 'conflict'):
                                contents = gzip.decompress(payload)
                                if state == 'conflict':
                                    contents = contents.replace(b'"1.0.0"', b'"9.0.0"')
                                payload = gzip.compress(contents, compresslevel=1, mtime=0)
                            checksum = hashlib.sha1(payload).hexdigest()
                            return result
                        if command[1] == 'view':
                            if command[3] == 'dist.tarball':
                                location = 'https://example.invalid/package.tgz'
                                return subprocess.CompletedProcess(command, 0, json.dumps([location] if state == 'recompressed' else location))
                            if state in ('identical', 'recompressed', 'conflict'):
                                return subprocess.CompletedProcess(command, 0, json.dumps([checksum] if state == 'recompressed' else checksum))
                            return subprocess.CompletedProcess(command, 1, json.dumps({'error': {'code': 'E404' if state == 'absent' else 'E401'}}))
                        published.append(command)
                        return subprocess.CompletedProcess(command, 0)

                    with patch.object(PUBLISH, 'urlopen', side_effect=lambda *args, **kwargs: io.BytesIO(payload)):
                        if state == 'conflict':
                            with self.assertRaises(ValueError):
                                PUBLISH.publish(package, 'next', run)
                        elif state == 'unauthorized':
                            with self.assertRaises(RuntimeError):
                                PUBLISH.publish(package, 'next', run)
                        else:
                            PUBLISH.publish(package, 'next', run)
                    self.assertEqual(len(published), 1 if state == 'absent' else 0)
