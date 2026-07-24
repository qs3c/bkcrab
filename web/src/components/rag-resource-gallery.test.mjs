import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

const {
  appendActAs,
  buildAgentSessionAttachmentURL,
  buildAgentSessionAssetURL,
  availableUploadExtensions,
  buildOwnerAttachmentURL,
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
  getRAGResourceKey,
  getRAGCapabilityRows,
  getRAGOptInDisclosure,
  isAutoAvailable,
  markAssetUnavailable,
  normalizeRAGResources,
  pdfAutoBehavior,
  safeRAGMarkdownURL,
  shouldApplyDocumentLoad,
  shouldPollDocuments,
  uploadLimitForFile,
} = await import(new URL("./rag-resource-gallery-state.ts", import.meta.url));

const {
  AgentMarkdownImage,
  RAGAnswerMarkdown,
  RAGPlainText,
} = await import(new URL("./rag-safe-render.ts", import.meta.url));

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

test("keeps distinct Visio attachments that share a preview asset", () => {
  const hits = [{
    kbId: "kb-1",
    docId: "doc-1",
    docName: "visio.docx",
    assets: [
      {
        id: "shared-preview",
        kind: "image",
        attachment: {
          id: "attachment-1",
          kind: "visio_source",
          fileName: "first.vsdx",
          mimeType: "application/vnd.ms-visio.drawing",
          sizeBytes: 1024,
        },
      },
      {
        id: "shared-preview",
        kind: "image",
        attachment: {
          id: "attachment-2",
          kind: "visio_source",
          fileName: "second.vsdx",
          mimeType: "application/vnd.ms-visio.drawing",
          sizeBytes: 2048,
        },
      },
      {
        id: "shared-preview",
        kind: "image",
        attachment: {
          id: "attachment-1",
          kind: "visio_source",
          fileName: "duplicate.vsdx",
          mimeType: "application/vnd.ms-visio.drawing",
        },
      },
    ],
  }];

  const resources = collectRAGResources(hits);
  assert.deepEqual(
    resources.map((resource) => getRAGResourceKey(resource.asset)),
    ["shared-preview\u0000attachment-1", "shared-preview\u0000attachment-2"],
  );
  assert.equal(resources[0].asset.attachment.fileName, "first.vsdx");
  assert.equal(resources[1].asset.attachment.sizeBytes, 2048);
  assert.equal(getRAGResourceKey({ id: "ordinary-image" }), "ordinary-image");
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
    buildOwnerAttachmentURL("attachment/a?#", "user +/一"),
    "/api/rag/attachments/attachment%2Fa%3F%23/download?actAs=user%20%2B%2F%E4%B8%80",
  );
  assert.equal(buildOwnerAttachmentURL("  "), "");
  assert.equal(
    appendActAs("/knowledge/chat/?id=kb%2F1", "user/1"),
    "/knowledge/chat/?id=kb%2F1&actAs=user%2F1",
  );
});

