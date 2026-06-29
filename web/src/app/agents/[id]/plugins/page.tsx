"use client";

import { useCallback, useEffect, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Plug } from "lucide-react";
import {
  getAgent,
  listHookPlugins,
  updateAgent,
  type HookPlugin,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

// 按智能体的插件启用页面。与技能页面布局一致（卡片网格 + 标题）。
// 默认关闭——此处列出的插件来自系统安装；拨动开关只会将插件钩子
// 挂载到此智能体。参见 internal/gateway/userspace.go 中的
// registerHookPluginsForAgent 了解选择加入语义。
export default function AgentPluginsPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const [hookPlugins, setHookPlugins] = useState<HookPlugin[]>([]);
  const [pluginEnabled, setPluginEnabled] = useState<Record<string, boolean>>({});
  const [pluginSaving, setPluginSaving] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(true);

  const fetchAll = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      const [agentRec, hooks] = await Promise.all([
        getAgent(agentId).catch(() => null),
        listHookPlugins(),
      ]);
      setPluginEnabled(
        agentRec?.plugins && typeof agentRec.plugins === "object"
          ? (agentRec.plugins as Record<string, boolean>)
          : {}
      );
      setHookPlugins(hooks);
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  useEffect(() => {
    fetchAll();
  }, [fetchAll]);

  // 按插件切换。采用补丁语义，拨动一个不会覆盖兄弟插件的覆盖设置。
  // 乐观更新，失败时回滚。
  const handleToggle = async (pluginID: string, next: boolean) => {
    const prev = pluginEnabled[pluginID] === true;
    setPluginEnabled((m) => ({ ...m, [pluginID]: next }));
    setPluginSaving((m) => ({ ...m, [pluginID]: true }));
    try {
      await updateAgent(agentId, { plugins: { [pluginID]: next } });
    } catch {
      setPluginEnabled((m) => ({ ...m, [pluginID]: prev }));
    } finally {
      setPluginSaving((m) => {
        const copy = { ...m };
        delete copy[pluginID];
        return copy;
      });
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
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">插件</h2>
        <p className="text-sm text-muted-foreground mt-1">
          此安装中发现的 Hook 插件，可按智能体启用：{" "}
          <strong>{agentName}</strong>。默认关闭，插件仅对你明确启用的智能体生效。后续消息会通过 <code className="text-[10px]">chat.send</code> 返回，不会触发新一轮智能体运行。
        </p>
      </div>

      {hookPlugins.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border bg-card/30 p-12">
          <div className="flex flex-col items-center justify-center">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <Plug className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">
              尚未安装 Hook 插件
            </p>
            <p className="text-xs text-muted-foreground/60 max-w-sm text-center">
              将插件目录放入{" "}
              <code className="text-[10px]">~/.bkcrab/plugins/</code>{" "}
              并在 <code className="text-[10px]">plugin.json</code> 中设置{" "}
              <code className="text-[10px]">type: &quot;hook&quot;</code>，然后重启守护进程。
            </p>
          </div>
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {hookPlugins.map((p) => {
            const enabled = pluginEnabled[p.id] === true;
            const saving = pluginSaving[p.id] === true;
            return (
              <div
                key={p.id}
                className="group rounded-lg border border-border bg-card p-5 transition-colors hover:bg-muted/50"
              >
                <div className="flex items-start justify-between mb-3 gap-3">
                  <div className="flex items-center gap-2.5 min-w-0">
                    <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-primary/10 shrink-0">
                      <Plug className="h-4 w-4 text-primary" />
                    </div>
                    <div className="min-w-0">
                      <p className="text-sm font-medium truncate">
                        {p.name || p.id}
                      </p>
                      {p.version && (
                        <Badge variant="outline" className="mt-1 text-[10px]">
                          v{p.version}
                        </Badge>
                      )}
                    </div>
                  </div>
                  <Switch
                    checked={enabled}
                    onCheckedChange={(v) => handleToggle(p.id, v)}
                    disabled={saving}
                    aria-label={`启用插件 ${p.id}`}
                  />
                </div>
                {p.description && (
                  <p className="text-xs text-muted-foreground line-clamp-3">
                    {p.description}
                  </p>
                )}
                <code className="text-[10px] text-muted-foreground/70 mt-3 block truncate">
                  {p.id}
                </code>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
