#!/usr/bin/env python3
"""Check GoReleaser's four-platform cask against release SHA256SUMS."""
import argparse
from pathlib import Path
import re


REPOSITORY = 'https://github.com/AgentFeedback/agentfeedback'


def check_upgrade(previous_cask, version):
    previous = re.findall(r'^  version "(\d+\.\d+\.\d+)"$', previous_cask, re.M)
    if len(previous) != 1 or not re.fullmatch(r'\d+\.\d+\.\d+', version):
        raise ValueError('expected stable versions for Homebrew upgrade')
    if tuple(map(int, version.split('.'))) <= tuple(map(int, previous[0].split('.'))):
        raise ValueError(f'Homebrew version {version} must be newer than {previous[0]}')


def check(cask, sums, tag, version):
    if re.findall(r'^  version "([^"]+)"$', cask, re.M) != [version]:
        raise ValueError('cask version differs from release')
    checksums = {}
    for line in sums.splitlines():
        fields = line.split()
        if len(fields) != 2:
            raise ValueError('invalid checksum entry: expected digest and filename')
        digest, name = fields
        if name in checksums or not re.fullmatch(r'[0-9a-f]{64}', digest):
            raise ValueError('invalid or duplicate checksum entry')
        checksums[name] = digest
    # Match the generated nested selectors, not merely four URLs: a valid
    # checksum in the wrong architecture branch still installs the wrong binary.
    blocks = re.findall(r'^  on_(macos|linux) do\n(.*?)^  end$', cask, re.M | re.S)
    if len(blocks) != 2 or {os for os, _ in blocks} != {'macos', 'linux'}:
        raise ValueError('expected exactly macOS and Linux selectors')
    for os, body in blocks:
        platforms = re.findall(
            r'^    on_(arm|intel) do\n      sha256 "([0-9a-f]{64})"\n'
            r'      url "([^"]+)"\n    end$', body, re.M)
        if len(platforms) != 2 or {arch for arch, _, _ in platforms} != {'arm', 'intel'}:
            raise ValueError(f'expected exactly ARM and Intel selectors for {os}')
        for arch, digest, url in platforms:
            goos = 'darwin' if os == 'macos' else os
            goarch = 'arm64' if arch == 'arm' else 'amd64'
            suffix = f'_{goos}_{goarch}.tar.gz'
            name = f'agentfeedback_{version}{suffix}'
            expected_url = f'{REPOSITORY}/releases/download/{tag}/agentfeedback_#{{version}}{suffix}'
            if url != expected_url or checksums.get(name) != digest:
                raise ValueError(f'{os}/{arch}: URL or checksum differs from {name}')
    if re.findall(r'^  binary "([^"]+)"$', cask, re.M) != ['agentfeedback']:
        raise ValueError('expected agentfeedback binary installation')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cask', type=Path, required=True)
    parser.add_argument('--sums', type=Path, required=True)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--version', help='defaults to the tag without v; snapshots override this')
    parser.add_argument('--previous-cask', type=Path, help='reject an equal or older stable version')
    args = parser.parse_args()
    check(args.cask.read_text(), args.sums.read_text(), args.tag,
          args.version or args.tag.removeprefix('v'))
    if args.previous_cask:
        check_upgrade(args.previous_cask.read_text(), args.version or args.tag.removeprefix('v'))
    print('Homebrew cask: all four platform URLs and checksums match')


if __name__ == '__main__':
    main()
