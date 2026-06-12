"use client";

import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

export default function AgentSessionsPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  return (
    <div className="p-6 max-w-3xl mx-auto">
      <h2 className="text-2xl font-semibold tracking-tight">会话</h2>
      <p className="text-sm text-muted-foreground mt-1">智能体： {agentName}</p>
      <div className="mt-6 rounded-lg border border-border bg-card p-8 text-center text-muted-foreground">
        即将推出
      </div>
    </div>
  );
}
