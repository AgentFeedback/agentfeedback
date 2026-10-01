#!/usr/bin/env python3
"""The playbook gate: run the fenced commands of AGENT-INSTALL.md and
AGENT-INSTALL-STACK.md in a clean Linux container.

  python3 scripts/playbooks.py check
  python3 scripts/playbooks.py list
  python3 scripts/playbooks.py run <tag> [github|tree|<release-dir>] [--keep]

`check` reads both playbooks and fails when a step the gate runs or excludes
lacks its one `sh` block, its **Verify** line or its **Outcome** JSON (which
must parse and name the step), when an `sh` block belongs to no route below
and is not excluded with a reason, or when an `sh` block sits outside every
step. `list` prints the commands in run order with the human's answers filled
in; values that earlier steps print stay placeholders. `run` does the same
checks, then starts the container of tests/playbooks/Dockerfile (systemd as
PID 1, so it runs privileged), runs the stack playbook as the user `stack` and
then the client playbook as the user `client` against the server the stack
playbook started, checks each step's exit code and Verify condition, and
prints one JSON line per step it reaches; the run stops at the first failure.

The release under test comes from `github` (the published tag, downloaded as
an agent would), a release directory such as GoReleaser's dist/release
(read through file://), or `tree` (a linux archive of the working tree,
built under the tag). Only step 2.1's command is rewritten for the last two:
its release root becomes the file:// copy.

The human's answers come from AF_PLAYBOOK_* variables, named after the
playbooks' placeholders and decisions (HUMAN_INPUTS below). Unset means no,
or the default for a placeholder: the step that needs a yes is skipped and
reported as skipped.

Standard library only. The steps are named as the playbooks number them; a
renumbered step updates the routes here in the same change, and `check`
(part of `just ci`) fails until it does.
"""

from __future__ import annotations

import hashlib
import ipaddress
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

ROOT = Path(__file__).resolve().parent.parent
CLIENT = "AGENT-INSTALL.md"
STACK = "AGENT-INSTALL-STACK.md"
RELEASES = "https://github.com/AgentFeedback/agentfeedback/releases"
IMAGE = "agentfeedback-playbooks"
TAG_RE = re.compile(r"^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$")
PLACEHOLDER_RE = re.compile(r"<[A-Za-z_]+>")
KEY_RE = re.compile(r"[0-9a-f]{64}")
HARNESSES = ("claude-code", "codex", "cursor", "opencode", "omp", "pi")
STEP_TIMEOUT = 300  # seconds for one step's command
SETUP_TIMEOUT = 900  # seconds for the image build

# The answers the gate gives in the human's place. A value is the placeholder
# it fills or "yes" for a branch the human chose; unset is "no".
HUMAN_INPUTS = {
    "AF_PLAYBOOK_HARNESS": "<harness> of client step 2.3, one of " + ", ".join(HARNESSES) + " (default claude-code)",
    "AF_PLAYBOOK_ADDRESS": "<address> of step S4: an explicit yes to exposure on that IPv4 address",
    "AF_PLAYBOOK_NO_BINARY": "yes: also run client steps 2.6 and 2.7 against the server",
}


class GateError(Exception):
    pass


# --- Parsing ---------------------------------------------------------------

STEP_RE = re.compile(r"^(#{2,3}) Step ([0-9A-Z][0-9A-Z.]*): ")
FENCE_RE = re.compile(r"^```(\S*)")


@dataclass
class Block:
    lang: str
    text: str
    after_outcome: bool  # the block follows an **Outcome:** marker


@dataclass
class Step:
    id: str
    blocks: list[Block] = field(default_factory=list)
    verify: int = 0
    outcome_markers: int = 0

    def sh(self) -> list[str]:
        return [b.text for b in self.blocks if b.lang == "sh"]

    def outcomes(self) -> list[str]:
        return [b.text for b in self.blocks if b.lang == "json" and b.after_outcome]


OUTSIDE = ""  # the id under which parse() collects blocks outside every step


