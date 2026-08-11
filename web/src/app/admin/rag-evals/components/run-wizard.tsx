"use client";

import { FormEvent, useMemo, useState } from "react";
import { AlertTriangle, Play } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { createRAGEvalRun, type RAGEvalCapabilities, type RAGEvalDatasetVersion, type RAGEvalProfile, type RAGEvalRun } from "@/lib/api";
import { estimateRunWork, validateRunDraft, type RAGEvalRunDraft } from "../rag-eval-state";

const initial: RAGEvalRunDraft = { datasetVersionId: "", profileId: "", mode: "FULL_PIPELINE", baselineRunId: "", indexGenerationId: "", metrics: [] };

export function RunWizard({ capabilities, versions, profiles, runs, onCreated }: {
  capabilities: RAGEvalCapabilities | null;
  versions: RAGEvalDatasetVersion[];
  profiles: RAGEvalProfile[];
  runs: RAGEvalRun[];
  onCreated: () => Promise<void>;
}) {
  const [draft, setDraft] = useState(initial);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const ready = versions.filter((item) => item.Status === "READY");
  const selected = ready.find((item) => item.ID === draft.datasetVersionId);
  const estimate = useMemo(() => estimateRunWork(selected?.CaseCount ?? 0, selected?.DocumentCount ?? 0, draft.mode, draft.metrics.length), [selected, draft.mode, draft.metrics.length]);

  function toggleMetric(metric: string) { setDraft((value) => ({ ...value, metrics: value.metrics.includes(metric) ? value.metrics.filter((item) => item !== metric) : [...value.metrics, metric] })); }
  async function submit(event: FormEvent) {
    event.preventDefault();
    const nextErrors = validateRunDraft(draft, capabilities?.metrics ?? []); setErrors(nextErrors);
    if (Object.keys(nextErrors).length || !capabilities?.sidecarHealthy) return;
    setBusy(true);
    try { await createRAGEvalRun(draft); setDraft(initial); await onCreated(); }
    catch (err) { setErrors({ submit: err instanceof Error ? err.message : "创建运行失败" }); }
    finally { setBusy(false); }
  }

  return <Card><CardHeader><CardTitle>新建测评运行</CardTitle><CardDescription>先固定数据版本、模式、候选参数与指标，再进入 durable queue。</CardDescription></CardHeader><CardContent><form className="grid gap-5 lg:grid-cols-2" onSubmit={submit}>
    <Field label="READY 数据版本" error={errors.datasetVersionId}><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={draft.datasetVersionId} onChange={(event) => setDraft({ ...draft, datasetVersionId: event.target.value })}><option value="">请选择</option>{ready.map((item) => <option key={item.ID} value={item.ID}>v{item.Version} · {item.CaseCount} cases / {item.DocumentCount} docs</option>)}</select></Field>
    <Field label="参数 Profile" error={errors.profileId}><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={draft.profileId} onChange={(event) => setDraft({ ...draft, profileId: event.target.value })}><option value="">请选择</option>{profiles.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></Field>
    <Field label="运行模式"><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={draft.mode} onChange={(event) => setDraft({ ...draft, mode: event.target.value as RAGEvalRunDraft["mode"], indexGenerationId: "" })}><option value="FULL_PIPELINE">完整 Pipeline</option><option value="ONLINE_ONLY">仅在线 Pipeline</option></select></Field>
    <Field label="Baseline（可选）"><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={draft.baselineRunId} onChange={(event) => setDraft({ ...draft, baselineRunId: event.target.value })}><option value="">无</option>{runs.filter((run) => run.status === "SUCCEEDED").map((run) => <option key={run.id} value={run.id}>{run.id}</option>)}</select></Field>
    {draft.mode === "ONLINE_ONLY" && <Field label="READY generation ID" error={errors.indexGenerationId}><Input value={draft.indexGenerationId} onChange={(event) => setDraft({ ...draft, indexGenerationId: event.target.value })} placeholder="rge_..." /></Field>}
    <div className="lg:col-span-2"><Label>指标（由 capabilities 驱动）</Label><div className="mt-2 flex flex-wrap gap-2">{(capabilities?.metrics ?? []).map((metric) => <Button key={metric} type="button" size="sm" variant={draft.metrics.includes(metric) ? "default" : "outline"} onClick={() => toggleMetric(metric)}>{metric}</Button>)}</div>{errors.metrics && <p className="mt-1 text-xs text-destructive">{errors.metrics}</p>}</div>
    <div className="lg:col-span-2 rounded-md border bg-muted/30 p-4 text-sm"><p className="font-medium">运行前估算</p><div className="mt-2 grid gap-2 sm:grid-cols-4"><span>{estimate.documents} 文档</span><span>{estimate.cases} cases</span><span>约 {estimate.externalCalls} 次评分调用</span><span>费用上限 ${capabilities?.maxRunCostUsd?.toFixed(2) ?? "—"}</span></div><p className="mt-2 text-xs text-muted-foreground">预算上限：{capabilities?.maxRunTokens?.toLocaleString() ?? "—"} tokens / {capabilities?.maxRunDurationSec ?? "—"} 秒；实际费用取决于模型计价。</p><p className="mt-2 flex items-center gap-1 text-xs text-amber-600"><AlertTriangle className="h-3.5 w-3.5" />不可复现风险：{estimate.reproducibilityRisk}</p></div>
    {!capabilities?.sidecarHealthy && <p className="lg:col-span-2 text-sm text-amber-600">评分 Sidecar 不健康：{capabilities?.reason || "暂不可启动新运行"}</p>}
    {errors.submit && <p className="lg:col-span-2 text-sm text-destructive">{errors.submit}</p>}
    <div className="lg:col-span-2"><Button disabled={busy || !capabilities?.enabled || !capabilities?.sidecarHealthy}><Play className="mr-2 h-4 w-4" />加入运行队列</Button></div>
  </form></CardContent></Card>;
}

function Field({ label, error, children }: { label: string; error?: string; children: React.ReactNode }) { return <div className="space-y-2"><Label>{label}</Label>{children}{error && <p className="text-xs text-destructive">{error}</p>}</div>; }
