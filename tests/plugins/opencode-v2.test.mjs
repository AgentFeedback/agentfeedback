// The generated opencode-v2 plugin against a fake OpenCode 2 ctx and a fake
// agentfeedback binary. Run by TestOpenCodeV2PluginNode in internal/harness,
// which renders the plugins and sets:
//   AF_PLUGIN          plugin whose BIN is the fake binary
//   AF_PLUGIN_MISSING  plugin whose BIN does not exist
//   AF_FAKE_LOG        log the fake binary appends "ARGS: $1 $2 $3" and its stdin to
import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync, rmSync } from "node:fs";

const { AF_PLUGIN, AF_PLUGIN_MISSING, AF_FAKE_LOG } = process.env;

// fakeCtx is one location instance at dir; sessions maps a session id to
// its directory, and session.get throws for an unknown id.
const sessions = { ses_1: "/work/dir", ses_b: "/work/b" };

function fakeCtx(dir = "/work/dir") {
  const hooks = {};
  const puts = [];
  const queue = [];
  let wake = null;
  let aborted = false;
  const ctx = {
    location: { directory: dir },
    tool: {
      hook: async (name, fn) => {
        hooks[name] = fn;
      },
    },
    event: {
      subscribe({ types, signal }) {
        ctx.subscribedTypes = types;
        signal.addEventListener("abort", () => {
          aborted = true;
          wake?.();
        });
        return {
          [Symbol.asyncIterator]() {
            return {
              async next() {
                while (!aborted && queue.length === 0) {
                  await new Promise((r) => (wake = r));
                  wake = null;
                }
                if (aborted) return { done: true, value: undefined };
                return { done: false, value: queue.shift() };
              },
            };
          },
        };
      },
    },
    session: {
      get: async ({ sessionID }) => {
        if (!(sessionID in sessions)) throw new Error("no session " + sessionID);
        return { id: sessionID, location: { directory: sessions[sessionID] } };
      },
      instructions: {
        entry: {
          put: async (v) => {
            puts.push(v);
          },
        },
      },
    },
  };
  return {
    ctx,
    hooks,
    puts,
    emit(ev) {
      queue.push(ev);
      wake?.();
    },
    get aborted() {
      return aborted;
    },
  };
}

// calls parses the fake binary's log into [{args, payload}].
function calls() {
  if (!existsSync(AF_FAKE_LOG)) return [];
  const lines = readFileSync(AF_FAKE_LOG, "utf8").split("\n");
  const out = [];
  for (let i = 0; i < lines.length; i++) {
    if (lines[i].startsWith("ARGS: ")) {
      out.push({ args: lines[i].slice(6), payload: JSON.parse(lines[i + 1]) });
    }
  }
  return out;
}

