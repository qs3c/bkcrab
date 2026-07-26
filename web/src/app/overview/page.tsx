"use client";

import { useEffect, useState } from "react";
import { Separator } from "@/components/ui/separator";
import {
  getStatus,
  adminListChats,
  getTools,
  getConfig,
  type StatusResponse,
  type ToolsConfig,
  type ConfigResponse,
} from "@/lib/api";
import {
  Bot,
  Radio,
  Brain,
  Users,
  MessagesSquare,
} from "lucide-react";

export default function OverviewPage() {
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [chats, setChats] = useState<number | null>(null);
  const [tools, setTools] = useState<ToolsConfig | null>(null);
  const [runtime, setRuntime] = useState<ConfigResponse["sandbox"] | null>(null);
  const [loading, setLoading] = useState(true);

  const fetchStatus = async () => {
    try {
      const s = await getStatus();
      setStatus(s);
      if (s.isAdmin) {
        adminListChats()
          .then((rows) => setChats(rows.length))
          .catch(() => setChats(null));
        getTools()
          .then(setTools)
          .catch(() => setTools(null));
        // getConfig 在后端仅限 super_admin；非超级管理员会收到 403，
        // 此时我们直接隐藏运行环境行，而不将其视为错误。
        getConfig()
          .then((cfg) => setRuntime(cfg.sandbox ?? null))
          .catch(() => setRuntime(null));
      }
    } catch {
      setStatus(null);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchStatus();
    const interval = setInterval(fetchStatus, 10000);
    return () => clearInterval(interval);
  }, []);

  if (loading && !status) {
    return (
      <div className="flex h-full items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-muted border-t-primary" />
      </div>
    );
  }

  // 隐藏空或尚未连接的部分，使仪表盘仅反映实际配置的内容：
  // 例如未连接任何渠道时不显示渠道统计。
  const channelCount = status?.channels?.length || 0;
  const showChannels = channelCount > 0;
  // 非管理员只需查看自己的智能体 — 网关基础设置
  // （服务商配置、用户、对话）仅管理员可见。
  const isAdmin = status?.isAdmin ?? false;

  // 以美观格式展示各工具类别已配置的回退链，如
  // "网页搜索: Exa, Brave"。未配置服务商的类别将被隐藏 —
  // 工具面板只展示实际已接入的内容。
  const toolSummary: { name: string; label: string; providers: string }[] = [];
  if (tools) {
    for (const cat of tools.categories) {
      const cfg = tools.tools[cat.name];
      const chain = [cfg?.primary, ...(cfg?.fallbacks || [])].filter(Boolean) as string[];
      if (chain.length === 0) continue;
      const labels = chain.map((ref) => {
        const [pname] = ref.split("/");
        return cat.providers.find((p) => p.name === pname)?.label || pname;
      });
      toolSummary.push({ name: cat.name, label: cat.label, providers: labels.join(", ") });
    }
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      {/* 页头 */}
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">仪表盘</h2>
        <p className="text-sm text-muted-foreground mt-1">
          监控你的 BkCrab 网关
        </p>
      </div>

{/* 统计卡片 — 智能体对所有人可见；用户 + 对话 +
           渠道为网关管理面板，仅管理员可见。 */}
      <div
        className={`grid gap-4 grid-cols-2 ${
          isAdmin
            ? showChannels
              ? "md:grid-cols-4"
              : "md:grid-cols-3"
            : "md:grid-cols-2"
        }`}
      >
        {/* 智能体 */}
        <div className="rounded-lg border border-border bg-card p-5">
          <div className="flex items-center justify-between mb-3">
            <span className="text-sm text-muted-foreground">智能体</span>
            <div className="flex h-8 w-8 items-center justify-center rounded-full bg-violet-500/10">
              <Bot className="h-4 w-4 text-violet-500" />
            </div>
          </div>
          <p className="text-3xl font-semibold tracking-tight">
            {status?.agents?.length || 0}
          </p>
          <p className="text-xs text-muted-foreground mt-1">活跃智能体</p>
        </div>

        {/* 用户 — 仅管理员 */}
        {isAdmin && (
          <div className="rounded-lg border border-border bg-card p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="text-sm text-muted-foreground">用户</span>
              <div className="flex h-8 w-8 items-center justify-center rounded-full bg-cyan-500/10">
                <Users className="h-4 w-4 text-cyan-500" />
              </div>
            </div>
            <p className="text-3xl font-semibold tracking-tight">
              {status?.users ?? 0}
            </p>
            <p className="text-xs text-muted-foreground mt-1">已注册</p>
          </div>
        )}

        {/* 对话 — 仅管理员 */}
        {isAdmin && (
          <div className="rounded-lg border border-border bg-card p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="text-sm text-muted-foreground">对话</span>
              <div className="flex h-8 w-8 items-center justify-center rounded-full bg-amber-500/10">
                <MessagesSquare className="h-4 w-4 text-amber-500" />
              </div>
            </div>
            <p className="text-3xl font-semibold tracking-tight">
              {chats ?? "—"}
            </p>
            <p className="text-xs text-muted-foreground mt-1">会话总数</p>
          </div>
        )}

        {/* 渠道 — 仅管理员 */}
        {isAdmin && showChannels && (
          <div className="rounded-lg border border-border bg-card p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="text-sm text-muted-foreground">渠道</span>
              <div className="flex h-8 w-8 items-center justify-center rounded-full bg-blue-500/10">
                <Radio className="h-4 w-4 text-blue-500" />
              </div>
            </div>
            <p className="text-3xl font-semibold tracking-tight">{channelCount}</p>
            <p className="text-xs text-muted-foreground mt-1">已连接</p>
          </div>
        )}

      </div>

{/* 配置 — 仅管理员可见的已配置 LLM 模型
           和已接入工具服务商摘要。非管理员不可见。 */}
      {isAdmin && (
        <div className="rounded-lg border border-border bg-card">
          <div className="p-5 pb-3">
            <div className="flex items-center gap-2 mb-1">
              <Brain className="h-4 w-4 text-amber-500" />
              <h3 className="font-medium">配置</h3>
            </div>
            <p className="text-sm text-muted-foreground">
              此网关连接的模型和工具
            </p>
          </div>
          <div className="px-5 pb-5 space-y-3">
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">模型</span>
              {status?.provider?.model ? (
                <code className="text-sm font-mono bg-muted px-2 py-0.5 rounded">
                  {status.provider.model}
                </code>
              ) : (
                <span className="text-sm text-muted-foreground">—</span>
              )}
            </div>
            <Separator />
            {toolSummary.length > 0 ? (
              toolSummary.map((t) => (
                <div key={t.name} className="space-y-3">
                  <div className="flex items-center justify-between gap-3">
                    <span className="text-sm text-muted-foreground">{t.label}</span>
                    <span className="text-sm truncate">{t.providers}</span>
                  </div>
                  <Separator />
                </div>
              ))
            ) : (
              <>
                <div className="flex items-center justify-between">
                  <span className="text-sm text-muted-foreground">工具</span>
                  <span className="text-sm text-muted-foreground">尚未配置</span>
                </div>
                <Separator />
              </>
            )}
            <div className="flex items-center justify-between gap-3">
              <span className="text-sm text-muted-foreground">运行环境</span>
              <span className="text-sm truncate">
                {runtime?.enabled
                  ? `${runtime.backend === "e2b" ? "E2B" : "Docker"}${
                      runtime.image ? ` (${runtime.image})` : ""
                    }`
                  : "已禁用"}
              </span>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
