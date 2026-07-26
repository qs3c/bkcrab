# RAG 多模态文档解析与结构化分块设计

日期：2026-07-20
状态：设计定稿（待按实施计划落地）
前置：[RAG 板块设计：用户级知识库 + agent 受限检索](2026-07-13-rag-module-design.md)

> 本设计中的“多模态”特指：**解析阶段可以使用视觉模型理解页面和图片，检索与回答阶段仍然是纯文本；命中的原始视觉资源由前端确定性展示，绝不作为视觉输入发送给回答模型。**

## 1. 背景与现状

当前 RAG 入库链路位于 `internal/rag/pipeline.go`：

```text
原件 → parse.Parse → split → embedding → Milvus → Search → 文字回答
```

当前实现已经具备：

- MD、TXT、PDF、DOCX 上传与异步索引；
- Markdown 标题感知分块，代码围栏中的伪标题不会再被误判；
- `Content` 保留正文，`SearchContent` 用标题上下文增强 embedding/BM25；
- Milvus dense + BM25 + RRF 混合召回，可选 reranker；
- 知识库问答把 `rag.Hit` 快照保存到 `rag_chat_turns.sources`，历史引用可以回放。

当前解析层仍是纯文本模型：

- `parse.Parse` 对整个文件执行 `io.ReadAll`；
- PDF 只读取文字层，不做 OCR，不保留图片、表格和版面；
- DOCX 只读取 `word/document.xml` 的段落和 Heading；
- `parse.Result` 只有 `Format + Pages[].Text`；
- 分块器不是 Markdown AST 分块器，表格和代码块可能被句子滑窗切碎；
- Milvus 的单个 `content` 字段同时承担检索文本和返回正文，无法可靠承载任意增强内容；
- 检索结果没有图片资源引用，前端只能展示命中文字。

本设计替代前置 RAG 设计中“解析、分块、chunk 载荷、检索结果资源、文档状态”相关部分，其余 KB 权限、embedding 快照、混合召回和 agent 授权模型保持不变。

## 2. 已确认的产品边界

### 2.1 目标

1. PDF 在知识库启用高级解析后使用 `auto` 模式逐页判断：简单页走原生解析，扫描页、图片页、复杂表格/代码/版面页走 VLM。
2. PDF VLM 失败时按页降级为普通 PDF 文本提取；单页失败不得拖垮已成功的其它页面。
3. Office 统一通过 Microsoft MarkItDown 转换成 Markdown，只支持 `docx / pptx / xlsx`。
4. Office 中的图片需要提取为独立资源，并在原位置写入图片说明和 OCR 文本。
5. MD/TXT 不导入图片；Markdown 中的本地、网络、`data:` 和 HTML 图片都不发起访问。
6. 分块层只面对统一的规范化 Markdown，不再按 PDF/DOCX/TXT 写四套分块逻辑。
7. 表格、代码块保留原始内容，同时增加独立的语义增强文本。
8. 图片说明/OCR、表格摘要、代码说明参与文字 embedding、BM25 和 reranker。
9. 命中带图片资源的 chunk 时，知识库问答和 Web Agent 聊天都可以展示相关图片。
10. 解析产物、图片资源和增强结果可缓存；只修改分块参数时不重复支付 VLM 解析成本。

### 2.2 明确不做

- 不把图片、图片 URL、base64 或视觉 content part 发送给回答模型。
- 不做图片向量、CLIP/SigLIP 向量或图文联合召回；一期图片只通过说明/OCR参与文字检索。
- 不要求回答模型在答案里生成图片标记；图片展示由后端结构化元数据和前端完成。
- 不支持 `doc / ppt / xls / rtf` 等旧 Office 格式。
- 不抓取 Markdown 中的网络图片，不读取 Markdown 相对路径指向的本地图片。
- 不追求 Office/PDF 像素级还原；目标是适合检索的结构化 Markdown。
- 不在微信等外部 channel 中主动发送知识库图片；一期资源展示范围是 Web 知识库问答、检索调试页和 Web Agent 聊天。
- 不让 MarkItDown sidecar 自行访问互联网或直接调用 VLM。

### 2.3 关键决策

| 决策点 | 结论 |
|---|---|
| 产品名称 | 可称“多模态文档 RAG”，产品说明必须补充“视觉解析、文字检索/回答、原图资源展示” |
| KB 配置 | 使用 `parseMode: "standard" \| "auto"`，UI 可表现为“高级 RAG”开关；关闭映射 standard，开启映射 auto |
| 迁移默认值 | 现有 KB 迁移为 standard，避免升级后自动产生 VLM 费用 |
| PDF auto | 逐页路由，复杂页才调用 VLM；不是整篇无条件调用 |
| PDF 降级 | VLM/sidecar 单页失败 → 当前普通文字层提取；全篇没有有效内容才失败 |
| Office | docx/pptx/xlsx 全部走 MarkItDown；sidecar 不可用时明确失败，不回退旧 DOCX XML 解析器 |
| MarkItDown OCR 插件 | 一期不直接启用；由 bkcrab 自己管理图片 ID、原图、VLM 缓存和说明，避免插件只返回内联文字而丢失稳定资源关系 |
| 回答模型 | 仅接收 `chunktext.Answer` 派生的文字 AnswerText，永远没有 RAG `image_url` |
| 图片展示 | `Hit.Assets` 与 assistant metadata 携带稳定 `assetId`；前端通过鉴权端点加载 |
| 资源 URL | 不保存预签名 URL、不保存 base64；历史快照只保存稳定 asset ID 和来源元数据 |
| 重建索引 | 同内容图片 ID 跨 reindex 稳定；active/history 引用资源保留到文档删除，未激活 staging/cache 按 TTL 回收 |
| 分块 | Markdown AST/block aware；网络模型调用放在独立 Enricher，不放进 splitter |
| 标题 | 完整 heading path 作为 breadcrumb 进入 SearchContent；RawContent/AnswerText 不机械重复标题，前端引用另带 SectionTitle |
| 大文件内存 | 50MB 是上传硬限制，不是 `io.ReadAll` 的理由；原件、sidecar tar 和图片都采用流式 I/O + 配额临时目录 |
| chunk 载荷 | 主库新增 chunk catalog 保存 raw/enhancement/资源关系；Milvus 继续只保存检索文本与向量 |
| 首个闭环 | PDF auto → 图片说明/资源 → chunk → 文字检索/回答 → Web 展示图片 |

## 3. 总体架构

```text
                              ┌──────────────────────────────┐
原始文件 ──对象存储──────────▶│ Document Parser              │
                              │ - MD/TXT sanitizer           │
                              │ - PDF auto router            │
                              │ - Office/MarkItDown sidecar  │
                              └──────────────┬───────────────┘
                                             │ ParsedDocument
                                             │ Markdown units + asset manifest
                                             ▼
                              ┌──────────────────────────────┐
                              │ Artifact / Asset Persistence │
                              │ parsed.json / normalized.md  │
                              │ original/display/thumbnail   │
                              └──────────────┬───────────────┘
                                             │ ParsedArtifact
                                             │ canonical asset IDs, no temp handles
                                             ▼
                              ┌──────────────────────────────┐
                              │ Markdown AST Splitter        │
                              │ heading/paragraph/table/code │
                              │ image occurrence association │
                              └──────────────┬───────────────┘
                                             ▼
                              ┌──────────────────────────────┐
                              │ Enricher                     │
                              │ table summary / code summary │
                              │ cached, failure-tolerant     │
                              └──────────────┬───────────────┘
                                             ▼
                        ┌────────────────────┴───────────────────┐
                        ▼                                        ▼
             SQL chunk/asset catalog                  embedding + Milvus
             raw/enhancement/asset refs               SearchContent/BM25
                        └────────────────────┬───────────────────┘
                                             ▼
                                    Search / Rerank
                                             │
                  ┌──────────────────────────┴─────────────────────────┐
                  ▼                                                    ▼
       回答模型：仅派生 AnswerText                       前端：AssetRef → 鉴权图片端点
       raw + 文字增强，无图片输入                         相关资源 gallery / source card
```

解析、分块、增强是三个独立边界：

- Parser 可以调用 sidecar 和视觉模型，但只产出规范化文档；
- Splitter 是确定性的纯本地逻辑；
- Enricher 可以调用文字模型，但失败只丢失增强，不丢失原文。

## 4. 配置与能力发现

### 4.1 系统配置

在现有 `config.RAGCfg` 中增加：

```go
type RAGCfg struct {
    Milvus        MilvusCfg
    Embedding     RAGEmbeddingCfg
    Reranker      RAGRerankerCfg
    Features      RAGFeatureCfg
    DocumentAI    RAGDocumentAICfg
    ParserSidecar RAGParserSidecarCfg
    Limits        RAGLimitsCfg
}

type RAGFeatureCfg struct {
    AdvancedParsingEnabled bool
    OfficeParsingEnabled   bool
    TextEnrichmentEnabled  bool
}

type RAGDocumentAICfg struct {
    APIType                 string // v1 只允许 openai-compatible
    Endpoint                string
    APIKey                  string
    VisionModel             string
    TextModel               string
    TimeoutMS               int
    VisionConcurrency       int
    EnrichmentConcurrency   int
    VisionPromptVersion     string
    EnrichmentPromptVersion string
    AllowedEndpointHosts    []string
    AllowPrivateEndpoint    bool
}

type RAGParserSidecarCfg struct {
    Endpoint  string
    TimeoutMS int
}
```

三个发布开关默认都为 `false`，对应环境变量固定为：

- `BKCRAB_RAG_ADVANCED_ENABLED` → `Features.AdvancedParsingEnabled`；
- `BKCRAB_RAG_OFFICE_ENABLED` → `Features.OfficeParsingEnabled`；
- `BKCRAB_RAG_ENRICHMENT_ENABLED` → `Features.TextEnrichmentEnabled`。

能力按用途拆分，避免 PDF 许可证阻塞 Office，也避免 capability 互相递归：

- 内部 `AdvancedConfigured()`：advanced 开关 + VisionModel + 固定 endpoint policy 均有效；它只表示视觉调用已配置，不单独作为用户可用能力；
- `OfficeAvailable(snapshot)`：Office 开关 + sidecar Office capability + 三格式定位 golden + TTL health，只表示可以转换 Office；
- `PDFAutoAvailable(snapshot)`：`AdvancedConfigured()` + sidecar PDF capability/许可证 ADR + TTL health；
- `OfficeVisionAvailable(snapshot)`：`AdvancedConfigured()` + `OfficeAvailable(snapshot)`，表示 auto Office 图片可送 VLM；
- `AdvancedAvailable(snapshot)`：`PDFAutoAvailable(snapshot) || OfficeVisionAvailable(snapshot)`，是 UI 是否允许新开启 auto 的聚合值；
- `EnrichmentAvailable()`：文本增强系统开关 + TextModel/endpoint policy 配置。

这些判断只消费缓存快照，不在 API 请求中同步探活。PDF capability 不可用时，Office converter/OfficeVision 仍可独立上线；auto KB 上传 PDF 时明确提示该文档会走 native fallback，不能假装 PDF advanced 已启用。

`DocumentAI` 是入库专用配置，不复用当前回答模型：

- `VisionModel` 用于 PDF 页和图片语义转写；
- `TextModel` 用于表格/代码增强；只有系统开关、模型配置和 KB 显式 opt-in 同时成立时才调用；
- 回答模型仍由用户聊天配置决定，两者能力和生命周期完全解耦。

新增资源限制的初始默认值：

| 配置 | 默认值 | 作用 |
|---|---:|---|
| `maxFileMB` | 50 | 保持当前后端默认 |
| `maxPagesPerDocument` | 300 | 所有 PDF/分页 Office 解析硬上限；超出明确失败，避免静默截断 |
| `maxVisionPagesPerDocument` | 100 | 超出部分走 native 并记录 degraded |
| `maxVisionAssetsPerDocument` | 100 | Office 图片视觉转写上限；超出后保留资源和 alt/占位并 degraded |
| `maxEnrichmentBlocksPerDocument` | 200 | 表格/代码增强请求上限；超出后原文正常入库 |
| `maxDocumentAIRequests` | 300 | 单个逻辑 indexing task 跨 Vision/repair/Enricher/retry 的总请求上限 |
| `maxDocumentAITokens` | 200000 | 单任务视觉+增强累计 token 预算；provider 无 usage 时按本地估算 |
| `maxEstimatedDocumentAICostUSD` | 1.00 | 配置了模型价格时的单任务估算费用上限；未知价格仍受 token/请求数上限 |
| `maxDocumentAIResponseBytes` | 2 MiB | 单次上游 HTTP body 硬上限，超出立即中止 |
| `maxDocumentAIOutputTokens` | 4096 | 单次请求显式输出 token 上限；repair 也受限 |
| `maxDocumentAIJSONDepth` | 32 | typed JSON 最大嵌套深度；字段/数组另受 schema 上限 |
| `maxDocumentAIRequestsPerUserPerDay` | 1000 | 用户/租户 UTC 日周期 durable 配额，所有 outbound attempt/repair 计入 |
| `maxDocumentAITokensPerUserPerDay` | 2000000 | 用户/租户 UTC 日输入+保守输出 token 配额 |
| `maxEstimatedDocumentAICostPerUserPerDayUSD` | 10.00 | 有价格表时的用户/租户 UTC 日估算费用配额 |
| `maxPendingAdvancedTasksPerUser` | 3 | 防批量上传/reindex 绕过单任务预算 |
| `minAdvancedReindexInterval` | 60 s | 同文档高级重建最短间隔；管理员可覆盖 |
| `maxVisionInputBytes` | 8 MiB | resize/编码后的单次视觉输入上限 |
| `maxSearchContentBytes` | 60 KiB | embedding/Milvus 前的 UTF-8 字节级上限；低于现有 `content VARCHAR(65535)` 并留协议余量 |
| `maxMilvusFilterBytes` | 32 KiB | active-version scalar predicate 的内部安全上限；须由 Milvus 集成测试验证 |
| `indexGCGracePeriod` | 15 min | 每个 retired active version 的独立最短保留期，必须大于最大检索请求时长加余量 |
| `stagingArtifactTTL` | 24 h | 从未 active/history 引用的 staging/orphan 最早回收时间 |
| `maxCacheFingerprintsPerDocument` | 3 | 每文档保留的未引用 parse/enrichment cache 世代上限 |
| `maxAssetsPerDocument` | 500 | 防 OOXML/PDF 图片炸弹 |
| `maxAssetBytes` | 20 MiB | 单个解压后资源 |
| `maxExtractedBytes` | 200 MiB | 单文档全部解压产物 |
| `maxImagePixels` | 40,000,000 | 解码后的宽×高 |
| `pdfRenderDPI` | 180 | VLM 页面与视觉区域裁剪 |
| `thumbnailMaxEdge` | 480 px | Web 缩略图 |
| `displayMaxEdge` | 2400 px | 安全展示版本 |
| `parseTimeoutMS` | 600,000 | 单文档 sidecar 上限 |

