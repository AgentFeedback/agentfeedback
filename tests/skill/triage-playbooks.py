#!/usr/bin/env python3
"""Run the agentfeedback-triage playbooks against a throwaway server.

Checks the structure of skills/agentfeedback-triage/playbooks/*.md, then, per
page, starts `agentfeedback serve` on a temp database and a free port, seeds
frictions through the CLI, creates a temp git checkout whose one commit names
a seeded friction id, and runs every ```bash block of the page, concatenated
in order, as one `bash -euo pipefail` script. SKILL.md's ```bash blocks, if
any, are checked and run the same way, and so are reference/clustering.md's,
after the prelude `DIGEST=$(agentfeedback digest --kind friction)`, with
TYPESAFE_API_KEY empty so cluster.py stops at missing_key and sends nothing.
After a page runs, its effects are checked: the verdicts it marked, and for
clustering that the digest holds only frictions.

Every child process gets an environment built from scratch: a throwaway URL,
key, HOME and XDG directories, so an inherited AGENT_FEEDBACK_API_KEY is
never used.

Placeholders in ```bash blocks are replaced before the run:

  <project>               a seeded project with open rows
  <model>                 a seeded model
  <text>                  a word in that model's seeded summaries
  <id>                    an open friction with --fix-status applied, named in <commit>
  <duplicate-id>          the second of two rows sharing a content_hash
  <uid>                   the uid of the first of those two rows
  <commit>                the one commit in <checkout>
  <checkout>              a temp git repository on branch main
  <previous-triage-date>  yesterday, YYYY-MM-DD
  <resolution>            a fixed resolution text
  <skill-dir>             this repository's skills/agentfeedback-triage
  <file>                  export.ndjson in the page's temp directory
  <increment-file>        export-increment.ndjson in the page's temp directory
  <db>                    a database path that does not exist yet
  <target-url>            a second throwaway server (TARGET_KEY holds its key)
  <remote>                the context.git_remote of the p-alpha frictions

Usage: python3 tests/skill/triage-playbooks.py   (after `just check`)
"""
import datetime
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
SKILL = ROOT / "skills/agentfeedback-triage"
BIN = ROOT / "bin/agentfeedback"
KEY = "playbook-check-key"
TARGET_KEY = "playbook-target-key"
PAGES = ("close-the-loop.md", "export-handoff.md", "fix-it-session.md",
         "model-struggles.md", "recurring-by-project.md", "weekly-digest.md")
SECTIONS = ("Purpose", "Commands", "Reading the output", "What to do with it",
            "What the hosted version adds")
PLACEHOLDERS = {"project", "model", "text", "id", "duplicate-id", "uid", "commit", "checkout",
                "previous-triage-date", "resolution", "skill-dir", "file", "increment-file",
                "db", "target-url", "remote"}
REMOTE = "git@example.invalid:check/repository.git"
OPTIONAL_TOOL = re.compile(r"(?:^|[;&|(]|\$\(|\b(?:then|do|xargs|sudo|command|exec)\s)\s*(gh|slack|linear|jira)\b")
PLACEHOLDER = re.compile(r"<([a-z][a-z0-9-]*)>")
FENCE = re.compile(r"^```bash\n(.*?)^```$", re.M | re.S)


def bash_blocks(text):
    return FENCE.findall(text)


def structure_problems(name, text, playbook=True):
    problems = []
    if playbook:
        found = [line[3:].strip() for line in text.splitlines() if line.startswith("## ")]
        if found != list(SECTIONS):
            problems.append(f"{name}: H2 sections are {found}, want {list(SECTIONS)}")
    for block in bash_blocks(text):
        for line in block.splitlines():
            if OPTIONAL_TOOL.search(line):
                problems.append(f"{name}: optional tool in a bash block: {line}")
            for token in PLACEHOLDER.findall(line):
                if token not in PLACEHOLDERS:
                    problems.append(f"{name}: unknown placeholder <{token}>: {line}")
    return problems


def free_port():
    while True:
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        if port != 18080:
            return port


