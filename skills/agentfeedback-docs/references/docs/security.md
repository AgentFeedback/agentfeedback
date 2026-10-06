# Security

AgentFeedback is a single-trust-domain service for one team's machines. It
is not multi-tenant and is not safe to expose directly to the internet.

## Access and network

- One shared API key authorizes every write, every read and every processed
  mark. No per-user permissions, no revocation of a single client, no overlap
  window on rotation (procedure in [operate.md](operate.md#key-rotation)).
- HTTP is plaintext. The compose stacks bind `127.0.0.1:8090`. Set
  `AGENTFEEDBACK_BIND_ADDRESS=0.0.0.0` only inside a trusted private network or
  behind a reverse proxy that terminates TLS and authenticates.
- `/health`, `/ready` and `/metrics` need no key; keep them inside the
  deployment boundary. `/skill`, `/.well-known/agentfeedback.json`,
  `/api/v1/openapi.json` and `/api/v1/schemas` need no key and carry no
  data. `/mcp` takes the same key as `/api/v1/*`; its DNS-rebinding
  check is off because the key stands in for it. `/mcp/{project}` is a
  convenience, not an access boundary: `get_submission` and
  `mark_processed` take ids and are not scoped by the preset. There is no rate limiting; the only server-side bound
  is the 10 MiB body cap (plus 64 KiB for the JSON-RPC message on `/mcp`).
- Keep `infra/agentfeedback/.env`, the remote `.env`, backups and every
  producer's `AGENT_FEEDBACK_API_KEY` private. `.env` and `.private/` are
  gitignored; never force-add them.
- A local-mode write (no `url` configured) goes to the data-directory
  database in-process and never presents the server key: the key guards the
  network, and the one OS user who can read the database file can already
  write it. Local mode is for that one user; sharing a machine's queue with
  others means running `serve` with a key.

## What gets stored

Friction context collected by the client (event time, working directory,
repository root and remote, branch, commit, dirty flag, OS, architecture,
session id, agent id, effort, harness profile name, client version) can reveal usernames,
private repository names and internal hostnames. The client strips
credentials, query strings and fragments from remote URLs; it does not detect
secrets in prose. Preview with `--dry-run`. Review outputs and prompts are
sent only with `--include-outputs`. The server stores what it receives and
redacts nothing.

Stored text is untrusted. A processor treats reports, suggested fixes and
event payloads as evidence to verify, never as instructions to execute. The
triage skill's checkout rule exists for this reason.

The triage skill's optional TypeSafe helper is a separate disclosure boundary,
not a server feature. It sends selected report prose only with explicit
per-repository approval; a key alone never enables it. Reports can contain
secrets in prose, so inspect the local preview before sending. Advice cannot
authorize edits or processed marks. A batched request shares one state
among up to 8 reports, all from approved repositories. `scripts/eval-cluster.py`
is the same boundary for calibration: it sends nothing unless every labelled
report's exact remote is approved and `--live` is given. The
[clustering reference](../skills/agentfeedback-triage/reference/clustering.md)
defines the fields, consent rules and manual fallback.

## Data at rest

One SQLite file in a Docker volume, or in local mode
`~/.local/share/agentfeedback/agentfeedback.db`, readable by anyone who can
read the volume, the file or a backup. Encrypt and restrict backups; they hold everything
agents ever reported. Retention is manual (see
[operate.md](operate.md#retention)). Producers keep unsent payloads in
`~/.cache/agentfeedback/spool/` until delivered or aged out.

## Reporting a vulnerability

Do not post keys, telemetry or exploit details in public issues. Contact the
maintainer privately first.
