# Submit to Hermes Agent

Hermes Agent has two catalogs. Submissions come from the repository
owner.

## Plugin catalog

```bash
git rev-list -n 1 <tag>            # the release commit, full 40 hex
```

1. In a fork of `NousResearch/hermes-agent`, copy
   `integrations/hermes/plugin-catalog/agentfeedback.yaml` from this
   repository to `plugin-catalog/agentfeedback.yaml`.
2. Replace `sha` with the release commit and set `version`.
3. Open a pull request. Disclose in it that the package carries the
   skill's `scripts/install.sh`, which downloads the checksum-verified
   binary from the GitHub release, and that the skill runs the
   `agentfeedback` binary, which writes a local SQLite database and reaches
   the network only when a server is configured.

Catalog CI clones the repository at the pinned commit and runs
`hermes plugins validate`. `subdir` points at `plugins/agentfeedback`, a
portable Agent Plugins v1 package (`plugin.json`, `skills/`; no
`mcp.json`). Hermes installs portable packages disabled until the user
enables them. Once listed, users run `hermes plugins install agentfeedback`.

## optional-mcps

1. In the same fork, copy
   `integrations/hermes/optional-mcps/agentfeedback/manifest.yaml` to
   `optional-mcps/agentfeedback/manifest.yaml`.
2. Open a pull request.

That catalog admits only entries Nous approves (there is no community
tier), so the pull request is a request. The entry runs
`agentfeedback mcp` over stdio and needs the binary on `PATH`
([AGENT-INSTALL.md step 2.1](../../AGENT-INSTALL.md#step-21-the-binary)).

Until it is listed, a Hermes user adds the server to
`~/.hermes/config.yaml`:

```yaml
mcp_servers:
  agentfeedback:
    command: agentfeedback
    args: [mcp]
```

## After listing

Set the `hermes` entry in `integrations.json` to `submitted` when a pull
request is open and `listed` once it is merged, with the date, then
regenerate the README table:

```bash
just box go test . -run TestIntegrationsTable -update
```
