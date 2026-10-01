#!/usr/bin/env bash
# Hermetic test suite for the agentfeedback-triage skill's scripts
# (digest.sh, process.sh, _common.sh, cluster.py). No running stack
# needed: a Python mock server plays the service, HOME is a temp dir so the
# spool never touches the real cache.
#
# Lives OUTSIDE skills/agentfeedback-triage/ on purpose: the skill directory is
# copied as-is into a harness, and test tooling must never travel with it.
#
# Portable to macOS and Linux: no GNU-only flags (no `touch -d`), and every
# path comparison uses the canonical (symlink-resolved) form, because macOS
# temp dirs are /var/... symlinks onto /private/var/....
#
# Usage: bash tests/skill/triage-tests.sh
# Requires: bash, curl, jq, python3.
set -u

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TRIAGE_SCRIPTS="$TESTS_DIR/../../skills/agentfeedback-triage/scripts"

WORK=$(mktemp -d)
WORK=$(cd "$WORK" && pwd -P)   # canonical: /private/var/... on macOS
STATE="$WORK/state"
mkdir -p "$STATE"
export HOME="$WORK/home"
mkdir -p "$HOME"
SPOOL="$HOME/.cache/agentfeedback/spool"

python3 "$TESTS_DIR/mock_server.py" "$STATE" &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null; chmod -R u+rwX "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

for _ in $(seq 1 50); do [ -s "$STATE/port" ] && break; sleep 0.1; done
[ -s "$STATE/port" ] || { echo "FATAL mock server did not start" >&2; exit 1; }
PORT=$(cat "$STATE/port")

export AGENT_FEEDBACK_URL="http://127.0.0.1:$PORT"
export AGENT_FEEDBACK_API_KEY="testkey"
export AGENT_FEEDBACK_MACHINE="testmach"
# Scrub harness identity the suite may have inherited (running under Claude
# Code or any other harness leaks these); individual tests set them explicitly.
unset REVIEW_CALLER_MODEL AI_AGENT CLAUDE_EFFORT CLAUDE_CODE_SESSION_ID \
      AGENT_FEEDBACK_SESSION_ID AGENT_FEEDBACK_HARNESS AGENT_FEEDBACK_MODEL \
      CLAUDECODE OPENCODE OPENCODE_MODEL OPENCODE_API_KEY PI_CODING_AGENT \
      PI_CODING_AGENT_DIR OMP_PROFILE PI_MODEL CODEX_SANDBOX CODEX_API_KEY \
      REVIEW_LOG_DIR AGENT_FEEDBACK_REVIEW_DIRS \
      2>/dev/null || true
# Run everything from a non-git temp cwd so auto-detected context (cwd, git)
# is deterministic regardless of where the suite was invoked.
cd "$WORK" || exit 1

pass=0; fail=0
chk() { # chk <desc> <ok 0|1>
  if [ "$2" = 1 ]; then pass=$((pass+1)); echo "PASS  $1";
  else fail=$((fail+1)); echo "FAIL  $1"; fi
}
set_mode() { printf '%s' "$1" >"$STATE/mode"; }
set_list_rows() { printf '%s' "$1" >"$STATE/list_rows"; }
# Cap the rows the mock returns per list page, so paging can be exercised with
# a handful of rows instead of hundreds. 0 removes the cap.
set_list_page_cap() {
  if [ "$1" = 0 ]; then rm -f "$STATE/list_page_cap"; else printf '%s' "$1" >"$STATE/list_page_cap"; fi
}
# Portable mtime setter: `touch -d '8 days ago'` is GNU-only.
set_mtime() { # <path> <seconds ago>
  python3 -c 'import os,sys,time; p=sys.argv[1]; t=time.time()-float(sys.argv[2]); os.utime(p,(t,t))' "$1" "$2"
}
realpath_of() { python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$1"; }
log_len() {
  if [ -f "$STATE/requests.jsonl" ]; then
    wc -l <"$STATE/requests.jsonl" | tr -d ' '
  else
    echo 0
  fi
}
last_req() { tail -n1 "$STATE/requests.jsonl"; }
outcome() { tail -n1 <<<"$1"; }

set_list_rows 1

# ── process.sh ───────────────────────────────────────────────────────────────

# 17. list: unprocessed filter + compact TSV with family
set_list_rows 1
out=$(bash "$TRIAGE_SCRIPTS/process.sh" list 2>"$WORK/list.err")
req=$(last_req)
case "$(cat "$WORK/list.err")" in *"total: 1"*) total_shown=1 ;; *) total_shown=0 ;; esac
chk "process list -> processed=false, TSV row with family, total on stderr" "$(jq -n --arg o "$out" \
  --arg path "$(jq -r .path <<<"$req")" --argjson t "$total_shown" \
  'if ($path|contains("processed=false")) and ($t==1)
      and ($o|contains("1\tfriction\tfriction\ttestmach\ttooling\tfixture summary 1")) then 1 else 0 end')"