所有默认值必须能通过系统配置覆盖。

### 4.2 KB 配置

`rag_kbs` 新增 `parse_mode` 与 `enrichment_enabled`：

```json
{
  "name": "产品资料",
  "chunkSize": 512,
  "chunkOverlap": 64,
  "parseMode": "auto",
  "enrichmentEnabled": true
}
```

- `standard`：PDF 使用原生文字层；Office 仍使用 MarkItDown，但不要求视觉模型描述图片；MD/TXT 按安全规则处理。
- `auto`：PDF 逐页自动路由；Office 图片进行视觉语义转写。
- 表格/代码增强属于统一分块后的独立阶段，不由 parseMode 控制；`enrichmentEnabled` 默认 false，必须由用户在看到“表格/代码原文会发送到管理员配置的第三方 DocumentAI”披露后显式开启。系统 gate 或 TextModel 不可用时不能开启；关闭时仍完整保留/分块原始表格和代码，不发任何增强请求。

KB 修改 `parseMode/enrichmentEnabled` 不直接重跑全部文档。文档 DTO 同时返回 active 版本实际/目标配置、`needsReparse` 与 `needsReindex`；UI 显示“需要重新解析/重新索引”。这两个布尔值分别比较 active 版本的 parse/index fingerprint 与当前 KB 目标配置，不能读取最近一次失败 attempt 的可变字段。`enrichmentEnabled` 只改变 index fingerprint；用户触发 reindex 时，若目标解析指纹变化，流水线自动 reparse。

当 auto 运行能力暂时不可用时：禁止新建或把 standard 改成 auto，但必须允许已有 auto KB 切回 standard；已有 active auto 索引仍可查询。

### 4.3 能力接口

新增 `GET /api/rag/capabilities`，避免前端硬编码当前 20MB 与后端 50MB 的不一致：

```json
{
  "supportedExtensions": [".md", ".markdown", ".txt", ".pdf", ".docx", ".pptx", ".xlsx"],
  "maxFileBytes": 52428800,
  "maxFileBytesByExtension": {
    ".md": 52428800,
    ".markdown": 52428800,
    ".txt": 52428800,
    ".pdf": 52428800,
    ".docx": 52428800,
    ".pptx": 52428800,
    ".xlsx": 52428800
  },
  "parseModes": ["standard", "auto"],
  "advanced": {
    "enabled": true,
    "configured": true,
    "healthy": true,
    "available": true,
    "reason": "",
    "checkedAt": "2026-07-20T10:00:00Z"
  },
  "office": {
    "enabled": true,
    "configured": true,
    "healthy": true,
    "available": true,
    "reason": "",
    "checkedAt": "2026-07-20T10:00:00Z"
  },
  "pdfAuto": {
    "available": true,
    "reason": ""
  },
  "officeVision": {
    "available": true,
    "reason": ""
  },
  "enrichment": {
    "enabled": true,
    "configured": true,
    "available": true,
    "reason": ""
  },
  "documentAIBudget": {
    "maxRequestsPerDocument": 300,
    "maxTokensPerDocument": 200000,
    "maxEstimatedCostUSDPerDocument": 1.0
  }
}
```

`advanced/office/enrichment.enabled` 是默认关闭且彼此独立的发布开关。capabilities 只读取后台缓存的健康快照，不在每个页面请求中同步探测 sidecar。`maxFileBytesByExtension` 对 sidecar 格式取主服务配置与 sidecar `/healthz.limits.maxInputBytes` 的较小值；MD/TXT 仍使用主服务限制。Office 的 `available` 还要求三种 converter/定位 golden 已通过，不能仅凭 `/healthz` 宣布可用。配置后来失效时，已经存在的 auto 任务按降级矩阵执行，而不是让整个 RAG 服务不可用。

UI 在用户开启 auto 前明确提示：PDF 页面和 Office 图片可能发送给管理员配置的第三方 DocumentAI 服务，并展示 capability 返回的文档级请求/token/费用预算；关闭 auto 不触发这类视觉请求。开启 `enrichmentEnabled` 使用独立确认文案，standard/auto 都一样披露表格和代码外发；关闭 enrichment 不触发文字增强请求。

## 5. 统一解析协议

### 5.1 Parser 接口

替换全局函数 `parse.Parse(io.Reader, fileName)`：

```go
type Parser interface {
    Parse(ctx context.Context, source Source, options ParseOptions) (*ParsedDocument, error)
}

type Source struct {
    DocID       string
    FileName    string
    Format      string
    Size        int64
    SHA256      string
    Open        func(context.Context) (io.ReadCloser, error)
}

type ParseOptions struct {
    Mode          ParseMode
    ParserVersion string
}
```

Parser 不依赖 splitter、embedding 或 Milvus。原件从对象存储流式下载到受限临时文件；不再对 50MB 文件无条件 `io.ReadAll`。

### 5.2 ParsedDocument IR

```go
type ParsedDocument struct {
    SchemaVersion int
    Source         ParsedSource
    Parser         ParserInfo
    Units          []MarkdownUnit
    Assets         []ExtractedAsset
    Occurrences    []AssetOccurrence
    Warnings       []ParseWarning

    cleanupOnce sync.Once
    cleanup     func() error
    cleanupErr  error
}

// Close 幂等释放 bundle/temp-dir 所有权；MD/TXT 可使用 no-op cleanup。
func (d *ParsedDocument) Close() error {
    d.cleanupOnce.Do(func() {
        if d.cleanup != nil {
            d.cleanupErr = d.cleanup()
        }
    })
    return d.cleanupErr
}

type MarkdownUnit struct {
    ID       string
    Location SourceLocation
    Markdown string
}

type SourceLocation struct {
    Kind  string // document / page / slide / sheet
    Index int    // 1-based；document 可为 0
    Label string // 第 3 页 / 幻灯片 2 / 工作表 Sheet1
}

type ExtractedAsset struct {
    LocalID        string
    ContentSHA256  string
    Kind           string // image
    SourceKind     string // embedded_original / page_crop / scanned_page
    SourceMIME     string
    Width          int
    Height         int
    ByteSize       int64
    BundleEntry    string `json:"-"` // 受限 tar entry 或临时文件句柄；不是对象存储 key
}

type AssetOccurrence struct {
    ID          string
    AssetLocalID string // persistence 后映射为 canonical Asset.ID
    UnitID      string
    Order       int
    Location    SourceLocation
    BBox        *NormalizedBBox
    AltText     string
    Caption     string
    OCRText     string
    Decorative  bool
    Confidence  float64
}

type ParseWarning struct {
    Code       string
    Message    string
    Location   *SourceLocation
    Degraded   bool
}
```

不变量：

1. `Units` 按源文档阅读顺序排列。
2. 每个 unit 都只含 UTF-8 规范化 Markdown。
3. 图片字节不进入 Markdown；Markdown 只包含内部 occurrence 引用。
4. parser 只产出 `ExtractedAsset`，不接触对象存储；persistence 阶段校验并流式写入后才生成 canonical `AssetRef` 与对象键。
5. canonical `Asset.ID` 由 `docID + contentSHA256` 派生，同一文档相同图片跨 reindex 稳定。
6. 相同图片出现多次时只有一个 ExtractedAsset，可以有多个 Occurrence。
7. parser 产物中不得出现可被后续自动抓取的外部图片 URL。
8. warning 是解析结果的一部分；部分降级仍可以得到成功的 ParsedDocument。

`BundleEntry` 的所有权随 ParsedDocument 交给调用方。所有 Parser 实现必须通过同一 constructor 注入 cleanup；调用方拿到非 nil document 后立即 `defer document.Close()`。`Close` 必须幂等，并在成功持久化、后续失败或 context 取消时关闭文件句柄、清理对应受限临时目录；MD/TXT 使用 no-op cleanup。不得把整包页面和图片表示成 `[][]byte`。

`ParsedDocument` 是带临时句柄的瞬态 IR，绝不直接序列化成 parse artifact。persistence 阶段先流式校验/写入图片，再把所有 local ID 映射为稳定的 canonical asset ID，并生成可缓存的 `ParsedArtifact`：

```go
type ParsedArtifact struct {
    SchemaVersion int
    Source         ParsedSource
    Parser         ParserInfo
    Units          []MarkdownUnit
    Assets         []ArtifactAsset
    Occurrences    []ArtifactOccurrence
    Warnings       []ParseWarning
}

type ArtifactAsset struct {
    ID             string
    ContentSHA256  string
    Kind           string
    SourceKind     string
    SourceMIME     string
    Width          int
    Height         int
    ByteSize       int64
    DisplayStatus  string
}

type ArtifactOccurrence struct {
    ID         string
    AssetID    string
    UnitID     string
    Order      int
    Location   SourceLocation
    BBox       *NormalizedBBox
    Caption    string
    OCRText    string
    Decorative bool
    Confidence float64
}
```

`ParsedArtifact` 只保存 canonical ID、可验证描述字段、Markdown units、occurrences 和 warnings；不得包含临时路径、asset bytes、对象存储 key 或可访问 URL。持久化必须在同一操作中校验 occurrence 映射、把 transient `AssetLocalID` 改写为 `AssetID`，再原子发布 `parsed.json`。缓存命中时依据 artifact 中的 asset ID 从 SQL catalog/对象存储重新 hydrate；任一 catalog 行、source object，或 `display_status=ready` 时必需的安全展示对象缺失，即判定缓存无效并重新解析，不能返回半失效 artifact。

`AltText` 只存在于 transient occurrence：发布 artifact 前先清理并折叠为最终 `Caption`。auto 转写成功时 Caption 优先；standard、转写失败或预算耗尽时依次使用非空安全 alt 和中性占位。artifact 不再同时保存 alt/caption 两套 fallback 语义，Markdown occurrence 文本和 `rag_chunk_assets.caption` 都消费这个最终 Caption。

### 5.3 内部图片 Markdown

Parser 生成的可信 Markdown 使用内部 scheme：

```markdown
![系统架构图：请求依次经过网关、检索器和回答模型](rag-asset://occ_p3_2)

> 图片文字：Gateway → Retriever → Text LLM
```

`rag-asset://` 只允许 parser 生成。AST splitter 解析 occurrence 后：

- `Content` 渲染为“图片说明 + OCR”的纯文本 Markdown，不保留 URL；
- `AssetRef` 单独附着到 chunk；
- `SearchContent` 使用同一说明/OCR做文字召回；
- 回答模型只看到渲染后的文字；
- 前端通过 AssetRef 获取图片。

用户上传的 MD 即使伪造 `rag-asset://` 也会被 sanitizer 当成不受支持图片，不能借此引用其它文档资源。

### 5.4 Sidecar bundle 协议

sidecar 以 tar 流返回 manifest 和文件 entry；Go client 边校验边落入带配额的临时目录，返回可流式打开的句柄。v1 JSON 字段统一 lowerCamelCase，Go/Python 都由同一组 golden JSON 固定。`manifest.json` 完整 schema：

```json
{
  "protocolVersion": "rag-parser/v1",
  "bundleKind": "office-convert",
  "source": {
    "format": "pptx",
    "byteSize": 123456,
    "sha256": "<64 lowercase hex>"
  },
  "parser": {
    "name": "markitdown",
    "version": "0.1.6",
    "wrapperVersion": "office-wrapper-v1"
  },
  "entries": [{
    "path": "units/0001.md",
    "sha256": "<64 lowercase hex>",
    "byteSize": 4321,
    "mimeType": "text/markdown; charset=utf-8"
  }, {
    "path": "assets/asset_0001.png",
    "sha256": "<64 lowercase hex>",
    "byteSize": 34567,
    "mimeType": "image/png"
  }],
  "units": [{
    "id": "unit_slide_0001",
    "location": {"kind": "slide", "index": 1, "label": "幻灯片 1"},
    "markdownEntry": "units/0001.md"
  }],
  "assets": [{
    "localId": "asset_0001",
    "entry": "assets/asset_0001.png",
    "kind": "image",
    "sourceKind": "embedded_original",
    "width": 1200,
    "height": 800
  }],
  "occurrences": [{
    "id": "occ_slide_0001_0001",
    "assetLocalId": "asset_0001",
    "unitId": "unit_slide_0001",
    "order": 3,
    "location": {"kind": "slide", "index": 1, "label": "幻灯片 1"},
    "bbox": [100, 200, 700, 800],
    "altText": "系统架构图",
    "caption": "",
    "ocrText": "",
    "decorative": false,
    "confidence": 1.0
  }],
  "pages": [],
  "warnings": [{
    "code": "office_image_coarse_location",
    "message": "图片只能定位到 slide",
    "location": {"kind": "slide", "index": 1, "label": "幻灯片 1"},
    "degraded": true
  }]
}
```

