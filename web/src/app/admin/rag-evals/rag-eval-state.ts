export interface RAGEvalRunDraft {
  datasetVersionId: string;
  profileId: string;
  mode: "FULL_PIPELINE" | "ONLINE_ONLY";
  baselineRunId: string;
  indexGenerationId: string;
  metrics: string[];
}

export function canShowRAGEvalNavigation(input: {
  role?: string;
  authMethod?: string;
  readOnly?: boolean;
}): boolean {
  return input.role === "super_admin" && input.authMethod === "session" && !input.readOnly;
}

export function validateRunDraft(
  draft: RAGEvalRunDraft,
  allowedMetrics: string[],
): Record<string, string> {
  const errors: Record<string, string> = {};
  if (!draft.datasetVersionId) errors.datasetVersionId = "请选择 READY 数据集版本";
  if (!draft.profileId) errors.profileId = "请选择实验参数配置";
  if (draft.metrics.length === 0) errors.metrics = "至少选择一个指标";
  if (draft.metrics.some((metric) => !allowedMetrics.includes(metric))) {
    errors.metrics = "包含当前服务不支持的指标";
  }
  if (draft.mode === "ONLINE_ONLY" && !draft.indexGenerationId) {
    errors.indexGenerationId = "仅在线模式必须指定 READY generation";
  }
  if (draft.mode === "FULL_PIPELINE" && draft.indexGenerationId) {
    errors.indexGenerationId = "完整 Pipeline 会创建隔离索引，不能指定 generation";
  }
  return errors;
}

export function validationIssueMessages(report: {
  errors?: Array<{ path?: string; message?: string; code?: string }>;
  warnings?: Array<{ path?: string; message?: string; code?: string }>;
} | null | undefined): string[] {
  const issues = [...(report?.errors ?? []), ...(report?.warnings ?? [])];
  return issues.map((issue) => {
    const location = issue.path ? `${issue.path}: ` : "";
    return `${location}${issue.message || issue.code || "未知校验问题"}`;
  });
}

export function nextRunPollDelay(hasActiveRuns: boolean, hidden: boolean): number | null {
  if (!hasActiveRuns) return null;
  return hidden ? 30_000 : 5_000;
}

export function estimateRunWork(caseCount: number, documentCount: number, mode: RAGEvalRunDraft["mode"], metricCount: number) {
  const externalCalls = Math.max(0, caseCount) * Math.max(1, metricCount);
  return {
    cases: Math.max(0, caseCount),
    documents: mode === "FULL_PIPELINE" ? Math.max(0, documentCount) : 0,
    externalCalls,
    reproducibilityRisk: mode === "ONLINE_ONLY" ? "依赖所选 generation 必须保持 READY" : "外部模型与解析服务可能随时间漂移",
  };
}