# 17a. list follows next_before_id across every page (500 rows per page)
set_list_rows 1200
before=$(log_len)
# The merged result is hundreds of KB: it goes to jq via --rawfile, never
# --arg (capped at 128 KiB per argument on Linux).
bash "$TRIAGE_SCRIPTS/process.sh" list --json >"$WORK/list2.out" 2>"$WORK/list2.err"
gets=$(tail -n +"$((before+1))" "$STATE/requests.jsonl" | jq -r 'select(.method=="GET") | 1' | wc -l | tr -d ' ')
cursors=$(tail -n +"$((before+1))" "$STATE/requests.jsonl" | jq -r 'select(.path|test("before_id=")) | 1' | wc -l | tr -d ' ')
case "$(cat "$WORK/list2.err")" in *"total: 1200"*) total_shown=1 ;; *) total_shown=0 ;; esac
chk "process list pages through 1200 rows in 3 requests, merged --json, total" "$(jq -n \
  --rawfile o "$WORK/list2.out" --argjson gets "$gets" --argjson cursors "$cursors" --argjson t "$total_shown" \
  '($o|fromjson) as $j |
   if ($j.submissions|length)==1200 and $j.total==1200 and $j.submissions[0].id==1200
      and $j.submissions[1199].id==1 and $gets==3 and $cursors==2 and $t==1 then 1 else 0 end')"

# 17b. --limit caps the total rows returned across pages
before=$(log_len)
bash "$TRIAGE_SCRIPTS/process.sh" list --limit 600 --json >"$WORK/list3.out" 2>/dev/null
gets=$(tail -n +"$((before+1))" "$STATE/requests.jsonl" | jq -r 'select(.method=="GET") | 1' | wc -l | tr -d ' ')
chk "process list --limit caps the whole result, not one page" "$(jq -n --rawfile o "$WORK/list3.out" --argjson gets "$gets" \
  '($o|fromjson) as $j | if ($j.submissions|length)==600 and $gets==2 then 1 else 0 end')"

# 17c. --include-processed drops the processed filter; --all is a hard error
out=$(bash "$TRIAGE_SCRIPTS/process.sh" list --include-processed --limit 1 2>/dev/null)
req=$(last_req)
inc_ok=$(jq -r 'if (.path|contains("processed=false")) then 0 else 1 end' <<<"$req")
before=$(log_len)
err=$(bash "$TRIAGE_SCRIPTS/process.sh" list --all 2>&1 >/dev/null)
rc=$?
case "$err" in *"--include-processed"*) all_msg=1 ;; *) all_msg=0 ;; esac
chk "list --include-processed drops the filter; --all errors pointing at it" \
  "$([ "$inc_ok" = 1 ] && [ "$rc" -ne 0 ] && [ "$all_msg" = 1 ] && [ "$(log_len)" -eq "$before" ] && echo 1 || echo 0)"
set_list_rows 1

# 17d. family/type/machine filters reach the server
bash "$TRIAGE_SCRIPTS/process.sh" list --family review --type review-panel --machine other >/dev/null 2>&1
req=$(last_req)
chk "process list passes family/type/machine" "$(jq -r '
  if (.path|test("family=review")) and (.path|test("type=review-panel"))
  and (.path|test("machine=other")) then 1 else 0 end' <<<"$req")"

