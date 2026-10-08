# AgentFeedback

Asked to install this? Read [`AGENT-INSTALL.md`](AGENT-INSTALL.md) (client) or [`AGENT-INSTALL-STACK.md`](AGENT-INSTALL-STACK.md) (server and client). Both describe release 4.0.0 and later.

Paste one of these into a coding agent:

```
Install the AgentFeedback client from https://github.com/AgentFeedback/agentfeedback and set it up for this harness.
```

```
Install the AgentFeedback stack from https://github.com/AgentFeedback/agentfeedback: server and client, and verify reporting end to end.
```

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
| **Three skills** | `agentfeedback`: submit and read, installed for every harness on every machine. `agentfeedback-triage`: process the queue, only when the user invokes it (`/agentfeedback-triage`). `agentfeedback-docs`: the reference docs, schemas and OpenAPI document, generated, for agents that integrate with or operate the service | [`skills/`](skills/) |

## Start here

| You want to | Read |
|---|---|
| Have an agent install the client, or the server and the client | [`AGENT-INSTALL.md`](AGENT-INSTALL.md), [`AGENT-INSTALL-STACK.md`](AGENT-INSTALL-STACK.md) (from release 4.0.0) |
| Point an agent at every machine-readable document (`llms.txt`) | [`llms.txt`](llms.txt) |
| Install the skill and file feedback from an agent | [`skills/agentfeedback` at v3.0.0](https://github.com/AgentFeedback/agentfeedback/tree/v3.0.0/skills/agentfeedback) (the bash client of the running v3.0.0 release) |
| Triage the queue | [`skills/agentfeedback-triage/SKILL.md`](skills/agentfeedback-triage/SKILL.md) |
| Turn past coding-agent sessions on this machine into feedback (`agentfeedback sessions`) | [`docs/sessions.md`](docs/sessions.md) |
| Call the API directly | [`docs/api.md`](docs/api.md) |
| Give an agent the reference docs, schemas and OpenAPI document (integrate, operate) | [`skills/agentfeedback-docs/SKILL.md`](skills/agentfeedback-docs/SKILL.md) (installed by `agentfeedback install --docs`) |
| Run, deploy, back up, migrate | [`docs/operate.md`](docs/operate.md) |
| Uninstall the skills or the service | [`docs/operate.md#uninstall`](docs/operate.md#uninstall) |
| Change the code | [`AGENTS.md`](AGENTS.md) then [`docs/develop.md`](docs/develop.md) |
| Trust boundary and credentials | [`docs/security.md`](docs/security.md) |
| Versions and upgrade notes | [`docs/releases.md`](docs/releases.md) |

## Five-minute local run

```bash
git clone https://github.com/AgentFeedback/agentfeedback.git && cd agentfeedback
git checkout v3.0.0   # the running release; its bash client is skills/agentfeedback/scripts/
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