test("normalizes persisted agent resources and builds session-scoped asset URLs", () => {
  const resources = normalizeRAGResources([
    {
      asset: { id: "ast_1", kind: "diagram", caption: "first" },
      kbId: "kb-1",
      kbName: "Knowledge",
      docId: "doc-1",
      docName: "one.pdf",
      chunkIndex: 2,
      sectionTitle: "Architecture",
      sourceLocation: { kind: "page", index: 3, label: "第 3 页" },
    },
    {
      asset: { id: "ast_1", kind: "diagram", caption: "duplicate" },
      kbId: "kb-1",
      docId: "doc-1",
      docName: "one.pdf",
      chunkIndex: 4,
    },
    ...Array.from({ length: 7 }, (_, index) => ({
      asset: { id: `ast_${index + 2}`, kind: "image" },
      kbId: "kb-1",
      docId: `doc-${index + 2}`,
      docName: `${index + 2}.pdf`,
      chunkIndex: index,
    })),
    { asset: { id: "" }, kbId: "kb-1", docId: "doc-bad" },
    { asset: { id: "ast_bad" }, kbId: "", docId: "doc-bad" },
  ]);

  assert.deepEqual(resources.map((resource) => resource.asset.id), [
    "ast_1",
    "ast_2",
    "ast_3",
    "ast_4",
    "ast_5",
    "ast_6",
  ]);
  assert.equal(resources[0].asset.caption, "first");
  assert.equal(resources[0].sourceLocation.label, "第 3 页");
  assert.deepEqual(normalizeRAGResources("not-an-array"), []);

  assert.equal(
    buildAgentSessionAssetURL("agent/a?#", "session +/一", "asset/a?#", "thumbnail", "user +/一"),
    "/api/agents/agent%2Fa%3F%23/chat/session%20%2B%2F%E4%B8%80/rag-assets/asset%2Fa%3F%23/thumbnail?actAs=user%20%2B%2F%E4%B8%80",
  );
  assert.equal(
    buildAgentSessionAssetURL("agent-1", "session-1", "asset-1", "display"),
    "/api/agents/agent-1/chat/session-1/rag-assets/asset-1",
  );
  assert.equal(buildAgentSessionAssetURL("", "session-1", "asset-1", "display"), "");
  assert.equal(
    buildAgentSessionAttachmentURL(
      "agent/a?#",
      "session +/一",
      "attachment/a?#",
      "user +/一",
    ),
    "/api/agents/agent%2Fa%3F%23/chat/session%20%2B%2F%E4%B8%80/rag-attachments/attachment%2Fa%3F%23/download?actAs=user%20%2B%2F%E4%B8%80",
  );
  assert.equal(buildAgentSessionAttachmentURL("", "session-1", "attachment-1"), "");
});

