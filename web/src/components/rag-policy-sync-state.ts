import type { RAGKBPolicyStatus } from "@/lib/api";

export type PolicyVisualState = "latest" | "outdated" | "syncing" | "failed";

export function policyVisualState(status: RAGKBPolicyStatus | null): PolicyVisualState {
  if (status?.syncTask?.status === "QUEUED" || status?.syncTask?.status === "RUNNING") return "syncing";
  if (status?.syncTask?.status === "FAILED") return "failed";
  return status?.drift ? "outdated" : "latest";
}

export function shouldShowPolicyDriftNotice(status: RAGKBPolicyStatus | null): boolean {
  return policyVisualState(status) === "outdated";
}

export function policySyncLocksWrites(status: RAGKBPolicyStatus | null): boolean {
  return policyVisualState(status) === "syncing";
}

export function requiresKBNameConfirmation(status: RAGKBPolicyStatus | null): boolean {
  return (status?.estimate.documentCount ?? 0) >= 1_000 || (status?.estimate.sourceBytes ?? 0) >= 5 * 1024 ** 3;
}

export function safePolicySyncFailure(): string {
  return "策略同步失败；旧索引仍正常。请检查能力配置后重试。";
}
