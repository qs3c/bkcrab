import test from "node:test";
import assert from "node:assert/strict";

const { caseFailureSummary, caseMetrics, denominatorSummary, directionalDelta, metricDistribution, promotionGateReasons, stageLatency, thresholdCurve } = await import(new URL("./result-state.ts", import.meta.url));

test("metric summary always exposes scored, skipped, error and total denominators", () => {
  assert.equal(denominatorSummary({ count: 20, scoredCount: 15, skippedCount: 3, errorCount: 2, mean: .8 }), "15/20 已评分 · 3 skipped · 2 error");
});

test("paired delta applies opposite direction semantics to quality and latency/cost", () => {
  assert.deepEqual(directionalDelta("faithfulness", .1), { improved: true, label: "+0.1000 · 改善" });
  assert.deepEqual(directionalDelta("latency_ms", -25), { improved: true, label: "-25.0000 · 改善" });
  assert.equal(directionalDelta("cost_usd", .2).improved, false);
});

test("distribution includes only successfully scored metric values", () => {
  const cases = [{ contexts: [], usage: {}, latencyMs: 1, metrics: [
    { Name: "faithfulness", Status: "ok", Value: .1 },
    { Name: "faithfulness", Status: "error", Value: .9 },
  ] }];
  assert.deepEqual(metricDistribution(cases, "faithfulness"), [1, 0, 0, 0]);
});

test("missing metric collections remain renderable and case failures are summarized", () => {
  const failed = { status: "error", errorCode: "search_error", errorMessage: "reranker timeout", contexts: [], usage: {}, latencyMs: 5000, metrics: null };
  assert.deepEqual(caseMetrics(failed), []);
  assert.deepEqual(metricDistribution([failed], "faithfulness"), [0, 0, 0, 0]);
  assert.deepEqual(caseFailureSummary([failed, { ...failed, metrics: [] }]), [
    { key: "search_error: reranker timeout", count: 2 },
  ]);
});

test("threshold curve never treats missing relevance labels as false", () => {
  const curve = thresholdCurve([{ contexts: [{ rerankScore: .9 }, { rerankScore: .8, relevant: true }], usage: {}, metrics: [], latencyMs: 1 }]);
  assert.equal(curve.find((point) => point.threshold === .75).recall, 1);
});

test("stage latency reads persisted production search and answer traces", () => {
  const cases = [10, 20, 100].map((value) => ({ contexts: [], usage: {}, metrics: [], latencyMs: value,
    searchTrace: { trace: { retrievalDurationMs: value, rerankerDurationMs: value / 2 } }, answerTrace: { latencyMs: value * 2 } }));
  assert.deepEqual(stageLatency(cases), {
    retrieval: { p50: 20, p95: 100 }, reranker: { p50: 10, p95: 50 }, answer: { p50: 40, p95: 200 },
  });
});

test("promotion requires success, independent confirmation and audit note", () => {
  assert.deepEqual(promotionGateReasons({ runStatus: "RUNNING", runId: "r1", confirmationRunId: "r1", note: "" }), [
    "候选运行尚未成功完成", "confirmation run 不能与候选运行相同", "发布或回滚必须填写审计备注",
  ]);
});
