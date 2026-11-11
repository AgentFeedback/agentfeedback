#!/usr/bin/env python3
"""Generate offline npm packages from verified release archives."""

import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile
import zipfile


PLATFORMS = tuple(
    (os_name, arch, "win32" if os_name == "windows" else os_name,
     "x64" if arch == "amd64" else arch)
    for os_name in ("linux", "darwin", "windows")
    for arch in ("amd64", "arm64")
)
TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-rc\.(0|[1-9][0-9]*))?\Z")


def release_version(tag):
    if not TAG.fullmatch(tag):
        raise ValueError("tag must be canonical vX.Y.Z or vX.Y.Z-rc.N")
    return tag[1:]


def verified_archives(tag, archives):
    """Return {(release_os, release_arch): archive_bytes} after verifying all six.

    No archive is opened until every expected archive matches SHA256SUMS.
    Consumers (including other distribution formats) can safely reuse these bytes.
    """
    version = release_version(tag)
    archives = Path(archives)
    expected = {
        f"agentfeedback_{version}_{os_name}_{arch}.{'zip' if os_name == 'windows' else 'tar.gz'}": (os_name, arch)
        for os_name, arch, _, _ in PLATFORMS
    }
    checksums = {}
    for line in (archives / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
        match = re.fullmatch(r"([0-9a-fA-F]{64}) [ *](\S+)", line)
        if not match:
            raise ValueError("malformed SHA256SUMS entry")
        digest, name = match.groups()
        if name in checksums:
            raise ValueError(f"duplicate checksum entry: {name}")
        if name not in expected and name != "install.sh":
            raise ValueError(f"unexpected checksum entry: {name}")
        checksums[name] = digest.lower()
    missing = expected.keys() - checksums.keys()
    if missing:
        raise ValueError(f"missing checksum entries: {', '.join(sorted(missing))}")
    verified = {}
    for name, platform in expected.items():
        payload = (archives / name).read_bytes()
        if hashlib.sha256(payload).hexdigest() != checksums[name]:
            raise ValueError(f"checksum mismatch: {name}")
        verified[platform] = payload
    return verified


def archive_binary(os_name, payload):
    """Read the unique regular root binary without extracting archive paths."""
    binary = "agentfeedback.exe" if os_name == "windows" else "agentfeedback"
    if os_name == "windows":
        with zipfile.ZipFile(io.BytesIO(payload)) as archive:
            members = [member for member in archive.infolist() if member.filename == binary]
            if len(members) != 1:
                raise ValueError(f"archive must contain exactly one root {binary}")
            member = members[0]
            mode = member.external_attr >> 16
            # DOS ZIP files commonly omit Unix file type bits entirely.
            if member.is_dir() or (mode & 0o170000) not in (0, 0o100000):
                raise ValueError(f"nonregular binary member: {binary}")
            return archive.read(member)
    with tarfile.open(fileobj=io.BytesIO(payload), mode="r:gz") as archive:
        members = [member for member in archive.getmembers() if member.name == binary]
        if len(members) != 1:
            raise ValueError(f"archive must contain exactly one root {binary}")
        if not members[0].isfile():
            raise ValueError(f"nonregular binary member: {binary}")
        with archive.extractfile(members[0]) as stream:
            return stream.read()


def generate_npm(tag, archives, output):
    version = release_version(tag)
    verified = verified_archives(tag, archives)
    # Validate every binary before writing any output package.
    binaries = {platform: archive_binary(platform[0], data) for platform, data in verified.items()}
    output = Path(output)
    templates = Path(__file__).parent / "npm"
    metadata = {
        "version": version,
        "license": "MIT",
        "repository": {"type": "git", "url": "https://github.com/AgentFeedback/agentfeedback.git"},
    }

    def package(directory, manifest):
        destination = output / directory
        destination.mkdir(parents=True, exist_ok=True)
        (destination / "package.json").write_text(json.dumps({**metadata, **manifest}, indent=2) + "\n", encoding="utf-8")
        (destination / "LICENSE").write_bytes((Path(__file__).resolve().parents[1] / "LICENSE").read_bytes())
        return destination

    dependencies = {}
    for os_name, arch, npm_os, npm_arch in PLATFORMS:
        suffix = f"cli-{npm_os}-{npm_arch}"
        name = f"@agentfeedback/{suffix}"
        dependencies[name] = version
        binary = "agentfeedback.exe" if os_name == "windows" else "agentfeedback"
        destination = package(suffix, {"name": name, "os": [npm_os], "cpu": [npm_arch], "files": [binary]})
        target = destination / binary
        target.write_bytes(binaries[(os_name, arch)])
        target.chmod(0o755)
    destination = package("cli", {
        "name": "@agentfeedback/cli",
        "bin": {"agentfeedback": "launcher.cjs"},
        "os": ["linux", "darwin", "win32"],
        "cpu": ["x64", "arm64"],
        "optionalDependencies": dependencies,
        "files": ["launcher.cjs", "check-platform.cjs"],
    })
    for name in ("launcher.cjs", "check-platform.cjs"):
        (destination / name).write_bytes((templates / name).read_bytes())
    (destination / "launcher.cjs").chmod(0o755)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("format", choices=["npm"])
    parser.add_argument("--tag", required=True)
    parser.add_argument("--archives", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    try:
        generate_npm(args.tag, args.archives, args.output)
    except (ValueError, OSError, tarfile.TarError, zipfile.BadZipFile) as error:
        parser.exit(1, f"package-release: {error}\n")


if __name__ == "__main__":
    main()
