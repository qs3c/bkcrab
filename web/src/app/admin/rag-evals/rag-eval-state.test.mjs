import test from "node:test";
import assert from "node:assert/strict";

const {
  canShowRAGEvalNavigation,
  estimateRunWork,
  nextRunPollDelay,
  validateRunDraft,
  validationIssueMessages,
} = await import(new URL("./rag-eval-state.ts", import.meta.url));

test("navigation is visible only to a writable super-admin browser session", () => {
  assert.equal(canShowRAGEvalNavigation({ role: "super_admin", authMethod: "session" }), true);
  assert.equal(canShowRAGEvalNavigation({ role: "user", authMethod: "session" }), false);
  assert.equal(canShowRAGEvalNavigation({ role: "super_admin", authMethod: "apikey" }), false);
  assert.equal(canShowRAGEvalNavigation({ role: "super_admin", authMethod: "session", readOnly: true }), false);
});

test("run form validates mode-specific generation and capability metrics", () => {
  const errors = validateRunDraft({
    datasetVersionId: "ready-v1",
    profileId: "profile-1",
    mode: "ONLINE_ONLY",
    baselineRunId: "",
    indexGenerationId: "",
    metrics: ["not-enabled"],
  }, ["faithfulness"]);
  assert.match(errors.indexGenerationId, /generation/);
  assert.match(errors.metrics, /不支持/);
});

test("validation errors preserve paths and warnings instead of flattening counts", () => {
  assert.deepEqual(validationIssueMessages({
    errors: [{ path: "cases[2].id", message: "重复" }],
    warnings: [{ code: "reference_missing" }],
  }), ["cases[2].id: 重复", "reference_missing"]);
});

test("active run polling slows while hidden and stops when terminal", () => {
  assert.equal(nextRunPollDelay(true, false), 5_000);
  assert.equal(nextRunPollDelay(true, true), 30_000);
  assert.equal(nextRunPollDelay(false, false), null);
});

test("run estimate exposes workload and reproducibility risk", () => {
  assert.deepEqual(estimateRunWork(20, 3, "FULL_PIPELINE", 2), {
    cases: 20,
    documents: 3,
    externalCalls: 40,
    reproducibilityRisk: "外部模型与解析服务可能随时间漂移",
  });
});
