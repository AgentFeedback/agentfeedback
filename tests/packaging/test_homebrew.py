"""Reject casks that route a platform to the wrong release archive."""
import importlib.util
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('check_homebrew', ROOT / 'scripts/check-homebrew.py')
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class HomebrewTests(unittest.TestCase):
    def setUp(self):
        self.sums = ''
        self.cask = 'cask "agentfeedback" do\n  version "1.2.3"\n'
        for os, goos in [('macos', 'darwin'), ('linux', 'linux')]:
            self.cask += f'  on_{os} do\n'
            for arch, goarch in [('arm', 'arm64'), ('intel', 'amd64')]:
                digest = str(len(self.sums) % 10) * 64
                suffix = f'_{goos}_{goarch}.tar.gz'
                self.sums += f'{digest}  agentfeedback_1.2.3{suffix}\n'
                self.cask += (f'    on_{arch} do\n      sha256 "{digest}"\n'
                              f'      url "{CHECK.REPOSITORY}/releases/download/v1.2.3/'
                              f'agentfeedback_#{{version}}{suffix}"\n    end\n')
            self.cask += '  end\n'
        self.cask += '  binary "agentfeedback"\nend\n'

    def test_release_and_snapshot_mappings(self):
        CHECK.check(self.cask, self.sums, 'v1.2.3', '1.2.3')
        CHECK.check(self.cask.replace('version "1.2.3"', 'version "1.2.3-snapshot"'),
                    self.sums.replace('_1.2.3_', '_1.2.3-snapshot_'),
                    'v1.2.3', '1.2.3-snapshot')

    def test_wrong_architecture_url_or_checksum_is_rejected(self):
        for cask in [self.cask.replace('_linux_arm64', '_linux_amd64'),
                     self.cask.replace('sha256 "' + '0' * 64, 'sha256 "' + 'f' * 64),
                     self.cask.replace('/download/v1.2.3/', '/download/v1.2.2/')]:
            with self.subTest(cask=cask), self.assertRaisesRegex(ValueError, 'URL or checksum'):
                CHECK.check(cask, self.sums, 'v1.2.3', '1.2.3')

    def test_missing_or_duplicate_platform_is_rejected(self):
        for cask in [self.cask.replace('on_linux do', 'on_macos do'),
                     self.cask.replace('on_intel do', 'on_arm do')]:
            with self.subTest(cask=cask), self.assertRaisesRegex(ValueError, 'selectors'):
                CHECK.check(cask, self.sums, 'v1.2.3', '1.2.3')

    def test_wrong_version_or_missing_checksum_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'version'):
            CHECK.check(self.cask, self.sums, 'v1.2.4', '1.2.4')
        with self.assertRaisesRegex(ValueError, 'checksum'):
            CHECK.check(self.cask, '\n'.join(self.sums.splitlines()[1:]), 'v1.2.3', '1.2.3')

    def test_stable_tap_never_moves_backwards(self):
        CHECK.check_upgrade(self.cask, '1.2.4')
        CHECK.check_upgrade(self.cask, '1.10.0')
        for version in ['1.2.2', '1.2.3', '1.2.4-rc.1']:
            with self.subTest(version=version), self.assertRaises(ValueError):
                CHECK.check_upgrade(self.cask, version)
        with self.assertRaisesRegex(ValueError, 'stable versions'):
            CHECK.check_upgrade('not a cask', '1.2.4')

    def test_swapped_platform_checksums_are_rejected(self):
        first, second = [line.split()[0] for line in self.sums.splitlines()[:2]]
        swapped = self.cask.replace(first, 'SWAP').replace(second, first).replace('SWAP', second)
        with self.assertRaisesRegex(ValueError, 'checksum'):
            CHECK.check(swapped, self.sums, 'v1.2.3', '1.2.3')

    def test_malformed_checksum_entry_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'invalid checksum entry'):
            CHECK.check(self.cask, self.sums + '\n', 'v1.2.3', '1.2.3')
