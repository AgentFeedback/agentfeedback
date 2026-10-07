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
mkdir -p "$workdir/home"
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

# Another listener on the address would answer the suite instead of the fresh
# server; fail with one clear line rather than dozens of failed checks.
if (exec 3<>"/dev/tcp/${ADDR%:*}/${ADDR##*:}") 2>/dev/null; then
  echo "gate-e2e: $ADDR is already in use; stop whatever listens there or set E2E_ADDR" >&2
  exit 1
fi

API_KEY="$KEY" DATABASE_PATH="$workdir/agentfeedback.db" HTTP_LISTEN_ADDR="$ADDR" \
  "$BIN" serve >"$workdir/server.log" 2>&1 &
server_pid=$!

for _ in $(seq 1 30); do
  if curl --fail --silent --show-error "http://$ADDR/ready" >/dev/null 2>&1; then
    break
  fi
  kill -0 "$server_pid" 2>/dev/null || break
  sleep 1
done
kill -0 "$server_pid" 2>/dev/null || { echo "gate-e2e: the server exited before it was ready" >&2; exit 1; }
curl --fail --silent --show-error "http://$ADDR/ready" >/dev/null

bash "$ROOT/scripts/e2e.sh" "$KEY" "http://$ADDR"

# The server-host commands on the database the suite just filled: backup while
# serve runs, then restore of the full export into a new database (dry run,
# real run, re-run as a no-op).
fail() { echo "gate-e2e: $*" >&2; exit 1; }
DATABASE_PATH="$workdir/agentfeedback.db" "$BIN" backup "$workdir/backup.db" >/dev/null ||
  fail "backup while serve runs failed"
[ -s "$workdir/backup.db" ] || fail "backup wrote no file"

curl --fail --silent --show-error -H "Authorization: Bearer $KEY" "http://$ADDR/api/v1/export" >"$workdir/export.ndjson"
count=$(tail -n 1 "$workdir/export.ndjson" | jq -e '.count') || fail "export has no trailer"
restore() { DATABASE_PATH="$workdir/restored.db" "$BIN" import "$@" "$workdir/export.ndjson"; }
check() { # check <label> <jq filter over the import result>
  local label=$1 out
  shift
  out=$(restore "${@:2}") || fail "import $label failed"
  jq -e --argjson n "$count" "$1" >/dev/null <<<"$out" || fail "import $label: unexpected result $out"
  echo "PASS  import $label"
}
# shellcheck disable=SC2016 # $n is a jq variable
check "--dry-run writes nothing" '.dry_run and .imported == $n and .skipped == 0' --dry-run
# shellcheck disable=SC2016
check "restores every record" '(.dry_run | not) and .imported == $n and .conflicts == []'
# shellcheck disable=SC2016
check "re-run is a no-op" '.imported == 0 and .skipped == $n and .conflicts == []'

# The client binary files one friction against the live server from an empty
# environment: its own HOME, config, cache and data directories (where a
# failed submission would spool), and none of the caller's variables.
out=$(env -i PATH="$PATH" HOME="$workdir/home" \
  XDG_CONFIG_HOME="$workdir/xdg-config" XDG_CACHE_HOME="$workdir/xdg-cache" \
  XDG_DATA_HOME="$workdir/xdg-data" \
  AGENT_FEEDBACK_URL="http://$ADDR" AGENT_FEEDBACK_API_KEY="$KEY" \
  "$BIN" submit friction --summary x) || fail "submit friction failed: $out"
tail -n 1 <<<"$out" | jq -e '.outcome == "submitted" and (.id | type) == "number"' >/dev/null ||
  fail "submit friction: unexpected outcome $out"
echo "PASS  submit friction is submitted with an id"
