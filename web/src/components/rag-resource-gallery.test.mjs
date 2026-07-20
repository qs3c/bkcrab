import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";

const {
  appendActAs,
  availableUploadExtensions,
  buildOwnerAssetURL,
  buildKnowledgeBasePayload,
  canChangeFeature,
  collectRAGResources,
  createPollController,
  documentActionLabel,
  documentNeedsLabel,
  formatDocumentAIBudget,
  formatParseModeTransition,
  formatSourceLocation,
  getDocumentProgressLabel,
  getDocumentStageLabel,
  getDocumentStatusState,
  getRAGCapabilityRows,
  getRAGOptInDisclosure,
  isAutoAvailable,
  markAssetUnavailable,
  pdfAutoBehavior,
  safeRAGMarkdownURL,
  shouldApplyDocumentLoad,
  shouldPollDocuments,
  uploadLimitForFile,
} = await import(new URL("./rag-resource-gallery-state.ts", import.meta.url));

const capabilities = {
  supportedExtensions: [".md", ".markdown", ".txt", ".pdf", ".docx", ".pptx", ".xlsx"],
  maxFileBytes: 50_000,
  maxFileBytesByExtension: {
    ".md": 50_000,
    ".markdown": 49_000,
    ".txt": 50_000,
    ".pdf": 40_000,
    ".docx": 30_000,
    ".pptx": 20_000,
    ".xlsx": 10_000,
  },
  parseModes: ["standard", "auto"],
  advanced: { available: true, reason: "" },
  office: { available: false, reason: "office converter unavailable" },
  pdfAuto: { available: false, reason: "pdf engine unavailable" },
  officeVision: { available: true, reason: "" },
  enrichment: { available: false, reason: "text model unavailable" },
  documentAIBudget: {
    maxRequestsPerDocument: 300,
    maxTokensPerDocument: 200_000,
    maxEstimatedCostUSDPerDocument: 1,
  },
};

test("derives upload extensions and per-extension limits from capabilities", () => {
  assert.deepEqual(availableUploadExtensions(capabilities), [
    ".md",
    ".markdown",
    ".txt",
    ".pdf",
  ]);
  assert.equal(uploadLimitForFile("guide.markdown", capabilities), 49_000);
  assert.equal(uploadLimitForFile("scan.PDF", capabilities), 40_000);
  assert.equal(uploadLimitForFile("no-extension", capabilities), 0);

  const withOffice = {
    ...capabilities,
    office: { available: true, reason: "" },
  };
  assert.deepEqual(availableUploadExtensions(withOffice), capabilities.supportedExtensions);
});

test("keeps auto and enrichment opt-ins fail-closed while always allowing disable", () => {
  assert.equal(isAutoAvailable(capabilities), true, "office vision can independently make auto available");
  assert.equal(pdfAutoBehavior("auto", capabilities), "native-fallback");
  assert.equal(pdfAutoBehavior("standard", capabilities), "standard");

  assert.equal(canChangeFeature(false, true, false), false);
  assert.equal(canChangeFeature(true, false, false), true);
  assert.equal(canChangeFeature(true, true, false), true);
  assert.equal(canChangeFeature(false, true, true), true);
});

test("builds the actual knowledge-base payload with independent parse and enrichment fields", () => {
  assert.deepEqual(buildKnowledgeBasePayload({
    name: "  产品资料  ",
    description: "  PDF 与代码  ",
    chunkSize: 768,
    chunkOverlap: 96,
    parseMode: "auto",
    enrichmentEnabled: true,
  }), {
    name: "产品资料",
    description: "PDF 与代码",
    chunkSize: 768,
    chunkOverlap: 96,
    parseMode: "auto",
    enrichmentEnabled: true,
  });
  assert.equal(buildKnowledgeBasePayload({
    name: "标准库",
    description: "",
    chunkSize: 512,
    chunkOverlap: 64,
    parseMode: "standard",
    enrichmentEnabled: false,
  }).enrichmentEnabled, false);
});

