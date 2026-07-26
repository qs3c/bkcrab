export const MAX_RAG_GALLERY_RESOURCES = 6;

const OFFICE_EXTENSIONS = new Set([".docx", ".pptx", ".xlsx"]);

export interface RAGCapabilityLike {
  available?: boolean;
  reason?: string;
}

export interface RAGCapabilitiesLike {
  supportedExtensions?: readonly string[];
  maxFileBytes?: number;
  maxFileBytesByExtension?: Readonly<Record<string, number>>;
  parseModes?: readonly string[];
  advanced?: RAGCapabilityLike;
  office?: RAGCapabilityLike;
  pdfAuto?: RAGCapabilityLike;
  officeVision?: RAGCapabilityLike;
  enrichment?: RAGCapabilityLike;
  documentAIBudget?: {
    maxRequestsPerDocument?: number;
    maxTokensPerDocument?: number;
    maxEstimatedCostUSDPerDocument?: number;
  };
}

export interface RAGSourceLocationLike {
  kind?: string;
  index?: number;
  label?: string;
}

export interface RAGAttachmentLike {
  id?: string;
  kind?: string;
  fileName?: string;
  mimeType?: string;
  sizeBytes?: number;
}

export interface RAGAssetLike {
  id?: string;
  kind?: string;
  caption?: string;
  pageNum?: number;
  location?: RAGSourceLocationLike;
  width?: number;
  height?: number;
  mimeType?: string;
  attachment?: RAGAttachmentLike;
}

export interface RAGHitLike {
  kbId?: string;
  kbName?: string;
  docId?: string;
  docName?: string;
  chunkIndex?: number;
  sectionTitle?: string;
  pageNum?: number;
  sourceLocation?: RAGSourceLocationLike;
  assets?: readonly RAGAssetLike[];
}

export interface RAGGalleryResource {
  asset: {
    id: string;
    kind: string;
    caption: string;
    pageNum?: number;
    location?: RAGSourceLocationLike;
    width?: number;
    height?: number;
    mimeType?: string;
    attachment?: {
      id: string;
      kind: string;
      fileName: string;
      mimeType: string;
      sizeBytes?: number;
    };
  };
  kbId: string;
  kbName: string;
  docId: string;
  docName: string;
  chunkIndex: number;
  sectionTitle: string;
  sourceLocation?: RAGSourceLocationLike;
}

export type RAGAssetURLBuilder = (
  assetID: string,
  variant: RAGAssetVariant,
) => string;

export type RAGAttachmentURLBuilder = (attachmentID: string) => string;

export interface DocumentPollingLike {
  status?: string;
  progress?: { stage?: string };
}

export interface DocumentActionLike {
  needsReparse?: boolean;
  needsReindex?: boolean;
}

export interface DocumentParseModeLike extends DocumentActionLike {
  appliedParseMode?: string;
  targetParseMode?: string;
}

export interface DocumentStatusLike extends DocumentPollingLike {
  degraded?: boolean;
  warningCount?: number;
}

export interface KnowledgeBasePayloadInput {
  name: string;
  description: string;
  chunkSize: number;
  chunkOverlap: number;
  parseMode: "standard" | "auto";
  enrichmentEnabled: boolean;
}

export type RAGOptInKind = "auto" | "enrichment";

export type RAGAssetVariant = "display" | "thumbnail";

function normalizedExtension(value: string): string {
  const lower = value.trim().toLowerCase();
  const lastSlash = Math.max(lower.lastIndexOf("/"), lower.lastIndexOf("\\"));
  const lastDot = lower.lastIndexOf(".");
  if (lastDot <= lastSlash || lastDot < 0) return "";
  return lower.slice(lastDot);
}

export function availableUploadExtensions(
  capabilities: RAGCapabilitiesLike | null | undefined,
): string[] {
  if (!capabilities || !Array.isArray(capabilities.supportedExtensions)) return [];
  const result: string[] = [];
  const seen = new Set<string>();
  for (const rawExtension of capabilities.supportedExtensions) {
    if (typeof rawExtension !== "string") continue;
    const extension = normalizedExtension(rawExtension);
    if (!extension || seen.has(extension)) continue;
    if (OFFICE_EXTENSIONS.has(extension) && capabilities.office?.available !== true) continue;
    seen.add(extension);
    result.push(extension);
  }
  return result;
}

