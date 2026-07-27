"use client";

import * as React from "react";
import Link from "next/link";
import {
  AlertCircle,
  Cable,
  Check,
  CheckCircle2,
  ExternalLink,
  Loader2,
  Search,
} from "lucide-react";

import {
  getAgentConfig,
  listMCPResources,
  updateAgent,
  type MCPResource,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

export default function AgentMCPPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const [resources, setResources] = React.useState<MCPResource[]>([]);
  const [selectedIDs, setSelectedIDs] = React.useState<string[]>([]);
  const [filter, setFilter] = React.useState("");
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [saved, setSaved] = React.useState(false);
  const [error, setError] = React.useState("");

  React.useEffect(() => {
    if (!agentId) return;
    let cancelled = false;
    setLoading(true);
    setError("");
    Promise.all([listMCPResources(), getAgentConfig(agentId)])
      .then(([response, config]) => {
        if (cancelled) return;
        const available = response.servers || [];
        const allowed = new Set(available.map((resource) => resource.id));
        setResources(available);
        setSelectedIDs((config.mcp?.servers || []).filter((id) => allowed.has(id)));
      })
      .catch((err) => {
        if (!cancelled) setError(errorMessage(err, "读取 MCP 授权失败"));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [agentId]);

  const filtered = React.useMemo(() => {
    const keyword = filter.trim().toLowerCase();
    if (!keyword) return resources;
    return resources.filter((resource) =>
      `${resource.name} ${resource.description || ""} ${resource.config.type}`
        .toLowerCase()
        .includes(keyword),
    );
  }, [filter, resources]);

  const toggle = (resource: MCPResource) => {
    setSaved(false);
    setSelectedIDs((current) => {
      const selected = current.includes(resource.id);
      if (!resource.enabled && !selected) return current;
      return selected
        ? current.filter((id) => id !== resource.id)
        : [...current, resource.id];
    });
  };

  const save = async () => {
    setSaving(true);
    setSaved(false);
    setError("");
    try {
      const response = await updateAgent(agentId, { mcp: { servers: selectedIDs } });
      if (response?.ok === false || response?.error) {
        throw new Error(response.error || "保存 MCP 授权失败");
      }
      setSaved(true);
      window.setTimeout(() => setSaved(false), 2000);
    } catch (err) {
      setError(errorMessage(err, "保存 MCP 授权失败"));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <div>
        <h2 className="text-xl font-semibold tracking-tight">MCP</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          决定 {agentName || "这个 Agent"} 可以调用哪些 MCP 服务。未授权服务的工具不会注册到这个 Agent。
        </p>
      </div>

      {error && (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/25 bg-destructive/10 px-3 py-2.5 text-sm text-destructive">
          <AlertCircle className="mt-0.5 size-4 shrink-0" />
          <span>{error}</span>
        </div>
      )}

      {loading ? (
        <div className="space-y-3">
          <Skeleton className="h-20 w-full rounded-xl" />
          <Skeleton className="h-20 w-full rounded-xl" />
          <Skeleton className="h-20 w-full rounded-xl" />
        </div>
      ) : resources.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-4 py-10 text-center">
            <div className="flex size-12 items-center justify-center rounded-xl bg-primary/10">
              <Cable className="size-6 text-primary" />
            </div>
            <div>
              <p className="font-medium">还没有可授权的 MCP 服务</p>
              <p className="mt-1 text-sm text-muted-foreground">先在用户级 MCP 页面创建连接，再回来为 Agent 授权。</p>
            </div>
            <Link href="/mcp/" className={buttonVariants({ variant: "outline" })}>
              前往 MCP
              <ExternalLink className="size-4" />
            </Link>
          </CardContent>
        </Card>
      ) : (
        <>
          <Card>
            <CardHeader className="border-b">
              <CardTitle>允许访问的 MCP 服务</CardTitle>
              <CardDescription>已选择 {selectedIDs.length} / {resources.length} 个</CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              {resources.length > 4 && (
                <div className="relative">
                  <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                  <Input
                    value={filter}
                    onChange={(event) => setFilter(event.target.value)}
                    placeholder="搜索 MCP 服务"
                    className="pl-9"
                  />
                </div>
              )}
              <div className="space-y-2">
                {filtered.map((resource) => {
                  const selected = selectedIDs.includes(resource.id);
                  return (
                    <button
                      key={resource.id}
                      type="button"
                      aria-pressed={selected}
                      disabled={!resource.enabled && !selected}
                      onClick={() => toggle(resource)}
                      className={cn(
                        "flex w-full items-start gap-3 rounded-lg border p-3 text-left transition-colors",
                        selected ? "border-primary/40 bg-primary/5" : "hover:bg-muted/50",
                        !resource.enabled && "cursor-not-allowed opacity-55",
                      )}
                    >
                      <span className={cn(
                        "mt-0.5 flex size-5 shrink-0 items-center justify-center rounded border",
                        selected ? "border-primary bg-primary text-primary-foreground" : "border-foreground/25 bg-background",
                      )}>
                        {selected && <Check className="size-3.5" />}
                      </span>
                      <span className="min-w-0 flex-1">
                        <span className="flex flex-wrap items-center gap-2">
                          <span className="font-medium">{resource.name}</span>
                          <Badge variant="outline" className="text-[10px]">{resource.config.type}</Badge>
                          {!resource.enabled && <Badge variant="secondary" className="text-[10px]">已停用</Badge>}
                        </span>
                        <span className="mt-1 block text-xs text-muted-foreground">
                          {resource.description || "未填写说明"}
                        </span>
                      </span>
                    </button>
                  );
                })}
                {filtered.length === 0 && (
                  <div className="rounded-lg border border-dashed py-8 text-center text-sm text-muted-foreground">
                    没有匹配的 MCP 服务
                  </div>
                )}
              </div>
            </CardContent>
          </Card>

          <div className="flex items-center justify-between gap-4 rounded-lg border bg-muted/30 px-4 py-3">
            <p className="text-xs text-muted-foreground">
              {selectedIDs.length === 0
                ? "保存后，这个 Agent 不会注册任何 MCP 工具。"
                : `保存后，这个 Agent 只能看到所选 ${selectedIDs.length} 个服务提供的工具。`}
            </p>
            <div className="flex shrink-0 items-center gap-3">
              {saved && (
                <span className="flex items-center gap-1 text-sm text-emerald-600">
                  <CheckCircle2 className="size-4" />
                  已保存
                </span>
              )}
              <Button onClick={() => void save()} disabled={saving}>
                {saving && <Loader2 className="size-4 animate-spin" />}
                {saving ? "保存中" : "保存授权"}
              </Button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
