"""Behavioral coverage for verified, offline npm release packaging."""

import hashlib
import importlib.util
import io
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest
import zipfile


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("package_release", ROOT / "scripts/package-release.py")
RELEASE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RELEASE)
NODE = shutil.which("node")


class PackagingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.archives = self.root / "archives"
        self.archives.mkdir()
        self.output = self.root / "npm"

    def fixtures(self, version="1.2.3", bad_member=None):
        checksums = []
        for os_name, arch, _, _ in RELEASE.PLATFORMS:
            windows = os_name == "windows"
            binary = "agentfeedback.exe" if windows else "agentfeedback"
            payload = b"#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf '%s\\n' \"$AF_PACKAGING_TEST\"\nexit 23\n"
            buffer = io.BytesIO()
            if windows:
                with zipfile.ZipFile(buffer, "w") as archive:
                    member = zipfile.ZipInfo(binary)
                    member.create_system = 3
                    member.external_attr = (0o120777 if bad_member == (os_name, arch) else 0o100755) << 16
                    archive.writestr(member, payload)
            else:
                with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
                    member = tarfile.TarInfo(binary)
                    if bad_member == (os_name, arch):
                        member.type = tarfile.SYMTYPE
                        member.linkname = "/untrusted"
                        archive.addfile(member)
                    else:
                        member.size = len(payload)
                        member.mode = 0o755
                        archive.addfile(member, io.BytesIO(payload))
            name = f"agentfeedback_{version}_{os_name}_{arch}.{'zip' if windows else 'tar.gz'}"
            data = buffer.getvalue()
            (self.archives / name).write_bytes(data)
            checksums.append(f"{hashlib.sha256(data).hexdigest()}  {name}")
        checksums.append(f"{hashlib.sha256(b'installer').hexdigest()}  install.sh")
        (self.archives / "SHA256SUMS").write_text("\n".join(checksums) + "\n")

    def generate(self, tag="v1.2.3"):
        RELEASE.generate_npm(tag, self.archives, self.output)

    def test_cli_generates_exact_release_and_rc_dependencies(self):
        for version in ("1.2.3", "1.2.3-rc.4"):
            with self.subTest(version=version):
                self.fixtures(version)
                result = subprocess.run([
                    "python3", str(ROOT / "scripts/package-release.py"), "npm",
                    "--tag", f"v{version}", "--archives", str(self.archives),
                    "--output", str(self.output),
                ], capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                main = json.loads((self.output / "cli/package.json").read_text())
                expected = {f"@agentfeedback/cli-{os_name}-{arch}": version for _, _, os_name, arch in RELEASE.PLATFORMS}
                self.assertEqual(main["optionalDependencies"], expected)
                self.assertEqual(main["version"], version)
                self.assertEqual(main["bin"], {"agentfeedback": "launcher.cjs"})
                self.assertEqual(set(main["os"]), {"linux", "darwin", "win32"})
                self.assertEqual(set(main["cpu"]), {"x64", "arm64"})
                for _, _, os_name, arch in RELEASE.PLATFORMS:
                    directory = self.output / f"cli-{os_name}-{arch}"
                    manifest = json.loads((directory / "package.json").read_text())
                    self.assertEqual(manifest["version"], version)
                    self.assertEqual(manifest["os"], [os_name])
                    self.assertEqual(manifest["cpu"], [arch])
                    self.assertNotIn("scripts", manifest)
                    binary = directory / ("agentfeedback.exe" if os_name == "win32" else "agentfeedback")
                    self.assertTrue(binary.is_file())
                    self.assertTrue(binary.stat().st_mode & 0o111)

    def test_rejects_noncanonical_tags(self):
        for tag in ("1.2.3", "v01.2.3", "v1.2.3-rc.01", "v1.2.3-beta.1", "v1.2.3+build", "v1.2.3\n"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                RELEASE.release_version(tag)

    def test_tampering_is_rejected_before_archive_parsing(self):
        self.fixtures()
        # The first archive is malformed but checksummed; the last is tampered.
        first = self.archives / "agentfeedback_1.2.3_linux_amd64.tar.gz"
        original = hashlib.sha256(first.read_bytes()).hexdigest()
        first.write_bytes(b"not an archive")
        sums = self.archives / "SHA256SUMS"
        sums.write_text(sums.read_text().replace(original, hashlib.sha256(first.read_bytes()).hexdigest()))
        (self.archives / "agentfeedback_1.2.3_windows_arm64.zip").write_bytes(b"tampered")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.generate()
        self.assertFalse(self.output.exists())

    def test_missing_duplicate_and_malformed_checksums(self):
        for failure in ("missing", "duplicate", "malformed", "wrong"):
            with self.subTest(failure=failure):
                self.fixtures()
                sums = self.archives / "SHA256SUMS"
                lines = sums.read_text().splitlines()
                if failure == "missing":
                    lines.pop(0)
                elif failure == "duplicate":
                    lines.append(lines[0])
                elif failure == "malformed":
                    lines[0] = "not-a-hash  file"
                else:
                    lines[0] = "0" * 64 + lines[0][64:]
                sums.write_text("\n".join(lines) + "\n")
                with self.assertRaises(ValueError):
                    self.generate()
                self.assertFalse(self.output.exists())

    def test_missing_archive_is_rejected(self):
        self.fixtures()
        (self.archives / "agentfeedback_1.2.3_linux_amd64.tar.gz").unlink()
        with self.assertRaises(FileNotFoundError):
            self.generate()
        self.assertFalse(self.output.exists())

    def test_nonregular_binary_members_are_rejected(self):
        for platform in (("linux", "amd64"), ("windows", "arm64")):
            with self.subTest(platform=platform):
                self.fixtures(bad_member=platform)
                with self.assertRaisesRegex(ValueError, "nonregular binary"):
                    self.generate()
                self.assertFalse(self.output.exists())

    @unittest.skipUnless(NODE, "Node is required for launcher tests")
    def test_platform_check_accepts_all_supported_and_rejects_unsupported(self):
        self.fixtures()
        self.generate()
        check = self.output / "cli/check-platform.cjs"
        for _, _, os_name, arch in RELEASE.PLATFORMS:
            result = subprocess.run([NODE, "-e", f"require({json.dumps(str(check))})({json.dumps(os_name)}, {json.dumps(arch)})"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
        result = subprocess.run([NODE, "-e", f"require({json.dumps(str(check))})('freebsd', 'x64')"], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        for _, _, os_name, arch in RELEASE.PLATFORMS:
            self.assertIn(f"{os_name}-{arch}", result.stderr)

    @unittest.skipUnless(NODE, "Node is required for launcher tests")
    def test_launcher_dispatch_arguments_environment_and_exit(self):
        import os
        self.fixtures()
        self.generate()
        platform = subprocess.check_output([NODE, "-p", "process.platform + '-' + process.arch"], text=True).strip()
        if not platform.startswith(("linux-", "darwin-")):
            self.skipTest("shell fixture requires a supported POSIX host")
        modules = self.output / "cli/node_modules/@agentfeedback"
        modules.mkdir(parents=True)
        shutil.copytree(self.output / f"cli-{platform}", modules / f"cli-{platform}")
        result = subprocess.run([NODE, str(self.output / "cli/launcher.cjs"), "space argument", "$(not-a-shell)", "--flag"], env={**os.environ, "AF_PACKAGING_TEST": "forwarded"}, capture_output=True, text=True)
        self.assertEqual(result.returncode, 23, result.stderr)
        self.assertEqual(result.stdout.splitlines(), ["space argument", "$(not-a-shell)", "--flag", "forwarded"])
        binary = modules / f"cli-{platform}/agentfeedback"
        binary.write_text("#!/bin/sh\nkill -TERM $$\n")
        result = subprocess.run([NODE, str(self.output / "cli/launcher.cjs")], capture_output=True, text=True)
        self.assertEqual(result.returncode, -15, result.stderr)
        import signal
        import sys
        binary.write_text(
            f"#!{sys.executable}\n"
            "import signal, sys\n"
            "signal.signal(signal.SIGTERM, lambda *_: sys.exit(42))\n"
            "print('ready', flush=True)\n"
            "signal.pause()\n"
        )
        process = subprocess.Popen([NODE, str(self.output / "cli/launcher.cjs")], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            self.assertEqual(process.stdout.readline().strip(), "ready")
            process.send_signal(signal.SIGTERM)
            stdout, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 42, stderr)
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()

    @unittest.skipUnless(NODE, "Node is required for launcher tests")
    def test_launcher_missing_optional_dependency_is_actionable(self):
        self.fixtures()
        self.generate()
        result = subprocess.run([NODE, str(self.output / "cli/launcher.cjs")], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("optional dependencies enabled", result.stderr)
        self.assertIn("--omit=optional", result.stderr)



if __name__ == "__main__":
    unittest.main()