`bundleKind` 只允许 `office-convert/pdf-analyze/pdf-render`。PDF manifest 使用同一顶层结构，并逐页增加：

```json
{
  "page": 1,
  "status": "ok",
  "errorCode": "",
  "unitId": "unit_page_0001",
  "nativeMarkdownEntry": "units/page-0001.md",
  "renderEntry": "",
  "primitiveEntry": "pages/page-0001.json"
}
```

page primitive v1 固定包含 `page/width/height/textChars/blockCount/textCoverage`、0..1000 normalized `textBlocks/embeddedImages[].bbox` 和布尔 `signals.table/code/scanned/multicolumn/readingOrderUncertain`；不得夹带任意文件路径或 URL。单页 `status=failed` 不使整个 bundle 失败，Go router 只降级该页。

entry 的 required/forbidden 规则由 `bundleKind + page.status` 固定，不能只检查“路径存在”：

| bundleKind / status | required | 必须为空/不存在 |
|---|---|---|
| `office-convert` | 至少一个 `units[].markdownEntry`；每个 asset/occurrence 引用合法 | `pages` |
| `pdf-analyze` / `ok` | `page`、确定性 `unitId`、`nativeMarkdownEntry`、`primitiveEntry`；`errorCode=""` | `renderEntry`、顶层 `units/assets/occurrences` |
| `pdf-render` / `ok` | `page`、确定性 `unitId`、`renderEntry`；`errorCode=""`，允许该页的 embedded assets/occurrences | `nativeMarkdownEntry`、`primitiveEntry`、顶层 `units` |
| 任一 PDF / `failed` | `page`、同算法生成的 `unitId`、非空 allowlisted `errorCode` | 三个 page entry 字段，以及引用该 unitId 的 occurrence/asset |

空白/扫描页的 `nativeMarkdownEntry` 仍指向一个允许为空的 Markdown payload，使 primitive 与页状态可以完整验证；不能用缺失 entry 表达“没有文字”。

Office bundle 由顶层 `units[].id` 声明 occurrence 可引用的 unit；PDF bundle 则由 `pages[].unitId` 声明，顶层 `units` 固定为空。`unitId` 按 source hash + 1-based page 确定性生成，在 analyze/render 两次响应中必须一致且 page 不能重复。`pdf-analyze` 必须为检测到的全部页按 1..N 无缺口返回一条 page record；`pdf-render` 响应 page 集合必须与请求 allowlist 恰好相等，即使某页失败也要返回 failed record，禁止静默漏页或附加未请求页。每个 PDF asset 必须至少被一个 ok page occurrence 引用，failed page unit 不得有 occurrence；其它页的合法资源不受单页失败影响。

`pdf-render` 可以在顶层 `assets/occurrences` 返回所请求页的内嵌 XObject：每个 occurrence 的 `unitId/location` 必须与同一 manifest 的 page 声明一致，asset entry 仍在顶层 `entries` 对账。page `renderEntry` 是供 Go 做 VLM 输入/crop 的临时安全栅格，不自动等同于可展示 asset；内嵌原图通过 assets 表达，后续 page crop/scanned-page asset 由 Go 按 VLM bbox 和策略派生。

tar 中 `manifest.json` 必须是第一个 entry 且不超过 1 MiB，后续 entry 按规范路径字典序排列。顶层 `entries` 是除 manifest 自身外所有 payload 的唯一目录，按 path 排序并声明 path/hash/byteSize/MIME；`units[].markdownEntry`、`assets[].entry` 和 `pages[]` 的三个非空 entry 字段都必须恰好引用其中一项，每项也必须恰好被协议对象引用，禁止重复维护另一套 hash/size/MIME。解码器先执行上表的 required/forbidden 校验，再核对引用。Go 边写临时文件边复算并在 EOF 对账。未知 JSON 字段、未知 entry、未声明/缺失/未引用 entry、hash/size/MIME 不符、重复 ID、悬空 occurrence、绝对路径、`..`、Windows volume/UNC path、PAX path override、symlink/hardlink/device 一律拒绝。协议限制 entry 数、单 entry/总解压字节；`manifest.json` 本身不允许引用外部资源。

`GET /healthz` 同样是版本化契约：

```json
{
  "protocolVersion": "rag-parser/v1",
  "serviceVersion": "<build>",
  "limits": {"maxInputBytes": 52428800, "maxOutputBytes": 209715200},
  "capabilities": {
    "office": {"enabled": true, "formats": ["docx", "pptx", "xlsx"], "markitdownVersion": "0.1.6", "wrapperVersion": "office-wrapper-v1"},
    "pdf": {"enabled": false, "engine": "", "engineVersion": ""}
  }
}
```

sidecar 的 `maxInputBytes` 由部署配置从同一 RAG limit 传入并在 health 回显；主服务对 sidecar 格式使用自身与 health 值的较小者。协议版本不匹配时 capability unavailable，不能猜测兼容。

### 5.5 所有 parser 输出的统一 Markdown 安全闸门

PDF native/VLM、Office/MarkItDown、MD/TXT 的每个 MarkdownUnit 都必须在 asset persistence/artifact 发布前经过同一个 Go AST normalizer；不能把“sidecar/VLM 生成”当成可信 HTML。规则：

1. raw HTML block/inline 一律转义为普通文本或丢弃；前端 Markdown renderer 也禁用 raw HTML/`dangerouslySetInnerHTML`，形成第二道防线。
2. 普通链接只保留经过 scheme/长度校验的 `http/https` 文本链接，parser 自身永不抓取；`javascript/data/file/vscode`、控制字符、协议相对 URL 和其它 scheme 转成可见纯文本。
3. 所有外部/本地 image、HTML/SVG/CSS 资源节点都转成忽略提示，不产生浏览器请求。
4. `rag-visual://` 必须在 VLM 结果绑定 occurrence 时全部解析完；到达 normalizer 仍存在即失败。
5. `rag-asset://<occurrence-id>` 只有在 `allowInternalAssets=true` 且 ID 精确存在于当前 transient ParsedDocument occurrence map 时保留；用户 MD 始终传 false，不能伪造跨文档引用。
6. fenced/indented code 内的相同文本不解释为链接、图片、HTML 或 internal scheme。
7. 规范化输出再次 parse 必须得到等价 AST；artifact 只保存该输出。

normalizer 测试必须把同一恶意 corpus 分别伪装成 MD、Office Markdown、PDF native 和 VLM Markdown，证明来源不影响结果。

## 6. 各格式解析策略

### 6.1 路由矩阵

| 格式 | standard | auto |
|---|---|---|
| MD | AST 清理图片；其余 Markdown 保留 | 同 standard |
| TXT | 纯文本转义成 Markdown 段落 | 同 standard |
| PDF | 流式/临时文件文字层提取并转规范 Markdown | page analyzer → native/VLM 逐页路由 |
| DOCX | MarkItDown；提取并保存图片，使用 alt/中性占位，不调用 VLM | MarkItDown + 图片提取 + VLM 说明/OCR |
| PPTX | MarkItDown；按 slide 输出并保存图片，使用 alt/中性占位 | MarkItDown + shape 图片提取 + VLM |
| XLSX | MarkItDown；按 sheet 输出表格并保存锚定图片，使用 alt/中性占位 | MarkItDown + 单元格锚点图片提取 + VLM |

旧格式 `doc/ppt/xls` 在上传校验阶段直接拒绝。

### 6.2 PDF auto

sidecar 的第一阶段 PDF analyze 对每页返回：

- 原生文字与按阅读顺序生成的 native Markdown；
- 页宽高、文字字符数、文字块数量和覆盖率；
- 内嵌栅格图的位置与面积；
- 表格、代码、扫描页、复杂多栏的启发式信号；
- 可供后续按页渲染的页索引；第一阶段不返回全篇高分辨率位图。

Go router 根据上述统计选出 VLM 页后，再调用第二阶段 PDF render，只渲染被选中的页和需要的内嵌图。禁止为了 auto 判定预先渲染并传回全部 300 页；这样页面位图总量受 `maxVisionPagesPerDocument` 约束。

Go 按稳定 `pages[].unitId` 合并两次 bundle：analyze 提供该 unit 的 native Markdown/primitive，render 提供同页临时栅格、embedded asset 与 occurrence。render 返回未知/重复 unitId、页码不在请求 allowlist、location 与 analyze 不一致或 XObject occurrence 悬空时，整份 render bundle 无效并按页降级，不能靠位置猜测关联。

路由器使用版本化的确定性规则。满足任一条件时走 VLM：

1. 非空白字符少于 80；
2. 有意义图片区域占页面至少 15%；
3. 检测到表格、代码、双栏或阅读顺序置信度不足；
4. 页面主要由扫描图构成；
5. native Markdown 与文字层统计明显不一致。

其余简单文本页走 native。阈值属于 parser version，调整阈值必须改变解析指纹。

VLM 必须返回结构化 JSON：

```json
{
  "markdown": "## 安装流程\n\n步骤一……\n\n![流程图](rag-visual://v1)",
  "visuals": [
    {
      "key": "v1",
      "kind": "diagram",
      "bbox": [102, 210, 914, 735],
      "caption": "安装流程从下载软件包开始，随后配置并启动服务。",
      "ocrText": "download → configure → start",
      "decorative": false,
      "confidence": 0.93
    }
  ]
}
```

约束：

- bbox 使用 0..1000 的归一化坐标；
- `markdown` 中每个 `rag-visual://key` 必须恰好对应一个 visual；
- 表格输出 GFM table，代码输出带语言的 fenced code；
- VLM 不得输出外部 URL、base64 或对象键；
- JSON/schema 校验失败允许一次格式修复请求，仍失败即降级；
- prompt 明确将页面内容视为不可信数据，忽略页面内的指令。

结构合法不等于内容可信。对有原生文字层的页面还要做保真校验：规范化后 VLM Markdown 必须覆盖绝大多数原生文字锚点，默认最低覆盖率 70%；明显遗漏大段文字、凭空扩写或输出长度异常时视为该页 VLM 失败并走 native。扫描页没有文字锚点时，使用 OCR 非空、visual bbox、置信度和输出长度上限做基本校验。该质量阈值同样属于 parser version。

视觉资源优先级：

1. 能把 visual bbox 与单一 PDF XObject 可靠对应时保存内嵌原始图；
2. 否则保存 180 DPI 页面的安全裁剪；
3. 扫描页必要时保存 `scanned_page`，但默认不作为普通 gallery 图片，除非该页本身是命中资源的唯一视觉内容。

PDF 降级按页处理：

```text
VLM 成功              → 使用 VLM Markdown + visual assets
VLM 超时/限流/格式错   → native Markdown + warning
sidecar 页面分析失败   → 新 Go streaming/ReaderAt PDF 文字层提取 + warning
native 也无文字        → 跳过该页 + degraded warning
全篇无有效 unit        → 文档 FAILED
```

render 已成功而 VLM 失败时，不能丢弃该页已经安全提取的 embedded-original：把这些 occurrence 绑定到 native page unit，使用可用 alt 或“图片（视觉识别失败）”作为最终 Caption，因此命中同页文字时原图仍可展示。没有合法 VLM bbox 时不伪造 page crop；临时整页 render 在降级后清理，除非它按 `scanned_page` 策略成为该页唯一可展示视觉且仍有可检索 unit。若整个 sidecar 不可用而走纯 Go native fallback，则明确只保证文字，不假装已经提取图片。

### 6.3 Office + MarkItDown

部署独立 `rag-parser` sidecar，固定 Python 与 MarkItDown 版本。sidecar：

1. 只接收服务端上传的字节流和受控扩展名；
2. 使用 `convert_stream()`，不调用宽泛的 URI/URL 转换接口；
3. MarkItDown 前做 OOXML 容器预检：XML parser 禁用 DTD/entity/network；出现 `DOCTYPE/ENTITY` 直接拒绝；relationships 中外部 include/attachedTemplate/OLE/altChunk/externalLink 一律拒绝，普通 hyperlink 只保留可见文本且绝不 dereference；`file://`、UNC、绝对本地目标和非安全 scheme 一律拒绝/移除并 warning；
4. 每请求使用随机、权限 0700 的独立临时目录，禁止跟随 OOXML 内 symlink/路径逃逸，不能读取其它请求或容器文件；
5. 先用锁定版本 fixture 验证 MarkItDown 是否暴露可用的图片 hook/data URI；不能把未承诺行为当协议；
6. 插件默认关闭，尤其不直接启用 `markitdown-ocr`；
7. 自有 wrapper 从已预检 OOXML relationships 提取图片，并通过可验证的 sentinel/converter hook 生成 occurrence marker；
8. 返回受限 tar bundle：`manifest.json`、`units/*.md`、`assets/*`；
9. 不持有对象存储凭据，不持有 VLM key，不允许外网访问。

格式锚点：