class Server:
    def __init__(self, work, name, key):
        self.port = free_port()
        self.url = f"http://127.0.0.1:{self.port}"
        self.log = open(work / f"{name}.log", "w")
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(work / "home"),
               "API_KEY": key, "DATABASE_PATH": str(work / f"{name}.db"),
               "HTTP_LISTEN_ADDR": f"127.0.0.1:{self.port}"}
        self.proc = subprocess.Popen([str(BIN), "serve"], env=env, cwd=work,
                                     stdout=self.log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                break
            try:
                with urllib.request.urlopen(self.url + "/ready", timeout=1) as response:
                    if response.status == 200:
                        return
            except OSError:
                time.sleep(0.1)
        self.stop()
        raise RuntimeError(f"server {name} did not become ready; log: {(work / f'{name}.log').read_text()}")

    def stop(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(5)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
        self.log.close()


def client_env(work, url):
    tools = work / "bin"
    tools.mkdir(exist_ok=True)
    link = tools / "agentfeedback"
    if not link.exists():
        link.symlink_to(BIN)
    for name in ("home", "cache", "config", "tmp"):
        (work / name).mkdir(exist_ok=True)
    return {"PATH": f"{tools}:{os.environ.get('PATH', '/usr/bin:/bin')}",
            "HOME": str(work / "home"), "XDG_CACHE_HOME": str(work / "cache"),
            "XDG_CONFIG_HOME": str(work / "config"), "TMPDIR": str(work / "tmp"),
            "LANG": "C.UTF-8", "GIT_CONFIG_NOSYSTEM": "1",
            "AGENT_FEEDBACK_URL": url, "AGENT_FEEDBACK_API_KEY": KEY, "TARGET_KEY": TARGET_KEY,
            "TYPESAFE_API_KEY": ""}


def run(args, env, cwd, stdin=None):
    proc = subprocess.run(args, env=env, cwd=cwd, input=stdin, capture_output=True, text=True)
    if proc.returncode:
        raise RuntimeError(f"{' '.join(map(str, args))} exited {proc.returncode}:\n{proc.stdout}{proc.stderr}")
    return proc.stdout


def submit(env, work, summary, category, project, model, *extra, remote=None):
    stdin = json.dumps({"context": {"git_remote": remote}}) if remote else None
    out = run([BIN, "submit", "friction", "--summary", summary, "--category", category,
               "--details", f"details of {summary}", "--suggested-fix", f"fix {summary}",
               "--project", project, "--model", model, "--machine", "check-host",
               "--harness", "check-harness", *extra, *(["--stdin"] if remote else [])], env, work, stdin)
    return json.loads(out.strip().splitlines()[-1])["id"]


def get(env, work, row_id):
    return json.loads(run([BIN, "get", str(row_id), "--json"], env, work))


def seed(env, work):
    """Seed frictions and a checkout; return the placeholder values."""
    applied = submit(env, work, "broken flag in deploy", "tooling", "p-alpha", "model-a",
                     "--fix-status", "applied", "--fix-ref", "fix-branch", "--severity", "high", remote=REMOTE)
    kept = submit(env, work, "docs flag is wrong", "docs", "p-alpha", "model-a", "--key", "dup-kept", remote=REMOTE)
    repeat = submit(env, work, "docs flag is wrong", "docs", "p-alpha", "model-a", "--key", "dup-repeat", remote=REMOTE)
    run([BIN, "submit", "event", "--stdin", "--project", "p-alpha", "--model", "model-a"], env, work,
        json.dumps({"summary": "check event", "name": "check"}))
    submit(env, work, "config path ignored", "config", "p-beta", "model-b")
    closed = submit(env, work, "tooling flag crash", "tooling", "p-beta", "model-b",
                    "--fix-status", "applied", "--fix-ref", "abc1234")
    invalid = submit(env, work, "environment flag missing", "environment", "p-alpha", "model-b")
    run([BIN, "done", str(closed), "--verdict", "fixed", "--ref", "abc1234", "--resolution", "seeded"], env, work)
    run([BIN, "done", str(invalid), "--verdict", "invalid", "--resolution", "seeded"], env, work)
    rows = {i: get(env, work, i) for i in (kept, repeat)}
    if kept == repeat or rows[kept]["uid"] == rows[repeat]["uid"]:
        raise RuntimeError("seeded repeats are one row, not two")
    if rows[kept]["content_hash"] != rows[repeat]["content_hash"]:
        raise RuntimeError("seeded repeats do not share a content_hash")
    checkout = work / "checkout"
    checkout.mkdir()
    git = ["git", "-C", str(checkout), "-c", "user.name=check", "-c", "user.email=check@example.invalid"]
    run([*git, "init", "-q", "-b", "main"], env, work)
    (checkout / "deploy.txt").write_text("fixed\n")
    run([*git, "add", "deploy.txt"], env, work)
    run([*git, "commit", "-q", "-m", f"fix: broken flag in deploy\n\nfriction {applied}"], env, work)
    commit = run([*git, "rev-parse", "--short", "HEAD"], env, work).strip()
    yesterday = (datetime.date.today() - datetime.timedelta(days=1)).isoformat()
    return {"project": "p-alpha", "model": "model-a", "text": "flag", "id": str(applied),
            "duplicate-id": str(repeat), "uid": rows[kept]["uid"], "commit": commit,
            "checkout": str(checkout), "previous-triage-date": yesterday,
            "resolution": "verified by triage-playbooks", "skill-dir": str(SKILL),
            "file": str(work / "export.ndjson"), "increment-file": str(work / "export-increment.ndjson"),
            "db": str(work / "restored.db"), "remote": REMOTE}


def effects(name, env, work, values):
    """Return the problems with what the page left behind."""
    problems = []

    def verdict(row_id, want, ref=None):
        row = get(env, work, row_id)
        if row.get("verdict") != want or not row.get("processed_at"):
            problems.append(f"row {row_id}: verdict {row.get('verdict')!r}, want {want!r}")
        if ref is not None and row.get("ref") != ref:
            problems.append(f"row {row_id}: ref {row.get('ref')!r}, want {ref!r}")
    if name in ("playbooks/fix-it-session.md", "playbooks/close-the-loop.md"):
        verdict(values["id"], "fixed", values["commit"])
    if name == "playbooks/fix-it-session.md":
        verdict(values["duplicate-id"], "duplicate", values["uid"])
    if name == "reference/clustering.md":
        digest = Path((work / "digest-dir").read_text().strip())
        kinds = {row["kind"] for row in json.loads((digest / "index.json").read_text())}
        if kinds != {"friction"}:
            problems.append(f"digest index kinds {sorted(kinds)}, want only friction")
        advice = json.loads((digest / "clusters.json").read_text())
        if (advice.get("status"), advice.get("reason")) != ("skipped", "missing_key"):
            problems.append(f"cluster.py: {advice.get('status')}/{advice.get('reason')}, want skipped/missing_key")
    return problems


def run_page(name, text, prelude="", postlude=""):
    blocks = bash_blocks(text)
    if not blocks:
        return True
    work = Path(tempfile.mkdtemp(prefix="triage-playbooks-"))
    servers = []
    try:
        servers.append(Server(work, "source", KEY))
        env = client_env(work, servers[0].url)
        values = seed(env, work)
        script = prelude + "\n".join(blocks) + postlude
        if "<target-url>" in script:
            servers.append(Server(work, "target", TARGET_KEY))
            values["target-url"] = servers[1].url
        script = PLACEHOLDER.sub(lambda match: values[match.group(1)], script)
        proc = subprocess.run(["bash", "-euo", "pipefail", "-c", script], env=env, cwd=work,
                              capture_output=True, text=True, timeout=120)
        if proc.returncode:
            print(f"FAIL  {name} (exit {proc.returncode})\n--- page\n{text}\n--- script\n{script}\n"
                  f"--- stdout\n{proc.stdout}\n--- stderr\n{proc.stderr}")
            return False
        problems = effects(name, env, work, values)
        if problems:
            print(f"FAIL  {name}\n" + "\n".join(f"  {problem}" for problem in problems)
                  + f"\n--- stdout\n{proc.stdout}\n--- stderr\n{proc.stderr}")
            return False
        print(f"PASS  {name}")
        return True
    finally:
        for server in servers:
            server.stop()
        shutil.rmtree(work, ignore_errors=True)


def main():
    if not BIN.is_file():
        print(f"triage-playbooks: {BIN} is missing: run just check")
        return 1
    pages = SKILL / "playbooks"
    found = sorted(path.name for path in pages.glob("*.md"))
    if found != sorted(PAGES):
        print(f"triage-playbooks: playbooks/ holds {found}, want {sorted(PAGES)}")
        return 1
    texts = {f"playbooks/{page}": (pages / page).read_text() for page in PAGES}
    skill = (SKILL / "SKILL.md").read_text()
    clustering = (SKILL / "reference/clustering.md").read_text()
    problems = [problem for name, text in texts.items() for problem in structure_problems(name, text)]
    problems += structure_problems("SKILL.md", skill, playbook=False)
    problems += structure_problems("reference/clustering.md", clustering, playbook=False)
    if problems:
        print("\n".join(f"FAIL  {problem}" for problem in problems))
        return 1
    print(f"PASS  structure of {len(texts)} playbooks, SKILL.md and reference/clustering.md")
    ok = all([run_page(name, text) for name, text in [("SKILL.md", skill), *texts.items()]])
    ok = run_page("reference/clustering.md", clustering,
                  prelude="DIGEST=$(agentfeedback digest --kind friction)\n",
                  postlude="\nprintf '%s\\n' \"$DIGEST\" > digest-dir\n") and ok
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
