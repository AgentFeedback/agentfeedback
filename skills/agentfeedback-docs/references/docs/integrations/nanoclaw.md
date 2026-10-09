# Submit to NanoClaw

NanoClaw agents run in containers without the `agentfeedback` binary, so
the template connects to an AgentFeedback server's `/mcp` endpoint over
streamable HTTP. A server is required ([operate.md](../operate.md#deploy-to-a-host)).

## Submission

In a fork of `nanocoai/nanoclaw-templates`:

```bash
cp -R <this repository>/integrations/nanoclaw/agentfeedback engineering/agentfeedback
node scripts/check-templates.mjs
node scripts/build-index.mjs
git add engineering/agentfeedback index.json
```

Commit, including the regenerated `index.json`, and open a pull request
filling "What this template does", "Where it lives" and "Services and
credentials" from the template's `README.md`. Acceptance is at NanoClaw's
discretion, and a merged template becomes theirs to maintain.

## Operators

```bash
ncl groups create --template engineering/agentfeedback
```

Then set the server and the key:

- **URL.** Replace `https://agentfeedback.example.com/mcp` in `mcp.json`
  with the server's HTTPS URL followed by `/mcp`. NanoClaw refuses
  `host.docker.internal`, `gateway.docker.internal` and `172.17.0.1`.
- **Key.** The server takes `Authorization: Bearer <key>`
  ([api.md](../api.md)). Let the OneCLI gateway inject it for the
  server's host, or replace `placeholder` in the header with `Bearer <key>` after stamping.
  Never commit the key: a checked-in header value is `placeholder`.

## After listing

Set the `nanoclaw` entry in `integrations.json` to `submitted` when a pull
request is open and `listed` once it is merged, with the date, then
regenerate the README table:

```bash
just box go test . -run TestIntegrationsTable -update
```
