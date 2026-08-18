import type { RAGEvalAggregate, RAGEvalCaseResult } from "@/lib/api";

const LOWER_IS_BETTER = new Set(["latency", "latency_ms", "cost", "cost_usd", "error_rate", "reranker_fallback_rate"]);

export function denominatorSummary(metric: RAGEvalAggregate): string {
  return `${metric.scoredCount}/${metric.count} 已评分 · ${metric.skippedCount} skipped · ${metric.errorCount} error`;
}

export function directionalDelta(metric: string, absoluteDelta?: number): { improved: boolean | null; label: string } {
  if (absoluteDelta === undefined || Number.isNaN(absoluteDelta)) return { improved: null, label: "无可比 paired delta" };
  const improved = LOWER_IS_BETTER.has(metric) ? absoluteDelta < 0 : absoluteDelta > 0;
  const sign = absoluteDelta > 0 ? "+" : "";
  return { improved, label: `${sign}${absoluteDelta.toFixed(4)} · ${improved ? "改善" : absoluteDelta === 0 ? "持平" : "退化"}` };
}

export function latencyAndCost(cases: RAGEvalCaseResult[]) {
  const latencies = cases.map((item) => item.latencyMs).filter((value) => Number.isFinite(value)).sort((a, b) => a - b);
  const costs = cases.map((item) => Number(item.usage?.actualCostUsd ?? item.usage?.estimatedCostUsd ?? 0)).filter(Number.isFinite);
  const percentile = (values: number[], p: number) => values.length ? values[Math.min(values.length - 1, Math.max(0, Math.ceil(values.length * p) - 1))] : 0;
  return { meanLatencyMs: latencies.length ? latencies.reduce((sum, value) => sum + value, 0) / latencies.length : 0, p95LatencyMs: percentile(latencies, .95), costUsd: costs.reduce((sum, value) => sum + value, 0) };
}

export function stageLatency(cases: RAGEvalCaseResult[]) {
  const percentile = (values: number[], p: number) => {
    const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);
    return sorted.length ? sorted[Math.min(sorted.length - 1, Math.max(0, Math.ceil(sorted.length * p) - 1))] : 0;
  };
  const values = (field: string) => cases.map((item) => Number(item.searchTrace?.trace?.[field] ?? 0));
  const answer = cases.map((item) => Number(item.answerTrace?.latencyMs ?? 0));
  return {
    retrieval: { p50: percentile(values("retrievalDurationMs"), .5), p95: percentile(values("retrievalDurationMs"), .95) },
    reranker: { p50: percentile(values("rerankerDurationMs"), .5), p95: percentile(values("rerankerDurationMs"), .95) },
    answer: { p50: percentile(answer, .5), p95: percentile(answer, .95) },
  };
}

export function metricDistribution(cases: RAGEvalCaseResult[], metric: string): number[] {
  const bins = [0, 0, 0, 0];
  for (const item of cases) {
    const value = item.metrics.find((entry) => entry.Name === metric && entry.Status === "ok")?.Value;
    if (value === undefined || !Number.isFinite(value)) continue;
    bins[Math.min(3, Math.max(0, Math.floor(value * 4)))]++;
  }
  return bins;
}

export function thresholdCurve(cases: RAGEvalCaseResult[]) {
  const observations = cases.flatMap((item) => item.contexts ?? []).filter((value): value is Record<string, unknown> => !!value && typeof value === "object").map((value) => ({ score: Number(value.rerankScore), relevant: typeof value.relevant === "boolean" ? value.relevant : null })).filter((value) => Number.isFinite(value.score));
  return [0, .25, .5, .75, 1].map((threshold) => {
    const selected = observations.filter((item) => item.score >= threshold);
    const labeled = observations.filter((item) => item.relevant !== null);
    const tp = selected.filter((item) => item.relevant === true).length;
    return { threshold, selected: selected.length, precision: selected.length ? tp / selected.length : null, recall: labeled.filter((item) => item.relevant === true).length ? tp / labeled.filter((item) => item.relevant === true).length : null };
  });
}

export function promotionGateReasons(input: { runStatus?: string; confirmationRunId: string; runId: string; note: string }): string[] {
  const reasons: string[] = [];
  if (input.runStatus !== "SUCCEEDED") reasons.push("候选运行尚未成功完成");
  if (!input.confirmationRunId) reasons.push("需要独立 confirmation run");
  if (input.confirmationRunId === input.runId) reasons.push("confirmation run 不能与候选运行相同");
  if (!input.note.trim()) reasons.push("发布或回滚必须填写审计备注");
  return reasons;
}
