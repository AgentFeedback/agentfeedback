#!/usr/bin/env bash
# Pull every unprocessed friction with its full payload and write a triage
# digest. Prints the digest directory on stdout as its last line.
#
# Usage: digest.sh [--out DIR] [--family friction]
#
# Without --out the digest lands in a freshly created
# $TMPDIR/agentfeedback-triage/<UTC timestamp>-XXXXXX directory, so two runs in the
# same second never merge into one another's output. With --out the given path
# is used as-is, and a non-empty directory is refused for the same reason.
#
# Output directory layout:
#   <id>.json    one full record per row (payload included)
#   index.json   array of every pulled record (same content as the files)
#   digest.md    grouped by project, then category; exact repeats (same
#                payload_hash) flagged; oldest first inside a group
#
# Exit codes: 0 ok (also when the queue is empty), 1 service unreachable or
# rejected the request, 2 a pulled row already had processed_at set (the
# pull is contaminated — re-run before trusting anything).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMMON="$SCRIPT_DIR/_common.sh"
[ -f "$COMMON" ] || { echo "agentfeedback-triage: _common.sh not found at $COMMON" >&2; exit 1; }
# shellcheck source=_common.sh
. "$COMMON"

OUT="" FAMILY="friction"
while [ $# -gt 0 ]; do
  case "$1" in
    # `shift 2` with one argument left fails and would kill the script under
    # `set -e` without a machine-readable outcome.
    --out) [ $# -ge 2 ] || af_reject "$1 requires a value"
           OUT="$2"; shift 2 ;;
    --family) [ $# -ge 2 ] || af_reject "$1 requires a value"
              FAMILY="$2"; shift 2 ;;
    *) echo "agentfeedback-triage: unknown flag $1" >&2
       af_outcome "$(jq -cn --arg m "unknown flag $1" '{status:"rejected",message:$m}')"
       exit 1 ;;
  esac
done

af_require_deps
af_require_key

if [ -n "$OUT" ]; then
  mkdir -p "$OUT" || { echo "agentfeedback-triage: cannot create $OUT" >&2; exit 1; }
  # Mixing a new pull into an old digest silently produces a wrong triage set.
  if [ -n "$(ls -A "$OUT" 2>/dev/null)" ]; then
    echo "agentfeedback-triage: --out directory $OUT is not empty — refusing to mix digests" >&2
    exit 1
  fi
else
  BASE="${TMPDIR:-/tmp}/agentfeedback-triage"
  mkdir -p "$BASE" || { echo "agentfeedback-triage: cannot create $BASE" >&2; exit 1; }
  OUT=$(mktemp -d "$BASE/$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX") \
    || { echo "agentfeedback-triage: cannot create a digest directory under $BASE" >&2; exit 1; }
fi
chmod 700 "$OUT"

PAGE_SIZE=100
before=""
: > "$OUT/.rows.jsonl"
total=""
while :; do
  args=(--data-urlencode "family=$FAMILY" --data-urlencode "processed=false"
        --data-urlencode "include=payload" --data-urlencode "limit=$PAGE_SIZE")
  [ -n "$before" ] && args+=(--data-urlencode "before_id=$before")
  af_request_get "/api/v1/submissions" "${args[@]}"
  if [ "$AF_HTTP_CODE" = 000 ]; then
    echo "agentfeedback-triage: service unreachable (curl exit $AF_CURL_EXIT) at $AF_URL" >&2; rm -f "$AF_RESP"; exit 1
  elif [ "$AF_HTTP_CODE" != 200 ]; then
    echo "agentfeedback-triage: HTTP $AF_HTTP_CODE: $(head -c 300 "$AF_RESP")" >&2; rm -f "$AF_RESP"; exit 1
  fi
  [ -n "$total" ] || total=$(jq -r '.total // 0' "$AF_RESP")
  page_rows=$(jq -c '.submissions[]?' "$AF_RESP" | tee -a "$OUT/.rows.jsonl" | wc -l | tr -d ' ')
  # A cursor that does not move strictly backwards, a missing cursor, or an
  # empty page that still claims more would silently truncate the triage set.
  if ! next=$(af_pagination_next "$AF_RESP" "$before" "$page_rows"); then
    rm -f "$AF_RESP"
    echo "agentfeedback-triage: malformed pagination response" >&2
    af_outcome '{"status":"error","message":"malformed pagination response"}'
    exit 1
  fi
  rm -f "$AF_RESP"
  [ -n "$next" ] || break
  before="$next"
done

count=$(wc -l < "$OUT/.rows.jsonl" | tr -d ' ')
contaminated=$(jq -r 'select(.processed_at != null) | .id' "$OUT/.rows.jsonl" | wc -l | tr -d ' ')

jq -s '.' "$OUT/.rows.jsonl" > "$OUT/index.json"
jq -c '.' "$OUT/.rows.jsonl" | while read -r row; do
  printf '%s\n' "$row" | jq '.' > "$OUT/$(printf '%s' "$row" | jq -r .id).json"
done
rm -f "$OUT/.rows.jsonl"

# digest.md: project → category → rows (oldest first); repeat hashes flagged.
jq -r --arg count "$count" --arg total "$total" --arg family "$FAMILY" '
  def trunc(n): if length > n then .[:n] + "…" else . end;
  def clean: gsub("[\\r\\n\\t]+"; " ");
  ( group_by(.payload_hash) | map(select(length > 1) | {(.[0].payload_hash): length}) | add // {} ) as $repeats
  | "# agentfeedback-triage digest\n",
    "family: \($family) · pulled: \($count) · server total: \($total) · exact-repeat groups: \($repeats | length)\n",
    ( sort_by(.id)
      | group_by(.payload.project // "(no project)")
      | .[]
      | "## project: \(.[0].payload.project // "(no project)") (\(length))\n",
        ( group_by(.payload.category // "(no category)")
          | .[]
          | "### \(.[0].payload.category // "(no category)") (\(length))\n",
            ( .[]
              | "- **#\(.id)** \(.created_at[:10]) `\(.machine_name)` `\(.coordinator_model)`"
                + (if .payload.harness then " `\(.payload.harness)`" else "" end)
                + (if $repeats[.payload_hash] then " ×\($repeats[.payload_hash]) exact repeats" else "" end)
                + "\n  " + ((.payload.summary // "") | clean | trunc(220))
                + (if (.payload.suggested_fix // "") != "" then "\n  fix: " + (.payload.suggested_fix | clean | trunc(220)) else "" end)
            ),
            ""
        )
    )
' "$OUT/index.json" > "$OUT/digest.md"

echo "agentfeedback-triage: pulled $count row(s) (server total $total) into $OUT" >&2
if [ "$contaminated" != 0 ]; then
  echo "agentfeedback-triage: $contaminated pulled row(s) already have processed_at set — pull contaminated" >&2
  printf '%s\n' "$OUT"
  exit 2
fi
printf '%s\n' "$OUT"
