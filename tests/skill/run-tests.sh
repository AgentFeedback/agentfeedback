#!/usr/bin/env bash
# Offline test suite for skills/agentfeedback/scripts/install.sh. A fixture
# release layout under a temp dir plays GitHub Releases through
# AGENT_FEEDBACK_RELEASE_URL=file://...; HOME is a fresh temp dir per case, so
# nothing touches the real ~/.local/bin and no request leaves the machine.
#
# Lives OUTSIDE skills/agentfeedback/ on purpose: the skill directory is
# copied as-is into a harness, and test tooling must never travel with it.
#
# Portable to macOS and Linux: archives are built for linux and darwin, both
# architectures, so the host's own platform is always in the fixture.
#
# Usage: bash tests/skill/run-tests.sh
# Requires: bash, curl, tar, gzip, sha256sum or shasum.
set -u

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL="$TESTS_DIR/../../skills/agentfeedback/scripts/install.sh"

WORK=$(mktemp -d)
WORK=$(cd "$WORK" && pwd -P)   # canonical: /private/var/... on macOS
trap 'rm -rf "$WORK"' EXIT
REL="$WORK/releases"

# The host must not already provide agentfeedback on the PATH the cases use,
# or every case would short-circuit to "already installed".
export PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
if command -v agentfeedback >/dev/null 2>&1; then
  echo "FATAL agentfeedback found at $(command -v agentfeedback) on the system PATH; the suite needs a PATH without it" >&2
  exit 1
fi
unset AGENT_FEEDBACK_VERSION AGENT_FEEDBACK_RELEASE_URL
cd "$WORK" || exit 1

if command -v sha256sum >/dev/null 2>&1; then sums() { sha256sum "$@"; }
else sums() { shasum -a 256 "$@"; }; fi

pass=0; fail=0
chk() { # chk <desc> <ok 0|1>
  if [ "$2" = 1 ]; then pass=$((pass+1)); echo "PASS  $1";
  else fail=$((fail+1)); echo "FAIL  $1"; fi
}

# release <tag> [members...]: archives for linux and darwin x amd64 and arm64
# holding a stand-in binary that prints its version, plus install.sh and a
# SHA256SUMS listing the archives and install.sh, as a published release does.
release() {
  local v=${1#v} dir="$REL/download/$1" stage os arch
  shift
  mkdir -p "$dir"
  stage=$(mktemp -d "$WORK/stage.XXXXXX")
  printf '#!/bin/sh\n%s\n' "${BINARY_BODY:-echo \"agentfeedback $v (fixture)\"}" >"$stage/agentfeedback"
  chmod 0755 "$stage/agentfeedback"
  echo license >"$stage/LICENSE"; echo readme >"$stage/README.md"
  for os in linux darwin; do
    for arch in amd64 arm64; do
      tar -czf "$dir/agentfeedback_${v}_${os}_${arch}.tar.gz" -C "$stage" "${@:-agentfeedback}" LICENSE README.md
    done
  done
  cp "$INSTALL" "$dir/install.sh"
  (cd "$dir" && sums agentfeedback_*.tar.gz install.sh >SHA256SUMS)
}
latest() { # latest <tag>: point latest/download at a release
  rm -rf "$REL/latest"; mkdir -p "$REL/latest"
  cp -R "$REL/download/$1" "$REL/latest/download"
}

release v1.2.3
release v1.3.0-rc.1
release v1.2.4   # checksum corrupted below
release v1.2.6 LICENSE   # archives without the binary
BINARY_BODY='exit 3' release v1.2.7   # a binary that does not run
release v1.2.8   # SHA256SUMS lists the host archive twice
for f in "$REL/download/v1.2.8/"*.tar.gz; do cp "$f" "${f%.tar.gz}.copy.tar.gz"; done
(cd "$REL/download/v1.2.8" && sums agentfeedback_*.tar.gz | sed 's/\.copy\.tar\.gz$/.tar.gz/' >SHA256SUMS)
mkdir -p "$REL/download/v1.2.5"   # an archive under the wrong version name
cp "$REL/download/v1.2.3/"* "$REL/download/v1.2.5/"
latest v1.2.3
awk '{ $1 = "0000000000000000000000000000000000000000000000000000000000000000"; print $1 "  " $2 }' \
  "$REL/download/v1.2.4/SHA256SUMS" >"$WORK/bad" && mv "$WORK/bad" "$REL/download/v1.2.4/SHA256SUMS"
export AGENT_FEEDBACK_RELEASE_URL="file://$REL"

# run [args...]: install.sh under a fresh HOME; sets out (stdout), err, rc, home.
run() {
  home=$(mktemp -d "$WORK/home.XXXXXX")
  out=$(HOME="$home" bash "$INSTALL" "$@" 2>"$WORK/err"); rc=$?
  err=$(cat "$WORK/err")
}
installed() { [ -x "$home/.local/bin/agentfeedback" ] && echo 1 || echo 0; }
nothing_installed() { [ -z "$(ls -A "$home/.local/bin" 2>/dev/null)" ] && echo 1 || echo 0; }

# ── install from the fixture release ──────────────────────────────────────────
run
chk "unpinned: installs the latest stable release to ~/.local/bin and prints its path" \
  "$([ "$rc" = 0 ] && [ "$out" = "$home/.local/bin/agentfeedback" ] && [ "$(installed)" = 1 ] &&
     [ "$("$home/.local/bin/agentfeedback")" = "agentfeedback 1.2.3 (fixture)" ] && echo 1 || echo 0)"
