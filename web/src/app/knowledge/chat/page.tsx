import { Suspense } from "react";

import KnowledgeChatClient from "./knowledge-chat-client";

export default function KnowledgeChatPage() {
  return (
    <Suspense fallback={<div className="min-h-[calc(100svh-2rem)] p-6" />}>
      <KnowledgeChatClient />
    </Suspense>
  );
}
