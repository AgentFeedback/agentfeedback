# Releases

## Versioning and publication

Source releases are annotated, immutable `vMAJOR.MINOR.PATCH` tags: patch for
compatible fixes, docs and dependency updates; minor for compatible features;
major for breaking API or operational contracts. Every delivered change belongs
to a release. The two skills carry their own `version` in their `SKILL.md`;
bump one only when its command contract changes. For `agentfeedback`, bump
`AF_CLIENT_VERSION` in `scripts/_common.sh` and its pin in
`tests/skill/run-tests.sh` with it: that value is what payloads report.

Maintainers need Git, Python 3, the tools `just ci` needs
([develop.md](develop.md)), Docker with buildx logged in to `ghcr.io`, and an
authenticated GitHub CLI (`gh`) with write access to the repository and its
tags. There is no hosted CI: every check runs on the releasing machine.

1. Update the stable version link in `README.md`, the `SERVICE_VERSION`
   default in `cmd/agentfeedback/main.go` and in the root `.env.example`, and add
   notes to this file.
2. Run `just ci`, commit, push `main`.
3. Write the release notes to a file outside the checkout or under the
   gitignored `.private/`.
4. Run the release tool, first in preflight, then for real:

   ```bash
   python3 scripts/release.py vX.Y.Z 'Release name' /path/to/notes.md --check
   python3 scripts/release.py vX.Y.Z 'Release name' /path/to/notes.md
   ```

   It checks a clean `main` against `origin`, refuses an existing tag, runs
   `just ci` on the checkout, pushes the annotated tag with an absence lease,
   creates and verifies a stable release, then builds and pushes the image
   with `just image-push X.Y.Z <sha> latest`. Deploy the SHA or the version
   tag, never `latest`.

### Recovery

If publication fails after tagging, inspect the local tag, the remote tag, the
GitHub release and the image tags on GHCR before doing anything. Never
force-move a published tag. Local tag only: push it with an absence lease.
Remote tag without a release:
`gh release create <tag> --verify-tag --latest --title '<name>' --notes-file <file>`.
Release exists: inspect it rather than creating another. Release without an
image: `just image-push X.Y.Z <sha> latest` from the tagged commit.

The notes file from step 3 is uncommitted, so it does not exist on another
machine: recovering there means rebuilding it from this file's section for
that version. Rewrite the section's relative links to repo-root paths
(`api.md` to `docs/api.md`) — a release body resolves them against the
repository root, not `docs/`. Corrections to source
need a new version; corrections to release prose need no new tag.

## v3.0.0 — AgentFeedback: new home, new names, fresh start

