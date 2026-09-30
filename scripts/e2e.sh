#!/usr/bin/env bash
# jq filters passed through j() hold $vars for jq, not the shell.
# shellcheck disable=SC2016
# Live HTTP contract suite for AgentFeedback, asserting docs/openapi.yaml (API v1).
# Runs against any running service: a compose deployment, a verification
# container, or the fresh server scripts/gate-e2e.sh starts. It creates rows in
# the service's database (submissions, one tombstone, processing marks); every
# run uses unique keys and content, so it is safe to rerun against a persistent
# database.
#
# Every request goes through `req <operationId> ...`; at the end the recorded
# operationIds are compared with docs/openapi.yaml, and any operation neither
# exercised nor listed in PENDING fails the run (UNCOVERED <operationId>).
#
# Usage: scripts/e2e.sh <API_KEY> [BASE_URL]
#   BASE_URL defaults to http://127.0.0.1:8090
# Needs bash, curl, jq, sha256sum. Exits non-zero on any failed check.
set -u
if [ -z "${1:-}" ]; then
  echo "usage: $0 <API_KEY> [BASE_URL]" >&2
  exit 2
fi
KEY="$1"
BASE="${2:-http://127.0.0.1:8090}"
OPENAPI="$(dirname "${BASH_SOURCE[0]}")/../docs/openapi.yaml"
RUN="e2e-$(date +%Y%m%d%H%M%S)-$$-$RANDOM"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
BODY="$TMP/body"
HDRS="$TMP/headers"

# Served by the MCP and discovery package; remove from this list when it lands.
PENDING=(postMcp postMcpProject getSkill getDiscovery)

