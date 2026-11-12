#!/usr/bin/env python3
"""Publish generated npm packages, verifying immutable versions on rerun."""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile
from urllib.request import urlopen


def dist_tag(tag):
    if not re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?', tag):
        raise ValueError('expected canonical release tag')
    return 'next' if '-rc.' in tag else 'latest'


def metadata_value(output):
    value = json.loads(output)
    if isinstance(value, list):
        value, = value
    return value


def publish(package, tag, run=subprocess.run):
    manifest = json.loads((package / 'package.json').read_text())
    with tempfile.TemporaryDirectory() as temporary:
        packed = run(['npm', 'pack', str(package.resolve()), '--pack-destination', temporary, '--json'], capture_output=True, text=True, check=True)
        data = json.loads(packed.stdout)
        entry = next(iter(data.values())) if isinstance(data, dict) else data[0]
        tarball = Path(temporary) / entry['filename']
        checksum = hashlib.sha1(tarball.read_bytes()).hexdigest()
        spec = f"{manifest['name']}@{manifest['version']}"
        existing = run(['npm', 'view', spec, 'dist.shasum', '--json'], capture_output=True, text=True)
        if existing.returncode == 0:
            published_checksum = metadata_value(existing.stdout)
            if published_checksum != checksum:
                location = run(['npm', 'view', spec, 'dist.tarball', '--json'], capture_output=True, text=True, check=True)
                with urlopen(metadata_value(location.stdout), timeout=30) as response:
                    published = response.read()
                if hashlib.sha1(published).hexdigest() != published_checksum:
                    raise ValueError(f'{spec}: downloaded tarball does not match registry checksum')
                # Node builds use different zlib implementations; gzip bytes can differ
                # even when every tar header and packaged byte is identical.
                if gzip.decompress(published) != gzip.decompress(tarball.read_bytes()):
                    raise ValueError(f'{spec} already exists with different package contents; never overwrite a published version')
            print(f'{spec}: identical published package, skipping')
            return
        error = json.loads(existing.stdout).get('error', {})
        if error.get('code') != 'E404':
            raise RuntimeError(f'cannot check {spec}: {error.get("code", "unknown npm error")}')
        run(['npm', 'publish', str(tarball), '--access', 'public', '--provenance', '--tag', tag], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--packages', type=Path, required=True)
    args = parser.parse_args()
    tag = dist_tag(args.tag)
    # Publish the launcher only after all exact-version platform dependencies exist.
    packages = sorted(args.packages.glob('cli-*')) + [args.packages / 'cli']
    for package in packages:
        publish(package, tag)


if __name__ == '__main__':
    main()