test("keeps all five capability reasons separate", () => {
  const rows = getRAGCapabilityRows({
    advanced: { available: false, reason: "advanced gate closed" },
    pdfAuto: { available: false, reason: "pdf license unavailable" },
    office: { available: false, reason: "office converter unavailable" },
    officeVision: { available: false, reason: "vision model unavailable" },
    enrichment: { available: false, reason: "text model unavailable" },
  });
  assert.deepEqual(rows.map(({ key, label, capability }) => [key, label, capability.reason]), [
    ["advanced", "高级 RAG", "advanced gate closed"],
    ["pdfAuto", "PDF 视觉解析", "pdf license unavailable"],
    ["office", "Office 转换", "office converter unavailable"],
    ["officeVision", "Office 图片转写", "vision model unavailable"],
    ["enrichment", "表格/代码增强", "text model unavailable"],
  ]);
});

test("uses separate outbound-data disclosures and the server document budget", () => {
  assert.match(getRAGOptInDisclosure("auto"), /PDF 页面和 Office 图片.*第三方 DocumentAI/);
  assert.match(getRAGOptInDisclosure("auto"), /关闭高级 RAG 不会触发/);
  assert.match(getRAGOptInDisclosure("enrichment"), /standard 还是 auto.*表格与代码原文/);
  assert.match(getRAGOptInDisclosure("enrichment"), /TextModel.*关闭增强不会发送/);
  assert.equal(
    formatDocumentAIBudget(capabilities),
    "每文档最多 300 次请求、200,000 tokens、预估 US$1.00",
  );
  assert.equal(formatDocumentAIBudget(null), "文档预算尚未加载");
});

test("covers every document stage and safely labels unknown stages", () => {
  const expected = new Map([
    ["queued", "等待处理"],
    ["loading", "正在读取文件"],
    ["parsing", "正在解析文档"],
    ["vision", "正在进行视觉解析"],
    ["chunking", "正在切分内容"],
    ["enriching", "正在增强表格/代码"],
    ["embedding", "正在生成向量"],
    ["indexing", "正在写入索引"],
    ["finalizing", "正在完成索引"],
    ["done", "已完成"],
    ["failed", "失败"],
  ]);
  for (const [stage, label] of expected) {
    assert.equal(getDocumentStageLabel(stage), label, stage);
  }
  assert.equal(getDocumentStageLabel("future-stage"), "处理中");
  assert.equal(getDocumentStageLabel(""), "处理中");
  assert.equal(getDocumentProgressLabel({ status: "DONE", progress: { stage: "" } }), "已完成");
  assert.equal(getDocumentProgressLabel({ status: "FAILED", progress: { stage: "" } }), "失败");
  assert.equal(getDocumentProgressLabel({ status: "PENDING", progress: { stage: "" } }), "等待处理");
  assert.equal(
    getDocumentProgressLabel({ status: "DONE", progress: { stage: "future-stage" } }),
    "处理中",
    "a non-empty unknown stage must not be hidden by the legacy status fallback",
  );
});

test("polling stops for terminal documents and replacement schedules clear old timers", () => {
  assert.equal(shouldPollDocuments([
    { status: "DONE", progress: { stage: "done" } },
    { status: "FAILED", progress: { stage: "failed" } },
  ]), false);
  assert.equal(shouldPollDocuments([
    { status: "PROCESSING", progress: { stage: "indexing" } },
  ]), true);
  assert.equal(shouldPollDocuments([
    { status: "PROCESSING", progress: { stage: "future-stage" } },
  ]), true);

  let nextHandle = 0;
  const callbacks = new Map();
  const cleared = [];
  const poller = createPollController(
    (callback) => {
      nextHandle += 1;
      callbacks.set(nextHandle, callback);
      return nextHandle;
    },
    (handle) => {
      cleared.push(handle);
      callbacks.delete(handle);
    },
  );
  poller.schedule(() => undefined, 100);
  poller.schedule(() => undefined, 100);
  assert.deepEqual(cleared, [1]);
  poller.stop();
  assert.deepEqual(cleared, [1, 2]);
});

