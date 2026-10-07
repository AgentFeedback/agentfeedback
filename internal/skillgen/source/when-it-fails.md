## When something fails

<!-- only: cli -->
Run `agentfeedback doctor` first. It prints the mode (a local database on
this machine when no server is configured, otherwise the server), checks the
configuration, the connection, the key, the server's version and the local
spool, and says what to do next. To report to a server instead of the local
database, run `agentfeedback doctor --init --url <server URL> --key-from-stdin`
with the key on stdin.
<!-- end -->
<!-- only: http -->
Call `GET {{server}}/api/v1/meta` with the same headers: `200` means the URL
and the key work, `401` means the key is wrong, `429` means wait and retry,
and no answer means the URL is wrong or the service is down.
<!-- end -->
<!-- only: mcp -->
A tool error that says `unauthorized` means the connection's key header is
missing or wrong; the user fixes it in the MCP client's configuration.
<!-- end -->

Never work around a failure by writing the report somewhere else; tell the
user the report was not filed and why.
<!-- only: file -->

The one exception is a machine where the `agentfeedback` command is not
found at all: write the report as one JSON object into a file in the inbox,
`${XDG_DATA_HOME:-~/.local/share}/agentfeedback/inbox/`. The next
`agentfeedback` command on this machine, or `agentfeedback ingest`, files it
with `context.origin` `inbox` and moves the file to `inbox/done/`, or to
`inbox/rejected/` with a `.reason` file beside it. The members are those
of the minimum body plus the ones listed above (`kind`, `summary`,
`category`, `details`, `model`, ...); nothing is collected for you, so set
`model`, `harness` and `project` yourself. Set `key` to an id unique to
this report, as the PowerShell and Python lines do, so a second attempt
never files a second report; without one, the file's name and bytes key
it. Write a dot-named temporary file and rename it to `<name>.json`, so a
half-written file is never read.

```bash
(umask 077; d="${XDG_DATA_HOME:-$HOME/.local/share}/agentfeedback/inbox"; k="$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$d" && cat > "$d/.$k.tmp" <<'JSON' && mv "$d/.$k.tmp" "$d/$k.json")
{"kind": "friction", "summary": "README install step 3 references a flag that no longer exists", "category": "documentation", "model": "<your model id>"}
JSON
```

```powershell
$d = Join-Path $(if ($env:XDG_DATA_HOME) { $env:XDG_DATA_HOME } else { Join-Path $HOME '.local/share' }) 'agentfeedback/inbox'; $k = [guid]::NewGuid().ToString()
New-Item -ItemType Directory -Force -Path $d | Out-Null; $t = Join-Path $d ".$k.tmp"
[IO.File]::WriteAllText($t, (@{ kind = 'friction'; key = $k; summary = 'README install step 3 references a flag that no longer exists'; category = 'documentation'; model = '<your model id>' } | ConvertTo-Json))
Move-Item $t (Join-Path $d "$k.json")
```

```bash
python3 - <<'PY'
import json, os, uuid
d = os.path.join(os.environ.get("XDG_DATA_HOME") or os.path.expanduser("~/.local/share"), "agentfeedback", "inbox")
os.makedirs(d, 0o700, exist_ok=True); k = str(uuid.uuid4()); t = os.path.join(d, "." + k + ".tmp")
with open(os.open(t, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as f:
    json.dump({"kind": "friction", "key": k, "summary": "README install step 3 references a flag that no longer exists", "category": "documentation", "model": "<your model id>"}, f)
os.replace(t, os.path.join(d, k + ".json"))
PY
```
<!-- end -->
