"use client";

import { useCallback, useEffect, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Brain, Check, MessageSquare, MessagesSquare, Puzzle } from "lucide-react";
import { getAgent, updateAgent } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

// Per-agent Context page — one knob (mode), one extension point (plugins).
//
// "Context" rather than "Tools" because the page is really about how
// the LLM's context window gets assembled: which framework sections
// participate in the system prompt AND which built-in tools come
// along. Prompt Mode picks both in one go. There's no per-agent
// allowlist anymore — what each mode includes is documented inline
// next to the dropdown; for the live tool list at runtime, look at
// the agent's chat session (tool calls in the transcript) or the
// /api/agents/{id}/tools/registered endpoint.

type PromptModeValue = "" | "agent" | "chatbot" | "customize";

const MODE_LABEL: Record<string, string> = {
  agent: "智能体",
  chatbot: "聊天机器人",
  customize: "自定义",
};

export default function AgentContextPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);

  // "" = no override saved; runtime falls back to "agent".
  const [promptMode, setPromptMode] = useState<PromptModeValue>("");
  // Per-agent multi-bubble toggle. Applies to every IM channel the
  // agent is bound to. False is the default; null on the wire is
  // treated as false here.
  const [splitReplies, setSplitReplies] = useState(false);
  const [splitRepliesSaving, setSplitRepliesSaving] = useState(false);
  // Per-agent auto-persist toggle. Off by default; null on the wire is
  // treated as false here. When on, every N turns the runtime fires an
  // LLM-driven distill pass that appends to USER.md / MEMORY.md.
  const [autoPersist, setAutoPersist] = useState(false);
  const [autoPersistSaving, setAutoPersistSaving] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  const fetchAll = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      const agentRec = await getAgent(agentId).catch(() => null);
      const pm = agentRec?.promptMode || "";
      if (pm === "agent" || pm === "chatbot" || pm === "customize") {
        setPromptMode(pm);
      } else {
        setPromptMode("");
      }
      setSplitReplies(agentRec?.splitReplies === true);
      setAutoPersist(agentRec?.autoPersist === true);
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  useEffect(() => {
    fetchAll();
  }, [fetchAll]);

  const flashSaved = () => {
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  };

  const handlePromptModeChange = async (next: PromptModeValue) => {
    const prev = promptMode;
    setPromptMode(next);
    setSaving(true);
    try {
      await updateAgent(agentId, { promptMode: next });
      flashSaved();
    } catch {
      setPromptMode(prev);
    } finally {
      setSaving(false);
    }
  };

  // Optimistic toggle for splitReplies. No "inherit" state anymore —
  // system-level fallback was removed; false is the absolute default
  // when nothing is saved.
  const handleSplitRepliesChange = async (next: boolean) => {
    const prev = splitReplies;
    setSplitReplies(next);
    setSplitRepliesSaving(true);
    try {
      await updateAgent(agentId, { splitReplies: next });
      flashSaved();
    } catch {
      setSplitReplies(prev);
    } finally {
      setSplitRepliesSaving(false);
    }
  };

  // Optimistic toggle for autoPersist. Same shape as splitReplies; on
  // failure roll back. The runtime falls back to system default (off
  // in practice today, since the dead-code NewAgentWithFullCfg path
  // never gets called) when no per-agent override is saved.
  const handleAutoPersistChange = async (next: boolean) => {
    const prev = autoPersist;
    setAutoPersist(next);
    setAutoPersistSaving(true);
    try {
      await updateAgent(agentId, { autoPersist: next });
      flashSaved();
    } catch {
      setAutoPersist(prev);
    } finally {
      setAutoPersistSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="p-6 space-y-6 max-w-5xl mx-auto">
        <Skeleton className="h-10 w-48" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">上下文</h2>
          <p className="text-sm text-muted-foreground mt-1">
            大模型看到的内容：{" "}
            <strong>{agentName || "此智能体"}</strong>。提示词模式会同时选择框架提示词方案和内置工具集。自定义工具来自插件，无论使用何种模式都会提供。
          </p>
        </div>
        <div className="flex items-center gap-2">
          {saved && (
            <span className="inline-flex items-center gap-1.5 text-xs text-emerald-600 dark:text-emerald-400">
              <Check className="h-3.5 w-3.5" /> 已保存
            </span>
          )}
        </div>
      </div>

      {/* Prompt Mode */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between gap-2 mb-3">
          <div className="flex items-center gap-2">
            <MessageSquare className="h-4 w-4 text-primary" />
            <h3 className="font-medium">提示词模式</h3>
            {promptMode === "" || promptMode === "agent" ? (
              <Badge variant="outline" className="text-[10px]">
                默认
              </Badge>
            ) : (
              <Badge className="bg-primary/10 text-primary hover:bg-primary/10 text-[10px]">
                {MODE_LABEL[promptMode]}
              </Badge>
            )}
          </div>
        </div>
        <Select
          value={promptMode || "agent"}
          onValueChange={(v: string | null) => {
            if (v === "agent" || v === "chatbot" || v === "customize") {
              handlePromptModeChange(v);
            }
          }}
          disabled={saving}
        >
          <SelectTrigger className="text-sm max-w-[240px]">
            {/* Explicit children override SelectValue's auto-extraction
                from the active SelectItem — shadcn sometimes falls back
                to rendering the raw `value` string. */}
            <SelectValue>{MODE_LABEL[promptMode || "agent"]}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="agent">智能体</SelectItem>
            <SelectItem value="chatbot">聊天机器人</SelectItem>
            <SelectItem value="customize">自定义</SelectItem>
          </SelectContent>
        </Select>
        <div className="mt-3 text-xs text-muted-foreground space-y-1.5">
          <div>
            <strong>智能体</strong> — 完整框架提示词（任务委派、工具使用规范、工作区自更新和定时任务）以及全部内置工具。这是自主任务智能体的默认模式。
          </div>
          <div>
            <strong>聊天机器人</strong> — 精简框架，让人格文件直接决定表达风格。内置工具缩减为{" "}
            <code className="text-[10px]">image_gen</code>,{" "}
            <code className="text-[10px]">tts</code>,{" "}
            <code className="text-[10px]">write_file</code>,{" "}
            <code className="text-[10px]">edit_file</code> — 后两个工具让大模型在了解聊天者后将信息持久化到 USER.md / MEMORY.md。记忆内容来自系统提示词中内嵌的 USER.md / MEMORY.md 段落，不提供{" "}
            <code className="text-[10px]">memory_search</code> 备用检索通道（聊天机器人模式不会写入它所扫描的日志，结果为空并会干扰模型）。主回复以纯文本输出，多气泡回复通过内联分隔标记实现。适合陪伴、角色扮演和客服机器人。
          </div>
          <div>
            <strong>自定义</strong> — 仅包含日期锚点和启动文件，不提供内置工具。你可以通过 SOUL.md / IDENTITY.md 完整编写系统提示词，并通过插件提供工具。
          </div>
        </div>
        <div className="mt-4 pt-3 border-t border-border flex items-start gap-2 text-xs text-muted-foreground">
          <Puzzle className="h-3.5 w-3.5 mt-0.5 shrink-0" />
          <span>
            插件和 MCP 工具不受模式影响，始终可用。创建插件请参阅{" "}
            <code className="text-[11px]">
              ~/.bkclaw/plugins/bkclaw-plugin-demo
            </code>{" "}
            查看最小示例。
          </span>
        </div>
      </div>

      {/* Multi-bubble replies — applies to every IM channel. Lives here
          rather than in the Channels tab because it's a property of how
          the LLM communicates, not of the channel binding. */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <MessagesSquare className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <h3 className="font-medium">多气泡回复</h3>
              <p className="text-sm text-muted-foreground mt-1">
                允许智能体使用分隔标记将一次回复拆成多个对话气泡，更适合即时通讯中的简短分段回复。适用于所有即时通讯渠道（微信、Telegram、Discord、Slack、LINE、飞书），网页端会忽略此设置。默认关闭，每次回复保持为一条消息。
              </p>
            </div>
          </div>
          <Switch
            checked={splitReplies}
            onCheckedChange={handleSplitRepliesChange}
            disabled={splitRepliesSaving}
            aria-label="多气泡回复"
          />
        </div>
      </div>

      {/* Auto-remember chatter — lives here because it's about how the
          agent retains context across turns / sessions, parallel to how
          Multi-bubble is about how it emits replies. */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <Brain className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <h3 className="font-medium">自动记住聊天者</h3>
              <p className="text-sm text-muted-foreground mt-1">
                备用持久化机制：每 5 轮用户消息，运行时会发起一次小型大模型调用，将近期对话提炼到 USER.md / MEMORY.md。主要机制仍是大模型通过以下工具直接写入这些文件：{" "}
                <code className="text-[10px]">write_file</code> /{" "}
                <code className="text-[10px]">edit_file</code> （现在聊天机器人模式也可使用）。此开关用于在模型忘记写入时确保仍有内容被持久化。默认关闭，以保持跨会话无状态行为。
              </p>
            </div>
          </div>
          <Switch
            checked={autoPersist}
            onCheckedChange={handleAutoPersistChange}
            disabled={autoPersistSaving}
            aria-label="自动记住聊天者"
          />
        </div>
      </div>
    </div>
  );
}
