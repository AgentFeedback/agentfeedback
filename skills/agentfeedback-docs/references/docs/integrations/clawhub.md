# Submit to ClawHub (OpenClaw)

OpenClaw installs skills from ClawHub. Publish the `agentfeedback` skill
only, from a checkout of the release tag:

```bash
clawhub login                      # device code; unattended: clawhub login --token <token>
clawhub skill publish skills/agentfeedback --slug agentfeedback --name AgentFeedback \
  --owner <owner> --version <release version without v> --categories development \
  --source-repo AgentFeedback/agentfeedback --source-commit <release commit> \
  --source-ref <tag> --source-path skills/agentfeedback --dry-run
```

Read the dry run's output, then run the same `clawhub skill publish`
command without `--dry-run`.

## Rules

- **License.** ClawHub releases every published skill under MIT-0,
  whatever the repository's license.
- **The binary.** The skill runs the `agentfeedback` binary, which must be
  on `PATH` on the OpenClaw host ([AGENT-INSTALL.md step 2.1](../../AGENT-INSTALL.md#step-21-the-binary)).
- **No hooks.** OpenClaw maps bundle hooks only from `HOOK.md` plus handler
  layouts and does not execute Claude `hooks/hooks.json`; the bundle carries
  no hooks either. There are no failure nudges on OpenClaw: the skill and
  the rule are the whole integration.
- **No package.** ClawHub requires `openclaw.plugin.json` for any plugin
  package. The bundle has none, so the package route is not offered.

## After listing

Set the `openclaw` entry in `integrations.json` to `submitted` when the
skill is published and `listed` once it is installable, with the date, then
regenerate the README table:

```bash
just box go test . -run TestIntegrationsTable -update
```