pass=0; fail=0
COVFILE="$TMP/covered"; : >"$COVFILE"  # req runs in $(...) subshells, so it records to a file
chk() { # chk <desc> <expected_status> <actual_status> [extra_ok]
  local desc=$1 want=$2 got=$3 extra=${4:-1}
  if [ "$want" = "$got" ] && [ "$extra" = 1 ]; then pass=$((pass+1)); echo "PASS  $desc";
  else fail=$((fail+1)); echo "FAIL  $desc (want $want got $got extra_ok=$extra)"; fi
}
ok() { # ok <desc> <1|0>: a check with no status of its own
  chk "$1" 1 1 "$2"
}
req() { # req <operationId> <curl args...>: body -> $BODY, headers -> $HDRS, prints status
  echo "$1" >>"$COVFILE"; shift
  curl -sS -D "$HDRS" -o "$BODY" -w '%{http_code}' "$@"
}
j() { # j [-f file] [jq options...] <filter>: 1 if the filter is true on the body (or file), else 0
  local f=$BODY flt
  if [ "${1:-}" = -f ]; then f=$2; shift 2; fi
  flt=${!#}; set -- "${@:1:$#-1}"
  jq -r "$@" "if ($flt) then 1 else 0 end" "$f" 2>/dev/null || echo 0
}
hdr() { # hdr <name>: value of a response header from the last request
  grep -i "^$1:" "$HDRS" | head -n1 | cut -d: -f2- | tr -d '\r' | sed 's/^ *//'
}
is() { [ "$1" = "$2" ] && echo 1 || echo 0; }
problem() { # problem <code>: Problem JSON shape with the given error code, application/json
  local ct; ct=$(hdr Content-Type)
  if [ "${ct%%;*}" = application/json ] && [ "$(j --arg c "$1" '.error==$c and (.message|type)=="string" and (.request_id|type)=="string"')" = 1 ]; then
    echo 1; else echo 0; fi
}
uuid() { # random RFC 4122 v4 UUID
  local h; h=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')
  printf '%s-%s-4%s-8%s-%s\n' "${h:0:8}" "${h:8:4}" "${h:13:3}" "${h:17:3}" "${h:20:12}"
}
A=(-H "Authorization: Bearer $KEY")
X=(-H "X-Api-Key: $KEY")
JSON=(-H 'Content-Type: application/json')
NDJSON=(-H 'Content-Type: application/x-ndjson')

# --- operations endpoints (public) -----------------------------------------
s=$(req getHealth "$BASE/health")
chk "GET /health -> 200 OK" 200 "$s" "$(is "$(cat "$BODY")" OK)"
s=$(req getReady "$BASE/ready")
chk "GET /ready -> 200 READY" 200 "$s" "$(is "$(cat "$BODY")" READY)"
s=$(req getMetrics "$BASE/metrics")
chk "GET /metrics has http_requests_total" 200 "$s" "$(grep -q http_requests_total "$BODY" && echo 1 || echo 0)"

# --- auth -------------------------------------------------------------------
s=$(req listSubmissions "$BASE/api/v1/submissions")
chk "list without key -> 401 unauthorized" 401 "$s" "$(problem unauthorized)"
ok "401 carries WWW-Authenticate: Bearer and Cache-Control: no-store" \
  "$( [ "$(hdr WWW-Authenticate)" = Bearer ] && [ "$(hdr Cache-Control)" = no-store ] && echo 1 || echo 0)"
s=$(req listSubmissions -H "X-Api-Key: wrong-$RUN" "$BASE/api/v1/submissions")
chk "list wrong X-Api-Key -> 401" 401 "$s" "$(problem unauthorized)"
s=$(req listSubmissions -H "Authorization: Bearer wrong-$RUN" "$BASE/api/v1/submissions")
chk "list wrong bearer -> 401" 401 "$s" "$(problem unauthorized)"
s=$(req getMeta "${A[@]}" "$BASE/api/v1/meta")
chk "Authorization: Bearer accepted" 200 "$s"
s=$(req getMeta -H "Authorization: bearer $KEY" "$BASE/api/v1/meta")
chk "bearer scheme name case-insensitive" 200 "$s"
s=$(req getMeta "${X[@]}" "$BASE/api/v1/meta")
chk "X-Api-Key accepted" 200 "$s"
s=$(req getMeta -H "Authorization: Bearer wrong-$RUN" "${X[@]}" "$BASE/api/v1/meta")
chk "valid X-Api-Key beside a wrong bearer authorises" 200 "$s"
s=$(curl -sS -D "$HDRS" -o "$BODY" -w '%{http_code}' "$BASE/api/v1/no-such-route-$RUN")
chk "unknown /api/v1 path without key -> 401" 401 "$s" "$(problem unauthorized)"

# --- meta, schemas, openapi --------------------------------------------------
s=$(req getMeta "${X[@]}" "$BASE/api/v1/meta")
chk "meta shape" 200 "$s" "$(j '(.service_version|type)=="string" and .api_version=="1.0" and .export_format==2
  and (.dedupe_window_s|type)=="number" and (.limits.list_max|type)=="number" and (.limits.body_bytes|type)=="number"
  and (.kinds|type)=="array" and (.features|type)=="array" and (.features|index("import")) != null
  and (.client|type)=="object"')"
ok "meta Cache-Control: no-store" "$(is "$(hdr Cache-Control)" no-store)"
s=$(req listSchemas "$BASE/api/v1/schemas")
chk "schemas list (no key) includes envelope and friction" 200 "$s" \
  "$(j '([.schemas[].kind]|index("envelope")) != null and ([.schemas[].kind]|index("friction")) != null')"
s=$(req getSchema "$BASE/api/v1/schemas/friction/1")
chk "schema friction/1 (no key) is application/schema+json" 200 "$s" \
  "$( [ "$(hdr Content-Type)" = application/schema+json ] && [ "$(j 'type=="object"')" = 1 ] && echo 1 || echo 0)"
s=$(req getSchema "$BASE/api/v1/schemas/envelope/1")
chk "schema envelope/1" 200 "$s" "$(j 'type=="object"')"
s=$(req getSchema "$BASE/api/v1/schemas/friction/999")
chk "unknown schema version -> 404 not_found" 404 "$s" "$(problem not_found)"
s=$(req getOpenApi "$BASE/api/v1/openapi.json")
chk "openapi.json (no key) is JSON, openapi 3.1.x" 200 "$s" "$(j '.openapi|test("^3\\.1\\.")')"

# --- create -----------------------------------------------------------------
FR=$(jq -nc --arg k "$RUN-fr" --arg r "$RUN" '{kind:"friction",key:$k,summary:("e2e friction " + $r),
  machine:"e2e-test",model:"e2e-model",harness:"e2e",project:"e2e",
  payload:{category:"documentation",details:("details " + $r),fix_status:"none"}}')
s=$(req createSubmission "${A[@]}" "${JSON[@]}" -d "$FR" "$BASE/api/v1/submissions")
id1=$(jq -r '.submission.id' "$BODY")
uid1=$(jq -r '.submission.uid' "$BODY")
chk "create -> 201 with submission and warnings" 201 "$s" \
  "$(j --arg k "$RUN-fr" '.submission.key==$k and .submission.kind=="friction" and (.submission.content_hash|test("^[0-9a-f]{64}$")) and (.warnings|type)=="array"')"
ok "create Location names the record" "$(is "$(hdr Location)" "/api/v1/submissions/$id1")"
ok "create Cache-Control: no-store and X-Request-Id" \
  "$( [ "$(hdr Cache-Control)" = no-store ] && [ -n "$(hdr X-Request-Id)" ] && echo 1 || echo 0)"

s=$(req createSubmission "${X[@]}" "${JSON[@]}" -d "$FR" "$BASE/api/v1/submissions")
chk "keyed replay of identical content -> 200 same record" 200 "$s" "$(j --argjson id "$id1" '.submission.id==$id')"
ok "replay Location names the existing record" "$(is "$(hdr Location)" "/api/v1/submissions/$id1")"

s=$(req createSubmission "${X[@]}" "${JSON[@]}" -d "$(jq -c '.summary="changed"' <<<"$FR")" "$BASE/api/v1/submissions")
chk "key reused with different content -> 409 replay_mismatch key_reused" 409 "$s" \
  "$( [ "$(problem replay_mismatch)" = 1 ] && [ "$(j --argjson id "$id1" '.details[0].code=="key_reused" and .details[0].pointer=="/key" and .details[0].existing_id==$id')" = 1 ] && echo 1 || echo 0)"

KL=$(jq -nc --arg r "$RUN" '{kind:"friction",summary:("keyless " + $r),machine:"e2e-test",model:"e2e-model",project:"e2e",payload:{category:"tooling",details:$r}}')
s=$(req createSubmission "${X[@]}" "${JSON[@]}" -d "$KL" "$BASE/api/v1/submissions")
id2=$(jq -r '.submission.id' "$BODY")
chk "keyless create -> 201" 201 "$s"
s=$(req createSubmission "${X[@]}" "${JSON[@]}" -d "$KL" "$BASE/api/v1/submissions")
chk "keyless duplicate -> 200 existing record" 200 "$s" "$(j --argjson id "$id2" '.submission.id==$id')"

# conformance/warnings.json: moved_to_payload, example pointer /payload/Model
MV=$(jq -nc --arg r "$RUN" '{kind:"friction",summary:("moved " + $r),machine:"e2e-test",model:"e2e-model",Model:"x",payload:{category:"tooling",details:$r}}')
s=$(req createSubmission "${X[@]}" "${JSON[@]}" -d "$MV" "$BASE/api/v1/submissions")
id3=$(jq -r '.submission.id' "$BODY")
chk "unknown top-level member -> 201 with moved_to_payload warning" 201 "$s" \
  "$(j '(.warnings|map(select(.code=="moved_to_payload" and .pointer=="/payload/Model"))|length)==1 and .submission.payload.Model=="x"')"

s=$(req createSubmission "${X[@]}" "${JSON[@]}" -d '[1,2]' "$BASE/api/v1/submissions")
chk "array body -> 400 bad_request" 400 "$s" "$(problem bad_request)"
s=$(req createSubmission "${X[@]}" "${JSON[@]}" -d '{"kind":' "$BASE/api/v1/submissions")
chk "malformed JSON -> 400 bad_request" 400 "$s" "$(problem bad_request)"

# --- read -------------------------------------------------------------------
s=$(req getSubmission "${X[@]}" "$BASE/api/v1/submissions/$id1")
chk "get by id returns the bare record" 200 "$s" "$(j --argjson id "$id1" --arg u "$uid1" '.id==$id and .uid==$u and .payload.category=="documentation"')"
s=$(req getSubmission "${X[@]}" "$BASE/api/v1/submissions/999999999")
chk "get unknown id -> 404 not_found" 404 "$s" "$(problem not_found)"
s=$(req getSubmission "${X[@]}" "$BASE/api/v1/submissions/0abc")
chk "get malformed id -> 400 validation_error" 400 "$s" "$(problem validation_error)"

s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?q=$RUN&limit=500")
chk "list q=<run> finds the three created rows, no payload" 200 "$s" \
  "$(j '.total==3 and (.submissions|length)==3 and all(.submissions[]; has("payload")|not) and .has_more==false and .next_before_id==null and .next_after_id==null')"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?key=$RUN-fr&include=payload")
chk "list key filter + include=payload" 200 "$s" "$(j --argjson id "$id1" '.total==1 and .submissions[0].id==$id and .submissions[0].payload.category=="documentation"')"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?q=$RUN&category=tooling")
chk "list category filter" 200 "$s" "$(j '.total==2 and all(.submissions[]; .kind=="friction")')"

s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?q=$RUN&limit=2")
chk "list newest-first page 1 (limit=2)" 200 "$s" \
  "$(j --argjson a "$id3" --argjson b "$id2" '[.submissions[].id]==[$a,$b] and .limit==2 and .total==3 and .has_more==true and .next_before_id==$b and .next_after_id==null')"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?q=$RUN&limit=2&before_id=$id2")
chk "list before_id page 2" 200 "$s" "$(j --argjson a "$id1" '[.submissions[].id]==[$a] and .has_more==false')"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?q=$RUN&limit=2&after_id=0")
chk "list after_id oldest-first page" 200 "$s" \
  "$(j --argjson a "$id1" --argjson b "$id2" '[.submissions[].id]==[$a,$b] and .has_more==true and .next_after_id==$b and .next_before_id==null')"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?limit=501")
chk "list limit=501 -> 400 validation_error naming ?limit" 400 "$s" \
  "$( [ "$(problem validation_error)" = 1 ] && [ "$(j 'any(.details[]?; .pointer=="?limit")')" = 1 ] && echo 1 || echo 0)"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?bogus=1")
chk "list unknown parameter -> 400 validation_error" 400 "$s" "$(problem validation_error)"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?before_id=5&after_id=1")
chk "list before_id with after_id -> 400" 400 "$s" "$(problem validation_error)"

# --- processing marks -------------------------------------------------------
s=$(req markSubmission -X PATCH "${X[@]}" "${JSON[@]}" -d '{"processed":true,"verdict":"fixed","resolution":"e2e","processed_by":"e2e"}' "$BASE/api/v1/submissions/$id2")
pa=$(jq -r '.processed_at // empty' "$BODY")
chk "mark one -> 200 with processed_at and verdict" 200 "$s" "$(j '.verdict=="fixed" and .resolution=="e2e" and (.processed_at|type)=="string"')"
s=$(req markSubmission -X PATCH "${X[@]}" "${JSON[@]}" -d '{"verdict":"duplicate"}' "$BASE/api/v1/submissions/$id2")
chk "re-mark replaces verdict, keeps processed_at" 200 "$s" "$(j --arg p "$pa" '.verdict=="duplicate" and .processed_at==$p')"
s=$(req markSubmission -X PATCH "${X[@]}" "${JSON[@]}" -d '{"processed":false}' "$BASE/api/v1/submissions/$id2")
chk "processed=false clears the mark (undo)" 200 "$s" \
  "$(j '[has("processed_at","verdict","resolution","ref","processed_by")]|any|not')"
s=$(req markSubmission -X PATCH "${X[@]}" "${JSON[@]}" -d '{"processed":"yes"}' "$BASE/api/v1/submissions/$id2")
chk "mark with a string processed -> 400 validation_error" 400 "$s" "$(problem validation_error)"
s=$(req markSubmission -X PATCH "${X[@]}" "${JSON[@]}" -d '{"processed":false,"verdict":"fixed"}' "$BASE/api/v1/submissions/$id2")
chk "verdict with processed=false -> 400" 400 "$s" "$(problem validation_error)"
s=$(req markSubmission -X PATCH "${X[@]}" "${JSON[@]}" -d '{"summary":"x"}' "$BASE/api/v1/submissions/$id2")
chk "content field in a mark -> 400" 400 "$s" "$(problem validation_error)"
s=$(req markSubmission -X PATCH "${X[@]}" "${JSON[@]}" -d '{"processed":true}' "$BASE/api/v1/submissions/999999999")
chk "mark unknown id -> 404" 404 "$s" "$(problem not_found)"

s=$(req markSubmissions "${X[@]}" "${JSON[@]}" -d "{\"ids\":[$id1,$id2,$id2,999999999],\"processed\":true,\"verdict\":\"fixed\"}" "$BASE/api/v1/submissions/processed")
chk "batch mark classifies ids, duplicates collapsed" 200 "$s" \
  "$(j --argjson a "$id1" --argjson b "$id2" '.processed==true and .verdict=="fixed" and (.updated|sort)==([$a,$b]|sort) and .unchanged==[] and .not_found==[999999999]')"
s=$(req markSubmissions "${X[@]}" "${JSON[@]}" -d "{\"ids\":[$id1],\"processed\":true,\"verdict\":\"fixed\"}" "$BASE/api/v1/submissions/processed")
chk "batch re-mark with same fields -> unchanged" 200 "$s" "$(j --argjson a "$id1" '.updated==[] and .unchanged==[$a]')"
s=$(req markSubmissions "${X[@]}" "${JSON[@]}" -d "{\"ids\":[$id1,$id2],\"processed\":false}" "$BASE/api/v1/submissions/processed")
chk "batch undo (processed=false)" 200 "$s" "$(j '.processed==false and (.updated|length)==2')"
s=$(req markSubmissions "${X[@]}" "${JSON[@]}" -d '{"ids":[],"processed":true}' "$BASE/api/v1/submissions/processed")
chk "batch with empty ids -> 400" 400 "$s" "$(problem validation_error)"

# --- stats ------------------------------------------------------------------
s=$(req getStats "${X[@]}" "$BASE/api/v1/stats?q=$RUN&by=kind,category&bucket=day&top=5")
chk "stats shape over the run's rows" 200 "$s" \
  "$(j '.total==3 and .open==3 and .processed==0 and (.redacted|type)=="number" and (.groups|type)=="array"
    and all(.groups[]; (.keys|type)=="object" and (.total|type)=="number" and (.open|type)=="number" and (.processed|type)=="number")
    and (.recurring|type)=="array" and (.series|type)=="array" and (.series|map(.total)|add)==3')"
s=$(req getStats "${X[@]}" "$BASE/api/v1/stats?by=nope")
chk "stats by=nope -> 400" 400 "$s" "$(problem validation_error)"

# --- redact -----------------------------------------------------------------
s=$(req redactSubmission -X DELETE "${X[@]}" "$BASE/api/v1/submissions/$id1")
chk "redact -> 200 tombstone" 200 "$s" \
  "$(j --arg u "$uid1" --arg k "$RUN-fr" '.payload=={"redacted":true} and (has("summary")|not) and (has("context")|not) and (.redacted_at|type)=="string" and .uid==$u and .key==$k and .machine=="e2e-test"')"
ra=$(jq -r '.redacted_at' "$BODY")
s=$(req redactSubmission -X DELETE "${X[@]}" "$BASE/api/v1/submissions/$id1")
chk "redact again -> 200 unchanged" 200 "$s" "$(j --arg r "$ra" '.redacted_at==$r')"
s=$(req createSubmission "${X[@]}" "${JSON[@]}" -d "$FR" "$BASE/api/v1/submissions")
chk "keyed replay of redacted content -> 200 tombstone" 200 "$s" "$(j '.submission.payload=={"redacted":true}')"
s=$(req redactSubmission -X DELETE "${X[@]}" "$BASE/api/v1/submissions/999999999")
chk "redact unknown id -> 404" 404 "$s" "$(problem not_found)"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?q=$RUN&redacted=true")
chk "q does not match a tombstone (summary removed)" 200 "$s" "$(j '.total==0')"
s=$(req listSubmissions "${X[@]}" "$BASE/api/v1/submissions?key=$RUN-fr&redacted=true")
chk "list key + redacted=true finds the tombstone" 200 "$s" "$(j --argjson id "$id1" '.total==1 and .submissions[0].id==$id')"

# --- export -----------------------------------------------------------------
after=$((id1 - 1))
EXP="$TMP/export.ndjson"
s=$(req exportSubmissions "${X[@]}" "$BASE/api/v1/export?after_id=$after")
cp "$BODY" "$EXP"
lines=$(wc -l <"$EXP")
nrec=$((lines - 2))
sed -n "2,$((lines - 1))p" "$EXP" >"$TMP/records"
digest=$(sha256sum "$TMP/records" | cut -d' ' -f1)
ok_ct=$(is "$(hdr Content-Type)" application/x-ndjson)
head -n1 "$EXP" >"$TMP/h"; tail -n1 "$EXP" >"$TMP/t"
chk "export framing: header, records, trailer" 200 "$s" \
  "$( [ "$ok_ct" = 1 ] && [ "$(j -f "$TMP/h" --argjson a "$after" '.export_format==2 and .after_id==$a and has("kind") and has("since") and (.exported_at|type)=="string"')" = 1 ] \
   && [ "$(j -f "$TMP/t" '.export_complete==true')" = 1 ] && echo 1 || echo 0)"
ok "export trailer count equals record lines ($nrec)" "$(j -f "$TMP/t" --argjson n "$nrec" '.count==$n')"
ok "export trailer sha256 equals sha256sum of record lines" "$(j -f "$TMP/t" --arg d "$digest" '.sha256==$d')"
ok "export records ascend by id, first_id/last_id match" \
  "$(jq -s --slurpfile t "$TMP/t" 'if ([.[].id]==([.[].id]|sort)) and .[0].id==$t[0].first_id and .[-1].id==$t[0].last_id then 1 else 0 end' "$TMP/records" 2>/dev/null || echo 0)"
ok "export carries the tombstone as a tombstone" \
  "$(jq -s --argjson id "$id1" 'if any(.[]; .id==$id and .payload=={"redacted":true} and (.redacted_at|type)=="string") then 1 else 0 end' "$TMP/records" 2>/dev/null || echo 0)"

s=$(req exportSubmissions "${X[@]}" "$BASE/api/v1/export?after_id=$after&limit=1")
chk "export limit=1" 200 "$s" "$(tail -n1 "$BODY" | jq -r --argjson id "$id1" 'if .count==1 and .first_id==$id then 1 else 0 end' 2>/dev/null || echo 0)"
s=$(req exportSubmissions "${X[@]}" "$BASE/api/v1/export?after_id=$after&kind=review")
chk "export kind=review over the run has no rows" 200 "$s" \
  "$( [ "$(head -n1 "$BODY" | jq -r 'if .kind=="review" then 1 else 0 end')" = 1 ] && [ "$(tail -n1 "$BODY" | jq -r 'if .count==0 then 1 else 0 end')" = 1 ] && echo 1 || echo 0)"
s=$(req exportSubmissions "${X[@]}" "$BASE/api/v1/export?limit=0")
chk "export limit=0 -> 400" 400 "$s" "$(problem validation_error)"
# --head makes curl write the headers to -o as well; the body size comes from -w.
s=$(req exportSubmissionsHead --head -w '%{http_code} %{size_download}' "${X[@]}" "$BASE/api/v1/export?after_id=$after")
chk "HEAD export -> 200 headers, no body" "200 0" "$s" \
  "$( [ "$(hdr Content-Type)" = application/x-ndjson ] && [ "$(hdr Cache-Control)" = no-store ] && echo 1 || echo 0)"

# --- import -----------------------------------------------------------------
s=$(req importSubmissions "${X[@]}" "${NDJSON[@]}" --data-binary "@$EXP" "$BASE/api/v1/import")
chk "import round trip of the export -> all skipped" 200 "$s" \
  "$(j --argjson n "$nrec" '.imported==0 and .skipped==$n and .conflicts==[]')"

# A tombstone keeps its exported hash on import, so the run's tombstone under a
# fresh uid and a different hash is a (kind, key) conflict.
fuid=$(uuid)
jq -c --argjson id "$id1" --arg u "$fuid" 'select(.id==$id) | .uid=$u | .content_hash=("0"*64)' "$TMP/records" >"$TMP/conf"
cdig=$(sha256sum "$TMP/conf" | cut -d' ' -f1)
{
  jq -c '.' "$TMP/h"
  cat "$TMP/conf"
  jq -nc --arg d "$cdig" --argjson id "$id1" '{export_complete:true,count:1,first_id:$id,last_id:$id,sha256:$d}'
} >"$TMP/conflict.ndjson"
s=$(req importSubmissions "${X[@]}" "${NDJSON[@]}" --data-binary "@$TMP/conflict.ndjson" "$BASE/api/v1/import")
chk "import key_mismatch listed as a conflict, request succeeds" 200 "$s" \
  "$(j --arg u "$fuid" --argjson id "$id1" '.imported==0 and .skipped==1 and .conflicts==[{line:2,uid:$u,existing_id:$id,reason:"key_mismatch"}]')"

sed '$s/"sha256":"[0-9a-f]/"sha256":"x/' "$EXP" >"$TMP/bad.ndjson"
s=$(req importSubmissions "${X[@]}" "${NDJSON[@]}" --data-binary "@$TMP/bad.ndjson" "$BASE/api/v1/import")
chk "import with corrupted digest -> 400 validation_error naming the line" 400 "$s" \
  "$( [ "$(problem validation_error)" = 1 ] && [ "$(j --arg l "line $lines" '.message|contains($l)')" = 1 ] && echo 1 || echo 0)"

# --- coverage of docs/openapi.yaml --------------------------------------------
echo
declare -A PEND=() COVERED=()
while read -r op; do COVERED[$op]=1; done <"$COVFILE"
for op in "${PENDING[@]}"; do PEND[$op]=1; done
ops=$(sed -n 's/^[[:space:]]*operationId:[[:space:]]*\([A-Za-z0-9_]*\).*/\1/p' "$OPENAPI")
if [ -z "$ops" ]; then
  fail=$((fail+1)); echo "FAIL  no operationIds read from $OPENAPI"
fi
for op in $ops; do
  if [ -n "${COVERED[$op]:-}" ] && [ -n "${PEND[$op]:-}" ]; then
    fail=$((fail+1)); echo "STALE $op is exercised but still listed in PENDING"
  elif [ -z "${COVERED[$op]:-}" ] && [ -z "${PEND[$op]:-}" ]; then
    fail=$((fail+1)); echo "UNCOVERED $op"
  fi
done
for op in "${!COVERED[@]}"; do
  grep -qx "$op" <<<"$ops" || { fail=$((fail+1)); echo "UNKNOWN $op is recorded but not in $OPENAPI"; }
done
echo "coverage: $(wc -w <<<"$ops") operations, ${#COVERED[@]} exercised, ${#PENDING[@]} pending"

echo; echo "e2e: $pass passed, $fail failed"
[ "$fail" = 0 ]
