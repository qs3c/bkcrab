interface TodoEvent {
  type?: string;
  seq?: number;
  data?: { name?: string };
}

// Used by both POST and subscription streams, independently of message rendering.
export function shouldRefreshChatTodo(event: TodoEvent): boolean {
  if (event.type === "done") return true;
  if (event.type !== "tool_result") return false;
  return !event.data?.name || ["write_file", "edit_file", "apply_patch", "exec"].includes(event.data.name);
}

export function createChatTodoLoader<T>(
  agentId: string,
  sessionId: string,
  load: (agentId: string, sessionId: string) => Promise<{ items: T[] }>,
  apply: (items: T[]) => void,
) {
  let disposed = false;
  let version = 0;
  let latestEventSeq = -1;
  const refresh = async (agent: string, session: string) => {
    if (disposed || agent !== agentId || session !== sessionId) return;
    const request = ++version;
    try {
      const todo = await load(agentId, sessionId);
      if (!disposed && request === version) apply(todo.items);
    } catch { /* Keep the last known plan on a transient failure. */ }
  };
  return {
    refresh,
    onEvent(agent: string, session: string, event: TodoEvent) {
      if (disposed || agent !== agentId || session !== sessionId || !shouldRefreshChatTodo(event)) return;
      if (typeof event.seq === "number" && event.seq >= 0) {
        if (event.seq <= latestEventSeq) return;
        latestEventSeq = event.seq;
      }
      void refresh(agent, session);
    },
    dispose() { disposed = true; },
  };
}
