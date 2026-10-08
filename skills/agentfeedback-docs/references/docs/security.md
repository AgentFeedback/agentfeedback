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
  check is off because the key stands in for it. Turning that check off
  (`DisableLocalhostProtection` in the MCP SDK) is acceptable only on a
  handler behind the API key; a keyless loopback listener keeps it, or
  validates `Host` and `Origin` itself as `agentfeedback ui` does. `/mcp/{project}` is a
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
  others means running `serve` with a key. The file inbox
  (`inbox/` in the data directory, `0700`) follows the same rule: a file
  there is filed by the next command, so only that user may write it; the
  client never follows a symlink out of it and never reads a file over the
  create limit.
- `agentfeedback ui` serves a read-only page over the queue on a loopback
  address only (`--addr` refuses any other). It is its own handler, never the
  API or the MCP handler: no `/api/v1` or `/mcp` route is reachable through
  it, and every method but `GET` and `HEAD` is refused. A request whose `Host`
  is not the bound address (or `localhost` on the same port) is refused, so a
  web page that rebinds its own DNS name to `127.0.0.1` reads nothing; so is
  a request with any foreign `Origin`, and no CORS header is ever sent. The
  path carries a random token made at each launch, so another local process
  or page that guesses the port still gets `404`, and no refused request's
  response carries the token; the URL is printed once,
  and anyone who has it can read the queue until the process exits. Every
  response is `Cache-Control: no-store` with a Content Security Policy that
  allows no remote source, stored text is HTML-escaped, and the page loads
  no external asset. With `--server` it reads that server with the
  configured key, which stays in the process.

- What the client touches on disk: it owns the data directory
  (`${XDG_DATA_HOME:-~/.local/share}/agentfeedback/`: the database, the spool,
  the inbox; `0700`, files `0600`). The harness session stores (Claude Code's `${CLAUDE_CONFIG_DIR:-~/.claude}/projects/`)
  are read only by `agentfeedback sessions`, `submit --context-from` and the
  stdio `sessions_*` tools, on demand and never written; the `[collect]`
  rules are applied per session before a log is opened, and a digest is
  bounded and scrubbed before it is shown. Only the watermark rows and what
  you file are stored, in the data-directory database, in both modes; nothing
  of a session is sent to a server unless filed. A digest reaches the
  provider of the model that reads it ([sessions.md](sessions.md#provider-boundary)).

## What gets stored

Friction context collected by the client (event time, working directory,
repository root and remote, branch, commit, dirty flag, OS, architecture,
session id, agent id, effort, harness profile name, client version) can reveal usernames,
private repository names and internal hostnames. The client strips
credentials, query strings and fragments from remote URLs. Preview with
`--dry-run`. Review outputs and prompts are sent only with
`--include-outputs`.

`submit <kind> --scrub`, and any submission whose `context.origin` is
`session-scan`, replaces known secret formats in every string value of the
body (summary, payload, context, any other member) with
`[REDACTED:<class>]` before checking or sending; member names are never
changed, and a non-empty string under a secret-named member (`password`,
`token`, `api_key`, `client_secret` and similar) is redacted whole. The
outcome line carries one `scrubbed` warning per member changed.
`submit review <run_dir>` and `submit review --sweep` do not scrub, and spool
entries queued before are sent as they were. The server does the same at
ingest with `INGEST_SCRUB=on` (default `off`), before decoding, silently: the
stored text carries the markers, the response has no warning, and the server
log records the counts per class, never the text. The classes, in the order
they apply: `private_key`, `jwt`, `aws_access_key`, `gcp_api_key`,
`github_token`, `gitlab_token`, `slack_token`, `stripe_key`,
`anthropic_key`, `openai_key`, `bearer_token`, `url_credentials`, `env_line`
(`NAME=value` lines whose name has a segment ending in KEY, TOKEN, SECRET,
PASSWORD, PASSWD, CREDENTIAL(S), AUTH or PRIVATE) and `assignment`
(`password=`, `token:` and similar, and secret-named members). This is
defence in depth: it detects these formats and no others, not secrets in
prose, and never member names. `import` does not scrub. Otherwise the server
stores what it receives and redacts nothing.

The harness hooks (`agentfeedback hook`) read only the payload the harness
passes on stdin, never the transcript or any other file it names, call no
model and send nothing about the session. They keep counters per session in
the cache directory (`0700` directories, `0600` files, no command text,
deleted after 7 days); the short note they may show quotes the failed
command, cut to 80 bytes after the same secret-format replacement as
`--scrub`. A note suggests a friction report and never blocks or redirects
the agent: no hook denies a tool call, continues a stopped turn or rewrites
a tool result. The client log keeps a submission's `context.session_id`, and
no other context; a friction report with one also leaves an empty marker
named by a digest of the session id, so the hook can tell a session already
filed a report without reading the log.

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
[operate.md](operate.md#retention)). Producers keep unsent payloads in the
same data directory, `~/.local/share/agentfeedback/spool/`, until delivered
or aged out; the client creates that directory `0700` and its files `0600`,
and `agentfeedback doctor` reports a path other users can reach (see
[operate.md](operate.md#data-directory)).

## Reporting a vulnerability

Do not post keys, telemetry or exploit details in public issues. Contact the
maintainer privately first.