def parse(text: str) -> dict[str, Step]:
    """Steps by id. A step runs from its heading to the next heading of level
    2 or 3; blocks anywhere else are collected under OUTSIDE."""
    outside = Step(OUTSIDE)
    steps: dict[str, Step] = {OUTSIDE: outside}
    cur: Step = outside
    lines = text.splitlines()
    i = 0
    pending_outcome = False
    while i < len(lines):
        line = lines[i]
        m = STEP_RE.match(line)
        if m or re.match(r"^#{1,3} ", line):
            cur = outside
            pending_outcome = False
            if m:
                if m.group(2) in steps:
                    raise GateError(f"step {m.group(2)} appears twice")
                cur = steps[m.group(2)] = Step(m.group(2))
            i += 1
            continue
        if re.match(r"^\s+```", line):
            raise GateError(f"an indented fence on line {i + 1}; the gate reads fences at the start of a line only")
        f = FENCE_RE.match(line)
        if f:
            body = []
            i += 1
            while i < len(lines) and not lines[i].startswith("```"):
                body.append(lines[i])
                i += 1
            if i == len(lines):
                raise GateError("an unterminated fence")
            cur.blocks.append(Block(f.group(1), "\n".join(body), pending_outcome))
            pending_outcome = False
            i += 1
            continue
        if line.startswith("**Verify:**"):
            cur.verify += 1
        if line.startswith("**Outcome:**"):
            cur.outcome_markers += 1
            pending_outcome = True
        i += 1
    return steps


# --- Routes ----------------------------------------------------------------


@dataclass
class Ctx:
    """What the run knows: the human's answers and what earlier steps printed."""

    values: dict[str, str]
    env: dict[str, str]

    def answer(self, name: str) -> str:
        return self.env.get("AF_PLAYBOOK_" + name, "").strip()


Check = Callable[[str, "Ctx"], None]


@dataclass
class Entry:
    doc: str
    step: str
    user: str
    when: Callable[[Ctx], str | None] = lambda c: None  # a reason to skip, or None
    expect: Check = lambda out, c: None  # raises GateError when Verify fails
    stdin_key: bool = False  # the human types the server key (client step 2.2)
    # A second command the step's Verify line names, run after the step's own
    # and checked with probe_expect.
    probe: str | None = None
    probe_expect: Check = lambda out, c: None


def last(out: str) -> str:
    rows = [r for r in out.splitlines() if r.strip()]
    return rows[-1] if rows else ""


def last_json(out: str) -> dict:
    row = last(out)
    try:
        v = json.loads(row)
    except ValueError:
        raise GateError(f"the last line is not JSON: {row!r}")
    if not isinstance(v, dict):
        raise GateError(f"the last line is not a JSON object: {row!r}")
    return v


def need(cond: bool, what: str) -> None:
    if not cond:
        raise GateError(what)


def x_unauthorized(out: str, c: Ctx) -> None:
    rows = [r for r in out.splitlines() if r.strip()]
    need(len(rows) >= 2 and rows[-1] == "401", f"expected 401 last, got {last(out)!r}")
    body = json.loads(rows[-2])
    need(body.get("error") == "unauthorized" and "X-Api-Key" in body.get("message", ""), f"not an AgentFeedback 401: {rows[-2]!r}")


def x_platform(out: str, c: Ctx) -> None:
    first = out.splitlines()[0] if out else ""
    need(re.fullmatch(r"Linux (x86_64|aarch64)", first) is not None, f"first line is {first!r}")


def x_binary(out: str, c: Ctx) -> None:
    path = last(out)
    need(path.startswith("/") and path.endswith("/agentfeedback"), f"the last line is not the binary's path: {path!r}")
    c.values["binary"] = path


def x_written(out: str, c: Ctx) -> None:
    v = last_json(out)
    need(v.get("status") == "written" and v.get("path"), f"not written: {v}")


def x_install(out: str, c: Ctx) -> None:
    v = last_json(out)
    need(v.get("status") in ("installed", "unchanged"), f"install status {v.get('status')!r}")
    h = c.values["harness"]
    entry = next((e for e in v.get("harnesses", []) if e.get("name") == h), None)
    need(entry is not None, f"{h} is not in the outcome: {v}")
    need(entry.get("skill") == "wired" and entry.get("hook") == "wired", f"{h} is not wired: {entry}")


def x_doctor(out: str, c: Ctx) -> None:
    v = last_json(out)
    need(v.get("status") == "ok" and v.get("problems") == [], f"doctor: {v.get('status')!r} {v.get('problems')!r}")
    need(v.get("meta", {}).get("ok") is True, f"doctor meta: {v.get('meta')}")
    need(v.get("url") == {"value": c.values["URL"], "source": "config"}, f"doctor url: {v.get('url')}")
    key = v.get("api_key", {})
    need(key.get("set") is True and key.get("source") == "config", f"doctor api_key: {key}")


