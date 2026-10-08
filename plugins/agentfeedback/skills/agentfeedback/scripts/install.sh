#!/usr/bin/env bash
# Install the agentfeedback binary for this machine.
#
# Usage: install.sh [--version vX.Y.Z | --version vX.Y.Z-rc.N]
#
# A working agentfeedback (its `version` exits 0 within 10 seconds) already on
# PATH or in ~/.local/bin is kept: the script prints its path and exits 0. One
# on PATH that does not run is passed over; a file in ~/.local/bin that does not
# run stops the script, which never overwrites it. Otherwise it downloads the archive for this OS
# and architecture from a tagged GitHub release of AgentFeedback/agentfeedback,
# verifies it against that release's SHA256SUMS, installs the binary to
# ~/.local/bin and prints its path.
#
# Without --version (or AGENT_FEEDBACK_VERSION) it installs the latest stable
# release; a pre-release is installed only when named. Any other version string
# (a branch, a commit, a bare number) is refused.
#
# AGENT_FEEDBACK_RELEASE_URL replaces the release root with a local copy of the
# same layout, file:// only: <root>/download/<tag>/<asset> and
# <root>/latest/download/<asset>. No other source is accepted.
#
# Output: progress and errors on stderr; the binary's path as the last line of
# stdout. Exit codes: 0 installed or already present, 1 failed (nothing was
# installed), 2 refused (bad arguments, version or source).
#
# Requires: bash, curl, tar (unzip on windows), sha256sum or shasum, and the
# standard tools awk, mktemp, cp, mv, chmod.
set -euo pipefail

RELEASES="https://github.com/AgentFeedback/agentfeedback/releases"
# The tags scripts/release.sh accepts: no leading zeros.
NUM='(0|[1-9][0-9]*)'
TAG_RE="^v$NUM\\.$NUM\\.$NUM(-rc\\.$NUM)?\$"
STABLE_RE="^$NUM\\.$NUM\\.$NUM\$"
USAGE="usage: install.sh [--version vX.Y.Z | --version vX.Y.Z-rc.N]"

say() { echo "install.sh: $*" >&2; }
die() { say "$*"; exit 1; }
refuse() { say "$*"; exit 2; }

version="${AGENT_FEEDBACK_VERSION:-}"
pinned=false
if [ -n "${AGENT_FEEDBACK_VERSION:-}" ]; then pinned=true; fi
while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || refuse "--version requires a value"
               version="$2" pinned=true; shift 2 ;;
    --version=*) version="${1#--version=}" pinned=true; shift ;;
    -h|--help) echo "$USAGE" >&2
               echo "installs the agentfeedback release binary to ~/.local/bin; see the comment at the top of this script" >&2
               exit 0 ;;
    *) refuse "unknown argument $1; $USAGE" ;;
  esac
done

if [ "$pinned" = true ] && ! [[ "$version" =~ $TAG_RE ]]; then
  refuse "refusing version '$version': install only from a release tag (vX.Y.Z or vX.Y.Z-rc.N)"
fi

root="$RELEASES"
proto="=https"
if [ -n "${AGENT_FEEDBACK_RELEASE_URL:-}" ]; then
  case "$AGENT_FEEDBACK_RELEASE_URL" in
    file://?*) root="${AGENT_FEEDBACK_RELEASE_URL%/}"; proto="=file" ;;
    *) refuse "refusing AGENT_FEEDBACK_RELEASE_URL '$AGENT_FEEDBACK_RELEASE_URL': only a file:// copy of a release replaces $RELEASES" ;;
  esac
fi

os="" arch=""
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  MINGW*|MSYS*|CYGWIN*) os=windows ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
esac
# A shell translated by Rosetta reports x86_64 on Apple silicon.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
  arch=arm64
fi
bin=agentfeedback ext=tar.gz
if [ "$os" = windows ]; then bin=agentfeedback.exe ext=zip; fi

dest="$HOME/.local/bin"
kept() { # kept <path> [note]: report an existing binary and stop
  if [ -n "$version" ]; then
    say "agentfeedback is already installed at $1; $version was NOT installed: remove $1 first to install it"
  else
    say "agentfeedback is already installed at $1${2:-}; remove it first to install another version"
  fi
  echo "$1"; exit 0
}
# runs <path>: its `version` exits 0 within 10 seconds, run without the API key
# in its environment. Used only to decide whether to keep an existing binary.
runs() {
  ( unset AGENT_FEEDBACK_API_KEY; exec "$1" version ) >/dev/null 2>&1 </dev/null &
  local pid=$! ticks=0
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$ticks" -ge 100 ]; then kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; return 1; fi
    sleep 0.1; ticks=$((ticks + 1))
  done
  wait "$pid"
}
on_path() { case ":$PATH:" in *":$dest:"*) return 0 ;; esac; return 1; }
shadow=""   # an agentfeedback on PATH whose version fails, left in place
if existing=$(command -v agentfeedback 2>/dev/null); then
  if runs "$existing"; then kept "$existing"; fi
  if [ "${existing%.exe}" != "$dest/agentfeedback" ]; then
    shadow="$existing"
    say "$existing is on PATH but '$existing version' fails (a launcher without the binary, or a pre-4 build); installing to $dest"
  fi
