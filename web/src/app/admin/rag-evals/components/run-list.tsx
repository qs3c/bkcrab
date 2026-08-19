"use client";

import { AlertTriangle } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cancelRAGEvalRun, type RAGEvalDatasetVersion, type RAGEvalProfile, type RAGEvalRun } from "@/lib/api";
import { isRunProgressStalled, parseRAGEvalRunProgress, runProgressAmount, runStageLabel, type RAGEvalRunProgress } from "../rag-eval-state";

function profileParser(profile?: RAGEvalProfile): string {
  try { return (JSON.parse(profile?.profileJson || "{}") as { ingestion?: { parserEngine?: string } }).ingestion?.parserEngine || "—"; }
  catch { return "—"; }
}

function durationLabel(milliseconds?: number): string {
  if (milliseconds === undefined) return "建库计时中";
  if (milliseconds < 1000) return `${milliseconds} ms`;
  return `${(milliseconds / 1000).toFixed(milliseconds < 10_000 ? 2 : 1)} s`;
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

function RunProgress({ run, progress }: { run: RAGEvalRun; progress: RAGEvalRunProgress }) {
  const amount = runProgressAmount(run.stage, progress);
  const percent = amount ? Math.round(amount.current / amount.total * 100) : 0;
  const stalled = run.status === "RUNNING" && isRunProgressStalled(progress);
  return <div className="min-w-64 space-y-1.5">
    <div className="flex items-center justify-between gap-3"><span>{runStageLabel(run.stage)}</span>{amount && <span className="font-mono text-xs text-muted-foreground">{amount.current}/{amount.total} {progressUnit(run.stage)} · {percent}%</span>}</div>
    {amount && <div className="h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${percent}%` }} /></div>}
    <div className="flex flex-wrap gap-x-3 text-[11px] text-muted-foreground"><span>{activityLabel(progress.lastActivityAt)}</span>{Number(progress.chunksCompleted) > 0 && <span>{progress.chunksCompleted} chunks</span>}</div>
    {stalled && <p className="flex items-center gap-1 text-[11px] text-amber-600"><AlertTriangle className="h-3 w-3" />超过 2 分钟没有新进度，请检查后台服务。</p>}
    {run.errorMessage && <p className="max-w-80 truncate text-[11px] text-destructive" title={run.errorMessage}>{run.errorMessage}</p>}
  </div>;
}

export function RunList({ runs, profiles, versions, onChanged }: { runs: RAGEvalRun[]; profiles: RAGEvalProfile[]; versions: RAGEvalDatasetVersion[]; onChanged: () => Promise<void> }) {
  return <Card><CardHeader><CardTitle>运行队列</CardTitle><CardDescription>完整 Pipeline 会持续展示隔离建库、回答和评分进度；超过两分钟无新进展时会提示检查后台。</CardDescription></CardHeader><CardContent><Table><TableHeader><TableRow><TableHead>运行</TableHead><TableHead>模式</TableHead><TableHead>解析器/建库耗时</TableHead><TableHead>阶段/进度</TableHead><TableHead>状态</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader><TableBody>
    {runs.length === 0 ? <TableRow><TableCell colSpan={6} className="text-center text-muted-foreground">暂无运行</TableCell></TableRow> : runs.map((run) => {
      const progress = parseRAGEvalRunProgress(run.progressJson);
      const version = versions.find((item) => item.ID === run.datasetVersionId);
      const parser = version?.Track === "TEXT_RAG" ? "canonical-text（绕过解析器）" : progress.parserEngine || profileParser(profiles.find((item) => item.id === run.profileId));
      return <TableRow key={run.id}>
        <TableCell className="font-mono text-xs">{run.id}</TableCell>
        <TableCell>{run.mode === "FULL_PIPELINE" ? "完整 Pipeline" : "仅在线"}</TableCell>
        <TableCell><div className="font-mono text-xs">{parser}</div><div className="text-xs text-muted-foreground">{durationLabel(progress.generationDurationMs)}{progress.generationReused ? " · 已复用" : ""}</div></TableCell>
        <TableCell><RunProgress run={run} progress={progress} /></TableCell>
        <TableCell><Badge variant="outline">{run.status}</Badge></TableCell>
        <TableCell className="text-right"><Button size="sm" variant="ghost" disabled={!['QUEUED','RUNNING'].includes(run.status)} onClick={() => void cancelRAGEvalRun(run.id).then(onChanged)}>取消</Button></TableCell>
      </TableRow>;
    })}
  </TableBody></Table></CardContent></Card>;
}
