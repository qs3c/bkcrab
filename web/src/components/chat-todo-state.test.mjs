import test from "node:test";
import assert from "node:assert/strict";
import { createChatTodoLoader, shouldRefreshChatTodo } from "./chat-todo-state.ts";

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

test("switching A to B discards A's late result, even after returning to A", async () => {
  const a = deferred(), b = deferred();
  let displayed = [];
  const apply = items => { displayed = items; };
  const loaderA = createChatTodoLoader("agent", "A", () => a.promise, apply);
  const pendingA = loaderA.refresh("agent", "A");
  loaderA.dispose();
  const loaderB = createChatTodoLoader("agent", "B", () => b.promise, apply);
  const pendingB = loaderB.refresh("agent", "B");
  b.resolve({ items: ["B"] });
  await pendingB;
  a.resolve({ items: ["old A"] });
  await pendingA;
  assert.deepEqual(displayed, ["B"]);
  await loaderB.refresh("agent", "A");
  assert.deepEqual(displayed, ["B"]);
  loaderB.dispose();
  const newA = createChatTodoLoader("agent", "A", async () => ({ items: ["new A"] }), apply);
  await newA.refresh("agent", "A");
  await loaderA.refresh("agent", "A");
  assert.deepEqual(displayed, ["new A"]);
});

test("older requests in one session cannot revert checked items", async () => {
  const requests = [deferred(), deferred()];
  let n = 0, displayed = [];
  const loader = createChatTodoLoader("agent", "A", () => requests[n++].promise, items => { displayed = items; });
  const old = loader.refresh("agent", "A");
  const fresh = loader.refresh("agent", "A");
  requests[1].resolve({ items: ["done"] });
  await fresh;
  requests[0].resolve({ items: ["pending"] });
  await old;
  assert.deepEqual(displayed, ["done"]);
});

test("subscription tool results refresh without a preceding tool call and deduplicate POST copies", async () => {
  let loads = 0;
  const loader = createChatTodoLoader("agent", "A", async () => { loads++; return { items: [] }; }, () => {});
  const evt = { type: "tool_result", seq: 12, data: { name: "edit_file" } };
  loader.onEvent("agent", "A", evt);
  loader.onEvent("agent", "A", evt);
  loader.onEvent("agent", "B", { ...evt, seq: 13 });
  loader.onEvent("agent", "A", { type: "tool_result", seq: 14, data: { name: "web_search" } });
  loader.onEvent("agent", "A", { type: "done", seq: 15 });
  assert.equal(loads, 2);
  assert.equal(shouldRefreshChatTodo({ type: "tool_result", data: { name: "exec" } }), true);
});

test("late errors and disposed subscriptions leave the current plan intact", async () => {
  const req = deferred();
  let displayed = ["B"];
  const loader = createChatTodoLoader("agent", "A", () => req.promise, items => { displayed = items; });
  const pending = loader.refresh("agent", "A");
  loader.dispose();
  req.reject(new Error("network"));
  await pending;
  loader.onEvent("agent", "A", { type: "done", seq: 1 });
  assert.deepEqual(displayed, ["B"]);
});
