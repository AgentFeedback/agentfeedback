#!/usr/bin/env python3
"""Tests for scripts/playbooks.py: the parser, the structure check, the
placeholder and release rewrites. No Docker; the container run is
`just playbooks`.

Usage: python3 tests/playbooks/test_playbooks.py
"""

import hashlib
import importlib.util
import shutil
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("playbooks", ROOT / "scripts/playbooks.py")
pb = importlib.util.module_from_spec(spec)
sys.modules["playbooks"] = pb
spec.loader.exec_module(pb)

STUB = """# A stub playbook

Intro text, belongs to no step.

```text
an example, not a command
```

## Step 0: First

```sh
echo zero
```

**Verify:** prints zero.

**Outcome:**

```json
{"step": "0", "status": "ok"}
```

## Step 1: Branch

### Step 1.1: Chosen

```sh
echo one
```

**Verify:** prints one.

**Outcome:**

```json
{"step": "1.1", "status": "ok"}
```

### Step 1.2: For humans

```text
nothing to run
```

**Verify:** the human confirms.

**Outcome:**

```json
{"step": "1.2", "status": "ok"}
```
"""

ROUTES = {"stub": [pb.Entry("stub.md", "0", "u"), pb.Entry("stub.md", "1.1", "u")]}


def problems(text, routes=ROUTES, excluded=None):
    return pb.check({"stub.md": text}, routes, excluded or {})


class Parse(unittest.TestCase):
    def test_steps_blocks_and_outcomes(self):
        steps = pb.parse(STUB)
        self.assertEqual(list(steps), [pb.OUTSIDE, "0", "1", "1.1", "1.2"])
        self.assertEqual(steps["0"].sh(), ["echo zero"])
        self.assertEqual(steps["1"].sh(), [])
        self.assertEqual(steps["1.1"].outcomes(), ['{"step": "1.1", "status": "ok"}'])
        self.assertEqual(steps["1.2"].sh(), [])

    def test_unterminated_fence(self):
        with self.assertRaises(pb.GateError):
            pb.parse("## Step 0: x\n\n```sh\necho\n")

    def test_indented_fence(self):
        with self.assertRaisesRegex(pb.GateError, "indented fence"):
            pb.parse("## Step 0: x\n\n   ```sh\n   echo\n   ```\n")


