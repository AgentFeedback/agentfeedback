#!/usr/bin/env bash
# Process AgentFeedback submissions from any machine: list what's unprocessed,
# mark submissions processed (with what was done) after acting on them, undo
# mistakes.
#
# Usage:
#   process.sh list [--family F] [--type T] [--machine M] [--include-processed]
#                   [--limit N] [--json]
#   process.sh done <id> [<id>...] [--resolution "what was done"]
#   process.sh undo <id> [<id>...]     # clear the processed mark
#
# `list` shows ONLY unprocessed submissions (processed=false) unless
# --include-processed is given, and fetches EVERY page: it follows the server's
# next_before_id cursor until has_more is false, so the output is the whole
# queue, not one page of it. --limit N caps the total number of rows returned.
# The server's total for the filter is printed to stderr as "total: N".
# Default output is one TSV line per submission:
#   id  family  type  machine  category-or-run_id  summary
# --json prints one merged object: {"submissions":[...all pages...],"total":N}
#
# `done`/`undo` print the server's classification verbatim as the outcome:
#   {"processed":true,"resolution":"...","updated":[43],"unchanged":[],"not_found":[999]}
# One --resolution applies to the whole batch; run one command per distinct
# resolution. Exit 1 when the server rejects the request or is unreachable.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_common.sh
. "$SCRIPT_DIR/_common.sh"

AF_PAGE_SIZE=500

# Local usage errors are a rejection, not a transport/HTTP error, and still end
# the command with exactly one machine-readable outcome line.
usage() {
  echo "usage: process.sh list [--family F] [--type T] [--machine M] [--include-processed] [--limit N] [--json]" >&2
  echo "       process.sh done <id> [<id>...] [--resolution \"what was done\"]" >&2
  echo "       process.sh undo <id> [<id>...]" >&2
  af_reject "${1:-invalid arguments (see usage above)}"
}

af_require_deps
af_require_key

set_processed() { # <true|false> <id...> [--resolution TEXT]
  local processed="$1"; shift
  local resolution="" have_resolution=0
  local -a ids=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --resolution)
        [ $# -ge 2 ] || af_reject "$1 requires a value"
        resolution="$2"; have_resolution=1; shift 2 ;;
      -*) usage "unknown flag: $1" ;;
      *) ids+=("$1"); shift ;;
    esac
  done
  [ "${#ids[@]}" -gt 0 ] || usage "at least one submission id is required"
  if [ "$have_resolution" = 1 ]; then
    [ "$processed" = true ] || af_reject "--resolution is only valid with 'done' (the server rejects it when unmarking)"
    resolution=$(af_trim "$resolution")
    [ -n "$resolution" ] || af_reject "--resolution must not be blank"
    af_check_summary "resolution" "$resolution"
  fi

  local id ids_json="[]"
  for id in "${ids[@]}"; do
    [[ "$id" =~ ^[0-9]+$ ]] || af_reject "not a numeric submission id: $id"
    ids_json=$(jq -cn --argjson acc "$ids_json" --argjson id "$id" '$acc + [$id]')
  done

  # AF_WORK_PAYLOAD is script-global on purpose: an EXIT trap cannot see a
  # function's locals (they are out of scope by the time it fires), so a
  # function-local path would leak the temp file into $TMPDIR.
  AF_WORK_PAYLOAD=$(mktemp)
  local payload="$AF_WORK_PAYLOAD"
  trap 'rm -rf "${AF_WORK_DIR:-}"; rm -f "${AF_WORK_PAYLOAD:-}" "${AF_RESP:-}"' EXIT
  jq -cn --argjson ids "$ids_json" --argjson p "$processed" \
    --arg resolution "$resolution" --argjson have "$have_resolution" \
    '{ids: $ids, processed: $p}
     + (if $have == 1 then {resolution: $resolution} else {} end)' >"$payload"

  af_flush_spool
  af_request POST "/api/v1/submissions/processed" "$payload"
  if [ "$AF_HTTP_CODE" = 200 ]; then
    # A 200 is only an answer when it has the shape the contract promises:
    # echoing an unrecognised body as the outcome would report a mark that may
    # never have happened.
    if ! jq -e '(.processed | type) == "boolean"
                and ((.updated | type) == "array")
                and ((.unchanged | type) == "array")
                and ((.not_found | type) == "array")' "$AF_RESP" >/dev/null 2>&1; then
      af_error "malformed response from /api/v1/submissions/processed: $(head -c 300 "$AF_RESP" | tr -d '\n')"
    fi
    # The classification must answer THIS request: the mark it reports is the
    # one that was asked for, every id it names was requested, and every
    # requested id is classified exactly once. Anything else (a foreign id, a
    # missing one, the opposite mark) means the outcome would describe a
    # different operation than the one performed.
    if ! jq -e --argjson want "$processed" --argjson ids "$ids_json" '
           .processed == $want
           and ([.updated[], .unchanged[], .not_found[]] as $all
                | ($all | length) == ($all | unique | length)
                and ($all | unique) == ($ids | unique))' "$AF_RESP" >/dev/null 2>&1; then
      af_error "/api/v1/submissions/processed answered about different submissions than the ones requested: $(head -c 300 "$AF_RESP" | tr -d '\n')"
    fi
    af_outcome "$(jq -c . "$AF_RESP")"
  elif [ "$AF_HTTP_CODE" = 000 ]; then
    af_error "service unreachable (curl exit $AF_CURL_EXIT) at $AF_URL"
  else
    af_error "HTTP $AF_HTTP_CODE: $(head -c 400 "$AF_RESP" | tr -d '\n')"
  fi
}

