import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";

const { denominatorSummary, directionalDelta, promotionGateReasons } = await import(new URL("./result-state.ts", import.meta.url));

const bundle = JSON.parse(await readFile(new URL("../../../../../testdata/rag-eval/e2e_golden.json", import.meta.url), "utf8"));

test("Phase H shared golden comparison remains promotable and denominator-safe", () => {
  const expected = bundle.expected;
  assert.ok(Math.abs((expected.candidateScore - expected.baselineScore) - expected.pairedDelta) < 1e-12);
  assert.deepEqual(directionalDelta("faithfulness", expected.pairedDelta), {
    improved: true,
    label: "+0.2000 · 改善",
  });
  assert.equal(
    denominatorSummary({
      count: expected.scoredCases,
      scoredCount: expected.scoredCases,
      skippedCount: 0,
      errorCount: 0,
      mean: expected.candidateScore,
    }),
    "1/1 已评分 · 0 skipped · 0 error",
  );
  assert.deepEqual(
    promotionGateReasons({
      runStatus: "SUCCEEDED",
      runId: "candidate",
      confirmationRunId: "baseline",
      note: "phase-h golden release gate",
    }),
    [],
  );
});
