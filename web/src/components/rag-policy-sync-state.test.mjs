import test from "node:test";
import assert from "node:assert/strict";

const { policySyncLocksWrites, policyVisualState, requiresKBNameConfirmation, safePolicySyncFailure, shouldShowPolicyDriftNotice } = await import(new URL("./rag-policy-sync-state.ts", import.meta.url));

const latest = { pinnedVersion: 2, latestVersion: 2, drift: false, differences: [], estimate: { documentCount: 1, sourceBytes: 10, temporaryBytesMax: 20 } };
const outdated = { ...latest, pinnedVersion: 1, drift: true };

test("only old knowledge bases show the non-blocking drift notice", () => {
  assert.equal(shouldShowPolicyDriftNotice(outdated), true);
  assert.equal(shouldShowPolicyDriftNotice(latest), false);
  assert.equal(policyVisualState(latest), "latest");
});

test("syncing locks writes while reads remain represented separately", () => {
  const syncing = { ...outdated, syncTask: { status: "RUNNING" } };
  assert.equal(policyVisualState(syncing), "syncing");
  assert.equal(policySyncLocksWrites(syncing), true);
});

test("failed state uses a safe message that confirms the old index remains live", () => {
  assert.equal(policyVisualState({ ...outdated, syncTask: { status: "FAILED" } }), "failed");
  assert.match(safePolicySyncFailure(), /旧索引仍正常/);
  assert.equal(safePolicySyncFailure().includes("endpoint"), false);
});

test("very large knowledge bases require typing their name", () => {
  assert.equal(requiresKBNameConfirmation({ ...outdated, estimate: { documentCount: 1000, sourceBytes: 1, temporaryBytesMax: 2 } }), true);
  assert.equal(requiresKBNameConfirmation(outdated), false);
});