- DOCX：drawing 所在 paragraph/run 的阅读顺序；
- PPTX：slide + shape 的 top/left 排序，group shape 递归展开；
- XLSX：sheet + cell anchor；图片插入对应行附近，无法精确插入时放在该 sheet 的“相关图片”小节并保留 cell label；
- 同一媒体关系重复使用时产生多个 occurrence，资产去重。

MarkItDown 负责标题、列表、表格、链接等基础 Markdown 转换；bkcrab wrapper 负责稳定图片资源与位置，不依赖 MarkItDown 输出中未承诺的临时文件名。实现前必须用 DOCX/PPTX/XLSX 各一个 fixture 完成阻断性定位 spike：优先选择“转换前注入唯一 sentinel”或明确 converter hook；若某格式无法精确映射，固定降级到 paragraph/slide/sheet 级相关图片小节并记录 warning，不能声称精确原位。

Office wrapper 还要做保守的结构后处理：Word 中明确的 Code/Preformatted 样式，以及 PPTX 中明确标记为代码的文本框可转换成 fenced code；只有样式/内容能可靠识别语言时才写 language info string，否则使用无语言 fence。连续等宽文本只能作为弱信号，普通等宽段落保持普通文本。PPTX speaker notes 一期保留在对应 slide 末尾的 `> 演讲者备注` 区块，参与检索但不影响图片位置。MarkItDown 产生的表格必须经 GFM AST 校验；不合法时保留单元格文字并记录 warning。

standard 模式也提取、去重、持久化 Office 原图；有 alt text 时把 alt 写在原位置，没有时写“图片（未进行视觉识别）”并记 warning。只有 auto 才把图片发送给 DocumentAI。超过视觉图片/token 预算时采取同样降级，文档仍可 DONE + degraded。

Office auto 图片不复用 PDF page schema，使用独立 typed contract：

```go
type ImageTranscriber interface {
    DescribeImage(ctx context.Context, input NormalizedImageInput, budget *TaskDocumentAIBudget) (ImageDescription, error)
}

type ImageDescription struct {
    Kind       string  `json:"kind"`
    Caption    string  `json:"caption"`
    OCRText    string  `json:"ocrText"`
    Decorative bool    `json:"decorative"`
    Confidence float64 `json:"confidence"`
}
```

输入是去元数据、限制像素/字节并重编码后的 JPEG/WebP，加上非敏感 `format + location + altText`；不发送对象键、原文件路径或 URL。输出 schema 限制 kind allowlist、caption/OCR 各自字节数、总字符、置信度 0..1，拒绝 URL/base64/internal scheme/未知字段；允许一次受预算约束的 repair。成功后把 Caption/OCR 写回对应 occurrence，图片原始字节保持独立 asset。缓存键为：

```text
sha256(normalized_image_bytes + vision_provider_fingerprint + vision_model
       + vision_prompt_version + image_description_schema_version)
```

相同 asset 的多个 occurrence 可以复用描述，但 location-specific alt/context 不进入缓存输出事实；若未来需要基于上下文重写说明，必须升级 schema/cache key，不能静默复用。

### 6.4 MD/TXT

MD 使用真正的 Markdown AST 做清理，不使用正则：

- Markdown image、reference image、HTML `<img>`、`picture/source`、`data:` image 全部不访问；
- raw HTML 中的 `<svg><image>`、CSS `background/url()` 等也不解析；不在明确文本 allowlist 的 raw HTML 整块转义或丢弃；
- 替换为可见文字 `[已忽略文档中的图片：<alt>]`；
- 没有 alt 时替换为 `[已忽略文档中的图片]`；
- 记录 `markdown_image_ignored` warning，文档状态可为 DONE + degraded；
- 普通超链接只作为文本/链接保留，解析器本身不发请求；
- fenced code 内形似图片的文本保持原样。

TXT 不解释 Markdown 图片语法，按纯文本处理并转义必要字符。

### 6.5 PDF engine 与许可证闸门

sidecar 需要“逐页文字/块/bbox 分析、内嵌图提取、指定页渲染”能力，但本设计不默认锁死 PyMuPDF。PyMuPDF 官方采用 AGPL 或商业双许可证；在组织明确接受 AGPL 合规义务或购买商业许可前，不能把它加入分发镜像。实现 PDF sidecar 前必须提交依赖 ADR，选择且记录以下一种结果：

1. 采用 PyMuPDF，并记录 AGPL 合规方案；
2. 采用 PyMuPDF 商业许可，并记录内部许可证明位置（不得提交密钥/合同）；
3. 采用通过组织审核的替代 PDF engine，并用同一 golden contract 验证功能、性能和安全限制。

后续代码只依赖 `PDFEngine` 窄接口，测试 fake 与 golden 不绑定具体实现。未通过该闸门时可以完成 MD/TXT/Office 契约工作，但不得发布 PDF auto sidecar。

## 7. 解析产物、缓存与对象存储

### 7.1 对象键

保留现有原件 key，并在文档前缀下增加：

```text
rag/<user>/<kb>/<doc>/<original-file>
rag/<user>/<kb>/<doc>/artifacts/<parse-fingerprint>/parsed.json
rag/<user>/<kb>/<doc>/artifacts/<parse-fingerprint>/normalized.md
rag/<user>/<kb>/<doc>/assets/<content-sha256>/source.<ext>
rag/<user>/<kb>/<doc>/assets/<content-sha256>/display.webp
rag/<user>/<kb>/<doc>/assets/<content-sha256>/thumbnail.webp
rag/<user>/<kb>/<doc>/cache/pages/<page-cache-key>.json
rag/<user>/<kb>/<doc>/cache/images/<image-description-cache-key>.json
rag/<user>/<kb>/<doc>/cache/enrich/<enrichment-cache-key>.json
```

对象键永远不返回客户端。

### 7.2 解析指纹

`parse_fingerprint` 至少包含：

```text
source_sha256
+ parse_mode
+ ParsedArtifact schema version
+ parser implementation version
+ MarkItDown version
+ PDF render/routing settings
+ vision provider fingerprint + model
+ vision prompt version
```

chunk size、overlap、embedding 模型和表格/代码增强不属于解析指纹，因此调整这些配置可以复用已持久化的 ParsedArtifact。

另定义 `index_fingerprint`：

```text
parse_fingerprint
+ chunk_size + chunk_overlap
+ splitter/search-content schema version
+ embedding model/dimensions
+ enrichment_enabled
+ if enabled: text provider fingerprint + model + enrichment prompt/schema version
```

每个排队版本保存不可变 parse/index 配置快照。active 文档状态和 UI 的 `needsReparse/needsReindex` 只比较 active 版本快照，不能让新 KB 配置或一次失败重试改变同一版本的语义。

页面 VLM cache key：

```text
sha256(page_render_bytes + vision_provider_fingerprint + vision_model
       + vision_prompt_version + page_schema_version)
```

表格/代码增强 cache key：

```text
sha256(raw_block + block_kind + text_provider_fingerprint + text_model
       + enrichment_prompt_version + enrichment_schema_version)
```

缓存只在同一文档前缀内复用，避免跨租户缓存侧信道。

### 7.3 资产 ID 与生命周期

`Asset.ID`：

```text
ast_<first-32-hex(sha256(doc_id + "\0" + content_sha256))>
```

数据库使用 `(doc_id, content_sha256)` 唯一约束处理理论碰撞。

生命周期：

- reindex：相同内容复用同一 asset 行和对象；
- active 版本、`rag_chat_turns.sources` 或 Agent assistant `ragResources` 历史 snapshot 引用的图片：保留到文档删除，保证历史 gallery 可继续显示；
- 从未激活且未被任何历史 snapshot 引用的 staging asset/artifact/page/enrichment cache：按 TTL 和每文档 fingerprint 配额由 durable GC 回收；
- FAILED/SUPERSEDED/RETIRED version 的向量和 chunk 通过精确版本删除；GCED version tombstone 在 v1 保留到文档删除，周期 sweep 可据此重复清理 GC 后才迟到写入的外部向量；
- 文档删除：先事务性进入 `DELETING` 并立即从搜索/claim/owner 与 session asset auth 排除，再通过可重试 cleanup 删除 Milvus、对象、asset/chunk 元数据，完成后删除/墓碑化 SQL 文档；即使对象仍存在，历史图片也立即 404，历史问答文字仍可读；
- KB/用户删除：KB/user 一进入 `DELETING` 就立即撤销其全部搜索、索引与资源访问，再由注入的 RAG cleaner 枚举清理 KB/Milvus/对象/catalog，全部成功后才删除用户 SQL 行；
- 文档/KB 删除和 reindex 不直接重置 DocumentAI aggregate：未发送的 RESERVED 只能按状态机 RELEASE，SENT/COMMITTED/OVERRUN 不退还。task budget/usage 至少保留到其 user-period 结束加审计 TTL，user-period aggregate 也不能因资源 cleanup 重建为零。真正删除用户后可按隐私保留策略匿名化/清理明细，但不能让仍有效的同一 user ID 绕过当期额度；
- `last_seen_version` 只用于去重/GC 候选，不能代替 active/history 引用判断；
- 不把预签名 URL写入 `rag_chat_turns.sources`。

## 8. 数据模型

### 8.1 rag_kbs

新增：

```text
parse_mode          VARCHAR(16) NOT NULL DEFAULT 'standard'
enrichment_enabled BOOLEAN     NOT NULL DEFAULT FALSE
```

### 8.2 rag_documents

新增：

```text
source_sha256         CHAR(64)     NOT NULL DEFAULT ''
active_version        BIGINT       NOT NULL DEFAULT 0
index_format_version  SMALLINT     NOT NULL DEFAULT 1
processing_stage      VARCHAR(24)  NOT NULL DEFAULT 'queued'
progress_current      INTEGER      NOT NULL DEFAULT 0
progress_total        INTEGER      NOT NULL DEFAULT 0
progress_unit         VARCHAR(16)  NOT NULL DEFAULT ''
degraded              BOOLEAN      NOT NULL DEFAULT FALSE
warning_count         INTEGER      NOT NULL DEFAULT 0
```

沿用现有 `version`（迁移为 BIGINT）作为已经分配的最大物理索引版本/当前 target，`active_version` 是当前检索可见版本。`active_version=0` 永远只表示“没有可检索版本”，绝不能表示“接受任意 legacy 版本”。`index_format_version=0` 才允许 pinned legacy payload 在 hydrate 时回退 `chunktext.Body`；第一次新格式成功激活后置为 1。

迁移规则：旧 DONE 文档固定为 `active_version=version,index_format_version=0`；旧非 DONE 只有能可靠确认一个完整旧版本时才固定到该版本，否则置 `active_version=0` 并排队重建。每个 pinned legacy active version 同时插入 synthetic `rag_document_versions` 行：`parse_mode=standard`、`enrichment_enabled=false`、parser/fingerprint 使用 `legacy-v0` sentinel、chunk/embedding contract 从 KB 快照复制、DocumentAI 模型字段为空且预算字段使用迁移时默认、artifact 为空、结果计数从 document 复制。这样 DTO 有 applied snapshot；sentinel 与新 fingerprint 不等时自然显示需要重建。禁止 legacy unpinned。

### 8.3 rag_document_versions

每个物理索引版本保存“不可变输入快照 + fenced 执行结果”：

```text
doc_id                    VARCHAR(64)  NOT NULL
doc_version               BIGINT       NOT NULL
status                    VARCHAR(20)  NOT NULL
source_sha256             CHAR(64)     NOT NULL
parse_mode                VARCHAR(16)  NOT NULL
chunk_size                INTEGER      NOT NULL
chunk_overlap             INTEGER      NOT NULL
parser_version            VARCHAR(64)  NOT NULL
splitter_version          VARCHAR(64)  NOT NULL
parse_fingerprint         CHAR(64)     NOT NULL
index_fingerprint         CHAR(64)     NOT NULL
vision_model              VARCHAR(128) NOT NULL
vision_provider_fingerprint CHAR(64)   NOT NULL
vision_prompt_version     VARCHAR(64)  NOT NULL
text_model                VARCHAR(128) NOT NULL
text_provider_fingerprint CHAR(64)     NOT NULL
enrichment_prompt_version VARCHAR(64)  NOT NULL
enrichment_enabled        BOOLEAN      NOT NULL
max_document_ai_requests  INTEGER      NOT NULL
max_document_ai_tokens    BIGINT       NOT NULL
max_document_ai_cost_microusd BIGINT   NOT NULL
embedding_provider        VARCHAR(64)  NOT NULL
embedding_model           VARCHAR(128) NOT NULL
embedding_dimensions      INTEGER      NOT NULL
embedding_contract_fingerprint CHAR(64) NOT NULL
parse_artifact_key        TEXT         NOT NULL DEFAULT ''
page_count                INTEGER      NOT NULL DEFAULT 0
asset_count               INTEGER      NOT NULL DEFAULT 0
degraded                  BOOLEAN      NOT NULL DEFAULT FALSE
warning_count             INTEGER      NOT NULL DEFAULT 0
created_at                 TIMESTAMP    NOT NULL
updated_at                 TIMESTAMP    NOT NULL
PRIMARY KEY(doc_id, doc_version)
```

