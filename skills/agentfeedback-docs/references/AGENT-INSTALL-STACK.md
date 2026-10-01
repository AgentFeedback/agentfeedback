# Install the AgentFeedback stack (for agents)

You are an AI agent asked to install the AgentFeedback server and client on
the machine you run on, and to verify reporting end to end. This file
includes the client playbook, [`AGENT-INSTALL.md`](AGENT-INSTALL.md): a step
named "client step N" is step N of that file, run exactly as written there,
with its own outcome. Fetch it from the same repository and revision as this
file. Follow this file top to bottom; it uses the client playbook's format:
every step has one command in an `sh` block, one **Verify** line, and one
**Outcome** in a `json` block that you fill in and quote back to the human
before you go on. A step that does not apply to you still gets its outcome,
with `"status": "skipped"` and the reason.

This file describes release 4.0.0 and later; until that release is out,
client step 2.1 works only with a pre-release tag the human names. The server
runs on Linux (a systemd user unit) or macOS (a launchd agent), or in Docker
Compose on either; not on Windows. The server and the client end up on this
machine; other machines join later with the client playbook.

Placeholders, beyond the client playbook's: `<key_file>`,
`<env_file>` and `<listen>` come from step S1's outcome, `<port>` is the
port of `<listen>`, `<path>` and `next` come from step S2's outcome, and
`<URL>` is set in step S5. Replace them before you run a command.

## Safety rules

The client playbook's [safety rules](AGENT-INSTALL.md#safety-rules) hold for
this whole file. On top of them:

- **The server key never enters your context.** Step S1's command removes it
  from the output, and the client reads it from the key file on stdin (step
  S5). Never `cat`, `grep` or open `<key_file>` with a file tool.
  `<env_file>` names the key file and holds no key; reading it is safe.
- **The server listens on `127.0.0.1` until the human decides otherwise** in
  step S4. Change the listen address or the published port only as step S4
  says, after the human's explicit yes. Never touch a firewall, a reverse
  proxy or a tunnel: those are the human's.
- **Never add `--force` on your own.** On `serve --init` it rotates the key
  and cuts off every client; on `server install` it replaces a service file.
  Both need the human's word; an explicit yes in step S4 is that word for
  the one `--force` step S4 names.

## Step S0: The client binary

Run client step 0. Then:

- A chat app (no shell) cannot run a server: tell the human, and explain the
  client side with client step 2.8.
- `binary_possible` false, or Windows: stop and tell the human. The server is
  the `agentfeedback` binary.
- Otherwise skip client step 1 (the server does not exist yet) and run client
  step 2.1. Its `<binary>` runs the server as well as the client.

Then check who you run as, and whether Docker Compose is usable, for step S2:

```sh
id -un; docker compose version && docker info --format '{{.ServerVersion}}'
```

**Verify:** client steps 0 and 2.1 report `"status": "ok"`. The first line
is the user the server will belong to; `root` means stop and ask the human
to run this file as the user who should own the server (a systemd user unit
refuses root, and files written as root are unreadable to that user).
Compose is usable when both later lines succeed; an error or `command not
found` means it is not, which is not a failure.

**Outcome:**

```json
{"step": "S0", "status": "ok", "user": "user", "os": "linux", "binary": "/home/user/.local/bin/agentfeedback", "compose": false}
```

## Step S1: The server configuration

`serve --init` writes two files, both readable by the owner only, into
`~/.config/agentfeedback/server`: `api-key`, a new random key, and
`serve.env`, which names the key file, the database
(`~/.local/share/agentfeedback/agentfeedback.db`) and the listen address
`127.0.0.1:8090`. It starts nothing. Its JSON outcome is the only place the
key is printed, so the `sed` drops that member before you see it. The `curl`
first checks what answers on port 8090.

```sh
c=$(curl -s -o /dev/null -m 3 -w '%{http_code}' http://127.0.0.1:8090/api/v1/meta); case "$c" in 000) "<binary>" serve --init | sed 's/,"api_key":"[^"]*"//' ;; 401) echo 'an AgentFeedback server already answers on port 8090' ;; *) echo "port 8090 is in use (HTTP $c)" ;; esac
```