export function uploadLimitForFile(
  fileName: string,
  capabilities: RAGCapabilitiesLike | null | undefined,
): number {
  const extension = normalizedExtension(fileName);
  if (!extension || !availableUploadExtensions(capabilities).includes(extension)) return 0;
  const extensionLimits = capabilities?.maxFileBytesByExtension;
  let limit = extensionLimits?.[extension];
  if (!Number.isFinite(limit) || Number(limit) <= 0) {
    limit = capabilities?.maxFileBytes;
  }
  return Number.isFinite(limit) && Number(limit) > 0 ? Number(limit) : 0;
}

export function canChangeFeature(
  currentEnabled: boolean,
  nextEnabled: boolean,
  available: boolean,
): boolean {
  return !nextEnabled || currentEnabled || available;
}

export function buildKnowledgeBasePayload(input: KnowledgeBasePayloadInput) {
  return {
    name: input.name.trim(),
    description: input.description.trim(),
    chunkSize: input.chunkSize,
    chunkOverlap: input.chunkOverlap,
    parseMode: input.parseMode,
    enrichmentEnabled: input.enrichmentEnabled,
  };
}

export function getRAGCapabilityRows(
  capabilities: RAGCapabilitiesLike | null | undefined,
) {
  if (!capabilities) return [];
  return [
    { key: "advanced", label: "高级 RAG", capability: capabilities.advanced || {} },
    { key: "pdfAuto", label: "PDF 视觉解析", capability: capabilities.pdfAuto || {} },
    { key: "office", label: "Office 转换", capability: capabilities.office || {} },
    { key: "officeVision", label: "Office 图片转写", capability: capabilities.officeVision || {} },
    { key: "enrichment", label: "表格/代码增强", capability: capabilities.enrichment || {} },
  ];
}

export function formatDocumentAIBudget(
  capabilities: RAGCapabilitiesLike | null | undefined,
): string {
  const budget = capabilities?.documentAIBudget;
  if (!budget) return "文档预算尚未加载";
  const maxRequests = Number.isFinite(budget.maxRequestsPerDocument)
    ? Math.max(0, Math.floor(Number(budget.maxRequestsPerDocument)))
    : 0;
  const maxTokens = Number.isFinite(budget.maxTokensPerDocument)
    ? Math.max(0, Math.floor(Number(budget.maxTokensPerDocument)))
    : 0;
  const maxCost = Number.isFinite(budget.maxEstimatedCostUSDPerDocument)
    ? Math.max(0, Number(budget.maxEstimatedCostUSDPerDocument))
    : 0;
  return `每文档最多 ${maxRequests.toLocaleString("zh-CN")} 次请求、${maxTokens.toLocaleString("zh-CN")} tokens、预估 US$${maxCost.toFixed(2)}`;
}

export function getRAGOptInDisclosure(kind: RAGOptInKind): string {
  return kind === "auto"
    ? "PDF 页面和 Office 图片可能发送给管理员配置的第三方 DocumentAI 服务。关闭高级 RAG 不会触发这些视觉请求。"
    : "无论知识库使用 standard 还是 auto，表格与代码原文都可能发送给管理员配置的第三方 DocumentAI TextModel。关闭增强不会发送这些内容。";
}

export function isAutoAvailable(
  capabilities: RAGCapabilitiesLike | null | undefined,
): boolean {
  return capabilities?.advanced?.available === true
    && capabilities.parseModes?.includes("auto") === true;
}

export function pdfAutoBehavior(
  parseMode: string,
  capabilities: RAGCapabilitiesLike | null | undefined,
): "standard" | "advanced" | "native-fallback" {
  if (parseMode !== "auto") return "standard";
  return capabilities?.pdfAuto?.available === true ? "advanced" : "native-fallback";
}

const DOCUMENT_STAGE_LABELS: Readonly<Record<string, string>> = {
  queued: "等待处理",
  loading: "正在读取文件",
  parsing: "正在解析文档",
  vision: "正在进行视觉解析",
  chunking: "正在切分内容",
  enriching: "正在增强表格/代码",
  embedding: "正在生成向量",
  indexing: "正在写入索引",
  finalizing: "正在完成索引",
  done: "已完成",
  failed: "失败",
};

export function getDocumentStageLabel(stage: string | null | undefined): string {
  return DOCUMENT_STAGE_LABELS[String(stage || "").trim().toLowerCase()] || "处理中";
}

export function getDocumentProgressLabel(document: DocumentPollingLike): string {
  const stage = String(document.progress?.stage || "").trim().toLowerCase();
  if (stage) return getDocumentStageLabel(stage);
  switch (String(document.status || "").trim().toUpperCase()) {
    case "DONE":
      return getDocumentStageLabel("done");
    case "FAILED":
      return getDocumentStageLabel("failed");
    case "PENDING":
      return getDocumentStageLabel("queued");
    default:
      return getDocumentStageLabel("");
  }
}