从 `source_sha256` 到 embedding contract 的字段是创建时不可变输入。`status/parse_artifact_key/page_count/asset_count/degraded/warning_count/updated_at` 是执行结果：创建时用 PENDING/空/0。普通执行结果只允许持有匹配 IndexFence 的 worker CAS 更新；claim/reclaim/supersede/activate/GC 事务则允许按“旧 task id + 旧 doc_version + 旧 generation + 预期状态/lease”做受限 system transition，不能无条件改 version。模型 endpoint/key 不入库；保存不含 secret 的 provider 配置 fingerprint、稳定 model identifier 和 prompt/schema 版本。重试时当前 provider fingerprint 必须匹配该 snapshot，否则 supersede 并创建新版本，不能用变化后的配置继续写同一 doc_version。embedding provider/model/dimensions 沿用 KB 创建时已有的不可变快照并进入每个 version；同一 KB 禁止混用 embedding contract。未来若允许修改 embedding，必须走整库新 collection generation 原子切换，不属于本期。

version 状态机固定为：

```text
PENDING → RUNNING → DONE → RETIRED → GCED
                  ↘ FAILED
                  ↘ SUPERSEDED
```

- 新 task/version 同事务创建为 PENDING；第一次 claim 同事务把二者置为 RUNNING。
- 匹配 fence 的 transient failure 在一个事务中把当前 version 置 FAILED、task 退回 PENDING、递增 `retry_count`、设置 `next_run_at` 并释放 lease；下一次 claim 必须分配从未使用的新 doc_version，不复用失败版本。
- lease expiry reclaim 在一个事务中把旧 RUNNING version 置 SUPERSEDED、递增 task 的 `retry_count + claim_generation`、分配新 doc_version/version RUNNING 并换 lease；旧 worker 此后没有可更新的 fence。
- 新 reindex supersede 旧非终态 task 时，同一事务把该 task 及其当前 PENDING/RUNNING version 都置 SUPERSEDED。
- permanent failure 或 retry budget 耗尽把匹配 task/version 置 FAILED；document 的 target 状态可显示 FAILED，但旧 `active_version` 仍可检索。
- `ActivateAndFinishRAGIndexTask` 把新 version/task 置 DONE、document target 置 DONE/激活，并把旧 active version 置 RETIRED、创建其 GC task。
- 精确版本 GC 完成后只把 RETIRED/FAILED/SUPERSEDED version 置 GCED；v1 保留 GCED tombstone 到文档删除，供周期 sweep 重删迟到向量。

`rag_documents.status/progress` 描述最新 target attempt，检索可用性只由 `active_version` 决定。任何旧 task/version 的终态都不能覆盖较新 target 的文档状态。

### 8.4 rag_index_tasks

新增：

```text
doc_version      BIGINT       NOT NULL
claim_generation BIGINT       NOT NULL DEFAULT 0
lease_owner      VARCHAR(96)  NOT NULL DEFAULT ''
lease_until      TIMESTAMP    NULL
heartbeat_at     TIMESTAMP    NULL
next_run_at      TIMESTAMP    NULL
```

任务保留现有 `retry_count/max_retry`，不再新增含义重叠的 `attempt_count`：`retry_count` 只统计导致再次执行的 transient failure/lease expiry，并受 `max_retry` 约束；`claim_generation` 在每次成功 claim/reclaim 时递增，只是 fencing nonce，不是重试预算。任务必须通过数据库 compare-and-set claim，不能只靠进程内锁。后台 poller 持续拉取 PENDING/过期 lease，解决队列满后只有重启才恢复的问题。claim/heartbeat/过期判断全部使用数据库时间。

`doc_version` 同时是跨 SQL/Milvus 的物理 fencing epoch。首次任务分配版本 N；任何已经开始过的 task 再次执行（显式 transient retry 或 lease expiry reclaim）时，都在一个事务中把 document.version 和 task.doc_version CAS 到从未使用过的 N+1、复制不可变 version snapshot、终态化旧 version，并递增 `claim_generation`。因此旧 worker 的迟到 Milvus 写仍落在旧主键空间，既不能覆盖新版本，也不能激活。heartbeat、progress、warning、activate、finish 全部按 `task_id + doc_version + claim_generation + lease_owner + RUNNING + lease 未过期` CAS；失败立即 cancel 当前解析/VLM context。

新 reindex 通过 `AdvanceDocumentVersionAndCreateTask(expectedVersion, snapshot)` 在同一事务中分配版本并创建唯一任务，同时把旧非终态任务标记 `SUPERSEDED`。重试按 transient/permanent 分类并使用 `next_run_at` 指数退避。索引激活与主任务完成必须由单一 `ActivateAndFinishRAGIndexTask(fence)` 事务完成，不能在激活后再调用独立 Finish 留出 lease 重抢窗口。

### 8.5 rag_index_gc_tasks

cleanup 不复用 index task 的“reclaim 必须推进 document.version”语义，使用独立 durable 表：

```text
id               BIGINT       PRIMARY KEY
doc_id           VARCHAR(64)  NOT NULL
retired_version   BIGINT       NOT NULL
retired_at        TIMESTAMP    NOT NULL
not_before       TIMESTAMP    NOT NULL
status           VARCHAR(16)  NOT NULL
claim_generation BIGINT       NOT NULL DEFAULT 0
lease_owner      VARCHAR(96)  NOT NULL DEFAULT ''
lease_until      TIMESTAMP    NULL
heartbeat_at     TIMESTAMP    NULL
attempt_count    INTEGER      NOT NULL DEFAULT 0
next_run_at      TIMESTAMP    NULL
created_at       TIMESTAMP    NOT NULL
UNIQUE(doc_id, retired_version)
```

每次 active N→M 时，为退休的 N（N>0）创建独立任务，`not_before = DB_NOW + gcGracePeriod`。GC claim 只递增自己的 generation/lease，不改变 document.version；执行时只允许删除仍为 RETIRED、不是当前 `active_version` 且已超过自身 grace 的版本。v1 没有其它索引-version pin：RAG chat sources/Agent `ragResources` 只 pin 稳定 asset 和快照文字，不阻止旧向量/chunk 回收。删除向量必须调用 `DeleteDocVersion(ctx, kbID, docID, retiredVersion)`，Milvus predicate 固定为 `doc_id == X && doc_version == N`；严禁用 `DeleteOldVersions(..., keepVersion)`，否则连续 1→2→3 会在 version 2 的 grace 到期前被 version 1 的 GC 连带删除。SQL chunk/mapping 同样按精确 doc_version 删除。

GC 完成后把 version 标为 GCED，但 v1 不删除该 version tombstone；周期 orphan sweep 对超过 `stagingArtifactTTL` 的 FAILED/SUPERSEDED/GCED version 再执行幂等精确删除，因此 GC 后才完成的迟到 Milvus upsert 仍能被后续 sweep 找到。version tombstone 只在文档整体删除时移除。失败 staging 和迟到 orphan 不借 GC task 制造新索引版本。

### 8.6 rag_assets

```text
id                    VARCHAR(40)  PRIMARY KEY
doc_id                VARCHAR(64)  NOT NULL
content_sha256        CHAR(64)     NOT NULL
source_kind           VARCHAR(24)  NOT NULL
source_mime           VARCHAR(96)  NOT NULL
display_mime          VARCHAR(96)  NOT NULL
source_object_key     TEXT         NOT NULL
display_object_key    TEXT         NOT NULL
thumbnail_object_key  TEXT         NOT NULL
display_status        VARCHAR(16)  NOT NULL
display_sha256        CHAR(64)     NOT NULL
byte_size             BIGINT       NOT NULL
width                 INTEGER      NOT NULL
height                INTEGER      NOT NULL
first_seen_version    BIGINT       NOT NULL
last_seen_version     BIGINT       NOT NULL
created_at            TIMESTAMP    NOT NULL
updated_at            TIMESTAMP    NOT NULL
UNIQUE(doc_id, content_sha256)
```

`rag_assets` 是 content-addressed、insert-only 的二进制/展示资源记录；caption、OCR、occurrence、model/prompt 等可变语义只保存在带 doc_version 的 `rag_chunk_assets`/artifact 中。旧 worker 因此不能覆盖 active 版本的图片说明。

### 8.7 rag_chunks

主库成为 chunk payload/metadata 的权威来源，Milvus 只负责召回：

```text
kb_id          VARCHAR(64) NOT NULL
doc_id         VARCHAR(64) NOT NULL
doc_version    BIGINT      NOT NULL
chunk_index    INTEGER     NOT NULL
section_title  TEXT        NOT NULL
location_json  TEXT        NOT NULL
raw_content    TEXT        NOT NULL
enhancement    TEXT        NOT NULL
search_content TEXT        NOT NULL
token_count    INTEGER     NOT NULL
created_at     TIMESTAMP   NOT NULL
PRIMARY KEY(doc_id, doc_version, chunk_index)
```

这样不需要立刻迁移 Milvus 固定 schema，也不再通过标题前缀猜测如何从 SearchContent 还原正文。

### 8.8 rag_chunk_assets

```text
doc_id         VARCHAR(64) NOT NULL
doc_version    BIGINT      NOT NULL
chunk_index    INTEGER     NOT NULL
asset_id       VARCHAR(40) NOT NULL
ordinal        INTEGER     NOT NULL
location_json  TEXT        NOT NULL
caption        TEXT        NOT NULL
ocr_text       TEXT        NOT NULL
PRIMARY KEY(doc_id, doc_version, chunk_index, asset_id, ordinal)
```

### 8.9 DocumentAI durable budget 与 usage ledger

DocumentAI 预算不能只放 worker 内存，也不能用无锁的 `SUM(usage) + INSERT` 判定剩余额度；PostgreSQL/MySQL 默认隔离级别下两个并发事务会同时通过检查。增加两个可锁定的聚合行：

```text
rag_document_ai_task_budgets
task_id                    BIGINT      PRIMARY KEY
user_id                    VARCHAR(64) NOT NULL
max_requests               BIGINT      NOT NULL
max_tokens                 BIGINT      NOT NULL
max_cost_microusd          BIGINT      NOT NULL
charged_requests           BIGINT      NOT NULL DEFAULT 0
charged_tokens             BIGINT      NOT NULL DEFAULT 0
charged_cost_microusd      BIGINT      NOT NULL DEFAULT 0
updated_at                 TIMESTAMP   NOT NULL

rag_document_ai_user_budgets
user_id                    VARCHAR(64) NOT NULL
period_start_utc           DATE        NOT NULL
charged_requests           BIGINT      NOT NULL DEFAULT 0
charged_tokens             BIGINT      NOT NULL DEFAULT 0
charged_cost_microusd      BIGINT      NOT NULL DEFAULT 0
updated_at                 TIMESTAMP   NOT NULL
PRIMARY KEY(user_id, period_start_utc)
```

每个 outbound attempt（Vision、Office image、repair、Enricher）再写 durable reservation 明细：

```text
idempotency_key          CHAR(64)     PRIMARY KEY
logical_request_key      CHAR(64)     NOT NULL
user_id                  VARCHAR(64)  NOT NULL
doc_id                   VARCHAR(64)  NOT NULL
task_id                  BIGINT       NOT NULL
doc_version              BIGINT       NOT NULL
claim_generation         BIGINT       NOT NULL
lease_owner              VARCHAR(96)  NOT NULL
operation                VARCHAR(24)  NOT NULL
provider_fingerprint     CHAR(64)     NOT NULL
period_start_utc         DATE         NOT NULL
reserved_input_tokens    BIGINT       NOT NULL
reserved_output_tokens   BIGINT       NOT NULL
actual_input_tokens      BIGINT       NOT NULL DEFAULT 0
actual_output_tokens     BIGINT       NOT NULL DEFAULT 0
estimated_cost_microusd  BIGINT       NOT NULL
state                    VARCHAR(16)  NOT NULL
reservation_expires_at   TIMESTAMP    NULL
sent_at                  TIMESTAMP    NULL
usage_estimated          BOOLEAN      NOT NULL DEFAULT FALSE
created_at               TIMESTAMP    NOT NULL
updated_at               TIMESTAMP    NOT NULL
```

`TaskDocumentAIBudget` 是所有 Vision/image/repair/Enricher 共享的并发安全 façade，`Reserve` 必须接收当前 `IndexFence`。reserve 事务使用固定锁顺序：先 `(user_id,period_start_utc)` budget，再 `task_id` budget，再当前 `rag_index_tasks` row，最后写 usage 明细；PostgreSQL/MySQL 对已 upsert 的聚合行和 task row `SELECT ... FOR UPDATE`，SQLite 使用 `BEGIN IMMEDIATE`。持锁后再次检查 idempotency key，并验证 task 仍为 RUNNING、`doc_version/claim_generation/lease_owner` 匹配且 lease 未按数据库时间过期，再对 task snapshot limit 与当前 user-period limit 同时做条件更新；任一条件不满足则两边都不扣。task claim/reclaim/supersede 事务不反向获取 budget row，避免锁顺序死锁。这样并发争抢最后一份额度时只有一个事务成功，过期 worker 也不能继续预留或出网，不依赖隔离级别碰巧串行化。

usage 状态机固定为：

```text
∅ → RESERVED → SENT → COMMITTED
       └────→ RELEASED
              SENT → OVERRUN
```

`MarkSent` 在真正写上游请求前再次按相同锁顺序验证 IndexFence；fence 已失效时执行 `RESERVED→RELEASED` 并禁止发送。只有 `RESERVED→SENT` 后才允许网络 I/O；SENT 之后即使 lease 丢失，旧 attempt 也只能结算该 idempotency key，不能再 Reserve。`Commit`/late response 可在 fence 失效后把自己的 SENT 结算为 COMMITTED/OVERRUN，确保费用不丢，但不能改变 task/version 状态。