**Verify:** the last line is JSON with `"status":"written"`, `dir`,
`key_file`, `env_file`, `database` and `"listen":"127.0.0.1:8090"`, and no
`api_key`. If any output shows a 64-character hex string, the key reached
you: tell the human, and after setup they rotate it themselves.
`port 8090 is in use` means something else listens there: ask the human for
a free port and run the command again with that port in both places and
`--port <N>` after `--init`. `an AgentFeedback server already answers`, or an
error naming an existing file, means this machine already has a server: show
the human `~/.config/agentfeedback/server` and ask whether to keep it or
replace it. Keep: `grep -v '^#' ~/.config/agentfeedback/server/serve.env`
shows the values for this outcome; step S2 then reports the existing service
file, and step S3 finds the server answering. Replace: the same `serve
--init` with `--force` after `--init`, which rotates the key; after step S2,
restart the running server with the restart command of its form (`next[5]`
for `systemd`, `next[4]` for `launchd` and `compose`) in place of step S3's
start commands.

**Outcome:**

```json
{"step": "S1", "status": "ok", "dir": "/home/user/.config/agentfeedback/server", "key_file": "/home/user/.config/agentfeedback/server/api-key", "env_file": "/home/user/.config/agentfeedback/server/serve.env", "database": "/home/user/.local/share/agentfeedback/agentfeedback.db", "listen": "127.0.0.1:8090"}
```

## Step S2: The service file

The default is a service of the operating system that runs the binary: a
systemd user unit on Linux, a launchd agent on macOS. When step S0 found
Docker Compose, ask the human whether they prefer to run the server in a
container instead; never pick Compose on your own. `server install` writes
exactly one file from `serve.env` and starts nothing; its outcome's `next`
lists the commands to start, check, back up and restart the server.

```sh
"<binary>" server install --systemd
```

On macOS, use `--launchd` in place of `--systemd`; for Compose, `--compose`.
Compose runs the container as your uid:gid, which rootless Docker or
userns-remap may not map to the owner of the key file and database
directory; there, tell the human and use the default form instead.

