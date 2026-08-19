import test from "node:test";
import assert from "node:assert/strict";

const {
  canShowRAGEvalNavigation,
  estimateRunWork,
  isRunProgressStalled,
  nextRunPollDelay,
  parseRAGEvalRunProgress,
  profileOptionLabel,
  runProgressAmount,
  runStageLabel,
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

test("duplicate immutable profiles are labeled by parser and recency", () => {
  const profiles = [
    { id: "old", name: "系统默认全功能", profileJson: JSON.stringify({ ingestion: { parseMode: "standard" } }), fingerprint: "a".repeat(64), createdAt: "2026-08-16T07:52:15Z" },
    { id: "new", name: "系统默认全功能", profileJson: JSON.stringify({ ingestion: { parserEngine: "anydoc", parseMode: "auto" } }), fingerprint: "b".repeat(64), createdAt: "2026-08-16T09:04:45Z" },
  ];
  assert.equal(profileOptionLabel(profiles[0], profiles), "系统默认全功能 · Standard · 历史 2026-08-16");
  assert.equal(profileOptionLabel(profiles[1], profiles), "系统默认全功能 · AnyDoc · 当前");
});

test("run progress exposes generation counts, translated stages and stalls", () => {
  const progress = parseRAGEvalRunProgress(JSON.stringify({ documentsCompleted: 145, documentsTotal: 488, lastActivityAt: "2026-08-18T16:10:00Z" }));
  assert.deepEqual(runProgressAmount("building_generation", progress), { current: 145, total: 488 });
  assert.equal(runStageLabel("building_generation"), "向量化并写入索引");
  assert.equal(isRunProgressStalled(progress, Date.parse("2026-08-18T16:11:59Z")), false);
  assert.equal(isRunProgressStalled(progress, Date.parse("2026-08-18T16:12:01Z")), true);
});