async function until(cond, what) {
  for (let i = 0; i < 200; i++) {
    if (cond()) return;
    await new Promise((r) => setTimeout(r, 10));
  }
  assert.fail("timed out waiting for " + what);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

test("opencode-v2 plugin", async (t) => {
  const plugin = (await import(AF_PLUGIN)).default;
  assert.equal(plugin.id, "agentfeedback");
  const f = fakeCtx();
  const cleanup = await plugin.setup(f.ctx);
  assert.equal(typeof cleanup, "function");
  assert.deepEqual(f.ctx.subscribedTypes, [
    "session.execution.succeeded",
    "session.execution.failed",
    "session.execution.interrupted",
  ]);

  await t.test("shell failure runs the hook", async () => {
    rmSync(AF_FAKE_LOG, { force: true });
    await f.hooks["execute.after"]({
      tool: "shell",
      sessionID: "ses_1",
      input: { command: "make test", description: "run" },
      status: "completed",
      result: { metadata: { exit: 2 } },
    });
    assert.deepEqual(calls(), [
      {
        args: "hook opencode-v2 tool.execute.after",
        payload: { sessionID: "ses_1", tool: "shell", args: { command: "make test", description: "run" }, exit: 2, cwd: "/work/dir" },
      },
    ]);
  });

  await t.test("success and other tools are ignored", async () => {
    rmSync(AF_FAKE_LOG, { force: true });
    await f.hooks["execute.after"]({ tool: "shell", sessionID: "ses_1", input: {}, result: { metadata: { exit: 0 } } });
    await f.hooks["execute.after"]({ tool: "read", sessionID: "ses_1", input: {}, result: { metadata: { exit: 1 } } });
    await f.hooks["execute.after"]({ tool: "shell", sessionID: "ses_1", input: {}, result: {} });
    assert.deepEqual(calls(), []);
  });

  await t.test("turn end delivers the note once per event id", async () => {
    rmSync(AF_FAKE_LOG, { force: true });
    const ev = { id: "evt_1", type: "session.execution.succeeded", data: { sessionID: "ses_1" } };
    f.emit(ev);
    f.emit({ ...ev });
    await until(() => f.puts.length >= 1, "instructions entry");
    await sleep(100);
    assert.deepEqual(calls(), [{ args: "hook opencode-v2 session.idle", payload: { sessionID: "ses_1", cwd: "/work/dir" } }]);
    assert.deepEqual(f.puts, [{ sessionID: "ses_1", key: "agentfeedback", value: "fake note" }]);
  });

  await t.test("another location's session runs with its own directory", async () => {
    rmSync(AF_FAKE_LOG, { force: true });
    f.puts.length = 0;
    const b = fakeCtx("/work/b");
    const cleanupB = await plugin.setup(b.ctx);
    const ev = { id: "evt_b", type: "session.execution.succeeded", data: { sessionID: "ses_b" } };
    f.emit(ev);
    await until(() => f.puts.length >= 1, "instructions entry");
    b.emit({ ...ev });
    await sleep(100);
    assert.deepEqual(calls(), [{ args: "hook opencode-v2 session.idle", payload: { sessionID: "ses_b", cwd: "/work/b" } }]);
    assert.deepEqual(b.puts, []);
    cleanupB();
  });

  await t.test("a session without a directory runs the hook without cwd and delivers nothing", async () => {
    rmSync(AF_FAKE_LOG, { force: true });
    f.puts.length = 0;
    f.emit({ id: "evt_unknown", type: "session.execution.succeeded", data: { sessionID: "ses_gone" } });
    await sleep(200);
    // A hook run without a cwd would consume the pending note undelivered.
    assert.deepEqual(calls(), []);
    assert.deepEqual(f.puts, []);
  });

  await t.test("a failed lookup does not claim the event from the owning instance", async () => {
    rmSync(AF_FAKE_LOG, { force: true });
    f.puts.length = 0;
    const other = fakeCtx("/work/other");
    other.ctx.session.get = async () => {
      throw new Error("not this instance's session");
    };
    const cleanupOther = await plugin.setup(other.ctx);
    const ev = { id: "evt_c", type: "session.execution.succeeded", data: { sessionID: "ses_1" } };
    other.emit(ev);
    await sleep(100);
    f.emit({ ...ev });
    await until(() => f.puts.length >= 1, "instructions entry");
    await sleep(100);
    assert.deepEqual(calls(), [
      { args: "hook opencode-v2 session.idle", payload: { sessionID: "ses_1", cwd: "/work/dir" } },
    ]);
    assert.deepEqual(other.puts, []);
    assert.deepEqual(f.puts, [{ sessionID: "ses_1", key: "agentfeedback", value: "fake note" }]);
    cleanupOther();
  });

  await t.test("cleanup aborts the subscription", async () => {
    cleanup();
    assert.equal(f.aborted, true);
  });
});

test("opencode-v2 plugin records a spawn error", async () => {
  const src = readFileSync(new URL(AF_PLUGIN_MISSING), "utf8");
  const spawnErr = JSON.parse(src.match(/^const SPAWN_ERR = (.*);$/m)[1]);
  rmSync(spawnErr, { force: true });
  const plugin = (await import(AF_PLUGIN_MISSING)).default;
  const f = fakeCtx();
  const cleanup = await plugin.setup(f.ctx);
  await f.hooks["execute.after"]({ tool: "shell", sessionID: "ses_2", input: { command: "x" }, result: { metadata: { exit: 1 } } });
  assert.ok(existsSync(spawnErr), "spawn-error file " + spawnErr);
  assert.match(JSON.parse(readFileSync(spawnErr, "utf8")).error, /ENOENT/);
  cleanup();
});

test("opencode-v2 plugin survives a failed hook or stream without a spawn error", async (t) => {
  const src = readFileSync(new URL(AF_PLUGIN), "utf8");
  const spawnErr = JSON.parse(src.match(/^const SPAWN_ERR = (.*);$/m)[1]);
  const plugin = (await import(AF_PLUGIN)).default;

  await t.test("abort writes no spawn error", async () => {
    rmSync(spawnErr, { force: true });
    const f = fakeCtx();
    const cleanup = await plugin.setup(f.ctx);
    cleanup();
    await sleep(50);
    assert.equal(existsSync(spawnErr), false);
  });

  await t.test("a rejected hook registration", async () => {
    rmSync(spawnErr, { force: true });
    const f = fakeCtx();
    f.ctx.tool.hook = async () => {
      throw new Error("hook registration refused");
    };
    const cleanup = await plugin.setup(f.ctx);
    assert.equal(typeof cleanup, "function");
    assert.equal(existsSync(spawnErr), false);
    cleanup();
  });

  await t.test("a stream that throws", async () => {
    rmSync(spawnErr, { force: true });
    const f = fakeCtx();
    f.ctx.event.subscribe = () => ({
      [Symbol.asyncIterator]() {
        return { next: async () => { throw new Error("stream closed"); } };
      },
    });
    const cleanup = await plugin.setup(f.ctx);
    await sleep(50);
    assert.equal(existsSync(spawnErr), false);
    cleanup();
  });
});
