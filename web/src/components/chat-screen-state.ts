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
