#!/usr/bin/env bash
# Run the client commands in local mode: no url configured, every command
# in-process against the data-directory database of a temporary XDG tree, no
# server process. Part of `just ci`; needs bin/agentfeedback from `just build`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/agentfeedback"
[ -x "$BIN" ] || { echo "gate-local: $BIN missing; run just build" >&2; exit 1; }

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
DATA="$workdir/xdg-data"
mkdir -p "$workdir/xdg-config" "$workdir/xdg-cache" "$DATA"

fail() { echo "gate-local: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

# af runs the binary with none of the caller's AGENT_FEEDBACK_* settings and
# isolated config, cache and data directories.
af() {
  env -u AGENT_FEEDBACK_URL -u AGENT_FEEDBACK_API_KEY -u AGENT_FEEDBACK_MACHINE \
    -u AGENT_FEEDBACK_MODEL -u AGENT_FEEDBACK_HARNESS -u AGENT_FEEDBACK_SESSION_ID \
    -u AGENT_FEEDBACK_REVIEW_DIRS \
    XDG_CONFIG_HOME="$workdir/xdg-config" XDG_CACHE_HOME="$workdir/xdg-cache" \
    XDG_DATA_HOME="$DATA" \
    "$BIN" "$@"
}

out=$(af doctor --json) || fail "doctor --json failed: $out"
# shellcheck disable=SC2016 # $data is a jq variable
jq -e --arg data "$DATA/" '.mode == "local" and (.database.path | startswith($data)) and (.database.path | endswith("/agentfeedback/agentfeedback.db")) and (.database.exists == false)' >/dev/null <<<"$out" ||
  fail "doctor --json: not local mode on the data-directory database: $out"
pass "doctor reports local mode on the data-directory database"

out=$(af submit friction --summary "local gate") || fail "submit friction failed: $out"
tail -n 1 <<<"$out" | jq -e '.outcome == "submitted" and (.id | type) == "number"' >/dev/null ||
  fail "submit friction: unexpected outcome $out"
id=$(tail -n 1 <<<"$out" | jq -r '.id')
pass "submit friction is submitted with id $id"

out=$(af list --json) || fail "list --json failed: $out"
jq -e --argjson id "$id" '(.submissions | length) == 1 and .submissions[0].id == $id' >/dev/null <<<"$out" ||
  fail "list --json: expected only submission $id: $out"
pass "list shows the submission"

out=$(af get "$id" --json) || fail "get $id --json failed: $out"
jq -e --argjson id "$id" '.id == $id' >/dev/null <<<"$out" ||
  fail "get $id --json: unexpected body $out"
pass "get returns the submission"

out=$(af "done" "$id" --verdict gate) || fail "done $id failed: $out"
out=$(af list --json --processed) || fail "list --json --processed failed: $out"
jq -e --argjson id "$id" '[.submissions[].id] == [$id]' >/dev/null <<<"$out" ||
  fail "list --processed after done: expected submission $id: $out"
out=$(af list --json --open) || fail "list --json --open failed: $out"
jq -e '(.submissions | length) == 0' >/dev/null <<<"$out" ||
  fail "list --open after done: expected none: $out"
pass "done marks the submission processed"

out=$(af undo "$id") || fail "undo $id failed: $out"
out=$(af list --json --open) || fail "list --json --open failed: $out"
jq -e --argjson id "$id" '[.submissions[].id] == [$id]' >/dev/null <<<"$out" ||
  fail "list --open after undo: expected submission $id: $out"
pass "undo reopens the submission"

out=$(af stats --json) || fail "stats --json failed: $out"
jq -e '.total == 1' >/dev/null <<<"$out" || fail "stats --json: expected total 1: $out"
pass "stats counts one submission"

af export >"$workdir/export.ndjson" || fail "export failed"
tail -n 1 "$workdir/export.ndjson" | jq -e '.export_complete == true and .count == 1' >/dev/null ||
  fail "export: unexpected trailer $(tail -n 1 "$workdir/export.ndjson")"
pass "export ends with a complete trailer for one record"

[ -f "$DATA/agentfeedback/agentfeedback.db" ] || fail "$DATA/agentfeedback/agentfeedback.db does not exist"
pass "the database is the data-directory file"

if pgrep -f "$BIN serve" >/dev/null; then
  fail "a $BIN serve process is running; local mode must not start a server"
fi
pass "no server process ran"