class Check(unittest.TestCase):
    def test_stub_is_clean(self):
        self.assertEqual(problems(STUB), [])

    def test_removed_outcome_fails(self):
        broken = STUB.replace('**Outcome:**\n\n```json\n{"step": "1.1", "status": "ok"}\n```\n', "")
        self.assertNotEqual(broken, STUB)
        self.assertTrue(any("step 1.1: no single **Outcome:**" in p for p in problems(broken)), problems(broken))

    def test_removed_outcome_marker_fails(self):
        broken = STUB.replace("**Verify:** prints one.\n\n**Outcome:**", "**Verify:** prints one.")
        self.assertTrue(any("step 1.1: no single **Outcome:**" in p for p in problems(broken)), problems(broken))

    def test_outcome_not_json_fails(self):
        broken = STUB.replace('{"step": "0", "status": "ok"}', '{"step": "0", "status": ok}')
        self.assertTrue(any("step 0: the outcome is not JSON" in p for p in problems(broken)))

    def test_outcome_names_another_step_fails(self):
        broken = STUB.replace('{"step": "0", "status": "ok"}', '{"step": "9", "status": "ok"}')
        self.assertTrue(any('"step": "0"' in p for p in problems(broken)))

    def test_missing_verify_fails(self):
        broken = STUB.replace("**Verify:** prints zero.\n", "")
        self.assertTrue(any("step 0: 0 **Verify:**" in p for p in problems(broken)))

    def test_second_command_fails(self):
        broken = STUB.replace("echo zero\n```", "echo zero\n```\n\n```sh\necho again\n```")
        self.assertTrue(any("step 0: 2 sh blocks" in p for p in problems(broken)))

    def test_unrouted_block_fails(self):
        extra = STUB + "\n## Step 2: New\n\n```sh\necho two\n```\n"
        self.assertTrue(any("step 2: an sh block no route runs" in p for p in problems(extra)))
        self.assertFalse(any("no route runs" in p for p in problems(extra, excluded={("stub.md", "2"): "reason"})))

    def test_sh_outside_every_step_fails(self):
        broken = STUB.replace("```text\nan example", "```sh\nan example")
        self.assertTrue(any("outside every step" in p for p in problems(broken)), problems(broken))
        broken = STUB + "\n## Notes\n\n```sh\necho after\n```\n"
        self.assertTrue(any("outside every step" in p for p in problems(broken)), problems(broken))

    def test_excluded_step_structure_is_checked(self):
        extra = STUB + "\n## Step 2: New\n\n```sh\necho two\n```\n"
        found = problems(extra, excluded={("stub.md", "2"): "reason"})
        self.assertTrue(any("step 2: 0 **Verify:**" in p for p in found), found)
        self.assertTrue(any("step 2: no single **Outcome:**" in p for p in found), found)
        # A human-only step needs no command.
        self.assertEqual(problems(STUB, excluded={("stub.md", "1.2"): "for humans"}), [])

    def test_route_names_missing_step(self):
        routes = {"stub": ROUTES["stub"] + [pb.Entry("stub.md", "3", "u")]}
        self.assertTrue(any("step 3, which the playbook does not have" in p for p in problems(STUB, routes)))

    def test_real_playbooks_are_clean(self):
        self.assertEqual(pb.check(pb.read_docs()), [])