def x_e2e(out: str, c: Ctx) -> None:
    rows = [json.loads(r) for r in out.splitlines() if r.strip()]
    need([r.get("step") for r in rows] == ["submit", "list", "mark"], f"doctor --e2e steps: {rows}")
    need(all(r.get("outcome") == "ok" for r in rows), f"doctor --e2e outcomes: {rows}")
    need(len({r.get("id") for r in rows}) == 1 and rows[0].get("id") is not None, f"doctor --e2e ids differ: {rows}")


def x_list(out: str, c: Ctx) -> None:
    v = last_json(out)
    h = c.values["harness"]
    entry = next((e for e in v.get("harnesses", []) if e.get("name") == h), None)
    need(entry is not None, f"{h} is not listed: {v}")
    need(entry.get("mode") == "cli" and entry.get("skill") == "wired" and entry.get("hook") == "wired", f"{h} listed as {entry}")


def x_user(out: str, c: Ctx) -> None:
    first = out.splitlines()[0] if out else ""
    need(first not in ("", "root"), f"runs as {first!r}")


def x_server_init(out: str, c: Ctx) -> None:
    need("api_key" not in out, "the key's member reached the output")
    v = last_json(out)
    need(v.get("status") == "written", f"serve --init: {v}")
    for k in ("key_file", "env_file", "listen"):
        need(bool(v.get(k)), f"serve --init has no {k}: {v}")
    need(v["listen"] == "127.0.0.1:8090", f"listen is {v['listen']}")
    c.values.update(key_file=v["key_file"], env_file=v["env_file"], listen=v["listen"], port=v["listen"].rsplit(":", 1)[1])


def x_service(out: str, c: Ctx) -> None:
    v = last_json(out)
    need(v.get("status") == "written" and v.get("mode") == "systemd", f"server install: {v}")
    need(bool(v.get("path")) and isinstance(v.get("next"), list) and len(v["next"]) == 6, f"server install: {v}")
    c.values["path"] = v["path"]


def x_exposed(out: str, c: Ctx) -> None:
    want = f"HTTP_LISTEN_ADDR={c.values['address']}:{c.values['port']}"
    need(last(out) == want, f"expected {want!r} last, got {last(out)!r}")
    c.values["listen"] = f"{c.values['address']}:{c.values['port']}"
    c.values["probe"] = f"{loopback(c.values['address'])}:{c.values['port']}"


def loopback(address: str) -> str:
    """The address a client on this machine dials for a listen address."""
    return "127.0.0.1" if address == "0.0.0.0" else address


# Step S4's Verify: step S3's curl against the new address ends in 401. The
# loop also waits out the restart.
S4_PROBE = r"for i in 1 2 3 4 5 6 7 8 9 10; do curl -sS -m 2 -w '\n%{http_code}\n' 'http://<probe>/api/v1/meta' && break; sleep 1; done"


def x_running(out: str, c: Ctx) -> None:
    need("active (running)" in out, "the service is not active (running)")


def x_prompt(out: str, c: Ctx) -> None:
    need(c.values["URL"] in out, "the prompt block does not name the server URL")


def unless(name: str) -> Callable[[Ctx], str | None]:
    return lambda c: None if c.answer(name) else f"AF_PLAYBOOK_{name} is unset: the human said no"


def client_route(user: str) -> list[Entry]:
    """Client steps 2.3 to 3.2, as both playbooks run them."""
    return [
        Entry(CLIENT, "2.3", user, expect=x_install),
        Entry(CLIENT, "3.1", user, expect=x_doctor),
        Entry(CLIENT, "3.2", user, expect=x_e2e),
    ]


STACK_ROUTE = [
    Entry(CLIENT, "0", "stack", expect=x_platform),
    Entry(CLIENT, "2.1", "stack", expect=x_binary),
    Entry(STACK, "S0", "stack", expect=x_user),
    Entry(STACK, "S1", "stack", expect=x_server_init),
    Entry(STACK, "S2", "stack", expect=x_service),
    Entry(STACK, "S3", "stack", expect=x_unauthorized),
    Entry(STACK, "S4", "stack", when=unless("ADDRESS"), expect=x_exposed, probe=S4_PROBE, probe_expect=x_unauthorized),
    Entry(STACK, "S5", "stack", expect=x_written),
    *client_route("stack"),
    Entry(CLIENT, "4", "stack", expect=x_list),
    Entry(STACK, "S7", "stack", expect=x_running),
]

