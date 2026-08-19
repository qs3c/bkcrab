export interface RAGEvalRunDraft {
  datasetVersionId: string;
  profileId: string;
  mode: "FULL_PIPELINE" | "ONLINE_ONLY";
  baselineRunId: string;
  indexGenerationId: string;
  metrics: string[];
}

export interface RAGEvalProfileSummary {
  id: string;
  name: string;
  profileJson: string;
  fingerprint: string;
  createdAt: string;
}

export interface RAGEvalRunProgress {
  total?: number;
  completed?: number;
  failed?: number;
  scored?: number;
  tokens?: number;
  costUsd?: number;
  parserEngine?: string;
  generationDurationMs?: number;
  generationReused?: boolean;
  documentsTotal?: number;
  documentsCompleted?: number;
  chunksCompleted?: number;
  lastActivityAt?: string;
}

function profileParserLabel(profileJson: string): string {
  try {
    const profile = JSON.parse(profileJson || "{}") as { ingestion?: { parserEngine?: string; parseMode?: string } };
    const parser = profile.ingestion?.parserEngine?.trim().toLowerCase();
    if (parser === "anydoc") return "AnyDoc";
    if (parser === "markitdown") return "MarkItDown";
    if (parser) return parser;
    return profile.ingestion?.parseMode === "standard" ? "Standard" : "默认解析器";
  } catch {
    return "配置异常";
  }
}

export function profileOptionLabel(profile: RAGEvalProfileSummary, profiles: RAGEvalProfileSummary[]): string {
  const siblings = profiles.filter((item) => item.name === profile.name);
  const parser = profileParserLabel(profile.profileJson);
  if (siblings.length < 2) return `${profile.name} · ${parser}`;
  const newest = siblings.reduce((current, item) => {
    const byCreatedAt = item.createdAt.localeCompare(current.createdAt);
    return byCreatedAt > 0 || (byCreatedAt === 0 && item.id > current.id) ? item : current;
  });
  const version = newest.id === profile.id ? "当前" : `历史 ${profile.createdAt.slice(0, 10) || profile.fingerprint.slice(0, 8)}`;
  return `${profile.name} · ${parser} · ${version}`;
}

export function parseRAGEvalRunProgress(value: string): RAGEvalRunProgress {
  try { return JSON.parse(value || "{}") as RAGEvalRunProgress; }
  catch { return {}; }
}

export function runStageLabel(stage: string): string {
  return ({
    queued: "等待调度",
    running: "任务已领取",
    preparing_generation: "准备隔离索引",
    preparing_index: "创建向量索引",
    building_generation: "向量化并写入索引",
    finalizing_generation: "固化索引版本",
    reusing_generation: "复用已有索引",
    answering: "执行检索与回答",
    scoring: "计算测评指标",
    scoring_retry: "评分重试中",
    budget_exceeded: "已达到预算上限",
    finished: "已完成",
  } as Record<string, string>)[stage] ?? stage;
}

export function runProgressAmount(stage: string, progress: RAGEvalRunProgress): { current: number; total: number } | null {
  if (["preparing_generation", "preparing_index", "building_generation", "finalizing_generation", "reusing_generation"].includes(stage)) {
    const total = Number(progress.documentsTotal ?? 0);
    return total > 0 ? { current: Math.min(total, Math.max(0, Number(progress.documentsCompleted ?? 0))), total } : null;
  }
  const total = Number(progress.total ?? 0);
  if (total <= 0) return null;
  const current = stage === "scoring" || stage === "scoring_retry" ? Number(progress.scored ?? 0) : Number(progress.completed ?? 0);
  return { current: Math.min(total, Math.max(0, current)), total };
}

export function isRunProgressStalled(progress: RAGEvalRunProgress, now = Date.now(), thresholdMs = 120_000): boolean {
  if (!progress.lastActivityAt) return false;
  const lastActivity = Date.parse(progress.lastActivityAt);
  return Number.isFinite(lastActivity) && now-lastActivity > thresholdMs;
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