chk "unpinned: SHA256SUMS listing install.sh beside the archives does not disturb the archive's line" \
  "$([ "$rc" = 0 ] && grep -q '  install\.sh$' "$REL/latest/download/SHA256SUMS" && echo 1 || echo 0)"
chk "unpinned: leaves only the binary in ~/.local/bin (no staging file)" \
  "$([ "$(ls -A "$home/.local/bin")" = agentfeedback ] && echo 1 || echo 0)"
chk "unpinned: says which version it installed and that ~/.local/bin is not on PATH" \
  "$(grep -q 'installed v1.2.3' <<<"$err" && grep -q 'not on PATH' <<<"$err" && echo 1 || echo 0)"

run --version v1.3.0-rc.1
chk "--version installs a named pre-release" \
  "$([ "$rc" = 0 ] && [ "$("$home/.local/bin/agentfeedback")" = "agentfeedback 1.3.0-rc.1 (fixture)" ] && echo 1 || echo 0)"

run --version=v1.3.0-rc.1
chk "--version=<tag> installs that release" \
  "$([ "$rc" = 0 ] && [ "$("$home/.local/bin/agentfeedback")" = "agentfeedback 1.3.0-rc.1 (fixture)" ] && echo 1 || echo 0)"

home=$(mktemp -d "$WORK/home.XXXXXX")
out=$(HOME="$home" AGENT_FEEDBACK_VERSION=v1.3.0-rc.1 bash "$INSTALL" 2>/dev/null); rc=$?
chk "AGENT_FEEDBACK_VERSION pins the release like --version" \
  "$([ "$rc" = 0 ] && [ "$("$home/.local/bin/agentfeedback")" = "agentfeedback 1.3.0-rc.1 (fixture)" ] && echo 1 || echo 0)"

# ── an existing binary is found, never replaced ───────────────────────────────
home=$(mktemp -d "$WORK/home.XXXXXX")
mkdir -p "$home/.local/bin"
printf '#!/bin/sh\necho mine\n' >"$home/.local/bin/agentfeedback"; chmod 0755 "$home/.local/bin/agentfeedback"
out=$(HOME="$home" bash "$INSTALL" --version v1.3.0-rc.1 2>/dev/null); rc=$?
chk "finds an existing ~/.local/bin/agentfeedback, prints it and leaves it alone" \
  "$([ "$rc" = 0 ] && [ "$out" = "$home/.local/bin/agentfeedback" ] && [ "$("$home/.local/bin/agentfeedback")" = mine ] && echo 1 || echo 0)"
err=$(HOME="$home" bash "$INSTALL" --version v1.3.0-rc.1 2>&1 >/dev/null)
chk "an existing binary says the pinned version was not installed" \
  "$(grep -q 'v1.3.0-rc.1 was NOT installed' <<<"$err" && echo 1 || echo 0)"