CLIENT_ROUTE = [
    Entry(CLIENT, "0", "client", expect=x_platform),
    Entry(CLIENT, "1", "client", expect=x_unauthorized),
    Entry(CLIENT, "2.1", "client", expect=x_binary),
    Entry(CLIENT, "2.2", "client", expect=x_written, stdin_key=True),
    *client_route("client"),
    Entry(CLIENT, "2.6", "client", when=unless("NO_BINARY"), expect=lambda out, c: need(last(out) == "401", f"expected 401, got {last(out)!r}")),
    Entry(CLIENT, "2.7", "client", when=unless("NO_BINARY"), expect=x_prompt),
    Entry(CLIENT, "4", "client", expect=x_list),
]

ROUTES = {"stack": STACK_ROUTE, "client": CLIENT_ROUTE}

# Steps no route runs, and why. `check` still reads their structure; the
# release's paste check into real harnesses covers what they do.
EXCLUDED = {
    (CLIENT, "2.4"): "needs the claude or codex CLI and its plugin marketplace",
    (CLIENT, "2.5"): "needs npx and the third-party skills CLI from the npm registry",
    (CLIENT, "2.8"): "chat apps have no shell; its block is text for the human",
    (STACK, "S6"): "needs npx and the third-party skills CLI from the npm registry",
}


def check(docs: dict[str, str], routes: dict[str, list[Entry]] = ROUTES, excluded: dict = EXCLUDED) -> list[str]:
    """Every problem with the playbooks' structure, as messages."""
    problems: list[str] = []
    parsed: dict[str, dict[str, Step]] = {}
    for name, text in docs.items():
        try:
            parsed[name] = parse(text)
        except GateError as e:
            problems.append(f"{name}: {e}")
            parsed[name] = {}
    routed = {(e.doc, e.step) for r in routes.values() for e in r}
    for doc, step in sorted(routed | set(excluded)):
        if step not in parsed.get(doc, {}):
            problems.append(f"{doc}: the gate names step {step}, which the playbook does not have")
    for name, steps in parsed.items():
        for sid, st in steps.items():
            where = f"{name} step {sid}"
            sh = st.sh()
            if sid == OUTSIDE:
                if sh:
                    problems.append(f"{name}: {len(sh)} sh block(s) outside every step; a command belongs to a step")
                continue
            covered = (name, sid) in routed or (name, sid) in excluded
            if sh and not covered:
                problems.append(f"{where}: an sh block no route runs; add the step to a route or to EXCLUDED with a reason")
            if not covered:
                continue
            if (name, sid) in excluded and not sh:
                pass  # a step for humans, such as a text block for a chat app
            elif len(sh) != 1:
                problems.append(f"{where}: {len(sh)} sh blocks, expected one command")
            if st.verify != 1:
                problems.append(f"{where}: {st.verify} **Verify:** lines, expected one")
            outs = st.outcomes()
            if st.outcome_markers != 1 or len(outs) != 1:
                problems.append(f"{where}: no single **Outcome:** with a json block")
                continue
            try:
                o = json.loads(outs[0])
            except ValueError as e:
                problems.append(f"{where}: the outcome is not JSON ({e})")
                continue
            if not isinstance(o, dict) or o.get("step") != sid or "status" not in o:
                problems.append(f"{where}: the outcome must be an object with \"step\": \"{sid}\" and a status")
    return problems


def read_docs(root: Path = ROOT) -> dict[str, str]:
    return {n: (root / n).read_text(encoding="utf-8") for n in (CLIENT, STACK)}


# --- Commands --------------------------------------------------------------


def fill(cmd: str, values: dict[str, str]) -> str:
    for k, v in values.items():
        cmd = cmd.replace(f"<{k}>", v)
    return cmd


def substitute(cmd: str, values: dict[str, str]) -> str:
    cmd = fill(cmd, values)
    left = sorted(set(PLACEHOLDER_RE.findall(cmd)))
    if left:
        raise GateError(f"unresolved placeholders {', '.join(left)}")
    return cmd


