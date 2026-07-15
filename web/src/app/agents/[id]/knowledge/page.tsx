"use client";

import * as React from "react";
import Link from "next/link";
import {
  AlertCircle,
  Check,
  CheckCircle2,
  Database,
  ExternalLink,
  Loader2,
  Search,
} from "lucide-react";

import {
  getAgentConfig,
  listKnowledgeBases,
  updateAgent,
  type KnowledgeBase,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

export default function AgentKnowledgePage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const [knowledgeBases, setKnowledgeBases] = React.useState<KnowledgeBase[]>([]);
  const [selectedIds, setSelectedIds] = React.useState<string[]>([]);
  const [topN, setTopN] = React.useState(5);
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
    Promise.all([listKnowledgeBases(), getAgentConfig(agentId)])
      .then(([rows, config]) => {
        if (cancelled) return;
        setKnowledgeBases(rows);
        const allowed = new Set(rows.map((kb) => kb.id));
        setSelectedIds((config.rag?.kbs || []).filter((id) => allowed.has(id)));
        setTopN(config.rag?.topN && config.rag.topN > 0 ? config.rag.topN : 5);
      })
      .catch((err) => {
        if (!cancelled) setError(errorMessage(err, "读取知识库授权失败"));
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
    if (!keyword) return knowledgeBases;
    return knowledgeBases.filter((kb) =>
      `${kb.name} ${kb.description}`.toLowerCase().includes(keyword),
    );
  }, [knowledgeBases, filter]);

  const toggleKnowledgeBase = (id: string) => {
    setSaved(false);
    setSelectedIds((current) =>
      current.includes(id) ? current.filter((item) => item !== id) : [...current, id],
    );
  };

  const save = async () => {
    setSaving(true);
    setSaved(false);
    setError("");
    try {
      const response = await updateAgent(agentId, {
        rag: {
          kbs: selectedIds,
          topN: selectedIds.length > 0 ? topN : 0,
        },
      });
      if (response?.ok === false || response?.error) {
        throw new Error(response.error || "保存知识库授权失败");
      }
      setSaved(true);
      window.setTimeout(() => setSaved(false), 2000);
    } catch (err) {
      setError(errorMessage(err, "保存知识库授权失败"));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <div>
        <h2 className="text-xl font-semibold tracking-tight">知识库</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          决定 {agentName || "这个智能体"} 可以检索哪些资料。未授权的知识库不会出现在工具调用中。
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
      ) : knowledgeBases.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-4 py-10 text-center">
            <div className="flex size-12 items-center justify-center rounded-xl bg-primary/10">
              <Database className="size-6 text-primary" />
            </div>
            <div>
              <p className="font-medium">还没有可授权的知识库</p>
              <p className="mt-1 text-sm text-muted-foreground">先创建知识库并上传资料，再回来为智能体授权。</p>
            </div>
            <Link href="/knowledge/" className={buttonVariants({ variant: "outline" })}>
              前往知识库
              <ExternalLink className="size-4" />
            </Link>
          </CardContent>
        </Card>
      ) : (
        <>
          <Card>
            <CardHeader className="border-b">
              <CardTitle>允许访问的知识库</CardTitle>
              <CardDescription>已选择 {selectedIds.length} / {knowledgeBases.length} 个</CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              {knowledgeBases.length > 4 && (
                <div className="relative">
                  <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                  <Input value={filter} onChange={(event) => setFilter(event.target.value)} placeholder="搜索知识库" className="pl-9" />
                </div>
              )}
              <div className="space-y-2">
                {filtered.map((kb) => {
                  const selected = selectedIds.includes(kb.id);
                  return (
                    <button
                      key={kb.id}
                      type="button"
                      aria-pressed={selected}
                      onClick={() => toggleKnowledgeBase(kb.id)}
                      className={cn(
                        "flex w-full items-start gap-3 rounded-lg border p-3 text-left transition-colors",
                        selected ? "border-primary/40 bg-primary/5" : "hover:bg-muted/50",
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
                          <span className="font-medium">{kb.name}</span>
                          <Badge variant="outline" className="text-[10px]">{kb.embedDims} 维</Badge>
                        </span>
                        <span className="mt-1 block text-xs text-muted-foreground">
                          {kb.description || "未填写描述"}
                        </span>
                        <span className="mt-1 block truncate font-mono text-[10px] text-muted-foreground/70">
                          {kb.embedModel}
                        </span>
                      </span>
                    </button>
                  );
                })}
                {filtered.length === 0 && (
                  <div className="rounded-lg border border-dashed py-8 text-center text-sm text-muted-foreground">没有匹配的知识库</div>
                )}
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>默认召回数量</CardTitle>
              <CardDescription>每次调用知识库检索工具时，最多返回的相关分片数。</CardDescription>
            </CardHeader>
            <CardContent>
              <div className="flex items-center gap-3">
                <Label htmlFor="agent-rag-top-n">Top N</Label>
                <Input
                  id="agent-rag-top-n"
                  type="number"
                  min={1}
                  max={20}
                  value={topN}
                  onChange={(event) => {
                    setSaved(false);
                    setTopN(Math.max(1, Math.min(20, Number(event.target.value) || 1)));
                  }}
                  className="w-24"
                  disabled={selectedIds.length === 0}
                />
                <span className="text-xs text-muted-foreground">允许 1–20，推荐 5</span>
              </div>
            </CardContent>
          </Card>

          <div className="flex items-center justify-between gap-4 rounded-lg border bg-muted/30 px-4 py-3">
            <p className="text-xs text-muted-foreground">
              {selectedIds.length === 0
                ? "保存后将关闭这个智能体的知识库检索能力。"
                : `保存后，智能体可以通过 rag_search 检索 ${selectedIds.length} 个知识库。`}
            </p>
            <div className="flex shrink-0 items-center gap-3">
              {saved && <span className="flex items-center gap-1 text-sm text-emerald-600"><CheckCircle2 className="size-4" />已保存</span>}
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