test("uses reparse wording before reindex wording", () => {
  assert.equal(documentActionLabel({ needsReparse: true, needsReindex: true }), "重新解析");
  assert.equal(documentActionLabel({ needsReparse: false, needsReindex: true }), "重新索引");
  assert.equal(documentActionLabel({ needsReparse: false, needsReindex: false }), "重新索引");
  assert.equal(documentNeedsLabel({ needsReparse: true, needsReindex: true }), "需要重新解析");
  assert.equal(documentNeedsLabel({ needsReparse: false, needsReindex: true }), "需要重新索引");
  assert.equal(documentNeedsLabel({ needsReparse: false, needsReindex: false }), "");
  assert.equal(
    formatParseModeTransition({ appliedParseMode: "standard", targetParseMode: "auto" }),
    "解析模式：standard → auto",
  );
  assert.equal(
    formatParseModeTransition({ targetParseMode: "standard" }),
    "解析模式：未应用 → standard",
  );
});

test("marks DONE documents with degradation or warnings as the amber state", () => {
  assert.deepEqual(getDocumentStatusState({
    status: "DONE",
    progress: { stage: "done" },
    degraded: true,
    warningCount: 0,
  }), { kind: "degraded", label: "已完成（有降级）", spinning: false });
  assert.equal(getDocumentStatusState({
    status: "DONE",
    progress: { stage: "done" },
    warningCount: 2,
  }).kind, "degraded");
  assert.equal(getDocumentStatusState({
    status: "DONE",
    progress: { stage: "done" },
    degraded: false,
    warningCount: 0,
  }).kind, "done");
});

test("rejects document responses from a stale KB, generation, or aborted request", () => {
  assert.equal(
    shouldApplyDocumentLoad("kb-2", 7, "kb-2", 7),
    true,
    "the latest request, including a quiet poll, owns spinner cleanup",
  );
  assert.equal(shouldApplyDocumentLoad("kb-1", 7, "kb-2", 7), false);
  assert.equal(shouldApplyDocumentLoad("kb-2", 6, "kb-2", 7), false);
  assert.equal(shouldApplyDocumentLoad("kb-2", 7, "kb-2", 7, true), false);
});

test("deduplicates assets in final hit order, supports old hits, and caps the gallery at six", () => {
  const hits = [
    {
      kbId: "kb-1",
      kbName: "Knowledge",
      docId: "doc-1",
      docName: "one.pdf",
      chunkIndex: 0,
      sectionTitle: "A",
      sourceLocation: { kind: "page", index: 2, label: "第 2 页" },
      assets: [
        { id: "asset-1", kind: "image", caption: "first", location: { kind: "page", index: 2, label: "第 2 页" } },
        { id: "asset-2", kind: "image", caption: "second", location: { kind: "page", index: 2, label: "第 2 页" } },
      ],
    },
    { docId: "legacy", docName: "legacy.md", chunkIndex: 1 },
    {
      kbId: "kb-1",
      docId: "doc-2",
      docName: "two.pdf",
      chunkIndex: 3,
      assets: [
        { id: "asset-1", kind: "image", caption: "duplicate" },
        ...Array.from({ length: 7 }, (_, index) => ({ id: `asset-${index + 3}`, kind: "image" })),
      ],
    },
  ];

  const resources = collectRAGResources(hits);
  assert.deepEqual(resources.map((resource) => resource.asset.id), [
    "asset-1",
    "asset-2",
    "asset-3",
    "asset-4",
    "asset-5",
    "asset-6",
  ]);
  assert.equal(resources[0].docName, "one.pdf");
  assert.equal(resources[2].docName, "two.pdf");
  assert.deepEqual(collectRAGResources([{ docId: "old", assets: undefined }]), []);
});

test("builds encoded same-origin owner URLs and preserves encoded admin actAs", () => {
  assert.equal(
    buildOwnerAssetURL("asset/a?#", "thumbnail", "user +/一"),
    "/api/rag/assets/asset%2Fa%3F%23/thumbnail?actAs=user%20%2B%2F%E4%B8%80",
  );
  assert.equal(
    buildOwnerAssetURL("asset/a?#", "display"),
    "/api/rag/assets/asset%2Fa%3F%23",
  );
  assert.equal(
    appendActAs("/knowledge/chat/?id=kb%2F1", "user/1"),
    "/knowledge/chat/?id=kb%2F1&actAs=user%2F1",
  );
});

