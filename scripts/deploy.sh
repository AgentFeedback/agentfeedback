#!/usr/bin/env bash
# Deploy a published image to a host over SSH with docker compose.
#
# Usage: scripts/deploy.sh <commit-sha-or-tag>
# Required: DEPLOY_REMOTE   SSH destination (user@host)
# Optional: DEPLOY_IMAGE    image repository, default ghcr.io/agentfeedback/agentfeedback
#           DEPLOY_DIR      remote directory, default ~/agentfeedback
# All three may live in the gitignored .private/deploy.env.
#
# Steps: install the compose file, create/preserve the remote .env (API_KEY is
# generated once and never printed), pin AGENTFEEDBACK_IMAGE to the requested tag in
# that .env, `docker compose pull && up -d --wait`, then check /ready.
# The host must be able to pull the image (public package or logged in).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ -f "$ROOT/.private/deploy.env" ]; then
  # shellcheck source=/dev/null
  source "$ROOT/.private/deploy.env"
fi
: "${DEPLOY_REMOTE:?Set DEPLOY_REMOTE to the SSH destination (user@host)}"
DEPLOY_IMAGE="${DEPLOY_IMAGE:-ghcr.io/agentfeedback/agentfeedback}"
DEPLOY_DIR="${DEPLOY_DIR:-agentfeedback}"
TAG="${1:?usage: scripts/deploy.sh <commit-sha-or-tag>}"

[[ "$DEPLOY_IMAGE" =~ ^[a-z0-9.-]+(/[a-z0-9._-]+)+$ ]] || { echo "invalid DEPLOY_IMAGE: $DEPLOY_IMAGE" >&2; exit 1; }
[[ "$TAG" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]] || { echo "invalid tag: $TAG" >&2; exit 1; }
[ "$TAG" != latest ] || echo "warning: deploying 'latest' — prefer a commit SHA; latest can move backwards" >&2
IMAGE="$DEPLOY_IMAGE:$TAG"

echo "==> Installing compose file to $DEPLOY_REMOTE:$DEPLOY_DIR"
ssh -- "$DEPLOY_REMOTE" "mkdir -p '$DEPLOY_DIR'"
scp -q -- "$ROOT/infra/agentfeedback/docker-compose.deploy.yml" "$DEPLOY_REMOTE:$DEPLOY_DIR/docker-compose.yml"

echo "==> Ensuring .env (API_KEY generated on first deploy, preserved afterwards; image pinned to $IMAGE)"
ssh -- "$DEPLOY_REMOTE" DEPLOY_DIR="$DEPLOY_DIR" IMAGE="$IMAGE" bash -s <<'REMOTE'
set -euo pipefail
cd "$DEPLOY_DIR"
umask 077
if [ ! -f .env ]; then
  printf 'API_KEY=%s\n' "$(openssl rand -hex 32)" > .env
  echo "    generated .env"
fi
grep -q '^API_KEY=.\+' .env || { echo "    .env has no API_KEY; refusing" >&2; exit 1; }
grep -v '^AGENTFEEDBACK_IMAGE=' .env > .env.next || true
printf 'AGENTFEEDBACK_IMAGE=%s\n' "$IMAGE" >> .env.next
mv .env.next .env
docker compose pull --quiet
docker compose up -d --wait --remove-orphans
REMOTE

echo "==> Readiness"
ssh -- "$DEPLOY_REMOTE" 'curl --fail --silent --show-error --retry 60 --retry-all-errors --retry-delay 1 --max-time 5 http://127.0.0.1:8090/ready' \
  && echo && echo "==> Deployed $IMAGE"