# 18. done: batch mark with a resolution, outcome echoed verbatim
out=$(bash "$TRIAGE_SCRIPTS/process.sh" 'done' 43 44 --resolution "fixed in example@1a2b3c4" 2>/dev/null)
rc=$?
o=$(outcome "$out")
req=$(last_req)
chk "process done --resolution -> ids marked, resolution sent and echoed" "$(jq -n --arg o "$o" --argjson rc "$rc" \
  --arg body "$(jq -c .body <<<"$req")" \
  '($o|fromjson) as $j | ($body|fromjson) as $b |
   if $j.processed==true and $j.updated==[43,44] and $j.resolution=="fixed in example@1a2b3c4"
      and $b.ids==[43,44] and $b.processed==true and $b.resolution=="fixed in example@1a2b3c4"
      and $rc==0 then 1 else 0 end')"

# 18a. a blank resolution is rejected locally (the server would 400)
before=$(log_len)
out=$(bash "$TRIAGE_SCRIPTS/process.sh" 'done' 43 --resolution "   " 2>/dev/null)
rc=$?
o=$(outcome "$out")
chk "blank --resolution -> rejected locally, no request" "$(jq -n --arg o "$o" \
  --argjson rc "$rc" --argjson b "$before" --argjson a "$(log_len)" \
  '($o|fromjson) as $j | if $j.status=="rejected" and $rc==1 and $b==$a then 1 else 0 end')"

# 19. undo -> processed:false, no resolution field
out=$(bash "$TRIAGE_SCRIPTS/process.sh" undo 44 2>/dev/null)
req=$(last_req)
chk "process undo -> processed:false without resolution" "$(jq -r '
  if .body.processed==false and .body.ids==[44] and (.body|has("resolution")|not) then 1 else 0 end' <<<"$req")"

# 20. non-numeric id dies
if bash "$TRIAGE_SCRIPTS/process.sh" 'done' abc >/dev/null 2>&1; then rc=0; else rc=$?; fi
chk "process done abc -> exit 1" "$([ "$rc" = 1 ] && echo 1 || echo 0)"

# ── agentfeedback-triage digest.sh ────────────────────────────────────────────────

# 21. happy path: 3 unprocessed frictions served across two pages.
set_list_rows 3
set_list_page_cap 2
before=$(log_len)
out=$(bash "$TRIAGE_SCRIPTS/digest.sh" --out "$WORK/digest1" 2>"$WORK/digest1.err")
rc=$?
dir=$(outcome "$out")
gets=$(tail -n +"$((before+1))" "$STATE/requests.jsonl" | jq -r 'select(.method=="GET") | 1' | wc -l | tr -d ' ')
cursors=$(tail -n +"$((before+1))" "$STATE/requests.jsonl" | jq -r 'select(.path|test("before_id=")) | 1' | wc -l | tr -d ' ')
payload_q=$(tail -n +"$((before+1))" "$STATE/requests.jsonl" | jq -r 'select(.path|test("include=payload")) | 1' | wc -l | tr -d ' ')
chk "digest pages through 3 rows in 2 requests with include=payload" \
  "$([ "$rc" = 0 ] && [ "$gets" = 2 ] && [ "$cursors" = 1 ] && [ "$payload_q" = 2 ] && echo 1 || echo 0)"
chk "digest prints its directory as the last stdout line" \
  "$([ "$dir" = "$WORK/digest1" ] && [ -d "$dir" ] && echo 1 || echo 0)"
chk "digest writes one <id>.json per row" \
  "$([ -f "$WORK/digest1/1.json" ] && [ -f "$WORK/digest1/2.json" ] && [ -f "$WORK/digest1/3.json" ] && echo 1 || echo 0)"
chk "digest index.json holds every pulled row with its payload" "$(jq -r '
  if length==3 and ([.[].id]|sort)==[1,2,3]
     and (map(select(.payload.summary|startswith("fixture summary")))|length)==3
  then 1 else 0 end' "$WORK/digest1/index.json" 2>/dev/null || echo 0)"