**Verify:** the last line is JSON with `"status":"written"`, `mode`, `path`
(the file it wrote) and `next`. An error naming an existing file means a
service file is already there: show the human the path and ask whether to
keep it (go on; `next` is the same for the same form, see
[`docs/operate.md`](docs/operate.md#bare-binary-no-docker)) or replace it
(the same command with `--force`). An error about the image means
a binary that is not a release: `--compose` then needs `--image <ref>`, which
only the human can name.

**Outcome:**

```json
{"step": "S2", "status": "ok", "mode": "systemd", "path": "/home/user/.config/systemd/user/agentfeedback.service", "next": ["systemctl --user daemon-reload", "systemctl --user enable --now agentfeedback.service", "systemctl --user status agentfeedback.service", "loginctl enable-linger \"$USER\"", "set -a; . /home/user/.config/agentfeedback/server/serve.env; set +a; /home/user/.local/bin/agentfeedback backup /home/user/.local/share/agentfeedback/agentfeedback-$(date -u +%Y%m%dT%H%M%SZ).db", "systemctl --user restart agentfeedback.service"]}
```

## Step S3: Start the server

Start it and wait until it answers. As in client step 1, no key is sent, so
an AgentFeedback server must refuse:

```sh
systemctl --user daemon-reload && systemctl --user enable --now agentfeedback.service && for i in 1 2 3 4 5 6 7 8 9 10; do curl -sS -m 2 -w '\n%{http_code}\n' 'http://<listen>/api/v1/meta' && break; sleep 1; done
```

For `launchd`, replace the two `systemctl` commands with `next[0]` and
`next[1]` from step S2 (`launchctl bootstrap …` and `launchctl kickstart …`);
for `compose`, with `next[0]` (`docker compose -f <path> up -d`).

**Verify:** the last line is `401` and the line before it is JSON with
`"error":"unauthorized"` and a message naming `X-Api-Key`. Anything else:
run the status command (`next[2]` for `systemd` and `launchd`, `next[1]` for
`compose`), read the server's log (`journalctl --user -u
agentfeedback.service -n 50 --no-pager`, the end of
`~/Library/Logs/agentfeedback/serve.log`, or `docker compose -f <path> logs
--tail 50 agentfeedback`), and show the human both.

With `systemd`, the user unit stops when the human's last session ends. Ask
whether the server should keep running without them logged in; only on yes,
run `loginctl enable-linger "$USER"` (`next[3]`).

**Outcome:**

```json
{"step": "S3", "status": "ok", "running": true, "linger": false}
```

## Step S4: Ask the human the four decisions

Ask exactly these four questions in one message, and wait for the answers.
Never assume one: a question left unanswered changes nothing and is recorded
as `"unanswered"`.

> 1. **Exposure.** The server listens on `<listen>`, so only this machine
>    reaches it. Should other machines reach it too? If so, on which address:
>    every interface (`0.0.0.0`) or one interface's IPv4 address?
> 2. **TLS.** Will a reverse proxy or a tunnel that you run terminate TLS in
>    front of it? Without TLS, every client sends the API key across the
>    network in clear text.
> 3. **The key.** It is in `<key_file>`, readable by you only, and after the
>    next step also in this machine's client configuration. Where do you want to keep your own copy,
>    for example a password manager? I will not print it; run
>    `cat <key_file>` in your own terminal to copy it.
> 4. **Who else gets it.** Which other machines or people should report to
>    this server? They all share this one key, and rotating it
>    (`agentfeedback serve --init --force`) cuts every client off until it is
>    reconfigured.

Only answer 1 changes what you do, and only when it is an explicit yes with
an address:

- A yes together with a reverse proxy or a tunnel on this machine (answer 2)
  needs no change: it reaches the server on `127.0.0.1`. Setting it up is the
  human's.
- Any other yes opens a plain-HTTP port. Before changing anything, tell the
  human: the server is not safe to expose directly to the internet; the
  port is safe only inside a trusted private network, or behind a TLS proxy
  that also authenticates; without TLS the key crosses the network in clear
  text; never bind an address on a public interface; with Compose, Docker
  publishes the port past host firewalls such as ufw. Change the address only
  when the human says yes after that warning; that yes also covers the
  `--force` below.
- An IPv6 address: tell the human this file supports IPv4 addresses only,
  and change nothing.

To change the address, `<address>` being the human's answer:

```sh
sed -i.bak 's/^HTTP_LISTEN_ADDR=127\.0\.0\.1:/HTTP_LISTEN_ADDR=<address>:/' '<env_file>' && systemctl --user restart agentfeedback.service && grep '^HTTP_LISTEN_ADDR=' '<env_file>'
```

For `launchd`, the plist holds a copy of `serve.env`: after the same `sed`,
run `"<binary>" server install --launchd --force`, then `next[4]` from step
S2 in place of the restart. For `compose`, the address lives in the compose
file instead: run `sed -i.bak 's/"127\.0\.0\.1:<port>:8080"/"<address>:<port>:8080"/' '<path>' && docker compose -f '<path>' up -d --force-recreate && grep -F ':8080"' '<path>'`
in place of the whole command; a later `server install --compose --force`
puts it back on `127.0.0.1`. With Compose, `serve.env` keeps `127.0.0.1`:
report the published address as `listen` in this outcome and in step S7.

**Verify:** without a change, nothing to check. After one, the last line
shows `<address>` and the `curl` command of step S3 against
`http://<address>:<port>/api/v1/meta` (`127.0.0.1` for `0.0.0.0`) ends in
`401`. Opening the firewall, setting up TLS and handing the key to others
go into step S7's `human_todo`.

**Outcome:**

```json
{"step": "S4", "status": "ok", "listen": "127.0.0.1:8090", "exposure": "this machine only", "tls": "none", "key_copy": "password manager", "shared_with": []}
```

## Step S5: Wire this machine's client

`<URL>` is `http://127.0.0.1:<port>`, or `http://<address>:<port>` when step
S4 bound one interface's address. The client reads the key from the key file
on stdin, so nobody types it:

```sh
"<binary>" doctor --init --url '<URL>' --key-from-stdin < '<key_file>'
```

**Verify:** the output is `{"status":"written","path":"…"}`. A `config already
exists` error is handled as in client step 2.2: ask the human whether to keep
the existing configuration or replace it with `--force` after
`--key-from-stdin`.

Then run client step 2.3 (or 2.4 or 2.5, as that step routes), client step
3.1 and client step 3.2, each with its own outcome. Client step 3.2's
`install-check` row proves the whole path: client, server and database.

**Outcome:**

```json
{"step": "S5", "status": "ok", "server": "<URL>", "config": "/home/user/.config/agentfeedback/config.toml", "install_check_id": 1}
```

## Step S6: The triage skill (optional)

`agentfeedback-triage` processes the queue with the human, only when they
invoke it (`/agentfeedback-triage`). It is installed with the third-party
`skills` CLI from the npm registry, so ask the human whether they want it at
all. Only on yes, as in client step 2.5 (`<agent>` is your harness's name in
that CLI's agent list). Client step 2.5's rule against an agent that client
step 2.3 wired does not apply here: it protects the `agentfeedback`
directory, and this skill installs into `agentfeedback-triage`.

```sh
DISABLE_TELEMETRY=1 npx --yes skills add AgentFeedback/agentfeedback --skill agentfeedback-triage -g -a <agent> -y
```

**Verify:** exit code 0 and the output lists `agentfeedback-triage` as
installed for `<agent>`. The triage scripts read the server from
`AGENT_FEEDBACK_URL` and `AGENT_FEEDBACK_API_KEY`: put into step S7's
`human_todo` that the human adds `export AGENT_FEEDBACK_URL=<URL>` and
`export AGENT_FEEDBACK_API_KEY="$(cat <key_file>)"` to their shell profile.
Those hold the same values as the client configuration; client step 3.1 then
reports `"source": "env"`, which client step 3.1 accepts for this case.

**Outcome:**

```json
{"step": "S6", "status": "skipped", "reason": "the human declined"}
```

## Step S7: Report

Run client step 4 first. Then check the service once more with the status
command of its form (`next[1]` for `compose`; `launchctl print
gui/$(id -u)/dev.agentfeedback.serve` for `launchd`):

```sh
systemctl --user status agentfeedback.service --no-pager
```

**Verify:** the output shows `active (running)` (`state = running` for
`launchd`; `Up` or `running` for `compose`).

**Outcome:** quote this to the human, with every earlier outcome above it.
`status_command` and `backup_command` come from step S2's `next`
(`next[2]` and `next[4]` for `systemd`, `next[2]` and `next[3]` for
`launchd`, `next[1]` and `next[3]` for `compose`).

```json
{"step": "S7", "status": "ok", "server": {"mode": "systemd", "listen": "127.0.0.1:8090", "env_file": "/home/user/.config/agentfeedback/server/serve.env", "key_file": "/home/user/.config/agentfeedback/server/api-key", "database": "/home/user/.local/share/agentfeedback/agentfeedback.db", "status_command": "systemctl --user status agentfeedback.service", "backup_command": "set -a; . /home/user/.config/agentfeedback/server/serve.env; set +a; /home/user/.local/bin/agentfeedback backup /home/user/.local/share/agentfeedback/agentfeedback-$(date -u +%Y%m%dT%H%M%SZ).db"}, "client": {"binary": "/home/user/.local/bin/agentfeedback", "server": "http://127.0.0.1:8090", "harnesses": ["claude-code"], "install_check_id": 1}, "reachable_from": "coding agents with the binary or an MCP client that sends the key header (Claude Code, Codex, Cursor, OpenCode, omp, pi), on this machine and on every machine that reaches the listen address; chat apps (Claude.ai, ChatGPT, mobile) only through the Cloud, except a Claude.ai organisation whose administrator configures static headers, for a server reachable over public HTTPS", "human_todo": ["copy the key from the key file into your password manager", "restart the harness so it loads the skill"]}
```

`human_todo` holds what is left for the human, for example: copy the key
(step S4); open the firewall, set up the reverse proxy or the tunnel (step
S4); run the client playbook on each machine named in step S4 with `<URL>`
as the server; the client playbook's own items. Back up the database
regularly with `backup_command`. To remove the stack, see
[the uninstall section of `docs/operate.md`](docs/operate.md#a-service-from-server-install).
