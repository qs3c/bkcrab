import type { ParserEvalCapabilities, ParserEvalRunStatus } from "@/lib/api";

export interface ParserEvalFileLike {
  name: string;
  size: number;
  lastModified?: number;
}

export type ParserEvalUploadStatus = "pending" | "uploading" | "uploaded" | "failed";

export interface ParserEvalUploadState<T extends ParserEvalFileLike = ParserEvalFileLike> {
  id: string;
  file: T;
  idempotencyKey: string;
  status: ParserEvalUploadStatus;
  progress: number;
  documentId?: string;
  error?: string;
}

const MIME_BY_EXTENSION: Record<string, string> = {
  docx: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
  pptx: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
  xlsx: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
};

export function parserEvalExtension(fileName: string): "docx" | "pptx" | "xlsx" | "" {
  const extension = fileName.trim().toLowerCase().match(/\.([^.]+)$/)?.[1] ?? "";
  return extension === "docx" || extension === "pptx" || extension === "xlsx" ? extension : "";
}

export function parserEvalMimeType(fileName: string): string {
  return MIME_BY_EXTENSION[parserEvalExtension(fileName)] ?? "";
}

export function parserEvalFileKey(file: ParserEvalFileLike): string {
  return `${file.name}\u0000${file.size}\u0000${file.lastModified ?? 0}`;
}

export function validateParserEvalFiles(
  current: ParserEvalFileLike[],
  incoming: ParserEvalFileLike[],
  capabilities: Pick<ParserEvalCapabilities, "maxFiles" | "maxFileBytes" | "maxBatchBytes">,
): { accepted: ParserEvalFileLike[]; errors: string[] } {
  const accepted: ParserEvalFileLike[] = [];
  const errors: string[] = [];
  const seen = new Set(current.map(parserEvalFileKey));
  let totalBytes = current.reduce((sum, file) => sum + Math.max(0, file.size), 0);

  for (const file of incoming) {
    if (!parserEvalExtension(file.name)) {
      errors.push(`${file.name}：仅支持 .docx / .pptx / .xlsx`);
      continue;
    }
    if (file.size <= 0) {
      errors.push(`${file.name}：文件为空`);
      continue;
    }
    if (file.size > capabilities.maxFileBytes) {
      errors.push(`${file.name}：超过单文件大小限制`);
      continue;
    }
    const key = parserEvalFileKey(file);
    if (seen.has(key)) {
      errors.push(`${file.name}：已在上传队列中`);
      continue;
    }
    if (current.length + accepted.length >= capabilities.maxFiles) {
      errors.push(`最多上传 ${capabilities.maxFiles} 个文件`);
      break;
    }
    if (totalBytes + file.size > capabilities.maxBatchBytes) {
      errors.push(`${file.name}：加入后会超过批次总大小限制`);
      continue;
    }
    accepted.push(file);
    seen.add(key);
    totalBytes += file.size;
  }
  return { accepted, errors };
}

export function patchParserEvalUpload<T extends ParserEvalFileLike>(
  items: ParserEvalUploadState<T>[],
  id: string,
  patch: Partial<Omit<ParserEvalUploadState<T>, "id" | "file" | "idempotencyKey">>,
): ParserEvalUploadState<T>[] {
  return items.map((item) => item.id === id ? { ...item, ...patch } : item);
}

export function parserEvalRunIsActive(status: ParserEvalRunStatus | string): boolean {
  return status === "QUEUED" || status === "RUNNING";
}

export function parserEvalRunIsTerminal(status: ParserEvalRunStatus | string): boolean {
  return ["SUCCEEDED", "PARTIAL", "FAILED", "CANCELLED"].includes(status);
}

export function parserEvalCanRetry(status: ParserEvalRunStatus | string): boolean {
  return status === "PARTIAL" || status === "FAILED";
}

export function nextParserEvalPollDelay(hasActiveRuns: boolean, hidden: boolean): number | null {
  if (!hasActiveRuns) return null;
  return hidden ? 30_000 : 4_000;
}

export function parserEvalStageLabel(stage: string): string {
  return ({
    UPLOADING: "上传文档",
    VALIDATING: "校验文档",
    RENDERING: "生成页面真值",
    PARSING: "双解析器执行",
    SCORING: "视觉模型评分",
    AGGREGATING: "汇总结果",
  } as Record<string, string>)[stage] ?? stage;
}

export function parserEvalWinnerLabel(winner?: string): string {
  if (winner === "markitdown") return "MarkItDown";
  if (winner === "anydoc") return "AnyDoc";
  if (winner === "tie") return "平局";
  return "—";
}

export function formatParserEvalDuration(value?: number): string {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  if (value < 1000) return `${Math.round(value)} ms`;
  return `${(value / 1000).toFixed(value < 10_000 ? 2 : 1)} s`;
}

export function formatParserEvalScore(value?: number): string {
  return typeof value === "number" && Number.isFinite(value) ? value.toFixed(1) : "—";
}

export function formatParserEvalBytes(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB"];
  const index = Math.min(units.length - 1, Math.floor(Math.log(value) / Math.log(1024)));
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}
