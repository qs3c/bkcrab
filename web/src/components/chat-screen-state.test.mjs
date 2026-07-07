import test from "node:test";
import assert from "node:assert/strict";

const {
  getChatHistoryRenderState,
  findProducedFileAttachmentIndex,
  isInternalWorkspaceFile,
  splitToolTurnForRender,
} = await import(
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

test("hides internal todo files from produced file panels", () => {
  assert.equal(isInternalWorkspaceFile("todo.md"), true);
  assert.equal(isInternalWorkspaceFile("sessions/s-1/todo.md"), true);
  assert.equal(isInternalWorkspaceFile("projects/p-1/sessions/s-1/todo.md"), true);

  assert.equal(isInternalWorkspaceFile("report.md"), false);
  assert.equal(isInternalWorkspaceFile("research/todo-notes.md"), false);
});

test("does not treat file-only agent placeholders as the final answer", () => {
  const toolRound = {
    id: "tools",
    role: "tool-group",
    content: "",
    toolCalls: [{ id: "tc-1" }],
  };
  const fileOnlyAgent = {
    id: "files",
    role: "agent",
    content: "",
    files: [{ path: "todo.md", size: 639 }],
  };
  const answer = {
    id: "answer",
    role: "agent",
    content: "Here is the report.",
  };

  const noAnswerSplit = splitToolTurnForRender([toolRound, fileOnlyAgent]);
  assert.deepEqual(noAnswerSplit.finalMsgs, []);
  assert.deepEqual(noAnswerSplit.processMsgs, [toolRound, fileOnlyAgent]);

  const answerBeforeFilesSplit = splitToolTurnForRender([
    toolRound,
    answer,
    fileOnlyAgent,
  ]);
  assert.deepEqual(answerBeforeFilesSplit.finalMsgs, [answer]);
  assert.deepEqual(answerBeforeFilesSplit.processMsgs, [toolRound, fileOnlyAgent]);
});

test("keeps visible file-only artifacts outside the folded process", () => {
  const toolRound = {
    id: "tools",
    role: "tool-group",
    content: "",
    toolCalls: [{ id: "tc-1" }],
  };
  const fileOnlyArtifact = {
    id: "artifact",
    role: "agent",
    content: "",
    files: [{ path: "reports/energy.pdf", size: 2048 }],
  };

  const split = splitToolTurnForRender([toolRound, fileOnlyArtifact]);
  assert.deepEqual(split.finalMsgs, [fileOnlyArtifact]);
  assert.deepEqual(split.processMsgs, [toolRound]);
});

test("attaches produced files to the latest substantive agent reply in the current turn", () => {
  const messages = [
    { id: "u1", role: "user", content: "make a report" },
    { id: "a1", role: "agent", content: "Here is the report." },
    { id: "closing-tools", role: "tool-group", content: "", toolCalls: [{ id: "tc-1" }] },
  ];

  assert.equal(findProducedFileAttachmentIndex(messages), 1);
});

test("does not attach produced files to a previous turn when the current turn has no agent reply", () => {
  const messages = [
    { id: "u1", role: "user", content: "old task" },
    { id: "a1", role: "agent", content: "Old answer" },
    { id: "u2", role: "user", content: "new task" },
    { id: "tools", role: "tool-group", content: "", toolCalls: [{ id: "tc-1" }] },
  ];

  assert.equal(findProducedFileAttachmentIndex(messages), -1);
});