def pin_release(cmd: str, tag: str, file_root: str | None) -> str:
    """Client step 2.1 as its prose says for a named tag, and, for a local
    release, with the release root replaced by its file:// copy."""
    need(cmd.count("latest/download/install.sh") == 1 and cmd.count('bash "$d/install.sh"') == 1, "step 2.1's command no longer has the shape the gate pins")
    cmd = cmd.replace("latest/download", f"download/{tag}").replace('bash "$d/install.sh"', f'bash "$d/install.sh" --version {tag}')
    if file_root:
        need(f"--proto '=https'" in cmd and RELEASES in cmd, "step 2.1's command no longer has the shape the gate pins")
        cmd = cmd.replace("--proto '=https'", "--proto '=file'").replace(RELEASES, file_root)
    return cmd


def commands(docs: dict[str, str], route: list[Entry], ctx: Ctx, tag: str, file_root: str | None):
    """(entry, command or None, skip reason) in run order, resolving what is known."""
    parsed = {n: parse(t) for n, t in docs.items()}
    for e in route:
        reason = e.when(ctx)
        cmd = parsed[e.doc][e.step].sh()[0]
        if e.doc == CLIENT and e.step == "2.1":
            cmd = pin_release(cmd, tag, file_root)
        yield e, (None if reason else cmd), reason


def answers(environ: dict[str, str]) -> Ctx:
    """The human's answers, validated: they become shell text in the
    commands, so each must have the form the playbook asks for."""
    env = {k: v for k, v in environ.items() if k.startswith("AF_PLAYBOOK_")}
    unknown = sorted(set(env) - set(HUMAN_INPUTS))
    need(not unknown, f"unknown inputs {', '.join(unknown)}; the gate reads {', '.join(HUMAN_INPUTS)}")
    ctx = Ctx({}, env)
    harness = ctx.answer("HARNESS") or "claude-code"
    need(harness in HARNESSES, f"AF_PLAYBOOK_HARNESS must be one of {', '.join(HARNESSES)}, got {harness!r}")
    ctx.values["harness"] = harness
    if ctx.answer("ADDRESS"):
        try:
            ipaddress.IPv4Address(ctx.answer("ADDRESS"))
        except ValueError:
            raise GateError(f"AF_PLAYBOOK_ADDRESS must be an IPv4 address, got {ctx.answer('ADDRESS')!r}")
        ctx.values["address"] = ctx.answer("ADDRESS")
    return ctx


def label(e: Entry) -> str:
    return e.step if e.doc == STACK else f"client {e.step}"


# --- Releases --------------------------------------------------------------


def go_arch() -> str:
    m = platform.machine().lower()
    if m in ("x86_64", "amd64"):
        return "amd64"
    if m in ("aarch64", "arm64"):
        return "arm64"
    raise GateError(f"unsupported architecture {m}")


def stage_release(tag: str, source: str, work: Path) -> str | None:
    """Lay out <work>/release/download/<tag>/ and return its file:// root, or
    None for the published release."""
    if source == "github":
        return None
    dest = work / "release" / "download" / tag
    dest.mkdir(parents=True)
    version = tag[1:]
    if source == "tree":
        arch = go_arch()
        stage = work / "tree"
        stage.mkdir()
        commit = subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT, check=True, capture_output=True, text=True).stdout.strip()
        env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch)
        subprocess.run(
            ["go", "build", "-trimpath", "-ldflags", f"-s -w -X main.version={version} -X main.commit={commit}", "-o", str(stage / "agentfeedback"), "./cmd/agentfeedback"],
            cwd=ROOT, env=env, check=True,
        )
        shutil.copy(ROOT / "LICENSE", stage)
        shutil.copy(ROOT / "README.md", stage)
        archive = f"agentfeedback_{version}_linux_{arch}.tar.gz"
        subprocess.run(["tar", "-czf", str(dest / archive), "-C", str(stage), "agentfeedback", "LICENSE", "README.md"], check=True)
        digest = hashlib.sha256((dest / archive).read_bytes()).hexdigest()
        (dest / "SHA256SUMS").write_text(f"{digest}  {archive}\n")
    else:
        src = Path(source)
        need((src / "SHA256SUMS").is_file(), f"{src} has no SHA256SUMS: not a release directory")
        shutil.copy(src / "SHA256SUMS", dest)
        archives = list(src.glob(f"agentfeedback_{version}_linux_*.tar.gz"))
        need(bool(archives), f"{src} has no linux archive for {tag}")
        for f in archives:
            shutil.copy(f, dest)
    # The published install.sh is the committed one (release.sh checks it).
    shutil.copy(ROOT / "skills/agentfeedback/scripts/install.sh", dest)
    # Readable by the container's users whatever the host's umask.
    for d, _, files in os.walk(work / "release"):
        os.chmod(d, 0o755)
        for f in files:
            os.chmod(os.path.join(d, f), 0o644)
    return "file:///release"


