#!/usr/bin/env bash
# Regenerate testdata/repo: a work tree whose git directory is committed as
# dotgit/ (a nested .git cannot be committed); the tests copy it and rename
# dotgit to .git. Identities and dates are fixed, so object ids are stable.
# Objects stay loose (no gc, no pack); refs are split between packed-refs and
# loose files on purpose:
#   first     lightweight, packed, on the first commit (not on HEAD)
#   old       lightweight, packed on the first commit, then moved to HEAD by a
#             loose ref that must override the packed one
#   v1.0.0    annotated, packed with a peeled line
#   v2        annotated, loose (peeled by reading the loose tag object)
#   zz-light  lightweight, loose
# main is packed; refs/remotes/origin/{main,HEAD} are loose.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out="$here/repo"

export GIT_AUTHOR_NAME=Fixture GIT_AUTHOR_EMAIL=fixture@example.com
export GIT_COMMITTER_NAME=Fixture GIT_COMMITTER_EMAIL=fixture@example.com
export GIT_AUTHOR_DATE='2026-01-01T00:00:00Z' GIT_COMMITTER_DATE='2026-01-01T00:00:00Z'
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 TZ=UTC LC_ALL=C

rm -rf "$out"
mkdir -p "$out/sub"
g() { git -C "$out" -c init.defaultBranch=main -c gc.auto=0 -c commit.gpgSign=false -c tag.gpgSign=false "$@"; }

g init -q --ref-format=files
printf 'fixture\n' > "$out/README.md"
printf 'nested\n' > "$out/sub/file.txt"
g add README.md sub/file.txt
g commit -q -m first
g tag first
g tag old
printf 'fixture, second revision\n' > "$out/README.md"
g commit -q -a -m second
g tag -a -m 'release 1.0.0' v1.0.0
g pack-refs --all --prune
g tag -f old HEAD >/dev/null
g tag -a -m 'release 2' v2
g tag zz-light

g config remote.origin.url 'https://user:tok@example.com/org/fixture.git?x=1#frag'
g config remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*'
g config branch.main.remote origin
g config branch.main.merge refs/heads/main
g update-ref refs/remotes/origin/main HEAD
g symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/main

# Drop everything the tests do not read and that carries local state.
rm -rf "$out/.git/hooks" "$out/.git/logs" "$out/.git/info" "$out/.git/description" \
	"$out/.git/COMMIT_EDITMSG" "$out/.git/ORIG_HEAD"
# Git does not commit empty directories; drop them so a checkout matches.
find "$out/.git" -type d -empty -delete
mv "$out/.git" "$out/dotgit"
