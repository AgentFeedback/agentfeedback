# AgentFeedback

An inbox for feedback from AI coding agents. Agents on any machine, in any
harness, report what slowed them down; later an agent triages the queue with a
human and fixes the causes. Go, SQLite, one container, one API key.

This is the open-source, self-hostable AgentFeedback
([agentfeedback.dev](https://agentfeedback.dev)). A hosted version runs at
[agentfeedback.io](https://agentfeedback.io); it speaks the same API, so the
skills below work against either.

Two parts:

| Part | What it is | Where |
|---|---|---|
| **The service** | HTTP API that stores write-once submissions (frictions, review runs, events) and a processed mark | this repository, one binary |
| **Two skills** | `agentfeedback`: submit and read, installed for every harness on every machine. `agentfeedback-triage`: process the queue, only when the user invokes it (`/agentfeedback-triage`) | [`skills/`](skills/) |

## Start here

| You want to | Read |
|---|---|
| Install the skill and file feedback from an agent | [`skills/agentfeedback/scripts/README.md`](skills/agentfeedback/scripts/README.md) (the bash client of the current release) |
| Triage the queue | [`skills/agentfeedback-triage/SKILL.md`](skills/agentfeedback-triage/SKILL.md) |
| Call the API directly | [`docs/api.md`](docs/api.md) |
| Run, deploy, back up, migrate | [`docs/operate.md`](docs/operate.md) |
| Uninstall the skills or the service | [`docs/operate.md#uninstall`](docs/operate.md#uninstall) |
| Change the code | [`AGENTS.md`](AGENTS.md) then [`docs/develop.md`](docs/develop.md) |
| Trust boundary and credentials | [`docs/security.md`](docs/security.md) |
| Versions and upgrade notes | [`docs/releases.md`](docs/releases.md) |

## Five-minute local run

```bash
git clone https://github.com/AgentFeedback/agentfeedback.git && cd agentfeedback
git checkout "$(git describe --tags --abbrev=0 --exclude '*-*')"   # latest stable release, not a pre-release
cd infra/agentfeedback && test ! -e .env && umask 077 && printf 'API_KEY=%s\n' "$(openssl rand -hex 32)" > .env
docker compose up -d --build --wait
export AGENT_FEEDBACK_URL=http://127.0.0.1:8090 AGENT_FEEDBACK_API_KEY=$(sed -n 's/^API_KEY=//p' .env)
bash ../../skills/agentfeedback/scripts/submit-friction.sh --category test --summary "hello" --model manual
bash ../../skills/agentfeedback/scripts/process.sh list
```

Needs Docker with Compose, `curl`, `jq`, `openssl`. The service binds
`127.0.0.1:8090`; the database lives in a named volume.

## Uninstall

The skills and the service are removed independently;
back up first. Commands in [`docs/operate.md#uninstall`](docs/operate.md#uninstall).

## What it is not

Not a review runner, benchmark, dashboard or automated fixer. It stores what
agents report and lets a processor work through it. Records are never
overwritten: a correction is a new submission.

MIT licensed. Current stable release: [v3.0.0](https://github.com/AgentFeedback/agentfeedback/releases/tag/v3.0.0).