md=$(cat "$WORK/digest1/digest.md" 2>/dev/null || true)
case "$md" in *"## project:"*) md_project=1 ;; *) md_project=0 ;; esac
case "$md" in *"### tooling"*) md_category=1 ;; *) md_category=0 ;; esac
case "$md" in *"#1"*) md_1=1 ;; *) md_1=0 ;; esac
case "$md" in *"#2"*) md_2=1 ;; *) md_2=0 ;; esac
case "$md" in *"#3"*) md_3=1 ;; *) md_3=0 ;; esac
case "$md" in *"pulled: 3"*) md_count=1 ;; *) md_count=0 ;; esac
chk "digest.md has project/category headings, every id, and pulled: 3" \
  "$([ "$md_project" = 1 ] && [ "$md_category" = 1 ] && [ "$md_count" = 1 ] \
     && [ "$md_1" = 1 ] && [ "$md_2" = 1 ] && [ "$md_3" = 1 ] && echo 1 || echo 0)"
set_list_page_cap 0

# 22. service unreachable -> exit 1, nothing on stdout
out=$(env AGENT_FEEDBACK_URL="http://127.0.0.1:1" bash "$TRIAGE_SCRIPTS/digest.sh" \
  --out "$WORK/digest2" 2>"$WORK/digest2.err")
rc=$?
chk "digest on an unreachable service -> exit 1, no directory on stdout" \
  "$([ "$rc" = 1 ] && [ -z "$out" ] && echo 1 || echo 0)"

# 23. empty queue -> exit 0 and a digest reporting pulled: 0
set_list_rows 0
out=$(bash "$TRIAGE_SCRIPTS/digest.sh" --out "$WORK/digest3" 2>"$WORK/digest3.err")
rc=$?
dir=$(outcome "$out")
md=$(cat "$WORK/digest3/digest.md" 2>/dev/null || true)
case "$md" in *"pulled: 0"*) md_count=1 ;; *) md_count=0 ;; esac
chk "digest on an empty queue -> exit 0, digest.md reports pulled: 0" \
  "$([ "$rc" = 0 ] && [ "$dir" = "$WORK/digest3" ] && [ "$md_count" = 1 ] && echo 1 || echo 0)"
set_list_rows 1

# ── outcome discipline on failure paths ──────────────────────────────────────