list_cmd() {
  local as_json=0 include_processed=0 limit=0
  local -a FILTERS=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --family)  [ $# -ge 2 ] || af_reject "$1 requires a value"
                 FILTERS+=(--data-urlencode "family=$2"); shift 2 ;;
      --type)    [ $# -ge 2 ] || af_reject "$1 requires a value"
                 FILTERS+=(--data-urlencode "type=$2"); shift 2 ;;
      --machine) [ $# -ge 2 ] || af_reject "$1 requires a value"
                 FILTERS+=(--data-urlencode "machine=$2"); shift 2 ;;
      --limit)
        [ $# -ge 2 ] || af_reject "$1 requires a value"
        limit="$2"
        [[ "$limit" =~ ^[0-9]+$ ]] || af_reject "--limit must be a non-negative integer"
        shift 2 ;;
      --include-processed) include_processed=1; shift ;;
      --all) af_reject "--all was replaced by --include-processed (list defaults to unprocessed only)" ;;
      --json) as_json=1; shift ;;
      *) usage "unknown flag: $1" ;;
    esac
  done
  [ "$include_processed" = 1 ] || FILTERS+=(--data-urlencode "processed=false")

  local rows total=0 before_id="" page_limit fetched=0 page_rows=0 next
  # Script-global for the EXIT trap (see the note in set_processed).
  AF_WORK_DIR=$(mktemp -d) || af_die "could not create a temporary directory"
  trap 'rm -rf "${AF_WORK_DIR:-}"; rm -f "${AF_WORK_PAYLOAD:-}" "${AF_RESP:-}"' EXIT
  rows="$AF_WORK_DIR/rows.ndjson"
  : >"$rows"

  # Keyset pagination: follow next_before_id until the server says there is
  # nothing older. One page is never the queue.
  while :; do
    page_limit="$AF_PAGE_SIZE"
    if [ "$limit" -gt 0 ]; then
      local remaining=$((limit - fetched))
      [ "$remaining" -gt 0 ] || break
      [ "$remaining" -lt "$page_limit" ] && page_limit="$remaining"
    fi
    local -a PARAMS=(${FILTERS[@]+"${FILTERS[@]}"} --data-urlencode "limit=$page_limit")
    [ -n "$before_id" ] && PARAMS+=(--data-urlencode "before_id=$before_id")
    af_request_get "/api/v1/submissions" ${PARAMS[@]+"${PARAMS[@]}"}
    if [ "$AF_HTTP_CODE" = 000 ]; then
      af_error "service unreachable (curl exit $AF_CURL_EXIT) at $AF_URL"
    elif [ "$AF_HTTP_CODE" != 200 ]; then
      af_error "HTTP $AF_HTTP_CODE: $(head -c 400 "$AF_RESP" | tr -d '\n')"
    fi
    page_rows=$(jq -c '.submissions[]?' "$AF_RESP" | tee -a "$rows" | wc -l | tr -d ' ') \
      || af_error "malformed response body from /api/v1/submissions"
    total=$(jq -r '.total // 0' "$AF_RESP") \
      || af_error "malformed response body from /api/v1/submissions"
    fetched=$(wc -l <"$rows" | tr -d ' ')
    # A cursor that does not move strictly backwards, a missing cursor, or an
    # empty page that still claims more would silently truncate the queue.
    next=$(af_pagination_next "$AF_RESP" "$before_id" "$page_rows") \
      || af_error "malformed pagination response"
    rm -f "$AF_RESP"
    [ -n "$next" ] || break
    before_id="$next"
  done

  if [ "$as_json" = 1 ]; then
    jq -sc --argjson total "$total" '{submissions: ., total: $total}' "$rows"
  else
    jq -r '[
        .id, (.family // "-"), .submission_type, .machine_name,
        (.category // .run_id // "-"),
        ((.summary // "-") | gsub("[\\r\\n\\t]"; " ") | .[0:160])
      ] | @tsv' "$rows"
  fi
  echo "total: $total" >&2
}

AF_WORK_DIR=""
AF_WORK_PAYLOAD=""

case "${1:-}" in
  list) shift; list_cmd "$@" ;;
  done) shift; set_processed true "$@" ;;
  undo) shift; set_processed false "$@" ;;
  *) usage "expected one of: list, done, undo" ;;
esac
