import test from "node:test";
import assert from "node:assert/strict";

const {
  appendTrendSample,
  statusLabel,
  trendMaximum,
  trendSample,
} = await import(new URL("./fairqueue-health-state.ts", import.meta.url));

const health = {
  redis: {
    activeCount: 3,
    ringMemberCount: 2,
    globalInflight: 4,
    stableCount: 3,
    processingCount: 2,
  },
  rabbit: { readyDepthSample: 7, dlqDepthSample: 0 },
};

test("projects a health snapshot into a bounded trend series", () => {
  const first = trendSample(health, 1000);
  const second = { ...first, at: 2000, rabbitReady: 9 };
  const third = { ...first, at: 3000, globalInflight: 1 };
  const original = [first];

  const result = appendTrendSample(appendTrendSample(original, second, 2), third, 2);

  assert.deepEqual(original, [first]);
  assert.deepEqual(result, [second, third]);
  assert.equal(first.stable, 3);
  assert.equal("processing" in first, false);
  assert.equal(trendMaximum(result), 9);
});

test("keeps an idle chart scale visible and translates operational states", () => {
  assert.equal(trendMaximum([]), 1);
  assert.equal(statusLabel("healthy"), "正常");
  assert.equal(statusLabel("recovering"), "降级");
  assert.equal(statusLabel("unavailable"), "异常");
  assert.equal(statusLabel("custom"), "custom");
});
