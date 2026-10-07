#!/usr/bin/env bash
# The live harness check: the real Claude Code CLI (pinned in
# tests/ci/Dockerfile) against the agentfeedback binary built from this tree,
# in a container with no network, a tmpfs HOME and nothing of the repository
# mounted but the binary and tests/live/. tests/live/claude-code.sh holds the
# assertions. Part of `just ci`; also `just live-harness`. Times out after
# AF_LIVE_TIMEOUT seconds (default 600).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd -P)
cd "$root"
image=$(bash scripts/in-container.sh --image)
mkdir -p dist
out=$(mktemp -d "$root/dist/live-harness.XXXXXX")
trap 'rm -rf "$out"' EXIT
bash scripts/in-container.sh env CGO_ENABLED=0 go build -trimpath -o "$out/agentfeedback" ./cmd/agentfeedback

# shellcheck disable=SC2054 # the tmpfs options are one comma-separated argument
timeout "${AF_LIVE_TIMEOUT:-600}" docker run --rm --init --network none -u "$(id -u):$(id -g)" \
  --tmpfs "/tmp:exec,mode=1777" -e HOME=/tmp/home \
  -e PATH=/opt/af:/usr/local/bin:/usr/bin:/bin \
  -v "$out:/opt/af:ro" -v "$root/tests/live:/opt/live:ro" \
  "$image" bash /opt/live/claude-code.sh