class Commands(unittest.TestCase):
    INSTALL = pb.parse(pb.read_docs()[pb.CLIENT])["2.1"].sh()[0]

    def test_substitute(self):
        self.assertEqual(pb.substitute('"<binary>" x < \'<key_file>\'', {"binary": "/b", "key_file": "/k"}), "\"/b\" x < '/k'")
        with self.assertRaisesRegex(pb.GateError, "<URL>"):
            pb.substitute("curl <URL>", {})

    def test_pin_published_tag(self):
        cmd = pb.pin_release(self.INSTALL, "v4.0.0-rc.1", None)
        self.assertIn(f"{pb.RELEASES}/download/v4.0.0-rc.1/install.sh", cmd)
        self.assertIn(f"{pb.RELEASES}/download/v4.0.0-rc.1/SHA256SUMS", cmd)
        self.assertNotIn("latest/download", cmd)
        self.assertIn('bash "$d/install.sh" --version v4.0.0-rc.1', cmd)
        self.assertEqual(cmd.count("--version"), 1)
        self.assertIn("--proto '=https'", cmd)

    def test_pin_file_release(self):
        cmd = pb.pin_release(self.INSTALL, "v4.0.0", "file:///release")
        self.assertIn("file:///release/download/v4.0.0/install.sh", cmd)
        self.assertIn("file:///release/download/v4.0.0/SHA256SUMS", cmd)
        self.assertIn("--proto '=file'", cmd)
        self.assertNotIn("github.com", cmd)
        self.assertNotIn("=https", cmd)

    def test_pin_refuses_other_shape(self):
        with self.assertRaises(pb.GateError):
            pb.pin_release("curl https://example.com/install.sh | sh", "v4.0.0", None)
        # SHA256SUMS fetched from another release than install.sh.
        with self.assertRaises(pb.GateError):
            pb.pin_release(self.INSTALL.replace("latest/download/SHA256SUMS", "download/v1/SHA256SUMS"), "v4.0.0", None)
        # Both downloaded, but install.sh run without the check.
        unchecked = self.INSTALL.replace(pb.VERIFY_LINE + " && ", "").split(" && (cd ")[0] + ' && bash "$d/install.sh")'
        self.assertNotEqual(unchecked, self.INSTALL)
        with self.assertRaisesRegex(pb.GateError, "before running it"):
            pb.pin_release(unchecked, "v4.0.0", None)

    @unittest.skipUnless(shutil.which("curl") and (shutil.which("sha256sum") or shutil.which("shasum")), "needs curl and sha256sum or shasum")
    def test_install_runs_only_when_its_checksum_matches(self):
        """Step 2.1's command, pinned to a file:// release whose install.sh
        is a stand-in that prints a path: it runs only when SHA256SUMS lists
        it with its checksum."""
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            rel = Path(tmp) / "download" / "v4.0.0"
            rel.mkdir(parents=True)
            script = b"echo /stand-in/agentfeedback\n"
            (rel / "install.sh").write_bytes(script)
            digest = hashlib.sha256(script).hexdigest()
            archive = f"{'0' * 64}  agentfeedback_4.0.0_linux_amd64.tar.gz\n"
            cmd = pb.pin_release(self.INSTALL, "v4.0.0", f"file://{tmp}")
            run = lambda: subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, timeout=30)
            (rel / "SHA256SUMS").write_text(f"{archive}{digest}  install.sh\n")
            r = run()
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(r.stdout.splitlines(), ["install.sh: OK", "/stand-in/agentfeedback"])
            # No install.sh line: nothing printed. A wrong one: FAILED.
            for sums, stdout in ((archive, []), (f"{archive}{'1' * 64}  install.sh\n", ["install.sh: FAILED"])):
                (rel / "SHA256SUMS").write_text(sums)
                r = run()
                self.assertNotEqual(r.returncode, 0)
                self.assertEqual(r.stdout.splitlines(), stdout)

    def skipped(self, route, environ):
        docs = pb.read_docs()
        return {(e.doc, e.step) for e, _, reason in pb.commands(docs, route, pb.answers(environ), "v4.0.0", None) if reason}

    def test_answers_skip_steps(self):
        self.assertEqual(self.skipped(pb.STACK_ROUTE, {}), {(pb.STACK, "S4")})
        self.assertEqual(self.skipped(pb.STACK_ROUTE, {"AF_PLAYBOOK_ADDRESS": "0.0.0.0"}), set())
        self.assertEqual(self.skipped(pb.CLIENT_ROUTE, {}), set())
        self.assertEqual(self.skipped(pb.LOCAL_ROUTE, {}), set())

    def test_local_route_has_no_server(self):
        steps = [e.step for e in pb.LOCAL_ROUTE]
        self.assertNotIn("1", steps)
        self.assertNotIn("2.2", steps)
        self.assertEqual({e.user for e in pb.LOCAL_ROUTE}, {"local"})
        self.assertEqual(next(iter(pb.ROUTES)), "client-local")
        ctx = pb.answers({})
        ctx.values["binary"] = "/b"
        pb.begin("client-local", ctx)
        self.assertEqual(ctx.values["target"], "local")
        self.assertNotIn("binary", ctx.values)
        pb.begin("client", ctx)
        self.assertEqual(ctx.values["target"], "server")

    def test_local_init_adds_local(self):
        docs = pb.read_docs()
        for playbook, want in (("client-local", True), ("client", False)):
            ctx = pb.answers({})
            pb.begin(playbook, ctx)
            cmd = next(c for e, c, _ in pb.commands(docs, pb.ROUTES[playbook], ctx, "v4.0.0", None) if e.step == "2.3")
            self.assertEqual(cmd.endswith(" --local"), want, cmd)
        with self.assertRaises(pb.GateError):
            pb.local_init("agentfeedback install claude-code --json")

    def test_answers_are_validated(self):
        self.assertEqual(pb.answers({"AF_PLAYBOOK_HARNESS": ""}).values["harness"], "claude-code")
        self.assertEqual(pb.answers({"AF_PLAYBOOK_HARNESS": "codex"}).values["harness"], "codex")
        for bad in ({"AF_PLAYBOOK_HARNESS": "all"}, {"AF_PLAYBOOK_HARNESS": "x; rm -rf ~"},
                    {"AF_PLAYBOOK_ADDRESS": "10.0.0.1'; id; '"}, {"AF_PLAYBOOK_ADDRESS": "::1"},
                    {"AF_PLAYBOOK_AGENT": "gemini-cli"}, {"AF_PLAYBOOK_NO_BINARY": "yes"}):
            with self.assertRaises(pb.GateError, msg=bad):
                pb.answers(bad)


