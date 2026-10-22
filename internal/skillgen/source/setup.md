<!-- only: skill -->
## Setup

Setup applies only when `command -v agentfeedback` finds nothing, or
`agentfeedback install --list` shows your harness with MODE `-`. Ask the
human first; then:

1. `bash <this skill's directory>/scripts/install.sh` installs the
   checksum-verified release binary into `~/.local/bin` and prints its path;
   run step 2 with that path when `agentfeedback` is not on PATH. A copy of
   this skill without `scripts/` uses step 2.1 of `AGENT-INSTALL.md` in
   github.com/AgentFeedback/agentfeedback instead.
2. `agentfeedback init --yes --harnesses <your harness> --json` reports
   to the server already configured on this machine (`config.toml` or
   `AGENT_FEEDBACK_URL`) and otherwise locally (a new server only when the
   human names one: `--server <URL> --key-from-stdin`, the key typed by the
   human, never by you), wires this harness's rule and hooks, leaves this skill as it is,
   installs the triage skill, and runs a check: the JSON shows
   `"status": "ok"`.

Other harnesses on the machine are the human's to wire with
`agentfeedback init`. If the human declines, file through the inbox as
"When something fails" says.
<!-- end -->
