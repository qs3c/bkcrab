"use client";

import { useState } from "react";
import { ArrowDownRight, ArrowUpRight, BarChart3 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { compareRAGEvalRuns, getRAGEvalRunAnalysis, listRAGEvalRunCases, type RAGEvalAggregate, type RAGEvalCaseResult, type RAGEvalPairedDelta, type RAGEvalRun, type RAGEvalUsageTotals } from "@/lib/api";
import { caseFailureSummary, caseMetrics, denominatorSummary, directionalDelta, latencyAndCost, metricDistribution, stageLatency, thresholdCurve } from "../result-state";

export function ResultsPanel({ runs }: { runs: RAGEvalRun[] }) {
  const [runId, setRunId] = useState("");
  const [run, setRun] = useState<RAGEvalRun | null>(null);
  const [aggregates, setAggregates] = useState<Record<string, RAGEvalAggregate>>({});
  const [cases, setCases] = useState<RAGEvalCaseResult[]>([]);
  const [comparisons, setComparisons] = useState<Record<string, RAGEvalPairedDelta>>({});
  const [usage, setUsage] = useState<RAGEvalUsageTotals>({ tokens: 0, costUsd: 0 });
  const [error, setError] = useState("");

  async function load(id: string) {
    setRunId(id); setComparisons({});
    if (!id) { setRun(null); setAggregates({}); setCases([]); setUsage({ tokens: 0, costUsd: 0 }); return; }
    try {
      const [analysis, nextCases] = await Promise.all([getRAGEvalRunAnalysis(id), listRAGEvalRunCases(id)]);
      setRun(analysis.run); setAggregates(analysis.aggregates ?? {}); setUsage(analysis.usage ?? { tokens: 0, costUsd: 0 }); setCases(nextCases); setError("");
      if (analysis.run.baselineRunId) {
        const pairs = await Promise.all(Object.keys(analysis.aggregates ?? {}).map(async (metric) => [metric, await compareRAGEvalRuns(id, analysis.run.baselineRunId!, metric)] as const));
        setComparisons(Object.fromEntries(pairs));
      }
    } catch (err) { setError(err instanceof Error ? err.message : "加载结果失败"); }
  }

  const performance = latencyAndCost(cases);
  const stages = stageLatency(cases);
  const curve = thresholdCurve(cases);
  const fallbackCount = cases.filter((item) => item.searchTrace?.trace?.rerankerFallback === true).length;
  const failedCount = cases.filter((item) => item.status === "error").length;
  const failures = caseFailureSummary(cases);
  const scoredCount = Object.values(aggregates).reduce((total, aggregate) => total + aggregate.scoredCount, 0);

  return <div className="space-y-6">
    <Card><CardHeader><CardTitle className="flex items-center gap-2"><BarChart3 className="h-5 w-5" />结果分析</CardTitle><CardDescription>质量、性能与成本均显示完整分母；paired diff 只比较可配对样例。</CardDescription></CardHeader><CardContent><div className="flex gap-3"><select className="h-9 min-w-80 rounded-md border bg-background px-3 text-sm" value={runId} onChange={(event) => void load(event.target.value)}><option value="">选择已完成运行</option>{runs.filter((item) => ["SUCCEEDED", "FAILED", "BUDGET_EXCEEDED"].includes(item.status)).map((item) => <option key={item.id} value={item.id}>{item.id} · {item.status}</option>)}</select><Button variant="outline" disabled={!runId} onClick={() => void load(runId)}>刷新</Button></div>{error && <p className="mt-3 text-sm text-destructive">{error}</p>}</CardContent></Card>
    {run && <>
      {failedCount > 0 && <Card className="border-destructive/50"><CardHeader><CardTitle className="text-destructive">Case 执行失败</CardTitle><CardDescription>{failedCount}/{cases.length} 个 case 失败{scoredCount === 0 ? "，没有可展示的评分结果" : ""}。运行状态：{run.status}。</CardDescription></CardHeader><CardContent className="space-y-2 text-sm">{failures.slice(0, 5).map((item) => <div key={item.key} className="rounded-md bg-destructive/10 p-3"><span className="font-medium">{item.count} 次</span><p className="mt-1 break-words text-xs text-muted-foreground">{item.key}</p></div>)}</CardContent></Card>}
      <div className="grid gap-4 md:grid-cols-3"><Summary title="平均延迟" value={`${performance.meanLatencyMs.toFixed(0)} ms`} detail={`P95 ${performance.p95LatencyMs.toFixed(0)} ms · ${cases.length} cases`} /><Summary title="总成本" value={`$${usage.costUsd.toFixed(4)}`} detail={`${usage.tokens.toLocaleString()} tokens · 回答和评分等全部账本阶段`} /><Summary title="Reranker fallback" value={`${cases.length ? (fallbackCount / cases.length * 100).toFixed(1) : "0.0"}%`} detail={`${fallbackCount}/${cases.length} cases`} /></div>
      <div className="grid gap-4 md:grid-cols-3"><Summary title="检索阶段" value={`P50 ${stages.retrieval.p50.toFixed(0)} ms`} detail={`P95 ${stages.retrieval.p95.toFixed(0)} ms（含 query embedding + recall）`} /><Summary title="Reranker 阶段" value={`P50 ${stages.reranker.p50.toFixed(0)} ms`} detail={`P95 ${stages.reranker.p95.toFixed(0)} ms`} /><Summary title="回答阶段" value={`P50 ${stages.answer.p50.toFixed(0)} ms`} detail={`P95 ${stages.answer.p95.toFixed(0)} ms`} /></div>
      <div className="grid gap-4 lg:grid-cols-2">{Object.entries(aggregates).map(([metric, aggregate]) => { const delta = directionalDelta(metric, comparisons[metric]?.absoluteDelta); return <Card key={metric}><CardHeader><CardDescription>{metric}</CardDescription><CardTitle>{formatNumber(aggregate.mean, 4)}</CardTitle></CardHeader><CardContent className="space-y-2 text-sm"><p>median {formatNumber(aggregate.median, 4)} · P95 {formatNumber(aggregate.p95, 4)}</p><p className="text-muted-foreground">{denominatorSummary(aggregate)}</p><Distribution bins={metricDistribution(cases, metric)} />{comparisons[metric] && <p className={`flex items-center gap-1 ${delta.improved ? "text-emerald-600" : "text-amber-600"}`}>{delta.improved ? <ArrowUpRight className="h-4 w-4" /> : <ArrowDownRight className="h-4 w-4" />}paired {comparisons[metric].pairs}: {delta.label}</p>}</CardContent></Card>; })}</div>
      <Card><CardHeader><CardTitle>最低 reranker 分数阈值曲线</CardTitle><CardDescription>基于阈值过滤前候选；relevance 是“候选所在文档是否为数据集参考文档”的文档级标签。</CardDescription></CardHeader><CardContent>{curve.capturedCases === 0 ? <p className="text-sm text-muted-foreground">该运行未保存阈值过滤前的 reranker 候选，无法可靠重建曲线。请用修复后的版本新建运行。</p> : <><p className="mb-3 text-xs text-muted-foreground">覆盖 {curve.capturedCases}/{curve.totalCases} cases · {curve.observations} candidates · {curve.labeledObservations} 个有 relevance 标签</p><Table><TableHeader><TableRow><TableHead>最低 reranker 分数</TableHead><TableHead>选中 contexts</TableHead><TableHead>Precision</TableHead><TableHead>Recall</TableHead></TableRow></TableHeader><TableBody>{curve.points.map((point) => <TableRow key={point.threshold}><TableCell>{point.threshold.toFixed(2)}</TableCell><TableCell>{point.selected}</TableCell><TableCell>{point.precision?.toFixed(3) ?? "—"}</TableCell><TableCell>{point.recall?.toFixed(3) ?? "—"}</TableCell></TableRow>)}</TableBody></Table></>}</CardContent></Card>
      <Card><CardHeader><CardTitle>Case drill-down</CardTitle><CardDescription>回答、评分、检索分数、阈值过滤统计和模型 trace 分开显示。</CardDescription></CardHeader><CardContent className="space-y-3">{cases.map((item) => { const metrics = caseMetrics(item); return <details key={item.caseId} className="rounded-md border p-3"><summary className="cursor-pointer"><span className="font-mono text-xs">{item.caseId}</span> <Badge className="ml-2" variant="outline">{item.status}</Badge> <span className="ml-2 text-xs text-muted-foreground">{item.latencyMs} ms</span></summary><div className="mt-3 space-y-3 text-sm">{item.errorCode && <div className="rounded-md bg-destructive/10 p-3 text-destructive"><p className="font-medium">{item.errorCode}</p>{item.errorMessage && <p className="mt-1 break-words text-xs">{item.errorMessage}</p>}</div>}<p className="whitespace-pre-wrap">{item.response || "无回答"}</p><div>{metrics.length ? metrics.map((metric) => <div key={`${metric.Name}-${metric.Version}`} className="flex justify-between border-t py-2"><span>{metric.Name} · {metric.Status}</span><span title={metric.Reason || undefined}>{formatNumber(metric.Value, 4, metric.Reason || "—")}</span></div>) : <p className="text-xs text-muted-foreground">无评分结果</p>}</div><TraceDetails title="Contexts（送入回答模型的正文）" value={item.contexts} /><TraceDetails title="检索 trace（阶段、数量、阈值和过滤原因）" value={item.searchTrace?.trace ?? {}} /><TraceDetails title="候选分数（recall / rerank / selected / relevant）" value={item.searchTrace?.hits ?? []} /><TraceDetails title="回答模型 trace" value={item.answerTrace ?? {}} /></div></details>; })}</CardContent></Card>
    </>}
  </div>;
}

function Summary({ title, value, detail }: { title: string; value: string; detail: string }) { return <Card><CardHeader><CardDescription>{title}</CardDescription><CardTitle>{value}</CardTitle></CardHeader><CardContent className="text-xs text-muted-foreground">{detail}</CardContent></Card>; }

function TraceDetails({ title, value }: { title: string; value: unknown }) { return <details><summary className="cursor-pointer text-muted-foreground">{title}</summary><pre className="mt-2 max-h-80 overflow-auto rounded bg-muted p-3 text-xs">{JSON.stringify(value, null, 2)}</pre></details>; }

function formatNumber(value: number | null | undefined, digits: number, fallback = "—"): string {
  return typeof value === "number" && Number.isFinite(value) ? value.toFixed(digits) : fallback;
}

function Distribution({ bins }: { bins: number[] }) {
  const max = Math.max(1, ...bins);
  return <div><p className="text-xs text-muted-foreground">分布 0–.25 / .25–.5 / .5–.75 / .75–1</p><div className="mt-1 flex h-6 items-end gap-1">{bins.map((count, index) => <div key={index} className="min-h-0.5 flex-1 rounded-sm bg-violet-500" style={{ height: `${Math.max(4, count / max * 100)}%` }} title={`${count}`} />)}</div></div>;
}
