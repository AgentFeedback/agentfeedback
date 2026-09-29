#!/usr/bin/env bash
# Run the live HTTP contract suite (scripts/e2e.sh) against a fresh server on a
# temporary database. Part of `just ci`; needs bin/agentfeedback from `just build`.
#
# Optional: E2E_ADDR   listen address, default 127.0.0.1:18080
#           E2E_API_KEY key for the throwaway server, default gate-e2e-key
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/agentfeedback"
ADDR="${E2E_ADDR:-127.0.0.1:18080}"
KEY="${E2E_API_KEY:-gate-e2e-key}"
[ -x "$BIN" ] || { echo "gate-e2e: $BIN missing; run just build" >&2; exit 1; }

workdir=$(mktemp -d)
server_pid=
cleanup() {
  if [ -n "$server_pid" ]; then
    kill -TERM "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [ "${1:-0}" != 0 ] && [ -s "$workdir/server.log" ]; then
    echo "gate-e2e: server log:" >&2
    cat "$workdir/server.log" >&2
  fi
  rm -rf "$workdir"
}
trap 'cleanup $?' EXIT

API_KEY="$KEY" DATABASE_PATH="$workdir/agentfeedback.db" HTTP_LISTEN_ADDR="$ADDR" \
  "$BIN" >"$workdir/server.log" 2>&1 &
server_pid=$!

for _ in $(seq 1 30); do
  if curl --fail --silent --show-error "http://$ADDR/ready" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl --fail --silent --show-error "http://$ADDR/ready" >/dev/null

bash "$ROOT/scripts/e2e.sh" "$KEY" "http://$ADDR"