export function getDocumentStatusState(document: DocumentStatusLike) {
  const stage = String(document.progress?.stage || "").trim().toLowerCase();
  const status = String(document.status || "").trim().toUpperCase();
  if (stage === "failed" || status === "FAILED") {
    return { kind: "failed" as const, label: "失败", spinning: false };
  }
  if (stage === "done" || status === "DONE") {
    if (document.degraded === true || Number(document.warningCount) > 0) {
      return { kind: "degraded" as const, label: "已完成（有降级）", spinning: false };
    }
    return { kind: "done" as const, label: "已索引", spinning: false };
  }
  const queued = stage === "queued" || status === "PENDING";
  return {
    kind: queued ? "queued" as const : "processing" as const,
    label: getDocumentProgressLabel(document),
    spinning: !queued,
  };
}

export function isTerminalDocument(document: DocumentPollingLike): boolean {
  const stage = String(document.progress?.stage || "").trim().toLowerCase();
  if (stage === "done" || stage === "failed") return true;
  const status = String(document.status || "").trim().toUpperCase();
  return status === "DONE" || status === "FAILED";
}

export function shouldPollDocuments(documents: readonly DocumentPollingLike[]): boolean {
  return documents.some((document) => !isTerminalDocument(document));
}

export function documentActionLabel(document: DocumentActionLike): "重新解析" | "重新索引" {
  return document.needsReparse ? "重新解析" : "重新索引";
}

export function documentNeedsLabel(
  document: DocumentActionLike,
): "需要重新解析" | "需要重新索引" | "" {
  if (document.needsReparse) return "需要重新解析";
  if (document.needsReindex) return "需要重新索引";
  return "";
}

export function formatParseModeTransition(document: DocumentParseModeLike): string {
  const applied = String(document.appliedParseMode || "").trim() || "未应用";
  const target = String(document.targetParseMode || "").trim() || "standard";
  return `解析模式：${applied} → ${target}`;
}

export function shouldApplyDocumentLoad(
  requestKBID: string,
  requestGeneration: number,
  selectedKBID: string,
  currentGeneration: number,
  aborted = false,
): boolean {
  return !aborted
    && requestKBID.length > 0
    && requestKBID === selectedKBID
    && requestGeneration === currentGeneration;
}

export function createPollController(
  setTimer: (callback: () => void, delay: number) => unknown,
  clearTimer: (handle: unknown) => void,
) {
  let handle: unknown;
  let scheduled = false;

  const stop = () => {
    if (!scheduled) return;
    clearTimer(handle);
    scheduled = false;
    handle = undefined;
  };

  return {
    schedule(callback: () => void, delay: number) {
      stop();
      handle = setTimer(() => {
        scheduled = false;
        handle = undefined;
        callback();
      }, delay);
      scheduled = true;
    },
    stop,
  };
}

function usableLocation(value: RAGSourceLocationLike | null | undefined) {
  if (!value || typeof value !== "object") return undefined;
  const kind = typeof value.kind === "string" ? value.kind : "";
  const index = Number.isFinite(value.index) ? Number(value.index) : 0;
  const label = typeof value.label === "string" ? value.label : "";
  if (!kind && !label && index <= 0) return undefined;
  return { kind, index, label };
}

function usableAttachment(value: RAGAttachmentLike | null | undefined) {
  if (!value || typeof value !== "object") return undefined;
  const id = typeof value.id === "string" ? value.id.trim() : "";
  if (!id) return undefined;
  return {
    id,
    kind: typeof value.kind === "string" ? value.kind.trim() : "",
    fileName: typeof value.fileName === "string" ? value.fileName.trim() : "",
    mimeType: typeof value.mimeType === "string" ? value.mimeType.trim() : "",
    sizeBytes: Number.isFinite(value.sizeBytes) && Number(value.sizeBytes) >= 0
      ? Number(value.sizeBytes)
      : undefined,
  };
}

export function getRAGResourceKey(asset: RAGAssetLike): string {
  const assetID = typeof asset?.id === "string" ? asset.id.trim() : "";
  if (!assetID) return "";
  const attachmentID = usableAttachment(asset.attachment)?.id;
  return attachmentID ? `${assetID}\u0000${attachmentID}` : assetID;
}

