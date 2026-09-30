# Operate AgentFeedback

Install, run, back up, upgrade and migrate the service. One container, one
SQLite file, one API key. Every step is a command an agent can run.

Contents: [Run locally](#run-locally) · [Deploy to a host](#deploy-to-a-host) ·
[Configuration](#configuration) · [Backups](#backups) ·
[Restore and migration](#restore-and-migration) · [Upgrade](#upgrade) ·
[Key rotation](#key-rotation) · [Retention](#retention) · [Monitoring](#monitoring) ·
[Uninstall](#uninstall)

## Run locally

Needs Docker with Compose. From the repository root:

```bash
cd infra/agentfeedback
test -e .env || (umask 077 && printf 'API_KEY=%s\n' "$(openssl rand -hex 32)" > .env)   # keeps an existing key
docker compose up -d --build --wait
curl --fail --silent --show-error http://127.0.0.1:8090/ready
```

The service listens on `127.0.0.1:8090` and stores its database in the named
volume `agentfeedback-data` (`/data/agentfeedback.db` in the container). First request:

```bash
export AGENT_FEEDBACK_URL=http://127.0.0.1:8090
export AGENT_FEEDBACK_API_KEY=$(sed -n 's/^API_KEY=//p' .env)
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"friction","summary":"local smoke test","payload":{"category":"test"}}' \
  "$AGENT_FEEDBACK_URL/api/v1/submissions"
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" "$AGENT_FEEDBACK_URL/api/v1/submissions?limit=5"
```

The service answers the v1 contract, [openapi.yaml](openapi.yaml); `bash scripts/e2e.sh "$AGENT_FEEDBACK_API_KEY" "$AGENT_FEEDBACK_URL"` from the repository root runs the whole contract suite against it (it creates rows).

Tear down with `docker compose down -v` (deletes the database volume; `.env`
stays).

Without Docker: `go build -o bin/agentfeedback ./cmd/agentfeedback && API_KEY=dev DATABASE_PATH=/tmp/agentfeedback.db HTTP_LISTEN_ADDR=127.0.0.1:8090 bin/agentfeedback serve`.

## Deploy to a host

Prerequisites on the host: Docker with Compose, `curl`, `openssl`, SSH access. The
image is published at release time ([releases.md](releases.md)) to
`ghcr.io/agentfeedback/agentfeedback` tagged with the version, the commit SHA
and `latest`. The package must be publicly pullable (GitHub package
settings) or the host must be logged in to GHCR.

```bash
DEPLOY_REMOTE=user@host bash scripts/deploy.sh <commit-sha>
```

`scripts/deploy.sh`:

1. copies `infra/agentfeedback/docker-compose.deploy.yml` to
   `~/agentfeedback/docker-compose.yml` on the host;
2. creates `~/agentfeedback/.env` with a generated `API_KEY` on first deploy
   and preserves it afterwards;
3. writes `AGENTFEEDBACK_IMAGE=ghcr.io/agentfeedback/agentfeedback:<commit-sha>` into that
   `.env` so the pinned image survives later `docker compose up` calls;
4. runs `docker compose pull && docker compose up -d --wait` and checks `/ready`.

Always deploy a commit SHA or a version tag, never `latest`, so the host pins
what it runs. `DEPLOY_REMOTE`, `DEPLOY_IMAGE` and `DEPLOY_DIR`
(remote directory, default `~/agentfeedback`, which the sections below
assume) can live in the gitignored `.private/deploy.env`.

The stack binds `127.0.0.1:8090` on the host. To serve other machines, set
`AGENTFEEDBACK_BIND_ADDRESS=0.0.0.0` in the host `.env` only inside a trusted
network or behind TLS; see [security.md](security.md).

## Configuration

Environment variables read by the binary:

| Variable | Default | Meaning |
|---|---|---|
| `API_KEY` | required | the shared key every client sends |
| `DATABASE_PATH` | `/data/agentfeedback.db` | SQLite file; its directory must be writable |
| `HTTP_LISTEN_ADDR` | `0.0.0.0:8080` | inside the container; Compose maps it to `127.0.0.1:8090` |
| `GRACEFUL_SHUTDOWN_TIMEOUT` | `30s` | drain time for in-flight requests |
| `SERVICE_VERSION` | build default | reported in logs |
| `LOG_LEVEL` | `info` | `debug` or `info` |

Compose-level variables (`infra/agentfeedback/.env`): `API_KEY`,
`AGENTFEEDBACK_IMAGE` (deploy stack only), `AGENTFEEDBACK_BIND_ADDRESS`.

## Backups

Two complementary forms.

**Physical backup** (fastest restore, a complete database file):

```bash
cd ~/agentfeedback
docker compose exec agentfeedback /opt/agentfeedback backup /data/backup-$(date -u +%Y%m%dT%H%M%SZ).db
mkdir -p backups
docker compose cp agentfeedback:/data/backup-<stamp>.db ./backups/
docker compose exec agentfeedback rm /data/backup-<stamp>.db
```

`agentfeedback backup <dest.db>` uses SQLite's `VACUUM INTO`, which is safe on
a live database in WAL mode, so the service keeps running. It refuses an
existing destination and a `DATABASE_PATH` that does not exist. Never copy
`agentfeedback.db` from the volume while the service runs; the `-wal` file
holds committed data the main file lacks.

**Logical backup** (portable, inspectable, the migration format):

```bash
curl -sS --fail -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/export" > feedback-$(date -u +%Y%m%d).ndjson
tail -n 1 feedback-*.ndjson   # the trailer: {"export_complete":true,"count":…,"sha256":…}
```

The export is format 2: a header line, one line per record (tombstones
included), and a trailer with the record count and the SHA-256 over the record
lines. A file without the trailer is incomplete. Store backups off-host and
restrict them: they contain everything agents reported.

## Restore and migration

**Restore a physical backup**: stop the stack, replace `/data/agentfeedback.db` in
the volume (remove any `-wal`/`-shm` files beside it), start the stack, check
`/ready`.

**Restore a logical export** (format 2) into the service's database:

```bash
docker compose stop agentfeedback
docker compose run --rm -v "$PWD/feedback.ndjson:/import.ndjson:ro" agentfeedback import --dry-run /import.ndjson
docker compose run --rm -v "$PWD/feedback.ndjson:/import.ndjson:ro" agentfeedback import /import.ndjson
docker compose start agentfeedback
```

`agentfeedback import [--dry-run] <file>` verifies the whole file (header,
record count, digest, every record, each `content_hash` recomputed) before it
writes anything, then restores it in one transaction. Records keep their `id`,
`uid`, `content_hash`, payload bytes, timestamps and processing fields, and
tombstones stay tombstones; the id sequence moves past the highest id in the
file, so a restored id is never handed out again. A record whose `uid` is
already stored is skipped whatever the two contain (a mark or redaction in
the file does not reach the stored row), so running the same restore twice
changes nothing.
A record whose `id`, or whose `(kind, key)` with different content, belongs to
another stored record is skipped and listed under `conflicts` (`id_taken`,
`key_mismatch`); conflicts never fail the restore. It prints one JSON line
(`imported`, `skipped`, `conflicts`, `warnings`, `first_id`, `last_id`,
`dry_run`); `--dry-run` reports the same counts and writes nothing; a
rejected file writes nothing either, and a dry run against a `DATABASE_PATH`
that does not exist reports a restore into an empty database without creating
it. Stop the service first, as above: a restore, dry run included, holds the
database's only writer until it finishes. The file is read whole into memory,
so the host needs a few times its size free. Flags go before the file name.

**Move to another server** with `agentfeedback migrate`, run from any
machine whose client is configured with the source's URL and key:

```bash
printf '%s' "$TARGET_KEY" | agentfeedback migrate --to https://target --to-key-from-stdin \
  [--kind K] [--include-kind K] [--since T] [--limit N] [--dry-run]
```

It pages the source's export and writes each chunk to the target's
`POST /api/v1/import` ([openapi.yaml](openapi.yaml)), which keeps uid,
content_hash, the timestamps and the processing fields and assigns new ids.
The target skips uids it already holds, so a re-run only adds what is
missing, and marks or redactions made on the source after a record moved do
not propagate. `install-check` rows stay behind unless `--kind` or
`--include-kind` names that kind. A record whose (kind, key) the target holds
under another uid is printed as one `conflict` line and skipped. The run
stops at the first failure and says how many records landed. `--dry-run`
sends nothing: it prints the count per kind, the first record of each, and
the source tombstones the target still holds unredacted; redact those on the
target by hand. `--to cloud` targets the hosted service at
`https://api.agentfeedback.io`.

## Upgrade

`bash scripts/deploy.sh <new-commit-sha>`. Schema changes apply automatically
at startup, forward only; a binary older than the database's schema refuses
to start rather than corrupting it. Take a physical backup first. Rolling back
the image does not roll back the schema.

From v3 to v4 the database starts over: a v4 binary refuses a v3 database
at startup and names the path. Keep the old database with the image that wrote
it, point `DATABASE_PATH` at a new file (a new volume), and start empty. A v3
export is not format 2, so `agentfeedback import` does not take it.

## Key rotation

No overlap window. Edit `API_KEY` in the host `.env`, `docker compose up -d`,
then update `AGENT_FEEDBACK_API_KEY` on every producer. Spooled payloads on
producers retry with the new key on their next call.

## Retention

Nothing is deleted automatically. `processed` means acted on, not removed.
To purge, export first, then delete inside the container with an explicit
predicate, as root because the image runs unprivileged (the `sqlite` package
lasts until the container is recreated), for example rows processed more than
a year ago:

```bash
docker compose exec -u root agentfeedback sh -c 'apk add --no-cache sqlite >/dev/null && sqlite3 /data/agentfeedback.db "DELETE FROM submissions WHERE processed_at < (strftime(\"%s\",\"now\")-31536000)*1000000"'
```

Client spools live in `~/.cache/agentfeedback/spool/` on each producer and
age out on their own.

## Monitoring

- `GET /health`: process is up. `GET /ready`: database answers and schema is
  current; `503` while shutting down.
- `GET /metrics` (Prometheus): `http_requests_total` and
  `http_request_duration_seconds` by route, method and code;
  `submissions_created_total` by outcome;
  `agentfeedback_submissions_unprocessed` (the backlog);
  `agentfeedback_db_bytes` (database plus WAL size); `agentfeedback_sqlite_busy_total`
  (writes that waited out the busy timeout, should stay at zero).
- Logs are JSON on stdout: `docker compose logs -f agentfeedback`.

## Uninstall

Two independent parts: the skills on each machine and the service. Take a backup before
removing any service; the data is gone with the volume.

### Skills, on every machine that has them

1. Find the installed copies (`agentfeedback`, `agentfeedback-triage`) in every harness skills directory you
   use, e.g. `ls -la ~/.claude/skills | grep feedback`. Entries may be symlinks into
   a shared checkout; remove the links, then the checkout if nothing else uses it.
2. Flush or discard unsent payloads first: `bash <skill-dir>/scripts/query.sh --flush --limit 1`
   sends whatever is spooled; or delete `~/.cache/agentfeedback/` to drop it.
3. Remove the directories or links, then `rm -rf ~/.cache/agentfeedback`.
4. Remove `AGENT_FEEDBACK_URL`, `AGENT_FEEDBACK_API_KEY`, `AGENT_FEEDBACK_MACHINE`,
   `AGENT_FEEDBACK_MODEL`, `AGENT_FEEDBACK_HARNESS`, `AGENT_FEEDBACK_SESSION_ID`
   `AGENT_FEEDBACK_REVIEW_DIRS` and `AGENT_FEEDBACK_TRIAGE_ROOTS` (plus
   `TYPESAFE_API_KEY` if only triage used it) from shell profiles (`grep -n AGENT_FEEDBACK ~/.zshenv ~/.zshrc ~/.bashrc ~/.profile 2>/dev/null`).
5. Remove any directive in your agent system prompt that tells agents to
   submit friction, and any hook in a review runner that calls `submit-review.sh`.

### The service

On the host, in the directory holding `docker-compose.yml` (default `~/agentfeedback`):

```bash
cd ~/agentfeedback
docker compose exec agentfeedback /opt/agentfeedback backup /data/final.db && docker compose cp agentfeedback:/data/final.db ./final-backup.db   # keep a copy elsewhere
docker compose down -v          # stops the container and deletes the agentfeedback-data volume
docker image rm $(sed -n 's/^AGENTFEEDBACK_IMAGE=//p' .env)
cd ~ && rm -rf ~/agentfeedback  # compose file, .env with the API key, local backups
```

Skip `-v` and the last line to keep the data for a later reinstall. Also
remove any reverse-proxy or firewall rule that exposed port 8090, and any
monitoring scrape of `/metrics`.

### Verify

`curl -s -o /dev/null -w '%{http_code}\n' http://<host>:8090/health` must fail
to connect; `docker ps -a | grep agentfeedback` and `docker volume ls | grep
feedback` must be empty; a fresh shell must have no `AGENT_FEEDBACK_*`
variables.
