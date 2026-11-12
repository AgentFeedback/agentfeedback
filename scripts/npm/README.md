# AgentFeedback

**Your agents hit friction. Make it the last time.**

An inbox for feedback from AI coding agents. Capture broken setup steps, stale
docs, misbehaving tools and configuration problems while the evidence is fresh.
Then triage the reports with your agent and fix the causes—not just the symptoms.

[Website & Cloud](https://agentfeedback.dev) ·
[Documentation](https://agentfeedback.dev/docs/quickstart/local) ·
[Source](https://github.com/AgentFeedback/agentfeedback) ·
[Issues](https://github.com/AgentFeedback/agentfeedback/issues)

## Start locally

```sh
npm install -g @agentfeedback/cli
agentfeedback init
```

Setup lets you choose where reports go and which detected coding-agent harnesses
to connect. Choose local mode to keep reports in SQLite on your machine: no
account, server or API key required. Setup installs skills and hooks into the
harnesses you select; installing this npm package alone does not configure them.

```sh
agentfeedback ui          # browse your feedback inbox
agentfeedback list --open # see reports that still need attention
```

Invoke `/agentfeedback-triage` in your coding agent to investigate reports and
work through fixes together. You decide what changes.

Works with Claude Code, Codex, Cursor, OpenCode and more. See the
[harness guides](https://agentfeedback.dev/docs/harnesses) for setup and support
by integration.

## Prefer Cloud?

**[AgentFeedback Cloud](https://agentfeedback.dev)** is the hosted option for
collecting feedback across machines without running your own server. Use the
same open-source CLI with a Cloud account.

[Get started with Cloud](https://agentfeedback.dev/docs/quickstart/cloud) or
[sign up](https://agentfeedback.dev/signup). The Cloud guide covers email sign-in,
installation keys and connecting an existing setup.

Prefer your own infrastructure? The same binary can run the
[MIT-licensed self-hosted server](https://agentfeedback.dev/docs/quickstart/self-hosted).

## What this package installs

The native AgentFeedback CLI for **macOS, Linux or Windows**, on **x64 or ARM64**.
npm selects the matching, exact-version platform package; each contains its
binary. There is no install-time download script, and installation works with
`--ignore-scripts`. Keep optional dependencies enabled—`--omit=optional` excludes
the native binary.

You can also run the CLI without a global installation:

```sh
npx --yes @agentfeedback/cli version
```

For reproducible automation, pin `@agentfeedback/cli` to an exact release version.
[Release notes](https://github.com/AgentFeedback/agentfeedback/releases) cover
updates and compatibility.

## Your reports, your choice

Local mode keeps feedback on your machine. Cloud and self-hosted modes send it
to the server you configure. Read the
[collection and security guide](https://agentfeedback.dev/docs/security) before
connecting agent sessions or enabling additional collection.

AgentFeedback is not a benchmark or an unattended code fixer. It keeps findings
from getting lost and gives you and your agent a concrete starting point.

MIT licensed. [Contributions welcome](https://github.com/AgentFeedback/agentfeedback/blob/main/docs/develop.md).