def tag_published(tag: str) -> bool:
    r = subprocess.run(["curl", "-fsSIL", "-o", "/dev/null", "-m", "20", f"{RELEASES}/download/{tag}/SHA256SUMS"], capture_output=True)
    return r.returncode == 0


# --- The container ---------------------------------------------------------


class Container:
    def __init__(self, release: Path | None):
        self.name = f"{IMAGE}-{os.getpid()}"
        subprocess.run(["docker", "build", "-q", "-t", IMAGE, str(ROOT / "tests/playbooks")], check=True, stdout=subprocess.DEVNULL, timeout=SETUP_TIMEOUT)
        args = ["docker", "run", "-d", "--name", self.name, "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock"]
        if release:
            args += ["-v", f"{release}:/release:ro"]
        subprocess.run(args + [IMAGE], check=True, stdout=subprocess.DEVNULL, timeout=120)
        self.uids: dict[str, str] = {}

    def wait(self) -> None:
        # degraded is fine: units a container cannot start fail, the user manager does not.
        self.root(["timeout", "120", "systemctl", "is-system-running", "--wait"], ok=(0, 1))
        uid = self.uid("stack")
        self.root(["sh", "-c", f"for i in $(seq 30); do systemctl is-active -q user@{uid}.service && exit 0; sleep 1; done; exit 1"])

    def root(self, argv: list[str], ok=(0,)) -> str:
        r = subprocess.run(["docker", "exec", self.name, *argv], capture_output=True, text=True, timeout=180)
        if r.returncode not in ok:
            raise GateError(f"container setup: {' '.join(argv)} exited {r.returncode}: {r.stderr.strip()}")
        return r.stdout

    def uid(self, user: str) -> str:
        if user not in self.uids:
            self.uids[user] = self.root(["id", "-u", user]).strip()
        return self.uids[user]

    def exec_args(self, user: str, env: dict[str, str]) -> list[str]:
        home = f"/home/{user}"
        base = {
            "HOME": home, "USER": user, "LOGNAME": user,
            "PATH": "/usr/local/bin:/usr/bin:/bin",
            "XDG_RUNTIME_DIR": f"/run/user/{self.uid(user)}",
            "DBUS_SESSION_BUS_ADDRESS": f"unix:path=/run/user/{self.uid(user)}/bus",
            **env,
        }
        args = ["docker", "exec", "-i", "-u", user, "-w", home]
        for k, v in base.items():
            args += ["-e", f"{k}={v}"]
        return args + [self.name]

    def run(self, user: str, cmd: str, env: dict[str, str], key_file: str | None) -> tuple[int, str, str]:
        """Exit code, stdout and stderr. Verify conditions read stdout: docker
        exec does not keep the order of lines across the two streams."""
        argv = self.exec_args(user, env) + ["sh", "-c", cmd]
        try:
            if not key_file:
                r = subprocess.run(argv, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=STEP_TIMEOUT)
                return r.returncode, r.stdout, r.stderr
            # The human types the key: it goes from the stack's key file to
            # the command's stdin without passing through this process.
            src = subprocess.Popen(["docker", "exec", self.name, "cat", key_file], stdout=subprocess.PIPE)
            try:
                r = subprocess.run(argv, stdin=src.stdout, capture_output=True, text=True, timeout=STEP_TIMEOUT)
            finally:
                if src.stdout:
                    src.stdout.close()
                src.kill()
                read = src.wait()
            # A command that failed before reading leaves cat to a broken
            # pipe; its own exit code is then the failure to report.
            if r.returncode == 0 and read not in (0, -9):
                raise GateError(f"cannot read {key_file}")
            return r.returncode, r.stdout, r.stderr
        except subprocess.TimeoutExpired:
            raise GateError(f"timed out after {STEP_TIMEOUT} s")

    def remove(self) -> None:
        subprocess.run(["docker", "rm", "-fv", self.name], capture_output=True)


def emit(**kv) -> None:
    print(json.dumps(kv), flush=True)


def redact(out: str) -> str:
    return KEY_RE.sub("[REDACTED]", out)


