# AgentFeedback: working in this repo

Asked to install this? Read [`AGENT-INSTALL.md`](AGENT-INSTALL.md) (client) or [`AGENT-INSTALL-STACK.md`](AGENT-INSTALL-STACK.md) (server and client). Both describe release 4.0.0 and later.

Go + SQLite HTTP service that stores write-once feedback from AI coding agents
(frictions, review runs, events) plus three skills: `agentfeedback`
(submit/read on the `agentfeedback` CLI, installed everywhere), `agentfeedback-triage` (process
the queue on the `agentfeedback` CLI plus an optional Python helper;
user-invoked only) and `agentfeedback-docs` (generated reference docs,
schemas and OpenAPI document for integrators and operators;
`skill render docs`).
Harness-neutral: this is the only agent instructions file (no `CLAUDE.md` or
`GEMINI.md`); Claude Code loads it when no `CLAUDE.md` exists.

Design records live in a private location; if `.kitchen/` is present in this
checkout, read `.kitchen/AGENTS.md` first.

This file is for agents **changing this repo**. To **use** AgentFeedback,
read the [README](README.md): the binary, `agentfeedback init` and the
generated [`agentfeedback` skill](skills/agentfeedback/SKILL.md), or
[`docs/api.md`](docs/api.md) for raw HTTP. `main` describes release 4.0.0
and later; the documents of an earlier release are at its tag.

## Route by task

| Task | Go to |
|---|---|
| Change service or client code | [`docs/develop.md`](docs/develop.md): layout, commands, invisible rules, verification |
| Change the API | [`docs/openapi.yaml`](docs/openapi.yaml), `schemas/` and `conformance/` are the contract, [`docs/api.md`](docs/api.md) its narrative; six artifacts move in one commit (see develop.md) |
| Change a skill or its scripts | `internal/skillgen/source/` or `skills/<name>/scripts/`, then the skill gates in [`docs/develop.md`](docs/develop.md#verification-before-you-are-done); [`docs/recipes/http-curl.md`](docs/recipes/http-curl.md) and [`docs/recipes/http-powershell.md`](docs/recipes/http-powershell.md) are generated from the same source |
| Change the session readers | [`docs/sessions.md`](docs/sessions.md) |
| Change the install playbooks or the agent index | [`AGENT-INSTALL.md`](AGENT-INSTALL.md), [`AGENT-INSTALL-STACK.md`](AGENT-INSTALL-STACK.md), [`llms.txt`](llms.txt); the artifact rule in develop.md |
| Change what the client collects, reads or writes | [`docs/security.md`](docs/security.md) states it; keep it true |
| Change the integrations list or a hub artifact | `integrations.json`, `schemas/integrations.v1.json`, `integrations/`, [`docs/integrations/clawhub.md`](docs/integrations/clawhub.md), [`docs/integrations/hermes.md`](docs/integrations/hermes.md), [`docs/integrations/nanoclaw.md`](docs/integrations/nanoclaw.md); regenerate the README table with `just box go test . -run TestIntegrationsTable -update` |
| Change docs | Keep the [README](README.md) route table and this one true (`just docs-check`); one doc per task, no duplicated facts |
| Run, deploy, back up, migrate | [`docs/operate.md`](docs/operate.md) |
| Release | [`docs/releases.md`](docs/releases.md) |

## Before you change anything

Read [`docs/develop.md`](docs/develop.md): the commands, the rules that are not
visible in the code, and the verification gates live there and only there.
Every delivered change lands in a `vMAJOR.MINOR.PATCH` release, which a
maintainer cuts when they decide ([`docs/releases.md`](docs/releases.md)).