export function collectRAGResources(
  hits: readonly RAGHitLike[] | null | undefined,
  requestedLimit = MAX_RAG_GALLERY_RESOURCES,
): RAGGalleryResource[] {
  if (!Array.isArray(hits)) return [];
  const limit = Math.min(
    MAX_RAG_GALLERY_RESOURCES,
    Math.max(0, Number.isFinite(requestedLimit) ? Math.floor(requestedLimit) : MAX_RAG_GALLERY_RESOURCES),
  );
  if (limit === 0) return [];

  const resources: RAGGalleryResource[] = [];
  const seen = new Set<string>();
  for (const hit of hits) {
    if (!Array.isArray(hit?.assets)) continue;
    for (const candidate of hit.assets) {
      const assetID = typeof candidate?.id === "string" ? candidate.id.trim() : "";
      const attachment = usableAttachment(candidate?.attachment);
      const resourceKey = getRAGResourceKey({ id: assetID, attachment });
      if (!resourceKey || seen.has(resourceKey)) continue;
      seen.add(resourceKey);

      const pageNum = Number.isFinite(candidate.pageNum) && Number(candidate.pageNum) > 0
        ? Number(candidate.pageNum)
        : undefined;
      const assetLocation = usableLocation(candidate.location);
      const hitLocation = usableLocation(hit.sourceLocation);
      const fallbackPage = Number.isFinite(hit.pageNum) && Number(hit.pageNum) > 0
        ? { kind: "page", index: Number(hit.pageNum), label: `第 ${Number(hit.pageNum)} 页` }
        : undefined;
      const sourceLocation = assetLocation || hitLocation || fallbackPage;

      resources.push({
        asset: {
          id: assetID,
          kind: typeof candidate.kind === "string" ? candidate.kind : "image",
          caption: typeof candidate.caption === "string" ? candidate.caption : "",
          pageNum,
          location: assetLocation,
          width: Number.isFinite(candidate.width) ? Number(candidate.width) : undefined,
          height: Number.isFinite(candidate.height) ? Number(candidate.height) : undefined,
          mimeType: typeof candidate.mimeType === "string" ? candidate.mimeType : undefined,
          attachment,
        },
        kbId: typeof hit.kbId === "string" ? hit.kbId : "",
        kbName: typeof hit.kbName === "string" ? hit.kbName : "",
        docId: typeof hit.docId === "string" ? hit.docId : "",
        docName: typeof hit.docName === "string" ? hit.docName : "",
        chunkIndex: Number.isFinite(hit.chunkIndex) ? Number(hit.chunkIndex) : 0,
        sectionTitle: typeof hit.sectionTitle === "string" ? hit.sectionTitle : "",
        sourceLocation,
      });
      if (resources.length >= limit) return resources;
    }
  }
  return resources;
}

// Agent assistant metadata is persisted JSON and may come from an older
// server or a partially migrated session. Normalize it at the rendering
// boundary so malformed entries cannot break the chat screen, while keeping
// the backend's stable first-reference ordering and six-resource cap.
export function normalizeRAGResources(value: unknown): RAGGalleryResource[] {
  if (!Array.isArray(value)) return [];

  const resources: RAGGalleryResource[] = [];
  const seen = new Set<string>();
  for (const raw of value) {
    if (!raw || typeof raw !== "object") continue;
    const candidate = raw as Record<string, unknown>;
    const rawAsset = candidate.asset;
    if (!rawAsset || typeof rawAsset !== "object") continue;
    const asset = rawAsset as Record<string, unknown>;
    const assetID = typeof asset.id === "string" ? asset.id.trim() : "";
    const kbID = typeof candidate.kbId === "string" ? candidate.kbId.trim() : "";
    const docID = typeof candidate.docId === "string" ? candidate.docId.trim() : "";
    const attachment = usableAttachment(asset.attachment as RAGAttachmentLike | undefined);
    const resourceKey = getRAGResourceKey({ id: assetID, attachment });
    if (!resourceKey || !kbID || !docID || seen.has(resourceKey)) continue;
    seen.add(resourceKey);

    const pageNum = Number.isFinite(asset.pageNum) && Number(asset.pageNum) > 0
      ? Number(asset.pageNum)
      : undefined;
    const assetLocation = usableLocation(asset.location as RAGSourceLocationLike | undefined);
    const sourceLocation = usableLocation(candidate.sourceLocation as RAGSourceLocationLike | undefined)
      || assetLocation;
    resources.push({
      asset: {
        id: assetID,
        kind: typeof asset.kind === "string" ? asset.kind : "image",
        caption: typeof asset.caption === "string" ? asset.caption : "",
        pageNum,
        location: assetLocation,
        width: Number.isFinite(asset.width) ? Number(asset.width) : undefined,
        height: Number.isFinite(asset.height) ? Number(asset.height) : undefined,
        mimeType: typeof asset.mimeType === "string" ? asset.mimeType : undefined,
        attachment,
      },
      kbId: kbID,
      kbName: typeof candidate.kbName === "string" ? candidate.kbName : "",
      docId: docID,
      docName: typeof candidate.docName === "string" ? candidate.docName : "",
      chunkIndex: Number.isFinite(candidate.chunkIndex) ? Number(candidate.chunkIndex) : 0,
      sectionTitle: typeof candidate.sectionTitle === "string" ? candidate.sectionTitle : "",
      sourceLocation,
    });
    if (resources.length >= MAX_RAG_GALLERY_RESOURCES) break;
  }
  return resources;
}

