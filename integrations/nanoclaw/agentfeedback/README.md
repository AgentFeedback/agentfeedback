# AgentFeedback

## What this template does

Connects the agent to an AgentFeedback server's `/mcp` endpoint over
streamable HTTP. The agent reports friction (missing docs, misbehaving
tools, stale config) as it works; the server's MCP instructions teach it
when to file and what to include. The operator reads and triages the
reports on the server.

There is no CLI skill: the agent container has no `agentfeedback` binary,
so the MCP tools are the whole integration.

## Where it lives

- Source: https://github.com/AgentFeedback/agentfeedback, directory
  `integrations/nanoclaw/agentfeedback/`.
- Submission and operator steps: `docs/integrations/nanoclaw.md` in that
  repository.

## Services and credentials

- An AgentFeedback server reachable from the agent container over HTTPS.
  Replace `https://agentfeedback.example.com/mcp` in `mcp.json` with the
  server's HTTPS URL followed by `/mcp`. Do not use
  `host.docker.internal`, `gateway.docker.internal` or `172.17.0.1`;
  NanoClaw refuses them.
- The server's API key, sent as `Authorization: Bearer <key>`. Let the
  OneCLI gateway inject it for the server's host, or replace `placeholder`
  in `mcp.json` with `Bearer <key>` after stamping the template. Never commit the key.
