import test from "node:test";
import assert from "node:assert/strict";

const { getChatHistoryRenderState } = await import(
  new URL("./chat-screen-state.ts", import.meta.url)
);

test("treats a routed session with pending history as loading instead of empty", () => {
  const state = getChatHistoryRenderState({
    messageCount: 0,
    urlSessionId: "session-b",
    sessionId: "session-b",
    loadedSessionId: "session-a",
  });

  assert.equal(state.isHistoryLoading, true);
  assert.equal(state.isEmpty, false);
});

test("shows the empty hero after an existing session finishes loading with no messages", () => {
  const state = getChatHistoryRenderState({
    messageCount: 0,
    urlSessionId: "session-b",
    sessionId: "session-b",
    loadedSessionId: "session-b",
  });

  assert.equal(state.isHistoryLoading, false);
  assert.equal(state.isEmpty, true);
});

test("keeps brand-new unrouted chats in the empty state immediately", () => {
  const state = getChatHistoryRenderState({
    messageCount: 0,
    urlSessionId: "",
    sessionId: "generated-session",
    loadedSessionId: null,
  });

  assert.equal(state.isHistoryLoading, false);
  assert.equal(state.isEmpty, true);
});
