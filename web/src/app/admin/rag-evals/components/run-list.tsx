"use client";

import { useMemo, useState } from "react";
import { AlertTriangle, RotateCcw, Trash2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cancelRAGEvalRun, deleteRAGEvalRun, retryRAGEvalRun, type RAGEvalDataset, type RAGEvalDatasetVersion, type RAGEvalProfile, type RAGEvalRun } from "@/lib/api";
import { isRunProgressStalled, parseRAGEvalRunProgress, runProgressAmount, runStageLabel, sortRAGEvalRunsNewestFirst, type RAGEvalRunProgress } from "../rag-eval-state";

function profileParser(profile?: RAGEvalProfile): string {
  try { return (JSON.parse(profile?.profileJson || "{}") as { ingestion?: { parserEngine?: string } }).ingestion?.parserEngine || "—"; }
  catch { return "—"; }
}

function shortRunID(id: string): string {
  return id.replace(/^rer_/, "").slice(0, 6) || id.slice(0, 6);
}

function datasetName(name?: string): string {
  return name?.replace(/\s*·\s*(TEXT_RAG|PDF_E2E)\s*$/u, "").trim() || "未知测评集";
}

function dateTimeLabel(value?: string): string {
  if (!value) return "等待开始";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "时间未知";
  return parsed.toLocaleString("zh-CN", { hour12: false });
}

function activityLabel(value?: string): string {
  if (!value) return "等待第一条进度";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "最近进度时间未知";
  return `最近进展 ${parsed.toLocaleTimeString("zh-CN", { hour12: false })}`;
}

function progressUnit(stage: string): string {
  if (["preparing_generation", "preparing_index", "building_generation", "finalizing_generation", "reusing_generation"].includes(stage)) return "文档";
  return stage === "scoring" || stage === "scoring_retry" ? "样例评分" : "样例";
}

function displayedStage(run: RAGEvalRun, progress: RAGEvalRunProgress): string {
  if (run.status !== "BUDGET_EXCEEDED") return runStageLabel(run.stage);
  if (run.errorCode !== "duration_budget_exceeded") return "已达到预算上限";
  return progress.evaluationStartedAt ? "回答与评分时长超限" : "任务总时长达到旧版上限";
}

function statusLabel(status: string): string {
  return ({ QUEUED: "等待中", RUNNING: "运行中", SUCCEEDED: "成功", FAILED: "失败", CANCELLED: "已取消", BUDGET_EXCEEDED: "预算终止" } as Record<string, string>)[status] ?? status;
}

