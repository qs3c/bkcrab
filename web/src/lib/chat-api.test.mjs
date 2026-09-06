import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import ts from "typescript";

// api.ts also contains parameter properties, which Node's strip-only runner
// cannot load. Compile the actual module so these tests exercise the HTTP client.
const source = await readFile(new URL("./api.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 },
});
const { ChatTurnBusyError, sendChatStream, steerChat } = await import(
  `data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`
);

globalThis.localStorage = { getItem: () => null };

test("steer accepts the starting and running reservations", async (t) => {
  for (const state of ["starting", "running"]) {
    t.mock.method(globalThis, "fetch", async () => Response.json({ buffered: true, state }));
    assert.equal(await steerChat("agent", "A", "adjust"), true);
    t.mock.restoreAll();
  }
});

test("only explicit idle allows steer to fall back to a normal POST", async (t) => {
  t.mock.method(globalThis, "fetch", async () => Response.json({ buffered: false, state: "idle" }, { status: 409 }));
  assert.equal(await steerChat("agent", "A", "adjust"), false);
});

test("stopping, finishing and unknown conflicts preserve the draft instead of resubmitting", async (t) => {
  for (const state of ["stopping", "finishing", undefined]) {
    t.mock.method(globalThis, "fetch", async () => Response.json({ buffered: false, state }, { status: 409 }));
    await assert.rejects(steerChat("agent", "A", "adjust"), ChatTurnBusyError);
    t.mock.restoreAll();
  }
});

test("missing acknowledgement cannot silently trigger another submission", async (t) => {
  t.mock.method(globalThis, "fetch", async () => Response.json({}));
  await assert.rejects(steerChat("agent", "A", "adjust"), /acknowledgement/);
});

test("ordinary POST conflicts also signal that the draft should be retained", async (t) => {
  t.mock.method(globalThis, "fetch", async () => Response.json({ error: "busy" }, { status: 409 }));
  await assert.rejects(sendChatStream("agent", "A", "hello", () => assert.fail("no stream should open")), ChatTurnBusyError);
});
