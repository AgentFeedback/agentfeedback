"""Binary wheel platform, version and integrity contracts."""
import base64
import csv
import hashlib
import io
import subprocess
import unittest
import zipfile

import test_package_release as fixtures

RELEASE = fixtures.RELEASE


class WheelTests(unittest.TestCase):
    setUp = fixtures.PackagingTests.setUp
    fixtures = fixtures.PackagingTests.fixtures

    def test_six_platform_wheels_have_verified_payloads_and_complete_records(self):
        self.fixtures()
        RELEASE.generate_wheels('v1.2.3', self.archives, self.output)
        expected = {
            'manylinux_2_17_x86_64.musllinux_1_2_x86_64',
            'manylinux_2_17_aarch64.musllinux_1_2_aarch64',
            'macosx_13_0_x86_64', 'macosx_13_0_arm64', 'win_amd64', 'win_arm64',
        }
        self.assertEqual({p.name for p in self.output.iterdir()}, {f'agentfeedback_cli-1.2.3-py3-none-{tag}.whl' for tag in expected})
        for platform, tag in RELEASE.WHEEL_PLATFORMS.items():
            with self.subTest(platform=platform):
                path = self.output / f'agentfeedback_cli-1.2.3-py3-none-{tag}.whl'
                with zipfile.ZipFile(path) as wheel:
                    info = 'agentfeedback_cli-1.2.3.dist-info'
                    binary = 'agentfeedback_cli/' + ('agentfeedback.exe' if platform[0] == 'windows' else 'agentfeedback')
                    archive = RELEASE.verified_archives('v1.2.3', self.archives)[platform]
                    self.assertEqual(wheel.read(binary), RELEASE.archive_binary(platform[0], archive))
                    self.assertEqual(wheel.getinfo(binary).external_attr >> 16, 0o100755)
                    metadata = wheel.read(f'{info}/WHEEL').decode()
                    self.assertIn('Root-Is-Purelib: false', metadata)
                    self.assertEqual({line[5:] for line in metadata.splitlines() if line.startswith('Tag: ')}, {f'py3-none-{value}' for value in tag.split('.')})
                    records = list(csv.reader(io.StringIO(wheel.read(f'{info}/RECORD').decode())))
                    self.assertEqual({row[0] for row in records}, set(wheel.namelist()))
                    for name, digest, size in records:
                        if name.endswith('/RECORD'):
                            self.assertEqual((digest, size), ('', ''))
                        else:
                            data = wheel.read(name)
                            expected_digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b'=').decode()
                            self.assertEqual(digest, 'sha256=' + expected_digest)
                            self.assertEqual(int(size), len(data))

    def test_rc_cli_uses_pep440_in_filename_and_metadata(self):
        self.fixtures('1.2.3-rc.4')
        result = subprocess.run(['python3', str(fixtures.ROOT / 'scripts/package-release.py'), 'pypi', '--tag', 'v1.2.3-rc.4', '--archives', str(self.archives), '--output', str(self.output)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        for path in self.output.iterdir():
            self.assertTrue(path.name.startswith('agentfeedback_cli-1.2.3rc4-'))
            with zipfile.ZipFile(path) as wheel:
                metadata = wheel.read('agentfeedback_cli-1.2.3rc4.dist-info/METADATA').decode()
                self.assertIn('Version: 1.2.3rc4\n', metadata)

    def test_tampered_archive_produces_no_wheel(self):
        self.fixtures()
        (self.archives / 'agentfeedback_1.2.3_linux_amd64.tar.gz').write_bytes(b'corrupt')
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            RELEASE.generate_wheels('v1.2.3', self.archives, self.output)
        self.assertFalse(self.output.exists())