set_mode created
set_list_rows 1
rm -f "$SPOOL"/* 2>/dev/null || true

# 24. Every exit path ends with exactly one machine-readable outcome line:
# "error" for configuration/transport/HTTP failures, "rejected" for local
# validation failures.
before=$(log_len)
out=$(bash "$TRIAGE_SCRIPTS/process.sh" list --bogus 2>/dev/null); rc=$?
p_ok=$(jq -n --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="rejected" and $rc==1 then 1 else 0 end')
chk "unknown flag -> rejected outcome, exit 1, no request" \
  "$([ "$p_ok" = 1 ] && [ "$(log_len)" -eq "$before" ] && echo 1 || echo 0)"

out=$(env AGENT_FEEDBACK_URL="http://127.0.0.1:1" bash "$TRIAGE_SCRIPTS/process.sh" list 2>/dev/null); rc=$?
list_ok=$(jq -n --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="error" and ($j.message|test("unreachable")) and $rc==1 then 1 else 0 end')
out=$(env AGENT_FEEDBACK_URL="http://127.0.0.1:1" bash "$TRIAGE_SCRIPTS/process.sh" 'done' 43 2>/dev/null); rc=$?
done_ok=$(jq -n --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="error" and ($j.message|test("unreachable")) and $rc==1 then 1 else 0 end')
chk "process.sh on an unreachable service -> error outcome, exit 1" \
  "$([ "$list_ok" = 1 ] && [ "$done_ok" = 1 ] && echo 1 || echo 0)"
rm -f "$SPOOL"/* 2>/dev/null || true

# 25. A 200 that is not the documented classification shape is not an answer.
set_mode processed_bad
out=$(bash "$TRIAGE_SCRIPTS/process.sh" 'done' 43 2>/dev/null); rc=$?
chk "process done with a malformed 200 -> error outcome, exit 1" "$(jq -n \
  --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="error" and ($j.message|test("malformed")) and $rc==1 then 1 else 0 end')"
set_mode created

# ── pagination guards ────────────────────────────────────────────────────────

# 30. has_more with no usable cursor must stop loudly, never return a silently
# truncated queue.
set_mode pagination_bad
set_list_rows 5
set_list_page_cap 2
out=$(bash "$TRIAGE_SCRIPTS/process.sh" list 2>/dev/null); rc=$?
list_ok=$(jq -n --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="error" and $j.message=="malformed pagination response" and $rc==1 then 1 else 0 end')
out=$(bash "$TRIAGE_SCRIPTS/digest.sh" --out "$WORK/digest-badpage" 2>/dev/null); rc=$?
digest_ok=$(jq -n --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="error" and $j.message=="malformed pagination response" and $rc==1 then 1 else 0 end')
chk "malformed pagination stops process.sh list and digest.sh with an error outcome" \
  "$([ "$list_ok" = 1 ] && [ "$digest_ok" = 1 ] && echo 1 || echo 0)"
set_mode created
set_list_page_cap 0
set_list_rows 1

# ── temp-file hygiene ────────────────────────────────────────────────────────

# 31. EXIT traps must not reference function-local paths: a trap firing after
# the function returned expands them to "" and leaves the temp file behind.
# TMPDIR is set for GNU mktemp; macOS mktemp ignores it and uses the Darwin
# per-user temp dir, so the check is a before/after snapshot of whichever
# directory mktemp actually writes to.
PRIV_TMP="$WORK/private-tmp"
mkdir -p "$PRIV_TMP"
TMP_ROOT=$(dirname "$(TMPDIR="$PRIV_TMP" mktemp -u)")
tmp_entries() { find "$TMP_ROOT" "$PRIV_TMP" -maxdepth 1 -mindepth 1 2>/dev/null | sort; }
tmp_before=$(tmp_entries)
set_list_rows 3
TMPDIR="$PRIV_TMP" bash "$TRIAGE_SCRIPTS/process.sh" list >/dev/null 2>&1
leaked=$(comm -13 <(printf '%s\n' "$tmp_before") <(tmp_entries) | wc -l | tr -d ' ')
chk "process.sh list leaves no temp files behind" \
  "$([ "$leaked" = 0 ] && echo 1 || echo 0)"
set_list_rows 1

# ── bash 3.2: possibly-empty arrays ──────────────────────────────────────────

# 32. macOS ships bash 3.2, where "${ARR[@]}" on an empty array is an unbound
# variable under `set -u`. An unfiltered list expands an empty array.
set_list_rows 2
bash "$TRIAGE_SCRIPTS/process.sh" list --include-processed >/dev/null 2>&1; list_rc=$?
chk "unfiltered list runs with empty parameter arrays" \
  "$([ "$list_rc" = 0 ] && echo 1 || echo 0)"
set_list_rows 1

# ── agentfeedback-triage digest.sh output directory ───────────────────────────────

# 42. Two digests in the same second must not land in the same directory, and
# an existing non-empty --out is refused rather than mixed into.
set_mode created
set_list_rows 1
DIGEST_TMP="$WORK/digest-tmp"
mkdir -p "$DIGEST_TMP"
d1=$(TMPDIR="$DIGEST_TMP" bash "$TRIAGE_SCRIPTS/digest.sh" 2>/dev/null | tail -n1)
d2=$(TMPDIR="$DIGEST_TMP" bash "$TRIAGE_SCRIPTS/digest.sh" 2>/dev/null | tail -n1)
out=$(bash "$TRIAGE_SCRIPTS/digest.sh" --out "$WORK/digest1" 2>&1); rc=$?
case "$out" in *"not empty"*) refused=1 ;; *) refused=0 ;; esac
chk "digest makes a fresh directory per run and refuses a non-empty --out" \
  "$([ -n "$d1" ] && [ -n "$d2" ] && [ "$d1" != "$d2" ] && [ -d "$d1" ] && [ -d "$d2" ] \
     && [ "$rc" != 0 ] && [ "$refused" = 1 ] && echo 1 || echo 0)"

# 43. digest.sh is executable as shipped (it is invoked directly, not via bash).
chk "digest.sh is executable" "$([ -x "$TRIAGE_SCRIPTS/digest.sh" ] && echo 1 || echo 0)"

# ── paging contract: a 200 without it is malformed, not an empty queue ───────

# 45. A 200 carrying {} has no has_more/submissions/total: reporting it as the
# end of the walk would be indistinguishable from an empty queue.
set_mode list_bad_shape
out=$(bash "$TRIAGE_SCRIPTS/process.sh" list 2>/dev/null); rc=$?
list_ok=$(jq -n --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="error" and $j.message=="malformed pagination response" and $rc==1 then 1 else 0 end')
out=$(bash "$TRIAGE_SCRIPTS/digest.sh" --out "$WORK/digest-badshape" 2>/dev/null); rc=$?
digest_ok=$(jq -n --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="error" and $j.message=="malformed pagination response" and $rc==1 then 1 else 0 end')
chk "a {} 200 is an error outcome for list and digest, not an empty queue" \
  "$([ "$list_ok" = 1 ] && [ "$digest_ok" = 1 ] && echo 1 || echo 0)"
set_mode created

# ── flags that need a value ─────────────────────────────────────────────────

# 46. A flag with no value must be rejected, never `shift 2` into a `set -e`
# death — and never an endless loop where `set -e` happens to be disabled.
# Each call is watchdogged so a regression fails the suite instead of hanging.
run_with_timeout() { # <secs> <outfile> <cmd...>
  local secs="$1" outf="$2"; shift 2
  "$@" >"$outf" 2>/dev/null &
  local pid=$! rc=0
  ( sleep "$secs"; kill -9 "$pid" 2>/dev/null ) >/dev/null 2>&1 &
  local watch=$!
  wait "$pid" || rc=$?
  kill "$watch" 2>/dev/null
  wait "$watch" 2>/dev/null
  return "$rc"
}
before=$(log_len)
run_with_timeout 10 "$WORK/noval4.out" bash "$TRIAGE_SCRIPTS/process.sh" list --family; rc=$?
pl_ok=$(jq -n --arg o "$(outcome "$(cat "$WORK/noval4.out")")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="rejected" and ($j.message|test("requires a value")) and $rc==1 then 1 else 0 end' 2>/dev/null || echo 0)
run_with_timeout 10 "$WORK/noval5.out" bash "$TRIAGE_SCRIPTS/process.sh" 'done' 43 --resolution; rc=$?
pd_ok=$(jq -n --arg o "$(outcome "$(cat "$WORK/noval5.out")")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="rejected" and ($j.message|test("requires a value")) and $rc==1 then 1 else 0 end' 2>/dev/null || echo 0)
run_with_timeout 10 "$WORK/noval7.out" bash "$TRIAGE_SCRIPTS/digest.sh" --out; rc=$?
dg_ok=$(jq -n --arg o "$(outcome "$(cat "$WORK/noval7.out")")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="rejected" and ($j.message|test("requires a value")) and $rc==1 then 1 else 0 end' 2>/dev/null || echo 0)
chk "a value-less flag -> rejected outcome, exit 1, no request, no loop" \
  "$([ "$pl_ok" = 1 ] && [ "$pd_ok" = 1 ] && [ "$dg_ok" = 1 ] \
     && [ "$(log_len)" -eq "$before" ] && echo 1 || echo 0)"

# ── process.sh: the classification must answer THIS request ─────────────────

# 50. A well-shaped 200 that classifies submissions nobody asked about is not
# an answer: echoing it would report a mark that never happened.
set_mode processed_foreign_ids
out=$(bash "$TRIAGE_SCRIPTS/process.sh" 'done' 43 2>/dev/null); rc=$?
chk "processed 200 naming foreign ids -> error outcome, exit 1" "$(jq -n \
  --arg o "$(outcome "$out")" --argjson rc "$rc" '($o|fromjson) as $j |
  if $j.status=="error" and $rc==1 then 1 else 0 end')"
set_mode created

python3 "$TESTS_DIR/test_cluster.py"
rc=$?
chk "triage advisory disclosure and failure boundaries" "$([ "$rc" = 0 ] && echo 1 || echo 0)"

echo
echo "triage tests: $pass passed, $fail failed"
[ "$fail" = 0 ]