reserve 按已知输入 + 最大输出保守扣减 request/token/cost；只有 RESERVED 且确认请求未发出才可 RELEASED 并在同一锁顺序下回退两张聚合表。已经 SENT 但超时/响应丢失仍计请求和估算输入成本；成功后 COMMITTED actual usage，并释放保守预留与实际用量之间的差额。若 provider 报告的实际量异常超过保守预留，仍要补记真实用量、置 OVERRUN 并阻止该 task/user 后续请求，不能为了守住数字而少记账。reconciler 对过期 RESERVED 在确认 fence 已失效/未 MarkSent 后 RELEASE；对超过上游最大超时的 SENT 按保守预留结算 COMMITTED 且 `usage_estimated=true`，绝不释放。所有 transition 都按 idempotency key + expected state CAS，可崩溃重入。task lease reclaim 用同一 task_id 读取聚合行，不能重置。`logical_request_key` 命中已提交 cache 时不再出网；真正再次发出的 retry/repair 使用独立 outbound attempt key 并再次计费。所有金额用 micro-USD 整数，禁止 float 累计。

至少建立以下索引/约束：

```text
rag_chunks(kb_id, doc_id, doc_version)
rag_assets(doc_id)
UNIQUE rag_assets(doc_id, content_sha256)
rag_chunk_assets(doc_id, doc_version, chunk_index, ordinal)
rag_index_tasks(status, next_run_at, lease_until, created_at)
UNIQUE rag_index_tasks(doc_id, doc_version)
rag_index_gc_tasks(status, next_run_at, lease_until, created_at)
UNIQUE rag_index_gc_tasks(doc_id, retired_version)
rag_document_ai_task_budgets(task_id)
rag_document_ai_user_budgets(user_id, period_start_utc)
rag_document_ai_usage(user_id, period_start_utc, provider_fingerprint)
rag_document_ai_usage(task_id, logical_request_key)
```

三种数据库方言都要提供真实的幂等 ALTER migration；只修改 `CREATE TABLE IF NOT EXISTS` 不算完成迁移。`rag_documents.version`、所有 doc_version 和 asset first/last version 统一迁移为 BIGINT，Go 边界统一使用 `int64`。

存量 task 迁移必须在旧 worker 已停止后分阶段执行，不能把同一文档所有历史 task 都回填为当前 `document.version`：

1. 先以 nullable 方式加入新列/表，尚不创建 `(doc_id,doc_version)` 唯一约束。
2. 归档或删除历史 DONE/FAILED task；每个 doc 的多个 PENDING/RUNNING 只按 `(created_at,id)` 保留最新一个，其余 task 与可关联 attempt 标记 SUPERSEDED 后归档。孤儿 task 标记 FAILED 后归档。
3. 对唯一 survivor 通过运行时 `SnapshotBuilder` 读取原件 hash、KB 不可变 embedding contract 与当前无 secret parser/provider 配置，分配大于该 doc 所有已知/active 版本的新 doc_version，创建完整 PENDING `rag_document_versions` snapshot，再回填 task。无法构造完整 snapshot 时不得保留 runnable row：标记 FAILED，并让文档显示 `needsReindex`。
4. 验证每个 runnable task 恰好对应一个完整 version snapshot、每 doc 最多一个非终态 task、没有 NULL/重复后，才收紧 NOT NULL 并建立唯一约束。

migration fixture 必须覆盖“同一 doc 有多个 DONE + 一个 RUNNING”“多个非终态”“孤儿 task”和“snapshot 构造失败”，并在三种数据库方言执行两遍证明幂等。

## 9. Markdown AST 分块

### 9.1 统一输入

所有格式都转成 `[]MarkdownUnit`。splitter 不接收 source format 分支，只接收：

```go
Split(document ParsedArtifact, config Config) []Chunk
```

AST 至少识别：

- heading；
- paragraph；
- list/list item；
- blockquote；
- fenced/indented code；
- GFM table；
- internal image occurrence；
- thematic break；
- HTML block（清理后作为普通文本或丢弃）。

### 9.2 Chunk 契约

```go
type Chunk struct {
    Index          int
    RawContent     string
    Enhancement    string
    SearchContent  string
    SectionTitle   string
    Location       SourceLocation
    AssetRefs      []AssetRef
    Tokens         int
}
```

语义：

- `RawContent`：规范化原文；图片节点已渲染成说明/OCR文字；
- `Enhancement`：表格/代码的模型生成摘要，失败时为空；
- `SearchContent`：标题路径 + RawContent + Enhancement，用于 embedding/BM25/reranker；
- `AnswerText` 不是第二份存储字段，而是公共 `chunktext.Answer(RawContent, Enhancement)` 派生的只读视图，供文字回答模型；
- `AssetRefs`：仅给后端/前端，不拼入任何模型输入；
- `Tokens` 是当前 `EstimateTokens` 对最终 SearchContent 的估算值，不宣称等于 provider tokenizer 的真实 token 数。

### 9.3 分块规则

1. heading 更新 breadcrumb，不单独丢弃；breadcrumb 进入 SearchContent。
2. paragraph/list/blockquote 可以贪心装箱和滑窗。
3. code、table、image 是原子 block，不与无关段落交叉切割。
4. image block 优先与同一 AST 父节点的 caption、前后一个短段落合并；绝不把图片关联扩散到整个章节。
5. table 超长时按行切分，每块重复表头；单个超长单元格继续按文本边界拆，所有块仍是合法 GFM table。
6. code 超长时优先按函数/空行，其次按行；每个块补齐 fence 和语言；单行仍超长时按 rune 安全切分并标注“续”。
7. overlap 只用于普通文本，不从代码/表格中截取非法后缀。
8. splitter 对 table/code 预留 `min(config.EnhancementReserveTokens, ChunkSize/5)`，初始 raw budget 为 `ChunkSize - breadcrumbTokens - reserve`；普通文本 reserve 为 0。
9. `FinalizeChunk` 在 Enricher 后重新构造并估算最终 SearchContent：先按实际剩余预算裁剪/丢弃 Enhancement，绝不为增强裁掉 RawContent；若 breadcrumb+raw 因估算误差仍超限，则确定性重新拆 raw。回答视图和搜索命中统一调用同一个 `chunktext.Answer`，不另存可能漂移的 AnswerText。
10. 最终 SearchContent 的 `EstimateTokens` 不得超过 `ChunkSize`；无任何“接近两倍 chunk size”的例外。
11. 原始完整表格/代码仍保存在 parsed artifact，chunk 只是可检索视图。

## 10. 表格与代码增强

Enricher 在 splitter 之后、`FinalizeChunk` 之前运行：

```go
type Enricher interface {
    Enrich(ctx context.Context, block EnrichableBlock, budget *TaskDocumentAIBudget) (Enhancement, error)
}
```

表格增强输出：

- 表格主题；
- 列含义；
- 关键实体、单位和范围；
- 适合自然语言检索的 1～3 句摘要；
- 不做原表中没有的推断。

代码增强输出：

- 语言；
- 主要职责；
- 输入/输出、副作用；
- 关键函数、类型、错误条件；
- 适合代码检索的符号和自然语言描述。

增强结果采用 JSON schema，缓存并设置独立的输出上限。调用时传入该 chunk 的 enhancement token/字符预算；模型输出超过预算时按 UTF-8/句子安全边界裁剪，预算为 0 时不调用。失败、超时或 TextModel 未配置时：

- RawContent 正常入库；
- Enhancement 为空；
- 文档记录 warning，但不标记 FAILED；
- 不重试整个文档。

回答 prompt 对 Enhancement 标注“语义辅助，可能有误，原文优先”，防止摘要覆盖原始表格/代码事实。

embedding 前还有第二道真实输入保护：使用 provider tokenizer（可用时）和 `maxSearchContentBytes` 校验。byte limit 按最终 UTF-8 bytes 而不是 rune 计数；默认 60 KiB，启动时必须验证不超过当前 Milvus `content` VarChar `maxLength=65535`。在没有显式 collection schema migration 前，管理员配置更大值必须启动失败，不能允许 embedding 成功后才在 Milvus insert 失败。超限仍先移除 Enhancement；若 raw 本身超 provider/Milvus 上限则返回 splitter 再拆，绝不静默截断原文。由此 `ChunkSize` 是应用 estimator 上限，而 provider/tokenizer/bytes 检查才是调用边界。

## 11. 索引、检索与版本切换

### 11.1 写入顺序

```text
1. claim 得到不可变 IndexFence 与 rag_document_versions snapshot
2. 读取并校验 ParsedArtifact；缓存未命中时生成瞬态 ParsedDocument，幂等保存 assets 后再发布 ParsedArtifact
3. Split + Enrich + Finalize + Embed
4. SQL 写入 fence.doc_version 的 rag_chunks / rag_chunk_assets
5. Milvus upsert fence.doc_version 的 SearchContent + vectors
6. 单一 SQL 事务：CAS 激活 documents.active_version、完成 version/index task，并为旧 active version 创建带宽限期的 GC task
7. 返回成功；GC 由独立 worker 执行
```

第 6 步由 `ActivateAndFinishRAGIndexTask` 在一个事务中验证 task id、doc version、claim generation、lease owner/expiry、RUNNING 状态以及 `document.version == fence.doc_version`。在此之前搜索仍过滤到旧 active version；之后只接受新 version。激活后主索引任务已经终态，cleanup 失败只重试 GC，不能重新执行 VLM/embed。

旧版本不立即删除。每一个被退休的 active version 都从自己的 `retired_at` 起独立保留 `gcGracePeriod`，该值必须大于最大检索请求时长加时钟/调度余量；即使期间又连续激活多个版本，也不能按“只留 current + previous”提前删除更早但仍在 grace 内的版本。GC/周期 sweep 只能按 `doc_id + exact doc_version` 删除 catalog/vector；GCED tombstone 留到文档删除并允许重复清扫迟到向量。搜索 hydrate 缺失且 active map 已变化时，整次请求最多重试一次。

### 11.2 Milvus

一期保留现有包含 `doc_id/doc_version` 的 collection schema：

- `content` 存 SearchContent；
- `vector.SearchQuery` 必须携带 SQL 查询开始时读取的 `doc_id → active_version` 快照；active map 为空时不访问 Milvus；
- 先按 doc_version、再按 canonical doc ID 排序，按 version 分组生成括号化 `(doc_version == N && doc_id in [...])`，最后用括号化 `OR` 合成唯一稳定 scalar predicate；把完全相同的字节串下推到 rewritten dense、HyDE dense、BM25 sparse 的每个 ANN request，单 dense Search 路径也必须设置 filter；
- filter 值只来自数据库中的 canonical doc ID/version，并通过单一严格 encoder 转义/限长；不得拼入 query 或其它用户文本；
- 一期利用现有 `maxDocsPerKB=200` 保证整张 active map 可放入一次 predicate；配置校验按最坏 canonical ID 长度确保表达式不超过 `maxMilvusFilterBytes`。不得把 MaxDocsPerKB 调到该安全容量以上；运行时超限/损坏 ID 返回可观测错误，不能退化成召回后过滤；
- 查询输出包含 `doc_version`，结果返回后再按同一 active map 做二次校验；SQL post-filter 是防御，不是版本隔离；
- `vector.SearchHit` 内部保留 SearchContent；
- vector store 增加 `DeleteDocVersion(ctx, kbID, docID string, version int64)`，Milvus/fake 都只删除相等版本；现有范围型 `DeleteOldVersions` 不得用于 delayed GC，并在调用点迁完后删除；
- 候选先根据 `doc_id + doc_version + chunk_index` 从 SQL 一次批量 hydrate raw/enhancement/SearchContent/location，再进入 reranker；
- 只有 `index_format_version=0` 的 pinned legacy hit 缺 catalog 时才允许使用 `chunktext.Body`，assets 为空；新格式缺 catalog 视为损坏并丢弃/告警。

这避免已有 Milvus collection 原地加字段，同时正式结束“任意增强文本靠前缀剥离”的脆弱协议。

### 11.3 Reranker

reranker 输入必须是候选级 SQL hydrate 得到的 `SearchContent`，不能再用 `SectionTitle + Content` 重新构造，否则表格/代码/图片增强只影响初召回而不影响精排。

只在最终 top-N 确定后第二次 hydrate `rag_chunk_assets + rag_assets`，并只返回 `display_status=ready` 的 AssetRef，避免给所有候选加载资源元数据或稳定返回 404。metadata 生成、source 和精确 chunk 查询都使用本次 SQL 快照的 active version，不能使用 document target `version`。

## 12. 回答模型与资源展示

### 12.1 不可变安全边界

知识库专用问答路径必须满足：

```text
provider.Message.Content      = 文本 source header + Hit.AnswerText()
provider.Message.ContentParts = nil
image_url                     = 不存在
asset URL / object key        = 不存在
base64 / image bytes          = 不存在
```

source header 只含经过转义/限长的文档名、完整 SectionTitle breadcrumb 和 page/slide/sheet location，因此标题仍作为文字上下文提供给回答模型，但不机械复制进 RawContent。检索资料整体放在明确标记的 untrusted data block 中，不能进入 system/developer role。

普通 Agent 聊天已有的“用户主动上传图片”能力保持不变，用户原始 `ContentParts` 可以继续发送给其选择的视觉模型；新的 RAG 逻辑绝不能因为 AssetRef/RAGResourceRef 追加 `ContentPart`、`image_url`、asset URL、对象键或 base64。换言之，边界针对 **RAG 生成的上下文和资源**，不是全局禁止用户图片。

文档图片在解析阶段已经被转写为 caption/OCR，因此文字回答模型可以依据这些文字回答。模型不得被告知“你看到了图片”；提示词应写成“资料中的图片说明由解析阶段生成”。