function RunProgress({ run, progress }: { run: RAGEvalRun; progress: RAGEvalRunProgress }) {
  const amount = runProgressAmount(run.stage, progress);
  const percent = amount ? Math.round(amount.current / amount.total * 100) : 0;
  const stalled = run.status === "RUNNING" && isRunProgressStalled(progress);
  return <div className="min-w-64 space-y-1.5">
    <div className="flex items-center justify-between gap-3"><span>{displayedStage(run, progress)}</span>{amount && <span className="font-mono text-xs text-muted-foreground">{amount.current}/{amount.total} {progressUnit(run.stage)} · {percent}%</span>}</div>
    {amount && <div className="h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${percent}%` }} /></div>}
    <div className="flex flex-wrap gap-x-3 text-[11px] text-muted-foreground"><span>{activityLabel(progress.lastActivityAt)}</span>{Number(progress.chunksCompleted) > 0 && <span>{progress.chunksCompleted} chunks</span>}</div>
    {stalled && <p className="flex items-center gap-1 text-[11px] text-amber-600"><AlertTriangle className="h-3 w-3" />超过 2 分钟没有新进度，请检查后台服务。</p>}
    {run.errorMessage && <p className="max-w-80 truncate text-[11px] text-destructive" title={run.errorMessage}>{run.errorMessage}</p>}
  </div>;
}

export function RunList({ runs, datasets, profiles, versions, onChanged }: { runs: RAGEvalRun[]; datasets: RAGEvalDataset[]; profiles: RAGEvalProfile[]; versions: RAGEvalDatasetVersion[]; onChanged: () => Promise<void> }) {
  const [actionError, setActionError] = useState("");
  const [deleting, setDeleting] = useState("");
  const [retrying, setRetrying] = useState("");
  const orderedRuns = useMemo(() => sortRAGEvalRunsNewestFirst(runs), [runs]);
  async function remove(run: RAGEvalRun) {
    if (!window.confirm(`删除已完成运行 ${run.id}？\n\n运行会先从列表隐藏；如果它仍被用作 Baseline，关联数据会延迟清理。`)) return;
    setDeleting(run.id); setActionError("");
    try { await deleteRAGEvalRun(run.id); await onChanged(); }
    catch (err) { setActionError(err instanceof Error ? err.message : "删除运行失败"); }
    finally { setDeleting(""); }
  }
  async function retry(run: RAGEvalRun) {
    if (!window.confirm("使用相同的数据版本、Profile 和指标重新创建测评？\n\n新任务会采用修复后的时长预算；未完成的索引准备可能需要重新执行。")) return;
    setRetrying(run.id); setActionError("");
    try { await retryRAGEvalRun(run.id); await onChanged(); }
    catch (err) { setActionError(err instanceof Error ? err.message : "重新创建运行失败"); }
    finally { setRetrying(""); }
  }
  return <Card><CardHeader><CardTitle>运行队列</CardTitle><CardDescription>按测评集、数据规模和实验配置区分运行，并持续展示当前阶段与进度；超过两分钟无新进展时会提示检查后台。</CardDescription></CardHeader><CardContent>{actionError && <p className="mb-3 text-sm text-destructive">{actionError}</p>}<Table><TableHeader><TableRow><TableHead>测评</TableHead><TableHead>开始时间</TableHead><TableHead>实验配置</TableHead><TableHead>阶段/进度</TableHead><TableHead>状态</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader><TableBody>
    {orderedRuns.length === 0 ? <TableRow><TableCell colSpan={6} className="text-center text-muted-foreground">暂无运行</TableCell></TableRow> : orderedRuns.map((run) => {
      const progress = parseRAGEvalRunProgress(run.progressJson);
      const version = versions.find((item) => item.ID === run.datasetVersionId);
      const dataset = datasets.find((item) => item.id === version?.DatasetID);
      const profile = profiles.find((item) => item.id === run.profileId);
      const parser = version?.Track === "TEXT_RAG" ? "canonical-text（绕过解析器）" : progress.parserEngine || profileParser(profiles.find((item) => item.id === run.profileId));
      return <TableRow key={run.id}>
        <TableCell><div className="min-w-44"><div className="font-medium">{datasetName(dataset?.name)}{version && <span className="ml-1 text-xs font-normal text-muted-foreground">v{version.Version}</span>}</div><div className="mt-1 text-xs text-muted-foreground">{version ? `${version.DocumentCount} 文档 · ${version.CaseCount} 样例` : "数据规模未知"} · <span className="font-mono" title={run.id}>ID {shortRunID(run.id)}</span></div></div></TableCell>
        <TableCell className="whitespace-nowrap text-xs">{dateTimeLabel(run.startedAt)}</TableCell>
        <TableCell><div className="max-w-52 truncate text-sm" title={profile?.name}>{profile?.name || "未知 Profile"}</div><div className="mt-1 text-xs text-muted-foreground"><span className="font-mono">{parser}</span>{progress.generationReused ? " · 索引已复用" : ""}</div></TableCell>
        <TableCell><RunProgress run={run} progress={progress} /></TableCell>
        <TableCell><Badge variant="outline" title={run.status}>{statusLabel(run.status)}</Badge></TableCell>
        <TableCell className="text-right"><div className="flex justify-end gap-1">{['QUEUED','RUNNING'].includes(run.status)
          ? <Button size="sm" variant="ghost" onClick={() => void cancelRAGEvalRun(run.id).then(onChanged)}>取消</Button>
          : <>{['FAILED','BUDGET_EXCEEDED'].includes(run.status) && <Button size="sm" variant="ghost" disabled={retrying === run.id} onClick={() => void retry(run)}><RotateCcw className="mr-1 h-3.5 w-3.5" />{retrying === run.id ? "重试中" : "重试"}</Button>}{['SUCCEEDED','FAILED','CANCELLED','BUDGET_EXCEEDED'].includes(run.status) && <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" disabled={deleting === run.id} onClick={() => void remove(run)}><Trash2 className="mr-1 h-3.5 w-3.5" />{deleting === run.id ? "删除中" : "删除"}</Button>}</>}</div></TableCell>
      </TableRow>;
    })}
  </TableBody></Table></CardContent></Card>;
}
