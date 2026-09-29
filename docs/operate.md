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
bash ../../skills/agentfeedback/scripts/submit-friction.sh --category test --summary "local smoke test" --model manual
bash ../../skills/agentfeedback/scripts/process.sh list
```

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

**Physical backup** (fastest restore, byte-identical database):

```bash
cd ~/agentfeedback
docker compose exec agentfeedback /opt/agentfeedback backup /data/backup-$(date -u +%Y%m%dT%H%M%SZ).db
mkdir -p backups
docker compose cp agentfeedback:/data/backup-<stamp>.db ./backups/
docker compose exec agentfeedback rm /data/backup-<stamp>.db
```

`agentfeedback backup` uses SQLite's `VACUUM INTO`, which is safe on a live
database in WAL mode. Never copy `agentfeedback.db` from the volume while the
service runs; the `-wal` file holds committed data the main file lacks.

**Logical backup** (portable, inspectable, the migration format):

```bash
bash skills/agentfeedback/scripts/query.sh export > feedback-$(date -u +%Y%m%d).jsonl
```

The client verifies the export header, record count and digest. Store
backups off-host and restrict them: they contain everything agents reported.

## Restore and migration

**Restore a physical backup**: stop the stack, replace `/data/agentfeedback.db` in
the volume (remove any `-wal`/`-shm` files beside it), start the stack, check
`/ready`.

**Restore or import a logical export** into an empty database:

```bash
docker compose stop agentfeedback
docker compose run --rm -v "$PWD/feedback.jsonl:/import.jsonl:ro" agentfeedback import /import.jsonl
docker compose start agentfeedback
```

`agentfeedback import` runs in one transaction and verifies the header, record
count and digest before committing anything. It preserves `id`, `created_at`,
`processed_at`, `resolution` and `payload_hash` (recomputing and checking each
hash; `--trust-hashes` skips the check), advances the id sequence past the
highest id in the stream (skipped rows included, so archived ids are never
reused), and refuses a non-empty database (`--allow-nonempty` to merge) or a
filtered export (`--allow-partial`). `--family friction` imports only that
family from a full export; `--reserve-ids-through N` raises the sequence
further when needed. Flags go before the file name.

## Upgrade

`bash scripts/deploy.sh <new-commit-sha>`. Schema changes apply automatically
at startup, forward only; a binary older than the database's schema refuses
to start rather than corrupting it. Take a physical backup first. Rolling back
the image does not roll back the schema.

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
  `submissions_created_total` by family and outcome;
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
