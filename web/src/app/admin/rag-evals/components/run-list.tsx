"use client";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cancelRAGEvalRun, type RAGEvalProfile, type RAGEvalRun } from "@/lib/api";

type RunProgress = { parserEngine?: string; generationDurationMs?: number; generationReused?: boolean };

function parseProgress(value: string): RunProgress {
  try { return JSON.parse(value || "{}") as RunProgress; } catch { return {}; }
}

function profileParser(profile?: RAGEvalProfile): string {
  try { return (JSON.parse(profile?.profileJson || "{}") as { ingestion?: { parserEngine?: string } }).ingestion?.parserEngine || "—"; } catch { return "—"; }
}

function durationLabel(milliseconds?: number): string {
  if (milliseconds === undefined) return "等待建库";
  if (milliseconds < 1000) return `${milliseconds} ms`;
  return `${(milliseconds / 1000).toFixed(milliseconds < 10_000 ? 2 : 1)} s`;
}

export function RunList({ runs, profiles, onChanged }: { runs: RAGEvalRun[]; profiles: RAGEvalProfile[]; onChanged: () => Promise<void> }) {
  return <Card><CardHeader><CardTitle>运行队列</CardTitle><CardDescription>完整 Pipeline 会记录隔离建库耗时；复用缓存的运行会单独标记，不能与首次建库直接比较。</CardDescription></CardHeader><CardContent><Table><TableHeader><TableRow><TableHead>运行</TableHead><TableHead>模式</TableHead><TableHead>解析器/建库耗时</TableHead><TableHead>阶段/进度</TableHead><TableHead>状态</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader><TableBody>
    {runs.length === 0 ? <TableRow><TableCell colSpan={6} className="text-center text-muted-foreground">暂无运行</TableCell></TableRow> : runs.map((run) => { const progress = parseProgress(run.progressJson); return <TableRow key={run.id}><TableCell className="font-mono text-xs">{run.id}</TableCell><TableCell>{run.mode === "FULL_PIPELINE" ? "完整 Pipeline" : "仅在线"}</TableCell><TableCell><div className="font-mono text-xs">{progress.parserEngine || profileParser(profiles.find((item) => item.id === run.profileId))}</div><div className="text-xs text-muted-foreground">{durationLabel(progress.generationDurationMs)}{progress.generationReused ? " · 已复用" : ""}</div></TableCell><TableCell><div>{run.stage}</div><div className="max-w-64 truncate font-mono text-xs text-muted-foreground">{run.progressJson || "{}"}</div></TableCell><TableCell><Badge variant="outline">{run.status}</Badge></TableCell><TableCell className="text-right"><Button size="sm" variant="ghost" disabled={!['QUEUED','RUNNING'].includes(run.status)} onClick={() => void cancelRAGEvalRun(run.id).then(onChanged)}>取消</Button></TableCell></TableRow>; })}
  </TableBody></Table></CardContent></Card>;
}
