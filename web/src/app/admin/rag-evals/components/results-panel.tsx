"use client";

import { useState } from "react";
import { ArrowDownRight, ArrowUpRight, BarChart3 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { compareRAGEvalRuns, getRAGEvalRunAnalysis, listRAGEvalRunCases, type RAGEvalAggregate, type RAGEvalCaseResult, type RAGEvalPairedDelta, type RAGEvalRun } from "@/lib/api";
import { denominatorSummary, directionalDelta, latencyAndCost, metricDistribution, thresholdCurve } from "../result-state";

export function ResultsPanel({ runs }: { runs: RAGEvalRun[] }) {
  const [runId, setRunId] = useState("");
  const [run, setRun] = useState<RAGEvalRun | null>(null);
  const [aggregates, setAggregates] = useState<Record<string, RAGEvalAggregate>>({});
  const [cases, setCases] = useState<RAGEvalCaseResult[]>([]);
  const [comparisons, setComparisons] = useState<Record<string, RAGEvalPairedDelta>>({});
  const [error, setError] = useState("");

  async function load(id: string) {
    setRunId(id); setComparisons({});
    if (!id) { setRun(null); setAggregates({}); setCases([]); return; }
    try {
      const [analysis, nextCases] = await Promise.all([getRAGEvalRunAnalysis(id), listRAGEvalRunCases(id)]);
      setRun(analysis.run); setAggregates(analysis.aggregates ?? {}); setCases(nextCases); setError("");
      if (analysis.run.baselineRunId) {
        const pairs = await Promise.all(Object.keys(analysis.aggregates ?? {}).map(async (metric) => [metric, await compareRAGEvalRuns(id, analysis.run.baselineRunId!, metric)] as const));
        setComparisons(Object.fromEntries(pairs));
      }
    } catch (err) { setError(err instanceof Error ? err.message : "加载结果失败"); }
  }

  const performance = latencyAndCost(cases);
  const curve = thresholdCurve(cases);
  const fallbackCount = cases.filter((item) => item.metrics.some((metric) => metric.Details?.rerankerFallback === true)).length;

  return <div className="space-y-6">
    <Card><CardHeader><CardTitle className="flex items-center gap-2"><BarChart3 className="h-5 w-5" />结果分析</CardTitle><CardDescription>质量、性能与成本均显示完整分母；paired diff 只比较可配对样例。</CardDescription></CardHeader><CardContent><div className="flex gap-3"><select className="h-9 min-w-80 rounded-md border bg-background px-3 text-sm" value={runId} onChange={(event) => void load(event.target.value)}><option value="">选择已完成运行</option>{runs.filter((item) => ["SUCCEEDED", "FAILED", "BUDGET_EXCEEDED"].includes(item.status)).map((item) => <option key={item.id} value={item.id}>{item.id} · {item.status}</option>)}</select><Button variant="outline" disabled={!runId} onClick={() => void load(runId)}>刷新</Button></div>{error && <p className="mt-3 text-sm text-destructive">{error}</p>}</CardContent></Card>
    {run && <>
      <div className="grid gap-4 md:grid-cols-3"><Summary title="平均延迟" value={`${performance.meanLatencyMs.toFixed(0)} ms`} detail={`P95 ${performance.p95LatencyMs.toFixed(0)} ms · ${cases.length} cases`} /><Summary title="总成本" value={`$${performance.costUsd.toFixed(4)}`} detail="actual 优先，否则 estimated" /><Summary title="Reranker fallback" value={`${cases.length ? (fallbackCount / cases.length * 100).toFixed(1) : "0.0"}%`} detail={`${fallbackCount}/${cases.length} cases`} /></div>
      <div className="grid gap-4 lg:grid-cols-2">{Object.entries(aggregates).map(([metric, aggregate]) => { const delta = directionalDelta(metric, comparisons[metric]?.absoluteDelta); return <Card key={metric}><CardHeader><CardDescription>{metric}</CardDescription><CardTitle>{aggregate.mean === undefined ? "—" : aggregate.mean.toFixed(4)}</CardTitle></CardHeader><CardContent className="space-y-2 text-sm"><p>median {aggregate.median?.toFixed(4) ?? "—"} · P95 {aggregate.p95?.toFixed(4) ?? "—"}</p><p className="text-muted-foreground">{denominatorSummary(aggregate)}</p><Distribution bins={metricDistribution(cases, metric)} />{comparisons[metric] && <p className={`flex items-center gap-1 ${delta.improved ? "text-emerald-600" : "text-amber-600"}`}>{delta.improved ? <ArrowUpRight className="h-4 w-4" /> : <ArrowDownRight className="h-4 w-4" />}paired {comparisons[metric].pairs}: {delta.label}</p>}</CardContent></Card>; })}</div>
      <Card><CardHeader><CardTitle>最低 reranker 分数阈值曲线</CardTitle><CardDescription>缺失 relevance 标签不会被当作 false；精确率/召回率无分母时显示为空。</CardDescription></CardHeader><CardContent><Table><TableHeader><TableRow><TableHead>最低 reranker 分数</TableHead><TableHead>选中 contexts</TableHead><TableHead>Precision</TableHead><TableHead>Recall</TableHead></TableRow></TableHeader><TableBody>{curve.map((point) => <TableRow key={point.threshold}><TableCell>{point.threshold.toFixed(2)}</TableCell><TableCell>{point.selected}</TableCell><TableCell>{point.precision?.toFixed(3) ?? "—"}</TableCell><TableCell>{point.recall?.toFixed(3) ?? "—"}</TableCell></TableRow>)}</TableBody></Table></CardContent></Card>
      <Card><CardHeader><CardTitle>Case drill-down</CardTitle><CardDescription>contexts 默认折叠；展开后可检查 raw recall/rerank score、过滤原因和逐项指标原因。</CardDescription></CardHeader><CardContent className="space-y-3">{cases.map((item) => <details key={item.caseId} className="rounded-md border p-3"><summary className="cursor-pointer"><span className="font-mono text-xs">{item.caseId}</span> <Badge className="ml-2" variant="outline">{item.status}</Badge> <span className="ml-2 text-xs text-muted-foreground">{item.latencyMs} ms</span></summary><div className="mt-3 space-y-3 text-sm"><p className="whitespace-pre-wrap">{item.response || "无回答"}</p><div>{item.metrics.map((metric) => <div key={`${metric.Name}-${metric.Version}`} className="flex justify-between border-t py-2"><span>{metric.Name} · {metric.Status}</span><span>{metric.Value?.toFixed(4) ?? (metric.Reason || "—")}</span></div>)}</div><details><summary className="cursor-pointer text-muted-foreground">展开 contexts / raw scores / filter reasons</summary><pre className="mt-2 max-h-80 overflow-auto rounded bg-muted p-3 text-xs">{JSON.stringify(item.contexts, null, 2)}</pre></details></div></details>)}</CardContent></Card>
    </>}
  </div>;
}

function Summary({ title, value, detail }: { title: string; value: string; detail: string }) { return <Card><CardHeader><CardDescription>{title}</CardDescription><CardTitle>{value}</CardTitle></CardHeader><CardContent className="text-xs text-muted-foreground">{detail}</CardContent></Card>; }

function Distribution({ bins }: { bins: number[] }) {
  const max = Math.max(1, ...bins);
  return <div><p className="text-xs text-muted-foreground">分布 0–.25 / .25–.5 / .5–.75 / .75–1</p><div className="mt-1 flex h-6 items-end gap-1">{bins.map((count, index) => <div key={index} className="min-h-0.5 flex-1 rounded-sm bg-violet-500" style={{ height: `${Math.max(4, count / max * 100)}%` }} title={`${count}`} />)}</div></div>;
}
