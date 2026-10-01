# AgentFeedback: working in this repo

Go + SQLite HTTP service that stores write-once feedback from AI coding agents
(frictions, review runs, events) plus two skills: `agentfeedback`
(submit/read, Bash, installed everywhere) and `agentfeedback-triage` (process
the queue, Bash plus an optional Python helper; user-invoked only).
Harness-neutral: this is the only agent instructions file (no `CLAUDE.md` or
`GEMINI.md`); Claude Code loads it when no `CLAUDE.md` exists.

Design records live in a private location; if `.kitchen/` is present in this
checkout, read `.kitchen/AGENTS.md` first.

This file is for agents **changing this repo**. If you only want to **use the
running service**, you need one document: the
[`agentfeedback` skill at v3.0.0](https://github.com/AgentFeedback/agentfeedback/tree/v3.0.0/skills/agentfeedback) (the bash client of the running
release), or
[`docs/api.md`](docs/api.md) for raw HTTP. The skill's `SKILL.md` is
generated and teaches the binary of the next major release.

## Route by task

| Task | Go to |
|---|---|
| Change service code | [`docs/develop.md`](docs/develop.md): layout, commands, invisible rules, verification |
| Change the API | [`docs/api.md`](docs/api.md) is the contract; six artifacts move in one commit (see develop.md) |
| Change a skill's scripts | `skills/<name>/scripts/`, then the skill gates in [`docs/develop.md`](docs/develop.md#verification-before-you-are-done) |
| Change docs | Keep the [README](README.md) route table true; one doc per task, no duplicated facts |
| Run, deploy, back up, migrate | [`docs/operate.md`](docs/operate.md) |
| Release | [`docs/releases.md`](docs/releases.md) |

## Before you change anything

Read [`docs/develop.md`](docs/develop.md): the commands, the rules that are not
visible in the code, and the verification gates live there and only there.
Every delivered change lands in a `vMAJOR.MINOR.PATCH` release, which a
maintainer cuts when they decide ([`docs/releases.md`](docs/releases.md)).
