# AgentFeedback bash client

The bash client scripts in this directory, for the API the running service
implements (`docs/api.md` in the service repository). The skill's `SKILL.md`
teaches the `agentfeedback` binary of the next major release; until that
release, these scripts are the client. Commands below run from the skill
directory (`bash scripts/…`).

Client for the AgentFeedback service (a small Go + SQLite HTTP service; API
contract in the service repository's `docs/api.md`). This skill is one of two:

| Skill | Role | Who runs it |
|---|---|---|
| **agentfeedback** (this) | submit and read | every agent, in every harness, as part of normal work |
| [**agentfeedback-triage**](../../agentfeedback-triage/SKILL.md) | process the queue | on demand, only when the user invokes it by name |

Three kinds of data, all write-once:

- **Frictions**: what slowed an agent down, for triage into better tooling and docs. The main path.
- **Reviews**: one record per completed multi-reviewer run (timings, status, grades).
- **Events**: anything else worth recording once, with a free-form JSON payload.

You only report. You never need to read before writing; the server absorbs
duplicates and replays itself.

## Install

Copy this directory (`SKILL.md` + `scripts/`) into your harness's skills
location, for example `~/.claude/skills/agentfeedback/`, or a shared skills
directory that several harnesses read. Nothing to build. Then set the
environment, typically in a shell profile every harness inherits. The service
is either one you run yourself or the hosted one: for agentfeedback.io set
`AGENT_FEEDBACK_URL=https://api.agentfeedback.io` and use an API key issued
there.

```
AGENT_FEEDBACK_URL        required: your service endpoint, e.g. http://192.0.2.10:8090
AGENT_FEEDBACK_API_KEY    required: the shared key from the operator
AGENT_FEEDBACK_MACHINE    optional: canonical machine name; defaults to `hostname -s`
AGENT_FEEDBACK_MODEL      optional: your model id when the harness cannot tell the scripts
AGENT_FEEDBACK_HARNESS    optional: overrides harness auto-detection
AGENT_FEEDBACK_SESSION_ID optional: overrides session-id detection
AGENT_FEEDBACK_REVIEW_DIRS optional: colon-separated review run-directory roots for submit-review.sh --sweep
AGENT_FEEDBACK_TRIAGE_ROOTS optional, triage only: colon-separated directories holding local checkouts
```

The sibling `agentfeedback-triage` skill installs the same way (the whole
directory, including `reference/`), beside this one, and only where someone processes the queue. It runs only when the user
invokes it (`disable-model-invocation: true`).

Check the install: `bash scripts/submit-friction.sh --category test --summary "install check" --model <your model> --dry-run`
prints the payload and `{"status":"valid"}` without sending anything.

Uninstall: flush or discard the spool (`bash scripts/query.sh --flush --limit 1`,
or `rm -rf ~/.cache/agentfeedback`), delete this directory (and the sibling
`agentfeedback-triage` if installed) from every harness's skills location, and
remove the `AGENT_FEEDBACK_*` variables from shell profiles. Removing the
service itself is described in the repository's `docs/operate.md`.

## Submit a friction

Prefer stdin JSON for prose: no shell quoting, newlines and quotes survive.

```bash
bash scripts/submit-friction.sh --stdin <<'JSON'
{
  "category": "documentation",
  "summary": "README install step 3 references a flag that no longer exists",
  "details": "README says --legacy; the CLI rejects it since v1.4 and the fix took two failed runs to find.",
  "suggested_fix": "replace --legacy with --compat in README step 3 (applied in a1b2c3d)",
  "model": "claude-fable-5-1"
}
JSON
```

Flags mode for one-liners:

```bash
bash scripts/submit-friction.sh --category tooling --summary "linter hangs on empty files" --model claude-fable-5-1
```

- `category` and `summary` are required. Categories are free-form; common
  ones: `documentation`, `tooling`, `config`, `environment`.
- **Always pass your model id** (`--model` or `"model"`); no harness exposes it
  to child processes and `unknown` rows are useless for analysis.
- `harness`, `project`, `machine` are auto-detected. Override `harness` and
  `project` with their flags, `machine` with `AGENT_FEEDBACK_MACHINE` or the
  `"machine"` key in stdin JSON (`submit-friction.sh` has no `--machine`).
- **Context is collected for you**: `occurred_at`, `cwd`, `repo_root`,
  `git_remote` (credentials stripped), `git_branch`, `git_commit`, `git_dirty`,
  `os`, `arch`, `session_id`, `client_version`, and harness metadata when
  available. A `"context"` object in stdin JSON adds string pairs on top.
  Context never affects duplicate detection.
- If `suggested_fix` says the fix was applied, name the commit; otherwise say
  it is uncommitted or pending. The script warns when an applied claim has no
  commit hash.
- Retries are safe: identical content within 24 hours is absorbed by the
  server (`{"status":"duplicate"}` is success, not an error).
- Fixed something a queued report already covers? Close its row as part of
  the fix (see [Process the queue](#process-the-queue)) and name the id in the
  commit body (`friction 43`).

## Outcomes and exit codes

Every submit and process command prints, as its **last stdout line**, one
JSON outcome; `submit-review.sh --sweep` prints one per submitted run and
none when nothing qualifies. Relay outcomes to the user verbatim.

| Outcome | Meaning | Exit |
|---|---|---|
| `{"status":"submitted","id":N}` | stored as a new record | 0 |
| `{"status":"duplicate","id":N}` | server already had identical content; fine | 0 |
| `{"status":"spooled","reason":"…"}` | service unreachable or 5xx; saved locally, retried on the next call | 0 |
| `{"status":"valid"}` | `--dry-run` passed local validation | 0 |
| `{"status":"mismatch",…}` | review/event replay differs from the stored record (409); submit a correction under a new key | 1 |
| `{"status":"collision",…}` | the server answered with a different record (another key or machine); nothing was marked | 1 |
| `{"status":"rejected",…}` | payload bug (4xx or local validation); do not retry as-is | 1 |
| `{"status":"skipped","reason":"…"}` | `submit-review.sh` found the run incomplete or unparseable; nothing was sent | 1 |
| `{"status":"failed","reason":"spool_unwritable",…}` | could not send and could not save; the payload is printed to stderr for recovery | 1 |
| `{"status":"error","message":"…"}` | configuration, transport or HTTP failure in a read or process command; nothing changed | 1 |

Spool location: `~/.cache/agentfeedback/spool/` (owner-only). `spooled`
means the payload file is written and renamed into place; a power loss in
the same instant can still lose it, so a backlog warning plus the payload on
stderr is the recovery path, not a durability guarantee. Frictions are
retried for 20 hours (inside the server's 24 hour dedupe window, so a retry
can never double-file); reviews and events for 30 days (they are idempotent).
A `401` leaves spooled payloads retryable so a key fix flushes them. The
submit scripts and `query.sh` print a one-line backlog warning on stderr while
unsent payloads exist (`process.sh` does not); surface it, it is the only
signal the service is down. The scripts
never print the API key, and send it via a mode-0600 header file, not argv.

## Process the queue

Used by the agentfeedback-triage skill and by any session closing a row it fixed.

```bash
bash scripts/process.sh list                          # every unprocessed row, all pages; TSV: id family type machine category-or-run_id summary
bash scripts/process.sh list --family friction --json # merged JSON {"submissions":[…],"total":N}
bash scripts/process.sh list --include-processed --limit 200
bash scripts/process.sh done 43 44 --resolution "fixed in example@a1b2c3d"
bash scripts/process.sh done 42 --resolution "invalid: flag exists since v1.4"
bash scripts/process.sh undo 44
```

`done` and `undo` print the server's classification verbatim:
`{"processed":true,"resolution":"…","updated":[43],"unchanged":[44],"not_found":[]}`.
An unexpected `unchanged` means the row was already in that state. One
`done` per distinct resolution.

## Read data back

```bash
bash scripts/query.sh --family friction --processed false --include-payload
bash scripts/query.sh --type deploy --since 2026-09-01T00:00:00Z --limit 20
bash scripts/query.sh 43 | jq .payload
bash scripts/query.sh export > backup.jsonl   # stdout is the verified NDJSON; the verdict goes to stderr
```

Raw JSON out. List rows are summaries (frictions carry `category`, `summary`,
`project`, `harness`); pass `--include-payload` or fetch by id for payloads.
`--before-id N` continues a page. Read-only unless `--flush` is given.
`export` verifies the header, record count and SHA-256 digest; on any
mismatch it exits 1 and prints an `error` outcome on stderr while still
writing what it received.

## Submit an event

```bash
bash scripts/submit-event.sh --kind deploy --key "$(hostname -s)-$(date -u +%Y%m%d-%H%M%S)" --model claude-fable-5-1 --stdin <<'JSON'
{"service":"agentfeedback","image":"sha-0e840b2","ok":true}
JSON
```

`kind` namespaces the key; `friction` is reserved. Replaying the same
`(kind, key)` with identical payload returns `duplicate`; a different payload
returns `mismatch`. Omit `--key` for a generated `<machine>-<timestamp>-<pid>`.

## Submit a review run

For review runners that keep one directory per run. Runners call this at the
end of grading; humans call it to retry.

```bash
bash scripts/submit-review.sh <run_dir>                    # timings + grades; identical replay → duplicate
bash scripts/submit-review.sh <run_dir> --include-outputs  # also prompt and raw reviewer outputs
bash scripts/submit-review.sh --sweep                      # flush the spool, submit fully graded runs without a .submitted marker
```

`--sweep` scans `REVIEW_LOG_DIR` and every directory in
`AGENT_FEEDBACK_REVIEW_DIRS`, skipping runs younger than `SWEEP_MIN_AGE_HOURS`
(default 2) and doing nothing while another sweep holds the lock; to retry a
fresh run, pass its directory. Runs sharing a timestamp are refused because a
timestamp-keyed ledger cannot tell their grades apart; fix the ledger, then
retry. `--include-outputs` after a default submission returns `mismatch`: the
enriched payload differs from the stored one, and the server never
overwrites.

### Run-directory contract

`<root>/<run_ts>-<pid>/` containing:

- `meta.json`: `machine`, `skill`, `run_ts`, `caller` (coordinator model at
  run time), and a `slots` map of slot → `{model, label}`; only `label` is
  read from it, the model comes from `summary.tsv`.
- `summary.tsv`: header, then one row per reviewer: `slot model status duration_s bytes`
  (`duration_s`/`bytes` may be empty).
- `<slot>.md`: raw reviewer output (sent only with `--include-outputs`).
- `prompt.md`: the review prompt (sent only with `--include-outputs`).
- `../scorecards.tsv` beside the run directories: grading ledger keyed by
  `run_ts` and reviewer **label**, tab-separated columns
  `run_ts source label score valid invalid note` (`source` is free-form,
  e.g. the grading session; `valid`, `invalid`, `note` may be empty);
  `PENDING` in `score` means not yet graded.

Every completed reviewer must have a grade before submission. `run_id` is
`<machine>-<run_ts>-<pid>`. A `.submitted` file holding the record id is
written into the run directory after a validated receipt.

## Failure behaviour, in one place

- 000 / 5xx → spool, outcome `spooled`, exit 0.
- 4xx → outcome `rejected` with the server's message (it names the field), exit 1.
- 1xx / 3xx → outcome `rejected`, exit 1: the URL does not point at the service.
- 409 → outcome `mismatch`, exit 1; stored record untouched.
- 2xx with a malformed body → treated as not delivered: spooled for retry.
- Spool directory unwritable → outcome `failed`, payload on stderr, exit 1.
- Rejected spool files (`*.rejected`) are kept 30 days for inspection.