### 12.2 Hit / AssetRef

```go
type AssetRef struct {
    ID          string         `json:"id"`
    Kind        string         `json:"kind"`
    Caption     string         `json:"caption,omitempty"`
    PageNum     int            `json:"pageNum,omitempty"`
    Location    SourceLocation `json:"location"`
    Width       int            `json:"width,omitempty"`
    Height      int            `json:"height,omitempty"`
    MIMEType    string         `json:"mimeType,omitempty"`
}

type Hit struct {
    Content       string     `json:"content"` // 兼容字段：RawContent
    Enhancement   string     `json:"enhancement,omitempty"`
    Assets        []AssetRef `json:"assets,omitempty"`
    DocVersion    int64      `json:"-"`
    SearchContent string     `json:"-"`
}

func (h Hit) AnswerText() string { return chunktext.Answer(h.Content, h.Enhancement) }

type RAGResourceRef struct {
    Asset         AssetRef      `json:"asset"`
    KBID          string        `json:"kbId"`
    KBName        string        `json:"kbName"`
    DocID         string        `json:"docId"`
    DocName       string        `json:"docName"`
    ChunkIndex    int           `json:"chunkIndex"`
    SectionTitle  string        `json:"sectionTitle,omitempty"`
    SourceLocation SourceLocation `json:"sourceLocation"`
}
```

AssetRef 不含对象键和 URL。`Hit.Content` 始终保持 RawContent，`SearchContent` 仅供 embedding/BM25/reranker，`FinalizeChunk`、`buildRAGChatPrompt` 与 `FormatHits` 必须统一使用 `chunktext.Answer`/`Hit.AnswerText()`，防止 AnswerText 派生逻辑漂移，也防止检索文本前缀泄漏到 API 正文。`RAGResourceRef` 是跨知识库问答和 Agent gallery 的展示 DTO，补齐 KB、文档、chunk 与位置来源；资源 URL 不持久化，由前端按当前 surface 构造同源鉴权端点。

### 12.3 知识库问答

`POST /api/rag/kbs/{id}/chat` 继续返回 `answer + hits`。`hits[].assets` 会自然进入 `rag_chat_turns.sources` 快照。

前端：

- 答案下直接显示“相关图片（来自检索资料）”gallery；
- 从最终 hits 按 asset ID 去重，默认最多 6 张；
- 图片 caption 作为普通文本渲染，不当 Markdown；
- 点击显示安全展示图、文档名、页/slide/sheet；
- 原有引用详情中也保留每个 chunk 的资源；
- 文案不得暗示回答模型分析过原图。

### 12.4 Web Agent 聊天

当前 registry 的工具 handler 只返回字符串。为避免把资源标记暴露给模型，新增进程内 typed side-channel：

```go
type ResultMetadata map[string]json.RawMessage

type ToolResult struct {
    Text     string
    Metadata ResultMetadata
}

type ResultHandler func(context.Context, json.RawMessage) (ToolResult, error)

func (r *Registry) RegisterResult(name, description string, parameters any, fn ResultHandler)
func (r *Registry) RegisterResultFrom(name, description string, parameters any, fn ResultHandler, source ToolSource)
func (r *Registry) GetResultFunc(name string) ResultHandler
func (r *Registry) ExecuteResult(ctx context.Context, name, args string) (ToolResult, error)
```

现有 `Register/RegisterFrom` 把 `ToolFunc` 包装成 `ResultHandler`；现有 `GetFunc/Execute` 继续只投影 `ToolResult.Text`，并保持当前错误后缀行为。`registeredTool` 内部保存 raw `ResultHandler` 与 `ToolSource`，但 raw handler 不得逸出 registry：`GetResultFunc` 返回执行后经过 producer/source/key/shape/size validator 的 wrapper，`ExecuteResult` 使用同一 wrapper，所有外部调用路径语义一致。

SDK bridge 选定唯一实现：`toolAdapter.Call` 必须按 tool name 调用 `Registry.ExecuteResult`（或仅调用上述 validator-wrapped `GetResultFunc`），禁止构造/缓存 raw `ResultHandler`。它把 Text 写入 `sdktypes.ToolResult.Content`，把已经过 registry validator 的 metadata 写入私有 Go 类型 `sdkBridgeData` 后放入 SDK 现有的 `ToolResult.Data`。`executeToolsConcurrently` 按每个 SDK response/tool-call ID 取回并 type-assert 该 Data，复制到对应 `toolCallResult.Metadata`；Data 不序列化给模型、SSE 或持久层，只有聚合后的 assistant metadata 持久化。禁止全局 map、字符串 framing 或“直接执行 typed builtin”的第二分支；这样并发同名 rag_search 的 metadata 天然随各自 response 隔离。错误结果不聚合 metadata。

1. `SearchForAgent` 返回给模型的仍是 `FormatHits(hits)` 纯文本；
2. `rag_search` builtin 在 `Metadata["ragResources"]` 中携带序列化后的 `[]RAGResourceRef`；普通工具正文永远不会被扫描/解析成 metadata，长度前缀或 base64 framing 不构成防伪；
3. registry 按 tool source=`SourceBuiltin`、producer name=`rag_search`、key=`ragResources`、JSON shape、数量和字节上限校验 metadata；非 builtin、其它 tool name、未知 key 或超限值一律剥离并记录受限 warning，不得持久化或进入 UI；
4. `ResultMetadata` 的值保持 `json.RawMessage`，只在上述 producer-specific validator 中解码，不允许任意 `map[string]any` 穿过信任边界；
5. agent loop 在当前 turn 最终回答时，从本轮所有成功 `rag_search` tool result 聚合、按首次最终 hit 顺序去重并限制资源数量；
6. 聚合结果写入最终 assistant `provider.Message.Metadata["ragResources"]`；
7. 只有上一步聚合后的最终 assistant Metadata 才持久化并发给 Web UI；原始 tool-result Metadata 不持久化、不发 SSE，provider、compaction 和下一轮模型历史序列化也忽略 assistant Metadata；
8. regular/streaming、iteration-cap delivery/fallback、metadata-only event、steer continuation 的所有最终 assistant 分支都必须在 `sess.Append` 前附着同一 metadata，保证 SSE 与 history reload 一致；
9. Web chat-screen 在 assistant 答案下展示 gallery；非 Web channel 忽略 metadata，继续文字回答。

现有 sandbox prefix 只保留在该受信 builtin 的兼容 adapter 内，不能成为通用 metadata 通道。不能依赖模型复制 asset ID，也不能把 `rag-asset://` 标记放进工具正文。

## 13. 资源访问 API

新增：

```text
GET /api/rag/assets/{assetId}
GET /api/rag/assets/{assetId}/thumbnail
GET /api/agents/{agentId}/chat/{sessionId}/rag-assets/{assetId}
GET /api/agents/{agentId}/chat/{sessionId}/rag-assets/{assetId}/thumbnail
```

知识库问答/检索调试页使用前两个 owner 端点，鉴权链：

```text
identity → rag_asset.doc_id → rag_document.kb_id → rag_kb.user_id → owner/admin 校验
```

公共/共享 Agent 的聊天者通常不是 KB owner，必须使用 session-scoped 端点。该端点同时验证：请求者有权查看该 agent/session；session 确实属于路径中的 agent；该 session 中已经持久化的、由受信 `rag_search` typed result 生成的 assistant `ragResources` 精确引用了该 asset；引用保存了检索当时已授权的 agent/KB/doc 来源。不能只凭 asset ID 不可猜，也不能仅检查 agent 当前配置后允许浏览整个 KB。历史授权只扩展到该 session 已引用的具体资源。

两类 handler 都必须先完成身份、session/message 精确引用和 asset→doc→KB→user 当前状态校验，再处理 `If-None-Match`/ETag 或读取对象。doc/KB/user 任一级为 DELETING/已删除时立即按统一 404 策略拒绝，历史 metadata 不能覆盖墓碑；Agent session 或承载该引用的 assistant message 被删除/撤权后，精确 grant 同步失效。搜索和 task claim 使用相同墓碑规则。

响应要求：

- 流式读取对象存储，不整图读入 handler 内存；
- `Content-Type` 来自数据库白名单，不信任扩展名；
- `Content-Disposition: inline`；
- `Cache-Control: private, no-cache`，使删除/撤权后每次复用都必须重新鉴权；
- `ETag` 使用实际 display/thumbnail 派生字节 hash + renderer version，而不是 source hash；
- `X-Content-Type-Options: nosniff`；
- `Cross-Origin-Resource-Policy: same-origin`，不允许宽泛 CORS；cookie/session 使用项目统一的 Secure/HttpOnly/SameSite 策略；
- 支持条件请求；
- 不支持 range 的实现可先明确返回完整图；
- 403/404 不泄露 asset 是否属于其它用户。

SVG、EMF、WMF、PDF XObject 等不直接 inline。只有经过所选、许可可接受的 rasterizer 成功解码、像素限制、去元数据并重编码为 PNG/WebP 的资源才设置 `display_status=ready` 并进入 AssetRef；一期没有安全 decoder 的格式保留 source、记录 degraded，但不承诺展示。这里的“原图”指原文档中的视觉资源本身，而不是让前端执行原始不可信文件格式。

## 14. 任务、并发、进度与成本

### 14.1 Durable worker

昂贵 VLM 上线前先修复当前任务语义：

- task 绑定物理 fencing `doc_version`，reclaim 分配新版本；
- SQL 原子 claim + lease + `claim_generation` + heartbeat，所有写入 CAS；
- 每个实例有稳定 worker ID；
- 后台 poller 持续扫描 PENDING 和 lease 过期任务；
- task-level retry 复用 page/enrichment cache；
- stale task 不得覆盖更新版本的状态；
- heartbeat/CAS 失败立即取消本 worker context；lease 使用数据库时间；
- 可重试错误（sidecar/VLM 429/5xx、embedding/Milvus/对象存储临时故障）按 `next_run_at` 指数退避；损坏容器、格式/许可/硬限制/schema 永久不兼容直接失败；
- 全局 Vision/Enrichment semaphore 限制并发。
- advanced upload/reindex 入队事务同时限制每用户 pending 数与同文档最短重建间隔；管理员显式 override 也写审计，不能靠反复建 task 绕过周期配额。

### 14.2 进度

阶段枚举：

```text
queued
loading
parsing
vision
chunking
enriching
embedding
indexing
finalizing
done
failed
```

API 文档状态示例：

```json
{
  "status": "PROCESSING",
  "progress": {
    "stage": "vision",
    "current": 7,
    "total": 18,
    "unit": "pages",
    "message": "正在解析第 7/18 页"
  },
  "degraded": false,
  "warningCount": 0
}
```

进度最多每秒或阶段切换时落库一次，避免每个 block 写一次数据库。

前端必须覆盖上述全部 stage，包括 `loading` 与 `indexing`；遇到新/未知 stage 显示通用“处理中”，不能崩溃或误判完成。文档 DTO 同时返回 applied/target parse mode、`needsReparse` 和 `needsReindex`。

### 14.3 成本可观测性

pipeline 为每个逻辑 task 只创建一个 `TaskDocumentAIBudget`，并把同一指针注入 PDF Vision、Office ImageTranscriber、所有 repair/retry 和 Enricher；各组件禁止维护自己的“完整文档预算”。每次调用都经过 durable usage ledger 的 `Reserve → MarkSent → Commit/Release`，并在进程崩溃/lease reclaim 后恢复。文档预算和用户 UTC 日配额任一不足时不发请求，按降级矩阵保留原文/alt/native，并记录明确 warning。

DocumentAI HTTP client 还必须：只使用管理员配置且通过 scheme/host/private-network policy 的固定 endpoint，文档内容不能改变 URL；默认 HTTPS，私网/HTTP 只能管理员显式开启；禁用重定向并在连接前校验解析地址；限制压缩后/解压后响应 body、输出 token、JSON 深度、数组数量和每个 markdown/caption/OCR 字段长度。repair 与 provider-level retry 都先重新 reserve，不能成为免费旁路。

日志/指标至少记录：

- doc/task/retrieval ID；
- parse mode、parser version、fingerprint cache hit；
- native/VLM/降级页数；
- 图片数、忽略装饰图数；
- vision/enrichment 请求数、耗时、token/计费用量（provider 可提供时）；
- 文档预算消耗与因页数/图片数/增强块/token 上限而跳过的数量；
- sidecar、VLM、enricher 各阶段错误码；
- 最终 degraded warning 数；
- 绝不记录原始页面、caption 全文、API key 或图片字节。

## 15. 错误与降级矩阵

| 故障 | 结果 |
|---|---|
| PDF auto sidecar 不可用 | 整篇走新的 streaming/ReaderAt Go PDF 文本提取，DONE + degraded；无文本才 FAILED |
| 某 PDF 页 VLM 超时/429/5xx | 该页 native，warning；其它页继续 |
| VLM JSON 两次不合法 | 该页 native，warning |
| PDF 某页 native 与 VLM 都为空 | 跳过该页，warning；全篇空才 FAILED |
| Office sidecar 不可用/格式损坏 | FAILED，提示 MarkItDown 解析失败 |
| Office 某图片 VLM 失败 | 保留 alt/“图片未能识别”，其它文本继续，DONE + degraded |
| task/user 周期请求、token、费用或 pending/rate budget 耗尽 | 不再出网；后续图片保留原图+alt/中性占位，表格/代码只保留原文；DONE + degraded 或入队 429 |
| DocumentAI redirect/响应 bytes/JSON 深度/schema 超限 | 该页/图片/增强按 typed failure 降级；repair 也必须先占预算 |
| 缩略图生成失败 | 说明文字仍入库，该 asset 不展示，DONE + degraded |
| 表格/代码增强失败 | 原始 Markdown 正常入库，Enhancement 为空，warning |
| embedding/Milvus 失败 | 保留旧 active version，任务重试；新版本不激活 |
| 资源端点 403/404 | 回答和文字引用正常，前端资源卡显示不可用 |
| MD 含图片 | 不访问，替换成忽略提示，DONE + degraded |
| 页数/解压/像素超过硬限制 | 明确 FAILED，不静默截断结构 |