class Verify(unittest.TestCase):
    def ctx(self, **values):
        return pb.Ctx(values, {})

    def test_unauthorized(self):
        pb.x_unauthorized('{"error":"unauthorized","message":"send X-Api-Key"}\n401\n', self.ctx())
        with self.assertRaises(pb.GateError):
            pb.x_unauthorized("<html>login</html>\n200\n", self.ctx())

    def test_server_init_derives_and_refuses_the_key(self):
        c = self.ctx()
        out = '{"status":"written","dir":"/d","key_file":"/d/api-key","env_file":"/d/serve.env","database":"/x.db","listen":"127.0.0.1:8090"}\n'
        pb.x_server_init(out, c)
        self.assertEqual((c.values["key_file"], c.values["port"]), ("/d/api-key", "8090"))
        with self.assertRaises(pb.GateError):
            pb.x_server_init(out.replace('"status"', '"api_key":"k","status"'), c)

    def test_e2e_needs_three_steps_one_id(self):
        ok = '{"step":"submit","outcome":"ok","id":3}\n{"step":"list","outcome":"ok","id":3}\n{"step":"mark","outcome":"ok","id":3}\n'
        pb.x_e2e(ok, self.ctx())
        with self.assertRaises(pb.GateError):
            pb.x_e2e(ok.replace('"id":3}\n{"step":"mark"', '"id":4}\n{"step":"mark"'), self.ctx())

    def test_binary(self):
        c = self.ctx()
        pb.x_binary("install.sh: OK\n/home/u/.local/bin/agentfeedback\n", c)
        self.assertEqual(c.values["binary"], "/home/u/.local/bin/agentfeedback")
        for out in ("install.sh: OK\ninstalled\n", "/home/u/.local/bin/agentfeedback\n", "install.sh: FAILED\n"):
            with self.assertRaises(pb.GateError):
                pb.x_binary(out, c)

    def test_init_and_list(self):
        h = {"name": "claude-code", "mode": "cli", "skill": "wired", "hook": "wired"}
        e2e = [{"step": "submit", "outcome": "ok", "id": 1}, {"step": "list", "outcome": "ok", "id": 1}, {"step": "mark", "outcome": "ok", "id": 1}]
        local = {"status": "ok", "mode": "local", "harnesses": [h], "e2e": e2e}
        remote = {**local, "mode": "remote", "server": "http://127.0.0.1:8090", "config": "/c.toml"}
        c = self.ctx(harness="claude-code", target="local")
        pb.x_init(pb.json.dumps(local), c)
        pb.x_list(pb.json.dumps({"harnesses": [h]}), c)
        s = self.ctx(harness="claude-code", target="server", URL="http://127.0.0.1:8090")
        pb.x_init(pb.json.dumps(remote), s)
        for ctx, change in ((c, {"mode": "remote"}), (c, {"config": "/c.toml"}), (s, {"server": "http://other:1"}), (s, {"mode": "local"}),
                            (c, {"harnesses": [{**h, "hook": "missing"}]}), (c, {"status": "error"}), (c, {"e2e": e2e[:2]}),
                            (c, {"e2e": e2e[:2] + [{**e2e[2], "id": 2}]}), (c, {"e2e": e2e[:2] + [{**e2e[2], "outcome": "error"}]})):
            base = local if ctx is c else remote
            with self.assertRaises(pb.GateError, msg=change):
                pb.x_init(pb.json.dumps({**base, **change}), ctx)
        with self.assertRaises(pb.GateError):
            pb.x_list(pb.json.dumps({"harnesses": [{**h, "mode": "-"}]}), c)

    def test_doctor(self):
        c = self.ctx(URL="http://127.0.0.1:8090", target="server")
        ok = {"status": "ok", "problems": [], "mode": "remote", "meta": {"ok": True}, "url": {"value": "http://127.0.0.1:8090", "source": "config"}, "api_key": {"set": True, "source": "config"}}
        pb.x_doctor(pb.json.dumps(ok), c)
        for change in ({"status": "error"}, {"problems": ["too old"]}, {"url": {"value": "http://127.0.0.1:8090", "source": "env"}},
                       {"api_key": {"set": True, "source": "env"}}, {"meta": None}, {"mode": "local"}):
            with self.assertRaises(pb.GateError, msg=change):
                pb.x_doctor(pb.json.dumps({**ok, **change}), c)
        local = self.ctx(target="local")
        lok = {"status": "ok", "problems": [], "mode": "local", "meta": {"ok": True}, "database": {"path": "/d.db", "exists": True}, "url": {"value": "", "source": ""}}
        pb.x_doctor(pb.json.dumps(lok), local)
        for change in ({"mode": "remote"}, {"database": {"path": "/d.db", "exists": False}}, {"url": {"value": "http://x", "source": "env"}}, {"meta": {"ok": False}}):
            with self.assertRaises(pb.GateError, msg=change):
                pb.x_doctor(pb.json.dumps({**lok, **change}), local)

    def test_service_and_exposure(self):
        c = self.ctx(port="8090", address="0.0.0.0")
        pb.x_service('{"status":"written","mode":"systemd","path":"/u.service","next":["a","b","c","d","e","f"]}', c)
        self.assertEqual(c.values["path"], "/u.service")
        with self.assertRaises(pb.GateError):
            pb.x_service('{"status":"written","mode":"systemd","path":"/u.service","next":[]}', c)
        pb.x_exposed("HTTP_LISTEN_ADDR=0.0.0.0:8090\n", c)
        self.assertEqual((c.values["listen"], c.values["probe"]), ("0.0.0.0:8090", "127.0.0.1:8090"))
        with self.assertRaises(pb.GateError):
            pb.x_exposed("HTTP_LISTEN_ADDR=127.0.0.1:8090\n", c)

    def test_redact(self):
        self.assertEqual(pb.redact("k=" + "a" * 64), "k=[REDACTED]")


