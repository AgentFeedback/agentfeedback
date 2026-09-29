# AgentFeedback HTTP API

The contract producers and processors integrate against. Self-contained: read
this, then write the calls. Prefer the shipped client scripts in
[`skills/agentfeedback/scripts/`](../skills/agentfeedback/scripts/README.md); they implement
spooling, retries, receipt validation and outcome reporting. Hand-roll HTTP
only for a producer the client does not cover.

API version: **1.1**. Additions since 1.0 are listed in
[Changes since API 1.0](#changes-since-api-10).

Contents:

1. [Base URL, auth, common rules](#base-url-auth-common-rules)
2. [The record](#the-record)
3. [Write endpoints](#write-endpoints): frictions, reviews, events
4. [Read endpoints](#read-endpoints): list, get, export
5. [Processing](#processing): mark processed with a resolution
6. [Idempotency and duplicates](#idempotency-and-duplicates)
7. [Operational endpoints](#operational-endpoints)
8. [Changes since API 1.0](#changes-since-api-10)

## Base URL, auth, common rules

Two environment variables, set by the operator on every machine that talks to
the service. There is no hosted default.

```
AGENT_FEEDBACK_URL=http://127.0.0.1:8090
AGENT_FEEDBACK_API_KEY=<shared key>
```

Every `/api/v1/*` request carries the key in either header:

```
Authorization: Bearer $AGENT_FEEDBACK_API_KEY
X-Api-Key: $AGENT_FEEDBACK_API_KEY
```

Missing or wrong key: `401 {"error":"unauthorized","message":"missing or invalid API key"}`.

Rules that apply to every endpoint:

- Responses are JSON, except `GET /api/v1/export` (NDJSON) and `/health`,
  `/ready` (plain text). Errors have the shape `{"error":"<code>","message":"<text>"}`.
  `message` names the offending field when there is one.
- POST bodies must be exactly one JSON object. Unknown fields are rejected with
  `400 bad_request` naming the field. A typo is a bug, not something to drop:
  a dropped field plus an idempotent replay would lose data for good.
- Bodies over 10 MiB (including trailing whitespace) get
  `413 {"error":"request_too_large","message":"request body exceeds 10 MiB"}`.
- Timestamps are RFC 3339 in UTC, microsecond precision, e.g.
  `2026-09-17T20:06:48.123456Z`. Query parameters accept any RFC 3339 offset.
- Stored content is never modified after creation. The only mutable state is
  the processing mark (`processed_at`, `resolution`).

Field length limits (`400` naming the field when exceeded). They protect the
database, not police content; free-text fields are bounded only by the body cap.

| Fields | Limit |
|---|---|
| `machine_name`, `coordinator_model`, `skill`, `run_id`, `kind`, `key`, `category`, `project`, `harness`, `reviewers[].slot`, `reviewers[].model`, `reviewers[].status` | 200 bytes |
| friction `summary`, `resolution` | 2000 bytes |
| `reviewers[].note` | 4000 bytes |
| `reviewers[]` count | 100 |
| `context` object | 32 entries, keys 64 bytes, values 2000 bytes |
| `details`, `suggested_fix`, `prompt`, `reviewers[].output`, event `payload` | body cap only |

## The record

Every submission, whatever its family, is returned in one shape:

```json
{
  "id": 43,
  "family": "friction",
  "submission_type": "friction",
  "machine_name": "workstation-a",
  "coordinator_model": "claude-fable-5-1",
  "run_id": null,
  "payload": { "category": "documentation", "summary": "…", "context": { "…": "…" } },
  "payload_hash": "9f3a…",
  "created_at": "2026-09-17T20:06:48.123456Z",
  "processed_at": null,
  "resolution": null
}
```

| Field | Meaning |
|---|---|
| `id` | Positive integer, unique across all families, never reused. |
| `family` | `friction`, `review` or `event`. Decides how the payload is shaped and how duplicates are handled. |
| `submission_type` | `friction` for frictions; the submitting `skill` for reviews; the `kind` for events. |
| `run_id` | Idempotency key within `(family, submission_type)`: the review `run_id` or the event `key`. `null` for frictions. |
| `payload` | The stored content (see [Stored payload shapes](#stored-payload-shapes)). Numbers are returned exactly as stored. |
| `payload_hash` | SHA-256 hex over the canonical content (see [Idempotency](#idempotency-and-duplicates)). Equal hashes mean the same content under the family's canonical form. |
| `created_at` | Receipt time at the server. For frictions the client's own `context.occurred_at` records when it happened. |
| `processed_at`, `resolution` | Set by a processor. Absent until then. |

As in API 1.0, `run_id`, `processed_at` and `resolution` are **omitted** when
unset rather than sent as `null`. `family` and `payload_hash` are always
present. The example above shows the omitted fields as `null` only to list
them.

### Stored payload shapes

| Family | `payload` as stored and returned |
|---|---|
| friction | `{category, summary, details?, suggested_fix?, project?, harness?, context?}`; `category` and `summary` trimmed; empty optional strings omitted |
| review | `{prompt?, reviewers}`; `reviewers` entries as submitted with empty optionals omitted |
| event | the submitted `payload` object, compacted (insignificant whitespace removed) with key order and number spelling preserved |

Identifier fields (`machine_name`, `coordinator_model`, `skill`, `run_id`,
`kind`, `key`) are trimmed of surrounding whitespace before validation,
storage and hashing. Comparison is case-sensitive.

## Write endpoints

### POST /api/v1/frictions

What slowed an agent down. The primary write path.

| Field | Type | Required | Notes |
|---|---|---|---|
| `machine_name` | string | yes | the reporting machine's canonical name |
| `coordinator_model` | string | yes | the reporting agent's model id; never `unknown` if you know it |
| `category` | string | yes | free-form, e.g. `documentation`, `tooling`, `config`, `environment` |
| `summary` | string | yes | one line, what the friction was |
| `details` | string | no | expected vs actual, what it cost |
| `suggested_fix` | string | no | the concrete fix; if already applied, name the commit |
| `project` | string | no | repository or project name |
| `harness` | string | no | e.g. `claude-code`, `opencode`, `codex` |
| `context` | object | no | flat string map of auto-collected metadata; excluded from duplicate detection |

`201` with the record when stored. `200` with the existing record when an
identical friction (same content, same reporter, `context` ignored) was stored
within the last 24 hours; blind retries are safe. `400 create_friction_failed`
on validation failure.

```bash
curl -sS -X POST "$AGENT_FEEDBACK_URL/api/v1/frictions" \
  -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" -H 'Content-Type: application/json' \
  -d '{"machine_name":"workstation-a","coordinator_model":"claude-fable-5-1",
       "category":"documentation","summary":"README install step references a flag that no longer exists",
       "suggested_fix":"replace --legacy with --compat in README step 3","project":"example","harness":"claude-code"}'
```

### POST /api/v1/reviews

One record per completed multi-reviewer run: per-reviewer status, timing and
grading. The client builds this from a run directory; see the
[run-directory contract](../skills/agentfeedback/scripts/README.md#run-directory-contract).

| Field | Type | Required | Notes |
|---|---|---|---|
| `skill` | string | yes | the submitting skill; `friction` is reserved and rejected |
| `machine_name`, `coordinator_model` | string | yes | as for frictions |
| `run_id` | string | yes | idempotency key with `skill`. Uniqueness is `(family, skill, run_id)` and does not include the machine, so put the machine name in the value (`<machine>-<run_ts>-<pid>`) to keep machines from colliding |
| `prompt` | string | no | the review prompt |
| `reviewers` | array | yes, non-empty | one entry per reviewer |
| `reviewers[].slot`, `.model`, `.status` | string | yes | `status` is free-form: `completed`, `timeout`, `error`, … |
| `reviewers[].duration_s`, `.bytes` | int ≥ 0 | no | |
| `reviewers[].output` | string | no | raw output; omit for timeouts |
| `reviewers[].score` | int 1–5 | no | grading score |
| `reviewers[].valid`, `.invalid` | int ≥ 0 | no | finding counts; omit when unknown, never send 0 for unknown |
| `reviewers[].note` | string | no | grading note |

`201` new, `200` identical replay, `409 replay_mismatch` when the same
`(skill, run_id)` arrives with different content (the stored record is never
changed; submit a correction under a new `run_id`), `400 create_review_failed`
on validation failure.

### POST /api/v1/events

Anything else worth recording once: a generic write-once envelope with a
free-form JSON object payload. Nothing in the service interprets the payload.

| Field | Type | Required | Notes |
|---|---|---|---|
| `kind` | string | yes | producer-chosen namespace, e.g. `deploy`, `benchmark`; `friction` is reserved |
| `key` | string | yes | idempotency key within `kind`. Uniqueness is `(family, kind, key)` and does not include the machine; put the machine name in the value |
| `machine_name`, `coordinator_model` | string | yes | as for frictions |
| `payload` | object | yes | any JSON object; duplicate keys anywhere in it are rejected with `400` |

`201` new, `200` identical replay, `409 replay_mismatch` on different content
under the same `(kind, key)`, `400 create_event_failed` on validation failure.

```bash
curl -sS -X POST "$AGENT_FEEDBACK_URL/api/v1/events" \
  -H "X-Api-Key: $AGENT_FEEDBACK_API_KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"deploy","key":"workstation-a-20260917-200648","machine_name":"workstation-a",
       "coordinator_model":"claude-fable-5-1","payload":{"service":"agentfeedback","image":"sha-0e840b2","ok":true}}'
```

## Read endpoints

### GET /api/v1/submissions

Filtered list, newest first (descending `id`). All parameters optional.

| Param | Type | Notes |
|---|---|---|
| `family` | `friction` \| `review` \| `event` | |
| `type` | string | filters `submission_type` (skill name, event kind, or `friction`) |
| `machine` | string | filters `machine_name` |
| `model` | string | filters `coordinator_model` |
| `since`, `until` | RFC 3339 | `created_at >=` / `<=` |
| `processed` | `true` \| `false` | `false` is the processor's work queue |
| `before_id` | int | keyset cursor: only rows with `id < before_id`. Preferred for paging |
| `offset` | int | API 1.0 offset paging, default 0, clamped to `>= 0`. Cannot be combined with `before_id` (`400`) |
| `limit` | int | default 50, max 500 (max 100 with `include=payload`); `<= 0` becomes 50 |
| `include` | `payload` | return full records instead of summaries |

Response:

```json
{
  "submissions": [ { "id": 43, "family": "friction", "…": "…" } ],
  "limit": 50,
  "offset": 0,
  "total": 45,
  "has_more": false,
  "next_before_id": null
}
```

- Rows are summaries: the record without `payload`, plus for frictions the
  `category`, `summary`, `project` and `harness` fields lifted to the top level
  so a list is scannable. With `include=payload` rows are the summary shape with
  `payload` added (the lifted friction fields stay); a page can then be large (up to 100 records of up to 10 MiB each), so keep
  `limit` small when payloads are big.
- `total` counts every row matching the filters (ignoring `before_id`,
  `offset` and `limit`), computed in the same read transaction as the page.
  It is a per-response snapshot and changes as rows arrive or are marked.
- `has_more` is true when rows older than this page match; `next_before_id` is
  then the `id` of the page's last row, ready to pass back as `before_id`.
- `limit` and `offset` echo the values actually used after clamping.

Draining a queue: page with `processed=false` following `next_before_id`
until `has_more` is false. With `before_id` it is safe to act on and mark a
page before fetching the next one (marking removes rows only above the
cursor). With `offset` it is not: marking shifts later pages and skips rows.
Rows created after the first page have higher ids and are picked up by the
next pass. This is a best-effort traversal for one processor at a time, not a
claim or lease; two concurrent processors can act on the same row.

`400 bad_request` names any malformed parameter (`since must be RFC 3339`,
`processed must be true or false`, `before_id must be a positive integer`, …).

```bash
curl -sS "$AGENT_FEEDBACK_URL/api/v1/submissions?family=friction&processed=false&include=payload&limit=100" \
  -H "X-Api-Key: $AGENT_FEEDBACK_API_KEY"
```

### GET /api/v1/submissions/{id}

The full record. `400 bad_request` if `{id}` is not numeric,
`404 get_submission_failed` if it does not exist.

### GET /api/v1/export

Every record as newline-delimited JSON (`Content-Type: application/x-ndjson`)
in one consistent read transaction:

1. A header line: `{"export_format":1,"family":null,"since":null,"exported_at":"2026-09-17T20:06:48.000000Z"}`
   (`family`/`since` echo the filters, `null` when unfiltered).
2. One full record per line, ascending `id`.
3. A terminator: `{"export_complete":true,"count":676,"sha256":"<hex>"}` where
   `sha256` is over the record lines (each with its trailing newline), header
   excluded.

A stream without the terminator, or whose record count or digest disagrees
with it, is damaged; do not restore from it. Optional filters: `family`,
`since`. Unfiltered exports are the logical backup and migration format; the
`agentfeedback import` command verifies the header, count and digest, refuses
filtered exports unless told otherwise, and preserves ids, timestamps, hashes
and processing state (see [operate.md](operate.md#restore-and-migration)).
For a physical backup of the live database use `agentfeedback backup`.

## Processing

### POST /api/v1/submissions/processed

Mark or unmark a batch after acting on it. The only write that changes an
existing row.

| Field | Type | Required | Notes |
|---|---|---|---|
| `ids` | int array | yes, 1–500 entries, positive | duplicates are collapsed |
| `processed` | bool | no, default `true` | `false` clears the mark |
| `resolution` | string | no | ≤ 2000 bytes after trimming; what was done, e.g. `fixed in example@1a2b3c4`, `invalid: premise wrong`, `duplicate of 41`. Only with `processed=true`. Omitted or `null` means not given; an empty or whitespace-only string is a `400`. |

One resolution applies to the whole batch. Issue one request per distinct
resolution. The response echoes the trimmed `resolution` when one was given
and omits it otherwise.

`200` with every id classified:

```json
{"processed": true, "resolution": "fixed in example@1a2b3c4", "updated": [43, 44], "unchanged": [42], "not_found": [999]}
```

- `updated`: the row changed. Marking an unprocessed row sets `processed_at`
  (now) and `resolution`. Re-marking an already processed row with a different
  non-empty `resolution` replaces the resolution and keeps the original
  `processed_at`.
- `unchanged`: already in the requested state with the same resolution (or no
  resolution given). Marking is idempotent.
- `not_found`: no such id.
- Unmarking clears both `processed_at` and `resolution`.
- Classification and updates happen in one transaction.

`400 set_processed_failed` for empty, oversized or non-positive `ids`, or a
`resolution` sent with `processed=false`.

## Idempotency and duplicates

| Family | Key | Identical replay | Different content, same key |
|---|---|---|---|
| review | `(skill, run_id)` | `200` existing record | `409 replay_mismatch` |
| event | `(kind, key)` | `200` existing record | `409 replay_mismatch` |
| friction | content hash, 24 h window | `200` existing record | new `201` row (it is a different friction) |

Uniqueness for reviews and events is enforced on `(family, submission_type,
run_id)`: a review and an event may use the same type and key without
colliding, and the machine name is not part of the key.

Content identity is `payload_hash`, SHA-256 hex over a canonical encoding of
`{machine_name, coordinator_model, payload}`:

- **Frictions and reviews** use the API 1.0 canonical form unchanged: the
  fields in the fixed order `machine_name`, `coordinator_model`, `payload`,
  with the stored payload shape above (fixed field order, empty optionals
  omitted). For frictions the `context` object is removed before hashing, so
  the same friction re-filed with a new timestamp, commit or working directory
  still dedupes. Hashes computed by a 1.0 service for the same content are
  identical, which is what makes imported rows replay-safe.
- **Events** use canonical JSON: object keys sorted by byte order at every
  level, strings escaped the way Go's `encoding/json` escapes them, numbers
  kept as their textual form (`1`, `1.0` and `1e0` are different content),
  no insignificant whitespace, duplicate keys rejected.

A friction re-encountered after the 24 h window is a new row on purpose:
recurrence stays visible to the processor.

Concurrent identical writes are serialized by the database; exactly one row
results. Rows imported from API 1.0 keep their original hash; rows that
predate hashing in 1.0 get one computed with the 1.0 form at import, so every
stored row has a hash and replays are always compared.

## Operational endpoints

No authentication. Keep them inside the deployment boundary.

- `GET /health`: process liveness, `200 OK`.
- `GET /ready`: `200 READY` when the database answers and the schema is
  current; `503` with `SHUTTING_DOWN` or `DB_UNAVAILABLE` otherwise.
- `GET /metrics`: Prometheus text format. Request counters and latency
  histograms are labelled by route pattern, allow-listed method and status
  code, never by raw path or client input.

## Changes since API 1.0

Every 1.0 request keeps working and every 1.0 response field keeps its name,
type and omission behaviour. Additions:

- `family` and `payload_hash` on every record; `family` as a list filter;
  `POST /api/v1/events`.
- `resolution` on the processed endpoint and on records; re-marking with a
  new resolution counts as `updated`.
- `before_id`, `include=payload`, `total`, `has_more`, `next_before_id` on
  the list endpoint. `offset` stays supported.
- `GET /api/v1/export`.
- Timestamps always carry exactly six fractional digits (1.0 emitted
  whatever precision the database held, from none to six).
- Every stored row has a hash, so the 1.0 "legacy row without hash replays
  without comparison" case no longer exists.
- `payload` is returned byte-exact as stored. 1.0 re-encoded it (keys sorted,
  numbers as floats); 1.1 keeps submission order and number spelling for new
  rows, and PostgreSQL's key order for imported rows. Consumers must not
  depend on key order.
- Unmatched routes and wrong methods under `/api/v1/` return the JSON error
  shape (`not_found`, `method_not_allowed`) instead of plain text.
- `GET /ready` also checks the schema version.