## 16. 安全

1. 上传扩展名、MIME magic 和实际容器格式三重校验。
2. OOXML zip 限制文件数、单项大小、总解压大小、压缩比和路径穿越。
3. sidecar 仅接受字节流和 allowlist format；不接受用户提供的文件路径、URL 或 URI。
4. sidecar 容器只读根文件系统、非 root、临时目录配额、CPU/内存限制、无对象存储/VLM 密钥；Compose 使用 `internal: true` 专用网络且主服务双网连接。Kubernetes 对 parser 分别设置 default-deny ingress/egress：ingress 只允许带 bkcrab workload selector 的 pod 访问 parser 端口，parser 不允许任何 egress；若命名空间还限制 bkcrab egress，则另加一条 bkcrab → parser 端口的 egress allow，不能用 parser egress 规则表达“谁可访问 parser”。
5. tar 响应解包校验相对路径、条目数、总字节和重复文件名。
6. PDF 渲染限制页数、DPI、像素、超时；图片解码防 decompression bomb。
7. 文档文字、OCR、图片说明、表格和代码均是不可信数据。VLM/ImageTranscriber/Enricher 使用无工具、无 agent 历史、无额外密钥的隔离 client，固定 system 指令和 JSON schema；资料只以 JSON-escaped、长度受限的 data block 进入 user/tool role，绝不拼入 system/developer 指令。知识库回答无工具；普通 Agent 的 rag_search 文本明确标为 untrusted retrieved data，不能作为调用有副作用工具、扩大路径/租户权限或跳过用户确认的授权依据。prompt 防护只能降低模型误从，绝不能替代工具层权限、typed metadata 和 side-effect confirmation。
8. Markdown sanitizer 禁止 user-authored internal asset scheme。
9. 图片输出去 EXIF、ICC 中非必要数据和动画帧；浏览器只接收安全 raster。
10. asset handler 必须做租户归属校验，不能仅凭不可猜 ID 当授权。
11. API/log 不返回对象键、临时目录、上游响应原文或密钥。
12. v1 DocumentAI client 只实现明确配置的 OpenAI-compatible 协议；页面/图片先 resize 并编码成受支持 JPEG/WebP，校验原始、编码后和 base64 膨胀后的请求字节上限，并为每次调用设置 timeout。
13. UI 在开启 auto 前披露文档页面/图片可能发送到第三方 DocumentAI；审计日志只记录 provider/model/计数和 usage，不记录内容。
14. UI 对 `enrichmentEnabled` 使用独立 opt-in，披露 standard/auto 的表格与代码都会发送给 TextModel；系统 gate、KB opt-in、审计和 capability 四处语义一致。
15. adversarial corpus 必须包含伪 system 指令、索要密钥、诱导工具调用、伪 `ragResources`、超深 JSON、恶意 OCR/caption/table/code；测试断言无权限提升、无 metadata 伪造、无隐式网络请求。模型输出本身不作为安全断言，授权由确定性代码验证。

## 17. 验收标准

### 17.1 功能黄金样本

至少包含：

- born-digital 纯文本 PDF；
- 扫描 PDF；
- 同页文字 + 流程图 PDF；
- 多列表格 PDF；
- 带 fenced code 的技术 PDF；
- 一页 VLM 失败但其它页成功的 PDF；
- 带 Heading、表格、重复图片的 DOCX；
- 带 group shape、图片、speaker text 的 PPTX；
- 带多 sheet、表格、cell-anchored image 的 XLSX；
- 含网络/相对/data URI/HTML 图片的 MD；
- 超长单元格、超长代码行、未闭合代码围栏；
- 同图多 occurrence；
- reindex 前后相同 asset ID。

### 17.2 必须断言

1. 所有 SearchContent 都不超过 `EstimateTokens` 的 ChunkSize 上限，并通过 provider/tokenizer 与字节级调用边界。
2. 表格分块重复表头，代码分块保持完整 fence。
3. 图片说明/OCR能通过 BM25、dense 和 reranker 命中。
4. 命中 chunk 只关联局部图片，不携带整个章节所有图片。
5. `buildRAGChatPrompt` 只有文字且专用 KB chat 没有 ContentParts；普通 Agent 不新增任何源自 RAG 的 ContentPart，同时保留用户主动上传的图片。
6. chat response、历史 turn 和 Web Agent assistant metadata 都能恢复同一 RAGResourceRef；公共 Agent 访客只能访问其 session 已引用的具体 asset。
7. reindex 后旧历史图片仍可访问；doc/KB/user 进入 DELETING 或 session/message grant 删除时，即使对象仍在也立即返回 404，且鉴权先于 304。
8. 跨用户 asset 请求不可访问，响应不泄漏存在性。
9. VLM/Enricher/sidecar 失败遵循降级矩阵。
10. 达到配置上传上限的文件不再由 Go parser 整体 `io.ReadAll`；sidecar health 与 per-extension capability 取值一致。
11. 多实例只能 claim 一次昂贵任务；lease 过期能恢复并获得新 doc_version，旧 worker 的迟到 Milvus 写不可见且不能覆盖新版本。
12. 最高分的未激活 staging vector 不会挤掉旧 active 命中；每条 ANN route 都有 active-version filter。
13. 旧 KB/旧 chat sources 可读取；只有 pinned legacy active version 可检索且 assets 为空，`active_version=0` 返回空。
14. capability 按 pdfAuto/officeVision 独立：PDF engine unavailable 不阻止 Office auto；暂不可用时仍允许 auto→standard，文档 `needsReparse/needsReindex` 与 active snapshot 一致。
15. 前端覆盖 loading/indexing/全部已知 stage，并安全显示未知 stage。
16. Python/Go 对 canonical manifest/health shared golden 解码一致；未知版本/字段/entry/hash 被拒绝。
17. Office DTD/entity/external relationship/local target 与四来源 Markdown adversarial corpus 不读取本地文件、不外联、不产生 raw-HTML XSS。
18. Vision/Office image/repair/Enricher 共享 task budget；lease reclaim 不重置，用户周期配额和 pending/reindex rate limit 生效。
19. enrichment 在 standard/auto 都只有系统 gate + KB opt-in 后出网；关闭时原表/代码正常检索。
20. 连续 1→2→3 的 delayed GC 精确删除单个 version，GCED tombstone 能清理 GC 后晚到向量。

## 18. 发布与迁移

存量 task 的首次迁移选择短维护窗口，不声称零停机兼容：先禁止 upload/reindex 并停止/缩容所有旧版本 indexing worker，等待超过旧 lease 上限且确认 heartbeat 不再变化；再由新版本执行 nullable expand → archive/backfill/validate → constraint contract，成功后只启动新 worker 并恢复写入口。旧 worker 未全部退出时禁止执行 backfill/contract。若未来要求滚动零停机，必须另设计双版本 task schema/worker 协议，本期不能靠启动顺序猜测兼容。

分阶段开启：

1. 先上线 schema、fenced task、canonical sidecar 协议、统一 sanitizer、artifact/asset 存储、durable delete/exact-version GC、durable usage ledger 和 capability API，三个 feature gate 均为 false。
2. Office 三格式安全预检/定位 golden 与 typed ImageTranscriber 通过后，可独立开启 Office/OfficeVision capability，不等待 PDF 许可证。
3. 依赖许可证 ADR 通过后上线 PDF auto 纵向闭环，仅内部管理员测试。
4. 多租户 asset/session 即时撤权、共享/周期成本上限、adversarial corpus 和格式 golden 通过后，才把对应高级开关开放给新 KB；现有 KB 仍 standard/enrichment disabled。
5. 表格/代码增强在独立系统 gate、KB opt-in/披露和共享预算完成后单独上线。
6. typed ToolResult 与 session-scoped 鉴权完成后，上线 Web Agent chat 资源 metadata。
7. 观察成本、降级率和解析质量后，再考虑把 auto 作为新 KB 默认值。

回滚：

- 关闭 advanced capability 后 standard RAG 继续工作；
- 已解析的 auto 文档仍可使用现有 active chunks 和 assets；
- sidecar/VLM 故障不影响普通检索；
- SQL 新字段/表保留，不做破坏性 down migration；
- 不自动批量 reindex 现有文档。

## 19. 被否决方案

### 19.1 把图片直接发给回答模型

否决。回答模型不一定支持视觉，成本和上下文协议显著复杂；当前需求只是用文字回答并展示相关原图。

### 19.2 让回答模型在答案里生成 `![...](assetId)`

否决。模型可能遗漏、篡改或引用未授权资源；展示必须由结构化 Hit/Metadata 确定。

### 19.3 Parser 只返回一段 Markdown string

否决。会丢失页/slide/sheet、bbox、warning、asset ID、缓存和降级信息。

### 19.4 直接使用 markitdown-ocr 的内联文字作为最终协议

否决。它适合作为实现参考，但不足以提供 bkcrab 所需的稳定原图、缩略图、资源鉴权、occurrence 与历史生命周期。

### 19.5 在 splitter 中同步调用 LLM

否决。会破坏确定性、测试性和重试成本边界；网络增强必须是独立阶段。

### 19.6 把 AssetRef JSON 塞进 Milvus `content`

否决。会污染 BM25、占用 VARCHAR、使正文剥离协议更脆弱，也无法安全服务对象。

### 19.7 把 metadata 编码进工具返回字符串

否决。长度前缀、特殊前缀和 base64 只能解决 framing，不能证明 producer 身份；不可信文档或工具输出可以伪造。RAG 资源只能来自 registry 内受信 builtin 的 typed `ToolResult.Metadata`。

## 20. 后续演进

本期完成后可以独立评估：

- 图片向量与图文混合召回；
- 公式/手写体专用识别；
- 用户可编辑图片说明和表格摘要；
- parser/enricher 模型的用户级覆盖；
- 文档解析结果预览与手工纠错；
- 外部 channel 的安全媒体发送；
- 按引用计数保留已删除文档的历史资源；
- 多语种 caption 与 query-time 翻译；
- PDF 高精度版面模型替换启发式 auto router。

## 21. 预计代码落点

```text
internal/config/config.go                 RAG DocumentAI/sidecar/limits
internal/config/env.go                    BKCRAB_RAG_* 环境变量
internal/store/rag.go                     KB/document/task/chunk/asset CRUD
internal/store/database.go                SQLite/PostgreSQL migrations
internal/store/database_mysql.go          MySQL migrations
internal/rag/document/                    transient ParsedDocument / canonical ParsedArtifact / Asset
internal/rag/parse/                       Parser facade + MD/TXT + PDF auto
internal/rag/parse/sidecar/               rag-parser client / tar validation
internal/rag/vision/                      VLM schema client / cache
internal/rag/enrich/                      table/code enricher
internal/rag/split/                       Markdown AST splitter
internal/rag/assets/                      persistence / thumbnail / auth lookup
internal/rag/pipeline.go                  versioned orchestration
internal/rag/vector/                      doc_version + internal SearchContent
internal/rag/search.go                    catalog hydration + AssetRef
internal/rag/agent_adapter.go             tool text + out-of-band metadata
internal/agent/tools/                     typed ToolResult + trusted metadata
internal/agent/loop.go                    aggregate ragResources to assistant
internal/setup/handlers_rag.go            capabilities / progress / KB chat APIs
internal/setup/handlers_rag_assets.go     owner/admin KB asset auth
internal/setup/handlers_agent_rag_assets.go agent-session exact-ref asset auth
internal/users/account.go                 injected durable RAG user cleaner
web/src/lib/api.ts                        parseMode/progress/asset types
web/src/app/knowledge/page.tsx            capability-driven upload/progress
web/src/app/knowledge/chat/               resource gallery
web/src/components/chat-screen.tsx        Agent answer resource gallery
services/rag-parser/app/pdf_engine.py     license-approved Python PDFEngine adapter
services/rag-parser/                      Python MarkItDown + approved PDFEngine sidecar
deploy/docker/docker-compose.rag.yml      sidecar service/config
deploy/k8s/ + deploy/helm/                required egress/security deployment
```

## 22. 参考

- [Microsoft MarkItDown](https://github.com/microsoft/markitdown)：Office/PDF 转 Markdown；官方说明其目标是面向 LLM 的结构化 Markdown，而非高保真展示。
- [MarkItDown OCR plugin](https://github.com/microsoft/markitdown/tree/main/packages/markitdown-ocr)：图片 OCR 与上下文占位实现参考。
- [MarkItDown security considerations](https://github.com/microsoft/markitdown#security-considerations)：服务端应限制输入，并优先使用 `convert_stream()` 等窄接口。
- [PyMuPDF license and copyright](https://pymupdf.readthedocs.io/en/latest/about.html#license-and-copyright)：官方说明为 AGPL/商业双许可证；PDF engine ADR 的依据。
- [Milvus multi-vector/hybrid search](https://blog.milvus.io/docs/multi-vector-search.md)：后续图片向量扩展参考；本期不启用图片向量。
