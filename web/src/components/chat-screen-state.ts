export interface ChatHistoryRenderInput {
  messageCount: number;
  urlSessionId: string;
  sessionId: string;
  loadedSessionId: string | null;
}

export interface ChatHistoryRenderState {
  isHistoryLoading: boolean;
  isEmpty: boolean;
}

export interface ChatTurnRenderMessage {
  id?: string;
  role: "user" | "agent" | "tool-group";
  content?: string;
  files?: Array<{ path?: unknown }>;
  toolCalls?: unknown[];
}

export interface ChatTurnRenderSplit<T extends ChatTurnRenderMessage> {
  processMsgs: T[];
  finalMsgs: T[];
}

const SYSTEM_WORKSPACE_FILES = new Set([
  "SOUL.md",
  "IDENTITY.md",
  "USER.md",
  "BOOTSTRAP.md",
  "MEMORY.md",
  "HEARTBEAT.md",
  "AGENTS.md",
  "TOOLS.md",
  "agent.json",
]);

export function getChatHistoryRenderState({
  messageCount,
  urlSessionId,
  loadedSessionId,
}: ChatHistoryRenderInput): ChatHistoryRenderState {
  const isHistoryLoading = !!urlSessionId && loadedSessionId !== urlSessionId;

  return {
    isHistoryLoading,
    isEmpty: !isHistoryLoading && messageCount === 0,
  };
}

export function isInternalWorkspaceFile(path: string): boolean {
  const normalized = path.replaceAll("\\", "/").replace(/^\/+/, "");
  const basename = normalized.split("/").pop() || normalized;

  if (!normalized.includes("/") && SYSTEM_WORKSPACE_FILES.has(basename)) {
    return true;
  }
  return basename.toLowerCase() === "todo.md";
}

export function splitToolTurnForRender<T extends ChatTurnRenderMessage>(
  turnMsgs: T[],
): ChatTurnRenderSplit<T> {
  const hasVisibleFile = (m: T) =>
    !!m.files?.some(
      (f) => typeof f.path === "string" && !isInternalWorkspaceFile(f.path),
    );
  const isBlankAgent = (m: T) =>
    m.role === "agent" && !(m.content && m.content.trim()) && !hasVisibleFile(m);

  let lastAgentIdx = turnMsgs.length - 1;
  while (
    lastAgentIdx >= 0 &&
    (turnMsgs[lastAgentIdx].role === "tool-group" ||
      isBlankAgent(turnMsgs[lastAgentIdx]))
  ) {
    lastAgentIdx--;
  }

  if (lastAgentIdx < 0) {
    return { processMsgs: turnMsgs, finalMsgs: [] };
  }

  let finalStart = lastAgentIdx;
  while (finalStart - 1 >= 0) {
    const previous = turnMsgs[finalStart - 1];
    if (
      !previous ||
      previous.role !== "agent" ||
      !previous.content ||
      !previous.content.trim()
    ) {
      break;
    }
    finalStart--;
  }

  return {
    finalMsgs: turnMsgs.slice(finalStart, lastAgentIdx + 1),
    processMsgs: [
      ...turnMsgs.slice(0, finalStart),
      ...turnMsgs.slice(lastAgentIdx + 1),
    ],
  };
}

export function findProducedFileAttachmentIndex<T extends ChatTurnRenderMessage>(
  messages: T[],
): number {
  let lastUserIdx = -1;
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role === "user") {
      lastUserIdx = i;
      break;
    }
  }

  for (let i = messages.length - 1; i > lastUserIdx; i--) {
    const msg = messages[i];
    if (msg.role === "agent" && msg.content && msg.content.trim()) {
      return i;
    }
  }

  for (let i = messages.length - 1; i > lastUserIdx; i--) {
    if (messages[i].role === "agent") {
      return i;
    }
  }

  return -1;
}