class Staging(unittest.TestCase):
    def test_release_directory(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            src = Path(tmp) / "dist"
            src.mkdir()
            (src / "SHA256SUMS").write_text("x  agentfeedback_4.0.0_linux_amd64.tar.gz\n")
            (src / "agentfeedback_4.0.0_linux_amd64.tar.gz").write_bytes(b"a")
            (src / "agentfeedback_4.0.0_darwin_arm64.tar.gz").write_bytes(b"b")
            work = Path(tmp) / "work"
            work.mkdir()
            self.assertEqual(pb.stage_release("v4.0.0", str(src), work), "file:///release")
            got = sorted(p.name for p in (work / "release/download/v4.0.0").iterdir())
            self.assertEqual(got, ["SHA256SUMS", "agentfeedback_4.0.0_linux_amd64.tar.gz", "install.sh"])
            mode = (work / "release/download/v4.0.0/install.sh").stat().st_mode & 0o777
            self.assertEqual(mode, 0o644)

    def test_release_directory_without_the_tag(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            src = Path(tmp)
            (src / "SHA256SUMS").write_text("")
            work = src / "work"
            work.mkdir()
            with self.assertRaisesRegex(pb.GateError, "no linux archive"):
                pb.stage_release("v4.0.1", str(src), work)


if __name__ == "__main__":
    unittest.main(verbosity=1)