The project moved to [github.com/AgentFeedback/agentfeedback](https://github.com/AgentFeedback/agentfeedback)
and is the open-source, self-hostable AgentFeedback
([agentfeedback.dev](https://agentfeedback.dev)); a hosted version with the
same API runs at [agentfeedback.io](https://agentfeedback.io). Every name
changes and v3 is a fresh start: no migration from or compatibility with
2.x deployments and skill installs. The HTTP API (1.1), payloads and hash forms are unchanged.

- **Skills** `agent-feedback` → `agentfeedback` 4.0 and
  `agent-feedback-triage` → `agentfeedback-triage` 3.0: new directory and
  invocation names (`/agentfeedback-triage`). Client environment variables
  keep their names (`AGENT_FEEDBACK_*`); the spool moves to `~/.cache/agentfeedback` and triage digests to
  `${TMPDIR:-/tmp}/agentfeedback-triage/`. The skills work against a
  self-hosted service or `https://api.agentfeedback.io`.
- **Service**: Go module `github.com/agentfeedback/agentfeedback`; binary
  `agentfeedback` (`cmd/agentfeedback`, `/opt/agentfeedback`, runs as user
  `agentfeedback`); image `ghcr.io/agentfeedback/agentfeedback`; default
  database `/data/agentfeedback.db`; metrics renamed `agentfeedback_*`
  (`agentfeedback_submissions_unprocessed`, `agentfeedback_db_bytes`,
  `agentfeedback_sqlite_busy_total`).
- **Deploy**: stack directory `infra/agentfeedback`, compose service
  `agentfeedback`, volume `agentfeedback-data`, server directory
  `~/agentfeedback`, variables `AGENTFEEDBACK_IMAGE` and
  `AGENTFEEDBACK_BIND_ADDRESS`.
- **Removed**: the 1.x PostgreSQL export script and migration and uninstall
  docs, and the `docs/agent-usage.md` redirect.
- CI lowercases the image name (the org name has capitals).

## v2.2.1 — Documentation sweep

Docs and skill documentation only; the service and scripts behave as in
v2.2.0.

- **Skill `agent-feedback-triage` 2.1**: the optional clustering contract
  moved to `reference/clustering.md`, read only when clustering is used, and
  repeated rules were collapsed: SKILL.md is a third shorter per invocation.
  Install the whole directory, including `reference/`. Resolutions now start
  with their verdict (`FIXED`, `INVALID`, `DUPLICATE-OF-<id>`). Fixed: the
  close-out commands' path to the sibling skill, digest exit codes, the
  meaning of `unchanged` versus `updated`, and cluster.py's statuses, reasons
  and exit codes.
- **Skill `agent-feedback`** (contract unchanged): corrected the `machine`
  override, `--sweep` age/lock/outcome behaviour, backlog-warning scope, TSV
  column 5, `slots` usage, 1xx/3xx outcomes, export digest tools, and added
  `AGENT_FEEDBACK_TRIAGE_ROOTS`.
- [api.md](api.md) states its version (1.1), the non-JSON responses and the
  `include=payload` row shape.
- [operate.md](operate.md): the retention purge runs as root, backups create
  their directory, `openssl` and `DEPLOY_DIR` are documented, and uninstall
  covers the triage variables.
- `CLAUDE.md` routes to [develop.md](develop.md), the single home for
  commands, rules and gates; the README quickstart checks out the latest tag
  and keeps an existing key.

## v2.2.0 — Triage skill renamed; batched, measured clustering

- **Skill `feedback-triage` is now `agent-feedback-triage` 2.0.** The
  invocation name and the install directory change: update installers,
  links and prompts that name `skills/feedback-triage`, and remove the old
  copy ([operate.md](operate.md#uninstall) lists both names). Digests move to
  `${TMPDIR:-/tmp}/agent-feedback-triage/`. No service/API or storage changes.
- The triage skill is direct invocation only: `disable-model-invocation: true`
  (Claude Code) and a description that forbids loading it from phrasing
  about the queue. Invoke it as `/agent-feedback-triage` or by name.
- Docs: install guidance for the triage skill, the rule to re-measure after
  any `cluster.py` prompt, threshold or batching change, and the eval
  script's disclosure boundary in [security.md](security.md).
- `cluster.py` batches comparisons: up to 8 reports per request, every pair
  asked once over a shared state, so 24 reports need 15 requests instead of
  276 (which exceeded the old cap and skipped advice). One request per pair
  remains the fallback for batch sizes below 4 and for chunks over the
  model's token budget; a pair too large alone is marked unassessed without
  a request. One invalid answer in a batch marks only that pair.
- **`--max-pairs` is replaced by `--max-requests`** (default 200); the old
  flag is rejected because its unit changed. New `--batch-size` (default 8).
- Probability sums tolerate the model's per-option two-decimal rounding.
- Calibration recorded in the skill: on 112 labelled pairs, no different
  pair was grouped at the 0.8 threshold; consent, dry-run, key handling and
  complete-link grouping are unchanged. `scripts/eval-cluster.py` repeats
  the measurement and refuses to send any report whose exact remote was not
  approved with `--allow-repo`.
- Go module dependency updates (indirect only).

## v2.1.0 — Optional advisory triage clustering

- **Skill `feedback-triage` 1.1** adds a Python 3.9+ helper that compares
  report mechanisms through TypeSafe and suggests clusters. The manual
  workflow remains the default; no service/API or storage changes.
- Disclosure requires explicit approval for each exact repository identity.
  Preview is local; unapproved or unidentified reports are never sent.
  No credential is copied, no queue item is marked, and no report is removed.
- Advice retains source IDs, the digest hash and individual probabilities.
  Groups require agreement for every member pair, not transitive matches.
  Uncertain, unavailable and invalid answers fall back to manual triage;
  bounded requests preserve partial results without dropping reports.

## v2.0.0 — SQLite, generic events, triage skill

Breaking operational contract, compatible API.

- **Storage**: PostgreSQL replaced by SQLite (`modernc.org/sqlite`, pure Go).
  One container, one named volume, no database service. Physical backups via
  `feedback backup`, logical via `GET /api/v1/export`; restore and migration
  via `feedback import` (all-or-nothing, header/count/digest verified, hashes
  recomputed, `--family` filter, ids never reused). See
  [operate.md](operate.md#restore-and-migration)
  for the 1.x migration procedure; `scripts/export-v1-postgres.sh` produces
  the import file from a 1.x deployment with ids, timestamps, processing state
  and hashes preserved.
- **API 1.1** ([api.md](api.md)): additive. `family` and `payload_hash` on
  every record, `POST /api/v1/events`, `resolution` when marking processed,
  keyset pagination (`before_id`, `has_more`, `next_before_id`, `total`),
  `include=payload`, `GET /api/v1/export`, six-digit timestamps. Every 1.0
  request and response field is unchanged.
- **Service**: stdlib `net/http`, Prometheus with bounded labels plus backlog,
  database size and busy counters. OpenTelemetry, chi, sqlc and the
  PostgreSQL-specific configuration are gone. `GRACEFUL_SHUTDOWN_TIMEOUT`
  default is now 30s; the fixed 5s drain sleep is removed. Payloads are
  returned byte-exact (large integers no longer round).
- **Deployment**: `scripts/deploy.sh <sha>` installs the compose file,
  preserves the host `.env`, pins the image there and runs `docker compose
  pull && up`. Requires the GHCR package to be pullable by the host. The
  `crane` streaming path is gone.
- **Skill `agent-feedback` 3.0**: new `submit-event.sh`; `process.sh list`
  pages through the whole queue and takes `--include-processed` (the old
  `--all` errors), `done` takes `--resolution`; `query.sh export`;
  `submit-review.sh --sweep` scans `REVIEW_LOG_DIR` and
  `AGENT_FEEDBACK_REVIEW_DIRS` only (set the latter where hardcoded cache
  paths were relied on). Fixes: spool persistence is verified (an unwritable
  spool now fails loudly instead of reporting `spooled`), review receipts are
  validated before a run is marked submitted, large prose no longer passes
  through argv, `--dry-run` applies the server's validation, rejected spool
  files are kept 30 days as documented.
- **New skill `feedback-triage` 1.0**: end-to-end queue processing with one
  consolidated interview. `scripts/digest.sh` pulls and groups the open queue.
- **Docs** rewritten for agents first: README route table, `api.md`,
  `operate.md`, `develop.md`, `security.md`. Template-era documents removed;
  `docs/agent-usage.md` redirects to `api.md`.

Upgrade: follow the migration procedure; the 1.x PostgreSQL volume is not
read by 2.x. Producers need skill 3.0 only for the new features; 2.1 clients
keep working against the 2.0.0 service.

## v1.0.1 — Public documentation and release workflow

Compact README with dedicated guides, guarded release tooling, updated Go
modules, toolchain, container bases and CI actions. No API or migration
changes.

## v1.0.0

Initial stable API and companion client release.