fi
if [ -f "$dest/$bin" ]; then
  runs "$dest/$bin" || die "$dest/$bin exists but '$dest/$bin version' fails; move it away and rerun"
  if [ -n "$shadow" ]; then kept "$dest/$bin" " (but $shadow comes first on PATH: remove it)"
  elif on_path; then kept "$dest/$bin"
  else kept "$dest/$bin" " (not on PATH: add $dest to PATH)"; fi
fi
if [ -e "$dest/$bin" ] || [ -L "$dest/$bin" ]; then
  [ -f "$dest/$bin" ] || die "$dest/$bin exists and is not a regular file; move it away and rerun"
fi
[ -n "$os" ] || die "unsupported OS $(uname -s); releases exist for linux, darwin and windows"
[ -n "$arch" ] || die "unsupported architecture $(uname -m); releases exist for amd64 and arm64"

command -v curl >/dev/null 2>&1 || die "curl is required"
if [ "$os" = windows ]; then
  command -v unzip >/dev/null 2>&1 || die "unzip is required"
else
  command -v tar >/dev/null 2>&1 || die "tar is required"
fi
if command -v sha256sum >/dev/null 2>&1; then sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else die "sha256sum or shasum is required"; fi

if [ -n "$version" ]; then base="$root/download/$version"; else base="$root/latest/download"; fi
# -q first: a ~/.curlrc must not change where or how the download happens.
fetch() {
  curl -q -fsSL --proto "$proto" --proto-redir "$proto" --retry 2 \
    --connect-timeout 20 --speed-limit 1024 --speed-time 60 -o "$2" "$1"
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if ! fetch "$base/SHA256SUMS" "$work/SHA256SUMS"; then
  if [ -n "$version" ]; then die "could not download $base/SHA256SUMS; is $version a published release?"; fi
  die "could not download $base/SHA256SUMS; the latest release may predate the binary: name a release with --version"
fi

# sha256sum lines are "<hash>  <name>" or "<hash> *<name>".
sums=$(awk -v os="$os" -v arch="$arch" -v ext="$ext" '{
  name = $2; sub(/^\*/, "", name)
  if (name ~ ("^agentfeedback_[^_]+_" os "_" arch "\\." ext "$")) print $1, name
}' "$work/SHA256SUMS")
[ -n "$sums" ] || die "the release at $base has no archive for ${os}_${arch}"
[ "$(printf '%s\n' "$sums" | wc -l | tr -d ' ')" = 1 ] || die "the release at $base lists several archives for ${os}_${arch}"
want=${sums%% *} asset=${sums#* }
asset_version=${asset#agentfeedback_}; asset_version=${asset_version%%_*}
if [ -n "$version" ]; then
  [ "$asset_version" = "${version#v}" ] || die "release $version lists $asset, not version ${version#v}"
else
  [[ "$asset_version" =~ $STABLE_RE ]] || die "the latest release lists $asset, which is not a stable version"
  version="v$asset_version"
fi

say "downloading $asset ($version)"
fetch "$base/$asset" "$work/$asset" || die "could not download $base/$asset"
got=$(sha256 "$work/$asset")
[ "$got" = "$(printf '%s' "$want" | tr 'A-F' 'a-f')" ] ||
  die "checksum mismatch for $asset: SHA256SUMS says $want, the download is $got; nothing was installed"

mkdir -p "$work/x"
if [ "$ext" = zip ]; then
  unzip -q "$work/$asset" "$bin" -d "$work/x" || die "could not extract $bin from $asset"
else
  tar -xzf "$work/$asset" -C "$work/x" "$bin" || die "could not extract $bin from $asset"
fi
[ -f "$work/x/$bin" ] && [ ! -L "$work/x/$bin" ] || die "$asset does not contain the file $bin"

# Stage in a fresh directory beside the target, so the final rename is atomic,
# the staged file keeps its name (Windows runs only *.exe), and the binary runs
# here rather than in a temp directory that may be mounted noexec.
mkdir -p "$dest"
stagedir=$(mktemp -d "$dest/.agentfeedback-install.XXXXXX")
trap 'rm -rf "$work" "$stagedir"' EXIT
stage="$stagedir/$bin"
cp "$work/x/$bin" "$stage"
chmod 0755 "$stage"
"$stage" version >&2 || die "the downloaded binary does not run on this machine; nothing was installed"
mv -f "$stage" "$dest/$bin"

say "installed $version to $dest/$bin"
if ! on_path; then
  say "$dest is not on PATH; add it, e.g. export PATH=\"$dest:\$PATH\""
elif [ -n "$shadow" ] && [ "$(command -v agentfeedback 2>/dev/null)" = "$shadow" ]; then
  say "$shadow comes before $dest on PATH and still answers to agentfeedback; remove it"
fi
echo "$dest/$bin"
