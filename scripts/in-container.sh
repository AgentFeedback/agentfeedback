#!/usr/bin/env bash
# Runs a command inside the gate toolchain image (tests/ci/Dockerfile), so a
# gate touches nothing on the machine but the repository and Docker.
#
#   bash scripts/in-container.sh <command> [args...]
#   bash scripts/in-container.sh --image    # build the image if needed, print its tag
#
# The repository is mounted read-write at its own path and the command runs in
# the current directory, as the caller's uid:gid, so rewrites (just check, just
# skills) land owned by the caller. The git directories are mounted read-only
# (also a linked worktree's, which live outside it), and the private paths
# below are hidden behind empty mounts. HOME and /tmp are a tmpfs; the Go, npm
# and uv caches live in a per-uid volume (agentfeedback-ci-cache-<uid>). No
# host configuration, credential or socket is mounted and no port is
# published; the network is open for module and tool downloads.
#
# Requires Linux with rootful Docker without user-namespace remapping: the uid
# mapping assumes container uid == host uid. AF_CI_IMAGE_REBUILD=1 rebuilds
# the image from fresh base layers; `docker volume rm
# agentfeedback-ci-cache-$(id -u)` resets the caches.
set -euo pipefail

[ $# -gt 0 ] || { echo "usage: in-container.sh <command> [args...]" >&2; exit 2; }
command -v docker >/dev/null 2>&1 || { echo "in-container.sh: docker is required" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || { echo "in-container.sh: the gate containers need Linux; on $(uname -s) run just ci-host" >&2; exit 1; }
case "$(docker info --format '{{json .SecurityOptions}}')" in
  *rootless*|*userns*) echo "in-container.sh: rootless Docker or userns-remap maps uids differently; use rootful Docker" >&2; exit 1 ;;
esac

here=$(cd "$(dirname "$0")/.." && pwd -P)
ctx="$here/tests/ci"
tag="agentfeedback-ci:$(sha256sum "$ctx/Dockerfile" | cut -c1-12)"
if [ "${AF_CI_IMAGE_REBUILD:-}" = 1 ]; then
  echo "in-container.sh: rebuilding $tag" >&2
  docker build -q --pull --no-cache -t "$tag" "$ctx" >/dev/null
elif ! docker image inspect "$tag" >/dev/null 2>&1; then
  echo "in-container.sh: building $tag" >&2
  docker build -q -t "$tag" "$ctx" >/dev/null
fi
if [ "$1" = --image ]; then
  echo "$tag"
  exit 0
fi

cwd=$(pwd -P)
top=$(git -C "$cwd" rev-parse --show-toplevel)
case "$cwd/" in "$top"/*) ;; *) echo "in-container.sh: run it from inside the repository" >&2; exit 1 ;; esac

# shellcheck disable=SC2054 # the tmpfs options are one comma-separated argument
args=(--rm --init -i -u "$(id -u):$(id -g)"
  -v "$top:$top" -w "$cwd"
  -v "agentfeedback-ci-cache-$(id -u):/cache"
  --tmpfs "/tmp:exec,mode=1777"
  -e HOME=/tmp/home -e AF_CI_CONTAINER=1 -e GIT_OPTIONAL_LOCKS=0
  -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0='*')

for d in "$(git -C "$top" rev-parse --path-format=absolute --git-dir)" \
         "$(git -C "$top" rev-parse --path-format=absolute --git-common-dir)"; do
  args+=(-v "$d:$d:ro")
done

# Private and local paths no gate reads: hidden, so code a gate runs with the
# network open cannot read them.
for p in .private .kitchen .claude; do
  [ -d "$top/$p" ] && args+=(--tmpfs "$top/$p:mode=0700,uid=$(id -u),gid=$(id -g)")
done
for f in "$top"/.env "$top"/.env.* "$top"/infra/agentfeedback/.env; do
  [ -f "$f" ] && [ "${f##*/}" != .env.example ] && args+=(-v "/dev/null:$f:ro")
done

[ -t 0 ] && [ -t 1 ] && args+=(-t)

exec docker run "${args[@]}" "$tag" bash -c 'mkdir -p "$HOME" && exec "$@"' bash "$@"
