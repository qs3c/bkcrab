export interface RAGEvalRunDraft {
  datasetVersionId: string;
  profileId: string;
  mode: "FULL_PIPELINE" | "ONLINE_ONLY";
  baselineRunId: string;
  indexGenerationId: string;
  metrics: string[];
}

const builtinCatalogLabels: Record<string, string> = {
  "ibm-multidoc2dial": "MultiDoc2Dial",
  "next-tat-tatqa": "TAT-QA",
  "vectara-open-ragbench": "Open RAGBench（Vectara）",
};

export function describeRAGEvalDatasetVersion(version: {
  DatasetID: string;
  SourceConfigJSON: string;
  Track: "TEXT_RAG" | "PDF_E2E";
}, dataset?: { name: string }): { key: string; name: string; split?: string; track: string } {
  let catalogId = "";
  let split = "";
  try {
    const source = JSON.parse(version.SourceConfigJSON || "{}") as { catalogId?: string; split?: string };
    catalogId = source.catalogId?.trim() || "";
    split = source.split?.trim() || "";
  } catch { /* fall back to the logical dataset for legacy/custom versions */ }
  const logicalName = dataset?.name.replace(/\s*·\s*(TEXT_RAG|PDF_E2E)\s*$/u, "").trim();
  return {
    key: catalogId || version.DatasetID,
    name: builtinCatalogLabels[catalogId] || logicalName || "自定义测评集",
    split: split || undefined,
    track: version.Track === "PDF_E2E" ? "PDF 端到端" : "文本 RAG",
  };
}

export interface RAGEvalMetricOption {
  id: string;
  description: string;
}

export interface RAGEvalMetricGroup {
  id: "document" | "answer" | "other";
  label: string;
  description: string;
  metrics: RAGEvalMetricOption[];
}

const metricGroups: Array<Omit<RAGEvalMetricGroup, "metrics"> & { definitions: RAGEvalMetricOption[] }> = [
  {
    id: "document",
    label: "文档级检索（自定义分块时推荐）",
    description: "只比较文档 ID；即使生产线与数据集的 Chunk 切分不同，也能判断是否找对文档。",
    definitions: [
      { id: "doc_hit_at_k", description: "Top-K 是否至少包含一篇标准相关文档。" },
      { id: "doc_recall_at_k", description: "Top-K 找回了多少标准相关文档。" },
      { id: "doc_mrr", description: "第一篇标准相关文档在结果中出现得有多早。" },
      { id: "doc_ndcg", description: "综合所有标准相关文档的召回情况和排序位置。" },
    ],
  },
  {
    id: "answer",
    label: "回答质量与引用",
    description: "评估回答是否忠于检索材料、是否切题、是否正确，以及引用和拒答行为。",
    definitions: [
      { id: "context_precision", description: "返回 Context 中与标准答案相关内容的集中程度。" },
      { id: "context_recall", description: "返回 Context 对标准答案所需信息的覆盖程度。" },
      { id: "faithfulness", description: "回答中的陈述能否由返回 Context 支撑。" },
      { id: "response_relevancy", description: "回答是否直接回应用户问题。" },
      { id: "factual_correctness", description: "回答与数据集标准答案的事实一致程度。" },
      { id: "citation_precision", description: "回答中的引用编号是否指向实际返回的 Context。" },
      { id: "citation_coverage", description: "回答中的主要陈述是否带有有效引用。" },
      { id: "abstention_accuracy", description: "应该拒答时是否拒答、不该拒答时是否正常回答。" },
    ],
  },
];

const hiddenMetrics = new Set(["hit_at_k", "recall_at_k", "mrr", "ndcg"]);

export function groupRAGEvalMetrics(availableMetrics: string[]): RAGEvalMetricGroup[] {
  const available = new Set(availableMetrics);
  const known = new Set([...metricGroups.flatMap((group) => group.definitions.map((metric) => metric.id)), ...hiddenMetrics]);
  const groups: RAGEvalMetricGroup[] = metricGroups.map((group) => ({
    id: group.id,
    label: group.label,
    description: group.description,
    metrics: group.definitions.filter((metric) => available.has(metric.id)),
  })).filter((group) => group.metrics.length > 0);
  const other = availableMetrics.filter((metric, index) => !known.has(metric) && availableMetrics.indexOf(metric) === index);
  if (other.length > 0) {
    groups.push({
      id: "other",
      label: "其他指标",
      description: "当前服务额外提供的指标。",
      metrics: other.map((id) => ({ id, description: id })),
    });
  }
  return groups;
}

export function toggleRAGEvalMetricGroup(selectedMetrics: string[], groupMetrics: string[]): string[] {
  const group = [...new Set(groupMetrics)];
  if (group.length === 0) return [...selectedMetrics];
  const selected = new Set(selectedMetrics);
  if (group.every((metric) => selected.has(metric))) {
    const groupSet = new Set(group);
    return selectedMetrics.filter((metric) => !groupSet.has(metric));
  }
  return [...selectedMetrics, ...group.filter((metric) => !selected.has(metric))];
}

export interface RAGEvalProfileSummary {
  id: string;
  name: string;
  profileJson: string;
  fingerprint: string;
  createdAt: string;
}

export interface RAGEvalRunSummary {
  id: string;
  datasetVersionId: string;
  mode: "FULL_PIPELINE" | "ONLINE_ONLY";
  indexGenerationId?: string;
  status: string;
}

export function compatibleBaselineRuns(runs: RAGEvalRunSummary[], draft: RAGEvalRunDraft): RAGEvalRunSummary[] {
  if (!draft.datasetVersionId) return [];
  return runs.filter((run) => run.status === "SUCCEEDED" && run.datasetVersionId === draft.datasetVersionId &&
    run.mode === draft.mode && (draft.mode !== "ONLINE_ONLY" || run.indexGenerationId === draft.indexGenerationId));
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

export function isProfileDeletionPending(deletingProfileId: string, sourceProfileId: string): boolean {
  return deletingProfileId !== "" && deletingProfileId === sourceProfileId;
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