test("normalizes persisted Visio attachment metadata without collapsing shared previews", () => {
  const resources = normalizeRAGResources([
    {
      asset: {
        id: "ast_shared",
        kind: "image",
        attachment: {
          id: "att_1",
          kind: "visio_source",
          fileName: "one.vsdx",
          mimeType: "application/vnd.ms-visio.drawing",
          sizeBytes: 10,
        },
      },
      kbId: "kb-1",
      docId: "doc-1",
      docName: "one.docx",
      chunkIndex: 0,
    },
    {
      asset: {
        id: "ast_shared",
        kind: "image",
        attachment: {
          id: "att_2",
          kind: "visio_source",
          fileName: "two.vsdx",
          mimeType: "application/vnd.ms-visio.drawing",
          sizeBytes: -1,
        },
      },
      kbId: "kb-1",
      docId: "doc-1",
      docName: "one.docx",
      chunkIndex: 1,
    },
  ]);

  assert.equal(resources.length, 2);
  assert.equal(resources[0].asset.attachment.id, "att_1");
  assert.equal(resources[0].asset.attachment.sizeBytes, 10);
  assert.equal(resources[1].asset.attachment.id, "att_2");
  assert.equal(resources[1].asset.attachment.sizeBytes, undefined);
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

test("RAG answer renderer drops raw HTML, dangerous links, and implicit image requests", () => {
  const rendered = renderToStaticMarkup(React.createElement(
    RAGAnswerMarkdown,
    { urlTransform: safeRAGMarkdownURL },
    [
      '<img src="https://attacker.invalid/raw.png" onerror="alert(1)">',
      '<script>alert("raw")</script>',
      '[dangerous link](javascript:alert(1))',
      '![tracking pixel](https://attacker.invalid/tracker.png)',
      '[allowed external link](https://example.com/docs)',
    ].join("\n\n"),
  ));

  assert.doesNotMatch(rendered, /<(?:img|script)\b/i);
  assert.doesNotMatch(rendered, /onerror=/i);
  assert.doesNotMatch(rendered, /src="https:\/\/attacker\.invalid/i);
  assert.match(rendered, /href=""[^>]*>dangerous link<\/a>/);
  assert.match(rendered, /已忽略回答中的外部图片：tracking pixel/);
  assert.match(rendered, /href="https:\/\/example\.com\/docs"/);
  assert.match(rendered, /target="_blank"/);
  assert.match(rendered, /rel="noopener noreferrer"/);
});

test("agent Markdown renderer cannot initiate external image requests", () => {
  const rendered = renderToStaticMarkup(React.createElement(
    ReactMarkdown,
    {
      remarkPlugins: [remarkGfm],
      components: { img: AgentMarkdownImage },
    },
    [
      "![tracking pixel](https://attacker.invalid/tracker.png)",
      "![protocol relative](//attacker.invalid/protocol.png)",
      "![custom scheme](file:///etc/passwd)",
      "![same origin](/api/agents/a/chat/s/rag-assets/ast_1/thumbnail)",
      "[normal link](https://example.com/docs)",
      "`const image = 'still code';`",
    ].join("\n\n"),
  ));

  assert.doesNotMatch(rendered, /src="(?:https?:)?\/\/attacker\.invalid/i);
  assert.doesNotMatch(rendered, /src="file:/i);
  assert.match(rendered, /external image omitted: tracking pixel/);
  assert.match(rendered, /external image omitted: protocol relative/);
  assert.match(rendered, /external image omitted: custom scheme/);
  assert.match(
    rendered,
    /<img[^>]+src="\/api\/agents\/a\/chat\/s\/rag-assets\/ast_1\/thumbnail"/,
  );
  assert.match(rendered, /href="https:\/\/example\.com\/docs"/);
  assert.match(rendered, /<code>const image = &#x27;still code&#x27;;<\/code>/);
});

test("agent RAG metadata captions remain inert plain text", () => {
  const caption = '<img src="https://attacker.invalid/a.png" onerror="alert(1)"> '
    + '![tracker](https://attacker.invalid/b.png) [click](javascript:alert(1)) '
    + '<script>alert("caption")</script>';
  const resources = normalizeRAGResources([{
    asset: {
      id: "ast_adversarial",
      kind: "image",
      caption,
      url: "https://attacker.invalid/untrusted-object-url.png",
    },
    kbId: "kb-1",
    docId: "doc-1",
    docName: "adversarial.md",
    chunkIndex: 0,
  }]);

  assert.equal(resources.length, 1);
  assert.equal(resources[0].asset.caption, caption);
  assert.equal(Object.hasOwn(resources[0].asset, "url"), false);
  const rendered = renderToStaticMarkup(React.createElement(RAGPlainText, {
    as: "p",
    value: resources[0].asset.caption,
  }));
  assert.doesNotMatch(rendered, /<(?:img|script)\b/i);
  assert.match(rendered, /&lt;img src=&quot;https:\/\/attacker\.invalid\/a\.png&quot;/);
  assert.match(rendered, /!\[tracker\]\(https:\/\/attacker\.invalid\/b\.png\)/);
  assert.match(rendered, /&lt;script&gt;alert\(&quot;caption&quot;\)&lt;\/script&gt;/);
});

test("RAG UI surfaces stay wired to the executable safe renderers", async () => {
  const [chat, gallery, knowledgePage, agentChat] = await Promise.all([
    readFile(new URL("../app/knowledge/chat/knowledge-chat-client.tsx", import.meta.url), "utf8"),
    readFile(new URL("./rag-resource-gallery.tsx", import.meta.url), "utf8"),
    readFile(new URL("../app/knowledge/page.tsx", import.meta.url), "utf8"),
    readFile(new URL("./chat-screen.tsx", import.meta.url), "utf8"),
  ]);
  assert.match(chat, /RAGAnswerMarkdown/);
  assert.match(gallery, /RAGPlainText/);
  assert.match(gallery, /assetURLBuilder/);
  assert.match(gallery, /attachmentURLBuilder/);
  assert.match(gallery, /selected\?\.asset\.attachment\?\.kind === "visio_source"/);
  assert.match(gallery, /下载 Visio 工程文件/);
  assert.match(gallery, /target="_blank"/);
  assert.match(gallery, /rel="noopener noreferrer"/);
  assert.match(gallery, /\bdownload\b/);
  assert.match(agentChat, /buildAgentSessionAssetURL/);
  assert.match(agentChat, /buildAgentSessionAttachmentURL/);
  assert.match(agentChat, /normalizeRAGResources/);
  assert.match(agentChat, /img: AgentMarkdownImage/);
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
