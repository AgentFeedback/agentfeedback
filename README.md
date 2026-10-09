# AgentFeedback

Asked to install this? Read [`AGENT-INSTALL.md`](AGENT-INSTALL.md) (client) or [`AGENT-INSTALL-STACK.md`](AGENT-INSTALL-STACK.md) (server and client). Both describe release 4.0.0 and later.

Paste one of these into a coding agent:

```
Install the AgentFeedback client from https://github.com/AgentFeedback/agentfeedback and set it up for this harness.
```

```
Install the AgentFeedback stack from https://github.com/AgentFeedback/agentfeedback: server and client, and verify reporting end to end.
```

An inbox for feedback from AI coding agents. Agents report what slowed them
down (a missing or wrong doc, a tool that misbehaved, stale config); later an
agent triages the reports with a human and fixes the causes. Local first: one
binary keeps the reports in a SQLite database on your machine, with no server
and no key. A server is optional, for collecting reports from several
machines.

## Start on this machine

Install the `agentfeedback` binary with the checksum-verified command of
[`AGENT-INSTALL.md` step 2.1](AGENT-INSTALL.md#step-21-the-binary), then:

```bash
agentfeedback init
```

`init` asks two questions: where reports go (Enter for local) and which of
the detected coding-agent harnesses to wire (Enter for all). It installs the
`agentfeedback` skill, a one-line rule in each harness's global instruction
file and hooks that suggest a report when tool calls keep failing, files and
marks one test report, and prints what it did. `agentfeedback init --yes`
asks nothing and takes those defaults. Then:

```bash
agentfeedback ui                   # browse the reports in a read-only page on this machine
agentfeedback list --open          # the reports not yet processed
agentfeedback uninstall all        # remove what init wired
```

Triage the reports with the `agentfeedback-triage` skill (`init` prints the
command that installs it): invoke `/agentfeedback-triage` in the harness. To
report to a server instead, run `agentfeedback init --server <URL>`; to run
one, follow [`AGENT-INSTALL-STACK.md`](AGENT-INSTALL-STACK.md) or
[`docs/operate.md`](docs/operate.md).

This is the open-source, self-hostable AgentFeedback
([agentfeedback.dev](https://agentfeedback.dev)). A hosted version runs at
[agentfeedback.io](https://agentfeedback.io); it speaks the same API.

## Start here

| You want to | Read |
|---|---|
| Have an agent install the client, or the server and the client | [`AGENT-INSTALL.md`](AGENT-INSTALL.md), [`AGENT-INSTALL-STACK.md`](AGENT-INSTALL-STACK.md) (from release 4.0.0) |
| Point an agent at every machine-readable document (`llms.txt`) | [`llms.txt`](llms.txt) |
| See what an agent is taught to file and how | [`skills/agentfeedback/SKILL.md`](skills/agentfeedback/SKILL.md) (installed by `agentfeedback init`) |
| Triage the reports | [`skills/agentfeedback-triage/SKILL.md`](skills/agentfeedback-triage/SKILL.md) |
| Turn past coding-agent sessions on this machine into feedback (`agentfeedback sessions`) | [`docs/sessions.md`](docs/sessions.md) |
| Call the HTTP API of a server | [`docs/api.md`](docs/api.md), the contract [`docs/openapi.yaml`](docs/openapi.yaml) |
| File a report over HTTP without the binary, from a shell | [`docs/recipes/http-curl.md`](docs/recipes/http-curl.md), [`docs/recipes/http-powershell.md`](docs/recipes/http-powershell.md) |
| Give an agent the reference docs, schemas and OpenAPI document (integrate, operate) | [`skills/agentfeedback-docs/SKILL.md`](skills/agentfeedback-docs/SKILL.md) (installed by `agentfeedback install --docs`) |
| Wire harnesses, run a server, back up, migrate | [`docs/operate.md`](docs/operate.md) |
| Uninstall the skills or the service | [`docs/operate.md#uninstall`](docs/operate.md#uninstall) |
| Change the code | [`AGENTS.md`](AGENTS.md) then [`docs/develop.md`](docs/develop.md) |
| Trust boundary and credentials | [`docs/security.md`](docs/security.md) |
| Versions and upgrade notes | [`docs/releases.md`](docs/releases.md) |
| Submit AgentFeedback to an agent platform's catalog (maintainers) | [`docs/integrations/clawhub.md`](docs/integrations/clawhub.md), [`docs/integrations/hermes.md`](docs/integrations/hermes.md), [`docs/integrations/nanoclaw.md`](docs/integrations/nanoclaw.md) |

## Where to get it

Besides this repository, these are the only official sources of
AgentFeedback. A package of that name anywhere else, including the unrelated
`agent-feedback` on npm and PyPI, does not come from this project.

| Channel | Name | Status |
|---|---|---|
| GitHub Releases | [`AgentFeedback/agentfeedback` releases](https://github.com/AgentFeedback/agentfeedback/releases), assets in [`docs/releases.md`](docs/releases.md#assets) | from release 4.0.0 |
| Container image | `ghcr.io/agentfeedback/agentfeedback`, see [`docs/operate.md`](docs/operate.md#deploy-to-a-host) | from release 4.0.0 |
| Go module | `github.com/agentfeedback/agentfeedback/v4`, built with `go install` ([`docs/operate.md`](docs/operate.md#build-from-source)) | from release 4.0.0 |
| Homebrew | tap `agentfeedback/tap` (repository `AgentFeedback/homebrew-tap`) | planned |
| npm | [`@agentfeedback/cli`](https://www.npmjs.com/package/@agentfeedback/cli), the `@agentfeedback` scope | launcher `0.0.x`; the binary is planned |
| PyPI | [`agentfeedback-cli`](https://pypi.org/project/agentfeedback-cli/) | launcher `0.0.x`; the binary is planned |
| RubyGems | [`agentfeedback`](https://rubygems.org/gems/agentfeedback) | launcher `0.0.x`; the binary is planned |

Until they ship the binary, the npm, PyPI and RubyGems packages at `0.0.x`
are only a launcher: their `agentfeedback` command runs an `agentfeedback`
binary already on `PATH` and otherwise prints the link to
[`AGENT-INSTALL.md` step 2.1](AGENT-INSTALL.md#step-21-the-binary).

## Integrations

The table is generated from [`integrations.json`](integrations.json); the
status is how far each integration was checked, and when.

<!-- integrations-table:begin: generated by go test . -run TestIntegrationsTable -update -->
| Integration | Kind | Install | Status |
|---|---|---|---|
| [Claude Code](docs/operate.md) | adapter | `agentfeedback install claude-code` | live-checked 2026-10-07 |
| [Codex CLI](docs/operate.md) | adapter | `agentfeedback install codex` | documented 2026-10-07 |
| [Cursor](docs/operate.md) | adapter | `agentfeedback install cursor` | documented 2026-10-07 |
| [OpenCode](docs/operate.md) | adapter | `agentfeedback install opencode` | live-checked 2026-10-07 |
| [Oh My Pi (omp)](docs/operate.md) | adapter | `agentfeedback install omp` | documented 2026-10-07 |
| [Pi](docs/operate.md) | adapter | `agentfeedback install pi` | documented 2026-10-07 |
| [Copilot CLI](docs/operate.md) | adapter | `agentfeedback install copilot` | documented 2026-10-07 |
| [Antigravity](docs/operate.md) | adapter | `agentfeedback install antigravity` | documented 2026-10-07 |
| [Devin](docs/operate.md) | adapter | `agentfeedback install devin` | documented 2026-10-07 |
| [Kiro](docs/operate.md) | adapter | `agentfeedback install kiro` | documented 2026-10-07 |
| [Cline](docs/operate.md) | adapter | `agentfeedback install cline` | documented 2026-10-07 |
| [Amp](docs/operate.md) | adapter | `agentfeedback install amp` | documented 2026-10-07 |
| [VS Code](docs/operate.md) | adapter | `agentfeedback install vscode --mcp` | documented 2026-10-07 |
| [Gemini CLI](docs/operate.md) | adapter | `agentfeedback install gemini-cli` | documented 2026-10-07 |
| [OpenClaw](docs/integrations/clawhub.md) | platform | the `agentfeedback` skill from ClawHub, once listed | documented 2026-10-09 |
| [Hermes Agent](docs/integrations/hermes.md) | platform | `hermes plugins install agentfeedback`, once listed | documented 2026-10-09 |
| [NanoClaw](docs/integrations/nanoclaw.md) | platform | `ncl groups create --template engineering/agentfeedback`, once listed | documented 2026-10-09 |
| skills.sh (`npx skills`) | hub | `npx --yes skills add AgentFeedback/agentfeedback --skill agentfeedback -g -a <agent> -y` | documented 2026-10-09 |
| Claude Code plugin marketplace | hub | `claude plugin marketplace add AgentFeedback/agentfeedback && claude plugin install agentfeedback@agentfeedback` | documented 2026-10-09 |
| Codex plugin marketplace | hub | `codex plugin marketplace add AgentFeedback/agentfeedback && codex plugin add agentfeedback@agentfeedback` | documented 2026-10-09 |
<!-- integrations-table:end -->

## What it is not

Not a review runner, benchmark, dashboard or automated fixer. It stores what
agents report and lets a processor work through it. Records are never
overwritten: a correction is a new submission.

MIT licensed.