export function appendActAs(url: string, actAs: string | null | undefined): string {
  const userID = String(actAs || "").trim();
  if (!userID || /[?&]actAs=/.test(url)) return url;
  const hashIndex = url.indexOf("#");
  const base = hashIndex >= 0 ? url.slice(0, hashIndex) : url;
  const hash = hashIndex >= 0 ? url.slice(hashIndex) : "";
  return `${base}${base.includes("?") ? "&" : "?"}actAs=${encodeURIComponent(userID)}${hash}`;
}

export function buildOwnerAssetURL(
  assetID: string,
  variant: RAGAssetVariant,
  actAs?: string | null,
): string {
  const id = String(assetID || "").trim();
  if (!id) return "";
  const suffix = variant === "thumbnail" ? "/thumbnail" : "";
  return appendActAs(`/api/rag/assets/${encodeURIComponent(id)}${suffix}`, actAs);
}

export function buildOwnerAttachmentURL(
  attachmentID: string,
  actAs?: string | null,
): string {
  const id = String(attachmentID || "").trim();
  if (!id) return "";
  return appendActAs(`/api/rag/attachments/${encodeURIComponent(id)}/download`, actAs);
}

export function buildAgentSessionAssetURL(
  agentID: string,
  sessionID: string,
  assetID: string,
  variant: RAGAssetVariant,
  actAs?: string | null,
): string {
  const agent = String(agentID || "").trim();
  const session = String(sessionID || "").trim();
  const asset = String(assetID || "").trim();
  if (!agent || !session || !asset) return "";
  const suffix = variant === "thumbnail" ? "/thumbnail" : "";
  const url = `/api/agents/${encodeURIComponent(agent)}/chat/${encodeURIComponent(session)}/rag-assets/${encodeURIComponent(asset)}${suffix}`;
  return appendActAs(url, actAs);
}

export function buildAgentSessionAttachmentURL(
  agentID: string,
  sessionID: string,
  attachmentID: string,
  actAs?: string | null,
): string {
  const agent = String(agentID || "").trim();
  const session = String(sessionID || "").trim();
  const attachment = String(attachmentID || "").trim();
  if (!agent || !session || !attachment) return "";
  const url = `/api/agents/${encodeURIComponent(agent)}/chat/${encodeURIComponent(session)}/rag-attachments/${encodeURIComponent(attachment)}/download`;
  return appendActAs(url, actAs);
}

export function markAssetUnavailable(
  unavailableAssetIDs: readonly string[],
  assetID: string,
): string[] {
  if (!assetID || unavailableAssetIDs.includes(assetID)) return [...unavailableAssetIDs];
  return [...unavailableAssetIDs, assetID];
}

export function formatSourceLocation(
  location: RAGSourceLocationLike | null | undefined,
  fallbackPageNum?: number,
): string {
  const label = typeof location?.label === "string" ? location.label.trim() : "";
  if (label) return label;
  const index = Number.isFinite(location?.index) ? Number(location?.index) : 0;
  switch (String(location?.kind || "").toLowerCase()) {
    case "page":
      if (index > 0) return `第 ${index} 页`;
      break;
    case "slide":
      if (index > 0) return `幻灯片 ${index}`;
      break;
    case "sheet":
      if (index > 0) return `工作表 ${index}`;
      break;
    case "document":
      return "文档";
  }
  return Number.isFinite(fallbackPageNum) && Number(fallbackPageNum) > 0
    ? `第 ${Number(fallbackPageNum)} 页`
    : "";
}

export function safeRAGMarkdownURL(url: string): string {
  const value = String(url || "").trim();
  if (!value || /[\u0000-\u001f\u007f]/.test(value)) return "";
  if (value.startsWith("//") || value.startsWith("\\") || value.startsWith("/\\")) return "";
  if (/^https?:\/\//i.test(value)) {
    try {
      const parsed = new URL(value);
      return parsed.protocol === "http:" || parsed.protocol === "https:" ? value : "";
    } catch {
      return "";
    }
  }
  if (/^[a-z][a-z0-9+.-]*:/i.test(value)) return "";
  return value;
}