def run(tag: str, source: str, keep: bool) -> int:
    need(TAG_RE.match(tag) is not None, f"expected a tag vX.Y.Z or vX.Y.Z-rc.N, got {tag!r}")
    docs = read_docs()
    problems = check(docs)
    if problems:
        raise GateError("the playbooks fail the structure check:\n  " + "\n  ".join(problems))
    if source == "github":
        need(tag_published(tag), f"{tag} is not a published release with a SHA256SUMS; name a release directory or `tree`")
    ctx = answers(dict(os.environ))
    failed = 0
    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp)
        file_root = stage_release(tag, source, work)
        box = Container(work / "release" if file_root else None)
        try:
            box.wait()
            exec_env = {"AGENT_FEEDBACK_RELEASE_URL": file_root} if file_root else {}
            for playbook, route in ROUTES.items():
                # Each playbook installs its own binary; the client joins the
                # stack's server at the URL step S5 set, as another machine would.
                ctx.values.pop("binary", None)
                for e, cmd, reason in commands(docs, route, ctx, tag, file_root):
                    if e.doc == STACK and e.step == "S5":
                        ctx.values["URL"] = f"http://{loopback(ctx.values.get('address', '127.0.0.1'))}:{ctx.values['port']}"
                    if reason:
                        emit(playbook=playbook, step=label(e), status="skipped", reason=reason)
                        continue
                    out = err_out = ""
                    try:
                        resolved = substitute(cmd, ctx.values)
                        code, out, err_out = box.run(e.user, resolved, exec_env, ctx.values["key_file"] if e.stdin_key else None)
                        if code != 0:
                            raise GateError(f"exited {code}")
                        if KEY_RE.search(out + err_out):
                            raise GateError("a 64-character hex string, the key's form, reached the output")
                        e.expect(out, ctx)
                        if e.probe:
                            cmd = e.probe
                            code, out, err_out = box.run(e.user, substitute(cmd, ctx.values), exec_env, None)
                            if code != 0:
                                raise GateError(f"the Verify command exited {code}")
                            e.probe_expect(out, ctx)
                    except Exception as err:
                        emit(playbook=playbook, step=label(e), status="failed", error=str(err))
                        print(f"--- command\n{cmd}\n--- stdout\n{redact(out)}--- stderr\n{redact(err_out)}---", file=sys.stderr)
                        failed += 1
                        break
                    emit(playbook=playbook, step=label(e), status="ok")
                if failed:
                    break
        finally:
            if keep:
                print(f"playbooks: kept container {box.name}; remove it with docker rm -fv {box.name}", file=sys.stderr)
            else:
                box.remove()
    emit(status="failed" if failed else "ok", tag=tag, source=source)
    return 1 if failed else 0


def list_commands() -> int:
    docs = read_docs()
    problems = check(docs)
    if problems:
        raise GateError("the playbooks fail the structure check:\n  " + "\n  ".join(problems))
    ctx = answers(dict(os.environ))
    for playbook, route in ROUTES.items():
        for e, cmd, reason in commands(docs, route, ctx, "<tag>", None):
            print(f"# {playbook} {label(e)} as {e.user}" + (f": skipped, {reason}" if reason else ""))
            if cmd:
                print(fill(cmd, ctx.values))
            if cmd and e.probe:
                print(fill(e.probe, ctx.values))
    return 0


def usage() -> str:
    lines = ["usage: playbooks.py check | list | run <tag> [github|tree|<release-dir>] [--keep]", "", "the human's answers (unset is no, or the default):"]
    lines += [f"  {k}: {v}" for k, v in HUMAN_INPUTS.items()]
    return "\n".join(lines)


def main(argv: list[str]) -> int:
    if not argv or argv[0] in ("-h", "--help"):
        print(usage(), file=sys.stderr)
        return 0 if argv else 2
    try:
        if argv[0] == "check" and len(argv) == 1:
            problems = check(read_docs())
            for p in problems:
                print(f"playbooks: {p}", file=sys.stderr)
            return 1 if problems else 0
        if argv[0] == "list" and len(argv) == 1:
            return list_commands()
        if argv[0] == "run" and len(argv) >= 2:
            rest = argv[2:]
            keep = "--keep" in rest
            rest = [a for a in rest if a != "--keep"]
            if len(rest) <= 1:
                return run(argv[1], rest[0] if rest else "github", keep)
    except (GateError, OSError, subprocess.SubprocessError) as e:
        print(f"playbooks: {e}", file=sys.stderr)
        return 1
    print(usage(), file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