test("tracks unavailable images without mutating other gallery state", () => {
  const first = markAssetUnavailable([], "asset-2");
  const second = markAssetUnavailable(first, "asset-2");
  const third = markAssetUnavailable(second, "asset-3");
  assert.deepEqual(first, ["asset-2"]);
  assert.deepEqual(second, ["asset-2"]);
  assert.deepEqual(third, ["asset-2", "asset-3"]);
});

test("formats source locations as text and rejects dangerous markdown URLs", () => {
  assert.equal(formatSourceLocation({ kind: "slide", index: 3, label: "幻灯片 3" }), "幻灯片 3");
  assert.equal(formatSourceLocation({ kind: "sheet", index: 2, label: "" }), "工作表 2");
  assert.equal(formatSourceLocation(undefined, 4), "第 4 页");

  assert.equal(safeRAGMarkdownURL("https://example.com/a"), "https://example.com/a");
  assert.equal(safeRAGMarkdownURL("/knowledge/"), "/knowledge/");
  assert.equal(safeRAGMarkdownURL("#section"), "#section");
  assert.equal(safeRAGMarkdownURL("javascript:alert(1)"), "");
  assert.equal(safeRAGMarkdownURL("data:text/html,boom"), "");
  assert.equal(safeRAGMarkdownURL("file:///etc/passwd"), "");
  assert.equal(safeRAGMarkdownURL("//evil.example/image"), "");
});

test("RAG renderers keep raw HTML disabled and captions out of markdown", async () => {
  const [chat, gallery, knowledgePage] = await Promise.all([
    readFile(new URL("../app/knowledge/chat/knowledge-chat-client.tsx", import.meta.url), "utf8"),
    readFile(new URL("./rag-resource-gallery.tsx", import.meta.url), "utf8"),
    readFile(new URL("../app/knowledge/page.tsx", import.meta.url), "utf8"),
  ]);
  for (const source of [chat, gallery]) {
    assert.doesNotMatch(source, /dangerouslySetInnerHTML/);
    assert.doesNotMatch(source, /rehypeRaw/);
  }
  assert.match(chat, /skipHtml/);
  assert.doesNotMatch(gallery, /ReactMarkdown/);
  assert.match(knowledgePage, /const payload = buildKnowledgeBasePayload\(/);
  assert.match(knowledgePage, /getRAGCapabilityRows\(capabilities\)/);
  assert.match(knowledgePage, /getRAGOptInDisclosure\(pendingOptIn \|\| "enrichment"\)/);
  assert.match(knowledgePage, /formatDocumentAIBudget\(capabilities\)/);
  assert.match(knowledgePage, /documentNeedsLabel\(doc\)/);
  assert.match(knowledgePage, /formatParseModeTransition\(doc\)/);
  assert.match(knowledgePage, /state\.kind === "degraded"[\s\S]*border-amber-500/);
  assert.match(knowledgePage, /shouldApplyDocumentLoad\(/);
  const loadDocumentsStart = knowledgePage.indexOf("const loadDocuments =");
  const loadDocumentsEnd = knowledgePage.indexOf("React.useEffect", loadDocumentsStart);
  assert.notEqual(loadDocumentsStart, -1);
  assert.notEqual(loadDocumentsEnd, -1);
  const loadDocumentsSource = knowledgePage.slice(loadDocumentsStart, loadDocumentsEnd);
  const finallyStart = loadDocumentsSource.lastIndexOf("} finally {");
  assert.notEqual(finallyStart, -1);
  const finallySource = loadDocumentsSource.slice(finallyStart);
  assert.match(
    finallySource,
    /finally \{[\s\S]*shouldApplyDocumentLoad\([\s\S]*documentsRequestGenerationRef\.current[\s\S]*setDocumentsLoading\(false\)/,
  );
  assert.doesNotMatch(
    finallySource,
    /!quiet/,
    "a latest quiet request must be allowed to clear loading started by an older non-quiet request",
  );
});