home=$(mktemp -d "$WORK/home.XXXXXX")
mkdir -p "$home/.local/bin/agentfeedback"
out=$(HOME="$home" bash "$INSTALL" 2>/dev/null); rc=$?
chk "a directory named ~/.local/bin/agentfeedback is not taken for an installed binary" \
  "$([ "$rc" = 1 ] && [ -z "$out" ] && echo 1 || echo 0)"

mkdir -p "$WORK/onpath"
printf '#!/bin/sh\necho onpath\n' >"$WORK/onpath/agentfeedback"; chmod 0755 "$WORK/onpath/agentfeedback"
home=$(mktemp -d "$WORK/home.XXXXXX")
out=$(HOME="$home" PATH="$WORK/onpath:$PATH" bash "$INSTALL" 2>/dev/null); rc=$?
chk "finds an agentfeedback on PATH, prints it and installs nothing" \
  "$([ "$rc" = 0 ] && [ "$out" = "$WORK/onpath/agentfeedback" ] && [ ! -e "$home/.local/bin" ] && echo 1 || echo 0)"

# ── verification failures install nothing ─────────────────────────────────────
run --version v1.2.4
chk "a checksum mismatch fails with exit 1 and installs nothing" \
  "$([ "$rc" = 1 ] && [ -z "$out" ] && grep -q 'checksum mismatch' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"

run --version v1.2.6
chk "an archive without the binary fails with exit 1 and installs nothing" \
  "$([ "$rc" = 1 ] && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"

run --version v1.2.7
chk "a binary that does not run fails with exit 1, installs nothing and leaves no staging directory" \
  "$([ "$rc" = 1 ] && grep -q 'does not run' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"

run --version v1.2.8
chk "SHA256SUMS listing the host archive twice fails with exit 1" \
  "$([ "$rc" = 1 ] && grep -q 'several archives' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"

run --version v1.2.5
chk "SHA256SUMS naming another version fails with exit 1" \
  "$([ "$rc" = 1 ] && grep -q 'not version 1.2.5' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"

run --version v9.9.9
chk "an unpublished release fails with exit 1 and names the version" \
  "$([ "$rc" = 1 ] && grep -q 'v9.9.9' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"

latest v1.3.0-rc.1
run
chk "latest listing a pre-release is refused (only a named pre-release installs)" \
  "$([ "$rc" = 1 ] && grep -q 'not a stable version' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"
rm -rf "$REL/latest"
run
chk "no latest release with SHA256SUMS fails with exit 1 and suggests --version" \
  "$([ "$rc" = 1 ] && grep -q -- '--version' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"
latest v1.2.3

# ── non-tagged sources are refused before any download ────────────────────────
for bad in main 1.2.3 v1.2 v1.2.3-beta.1 v1.2.3-rc v01.2.3 v1.2.3-rc.01 0123abcd 'v1.2.3/../../x' ''; do
  run --version "$bad"
  chk "--version '$bad' is refused with exit 2" \
    "$([ "$rc" = 2 ] && grep -q 'release tag' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"
done

for src in "https://example.invalid/releases" "http://127.0.0.1:1/releases" "$REL"; do
  home=$(mktemp -d "$WORK/home.XXXXXX")
  out=$(HOME="$home" AGENT_FEEDBACK_RELEASE_URL="$src" bash "$INSTALL" 2>"$WORK/err"); rc=$?
  chk "AGENT_FEEDBACK_RELEASE_URL='$src' is refused with exit 2 (file:// only)" \
    "$([ "$rc" = 2 ] && grep -q 'file://' "$WORK/err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"
done

run --version=
chk "an empty --version= is refused with exit 2, not read as unpinned" \
  "$([ "$rc" = 2 ] && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"

run --help
chk "--help prints the usage and exits 0 without installing" \
  "$([ "$rc" = 0 ] && grep -q 'usage: install.sh' <<<"$err" && [ "$(nothing_installed)" = 1 ] && echo 1 || echo 0)"

run --bogus
chk "an unknown argument is refused with exit 2" "$([ "$rc" = 2 ] && echo 1 || echo 0)"

echo
echo "install tests: $pass passed, $fail failed"
[ "$fail" = 0 ]
