# 文档解析评测平台设计

日期：2026-08-31
状态：设计定稿，等待规格确认

## 1. 背景

现有 RAG 测评平台主要使用已经整理成文本的 benchmark 数据，能够评价检索、回答和工程指标，但没有覆盖“用户原始文档进入系统后是否被正确解析”这一环节。公开 RAG 数据集大多也只提供文本，无法作为解析器评测真值。

bkcrab 已经部署协议兼容的 MarkItDown 与 Anydoc Office parser sidecar，并且生产 RAG pipeline 会在解析后继续执行结构归一化、分块、Embedding、索引和检索。本设计增加一套与现有 RAG 测评平台分离的文档解析评测平台，只执行到分块前的最终标准化 Markdown，不运行任何后续 RAG 流程。

评测方法参考 [firecrawl/anydoc benchmark](https://github.com/firecrawl/anydoc/tree/main/bench)：将原文档渲染为页面图作为视觉真值，使用多模态模型对两个匿名 Markdown 结果进行位置交换盲评，同时记录解析延迟和确定性结构统计。

## 2. 目标与非目标

### 2.1 目标

1. 提供独立的超级管理员“解析测评”前端和 API，不与旧 RAG 测评的数据集、运行模式和页面混用。
2. 接受超级管理员上传的 `.docx`、`.pptx`、`.xlsx` 文件；每个文件固定分别使用 MarkItDown 和 Anydoc 解析一次。
3. 评测对象是 bkcrab parser wrapper 完成结构整理和 Markdown 规范化后的最终解析产物，边界位于 chunker 之前。
4. 同时记录 sidecar 内部解析耗时与 Go 调用端端到端耗时。
5. 以原文档前 6 个渲染页作为视觉真值，对两个解析结果执行双位置盲评，产生完整性、结构、格式忠实度、清洁度四维分数和相对胜负。
6. 支持一个任务上传 1–50 个文件，展示逐文件结果以及按格式、按解析器和整个批次的汇总。
7. 保存原文、真值页、完整 Markdown、解析统计和两次裁判证据，支持超级管理员诊断和下载。
8. 文件级失败不破坏其他文件；任务可以取消，并能只重试失败或缺失的步骤。
9. 原文和评测证据默认保留 90 天，支持提前删除。

### 2.2 非目标

- 不支持 `.pdf`、旧版二进制 Office、OpenDocument、CSV、EPUB 或其他格式。
- 不运行 DocumentAI、图片转写、内容增强、分块、Embedding、Milvus、检索、rerank 或回答生成。
- 不创建可复用 benchmark 数据集，不导入开源 benchmark，不提供生产参数发布功能。
- 不向普通管理员或普通用户开放，不做用户级任务共享和租户协作。
- 不增加第三个解析器，不提供自定义解析器插件。
- 不对超过前 6 页或每个输出前 40,000 个 Unicode 字符的内容声称完成质量评分。
- 不把确定性结构统计混入主质量分。
- 不新增独立的 parser evaluator Python 服务，也不扩展旧 `rag-evaluator` 的 Ragas 协议。
- 一期不保存 parser bundle 内的独立二进制图片和附件；完整 Markdown 中的资源标记仍然保留。

## 3. 已确认的产品决策

| 决策点 | 结论 |
|---|---|
| 入口与权限 | 独立 `/admin/parser-evals/`，仅超级管理员 |
| 格式 | 仅 `.docx`、`.pptx`、`.xlsx` |
| 解析器 | 每个文件固定 MarkItDown 与 Anydoc 各一次 |
| 解析结果边界 | parser wrapper 的最终标准化产物，chunker 之前 |
| 质量真值 | LibreOffice 渲染的原文档页面图 |
| 评分覆盖 | 前 6 个渲染页；每个 Markdown 前 40,000 字符 |
| 评分方式 | 四维绝对分 + A/B、B/A 双位置相对胜负 |
| 裁判模型 | 每次任务由超级管理员从支持图片输入的已配置模型中选择 |
| 延迟 | sidecar 解析耗时为主，端到端耗时为辅 |
| 任务粒度 | 1–50 个文件的批量任务，逐文件容错 |
| 证据 | 保存原文、真值图、完整 Markdown、结构统计、两份判决 |
| 保留期 | 默认 90 天，可提前删除 |
| 数据模型 | 两张专用表，大对象进入对象存储 |
| 运行隔离 | 专用 parser 容器实例，全局串行执行解析 |

## 4. 总体架构

```text
Super Admin Web /admin/parser-evals
                 |
                 v
Parser Eval Admin API -------- SQL: parser_eval_runs
        |                           parser_eval_documents
        |
        +---------------------- Object Store: parser-eval/runs/...
        |
        v
Durable Parser Eval Runner
        |
        +--> parser-eval-renderer (LibreOffice + PyMuPDF)
        +--> parser-eval-markitdown (现有 rag-parser 镜像的专用实例)
        +--> parser-eval-anydoc    (现有 rag-parser 镜像的专用实例)
        +--> bkcrab multimodal provider boundary
```

### 4.1 组件职责

**Parser Eval Admin API**

- 执行超级管理员鉴权；
- 创建草稿任务，接收逐文件上传；
- 校验文件和裁判模型选择；
- 启动、取消、重试和删除任务；
- 返回批次、逐文件结果和受保护 artifact。

**Parser Eval Runner**

- 使用 SQL 租约、心跳和 fence token 恢复任务；
- 依次处理真值渲染、双解析、双判决和汇总；
- 只补齐失败或缺失的 JSON 槽位；
- 将大产物写入对象存储，将状态和摘要写入两张 SQL 表；
- 执行保留期清理。

**Renderer sidecar**

- 接收一个已校验的 Office 文件；
- 使用 LibreOffice headless 转为 PDF；
- 使用 PyMuPDF 以 100 DPI 栅格化前 6 页；
- 返回带有 manifest 与 PNG 的受限 tar bundle；
- 不持有 SQL、对象存储或模型凭证。

**专用 parser sidecar**

- 使用与生产 parser 相同的镜像、协议、版本与资源限制；
- 分别固定 `markitdown` 和 `anydoc` engine；
- 使用独立容器实例，避免评测流量影响生产解析，并避免生产流量污染延迟；
- 继续返回现有 `rag-parser/v2` bundle。

**多模态裁判**

- 通过 Go 主进程现有 provider/凭证边界调用；
- 不把 provider 凭证交给 renderer 或 parser sidecar；
- 同一任务的所有判决使用创建时选择并在启动时冻结的同一模型绑定。

## 5. 任务工作流

### 5.1 创建和上传

1. 前端获取 capabilities，包括服务健康、上传限制和可选视觉模型。
2. 超级管理员选择裁判模型并创建 `DRAFT` 任务。
3. 前端逐文件调用上传接口；同一个 `Idempotency-Key` 必须返回同一文档记录。
4. 服务端流式计算 SHA-256，不把整个批次放入内存。
5. 服务端验证扩展名、MIME、OOXML ZIP 结构、单文件限制和批次总字节限制。
6. 原件写入任务专用对象前缀；上传失败只影响当前文件。
7. `DRAFT` 阶段允许删除文件；任务启动后文件清单不可变。

### 5.2 启动和执行快照

启动任务时验证：

- 文件数在 1–50 之间；
- 每个文件都已完成上传与校验；
- renderer、MarkItDown 和 Anydoc capability 健康且版本受支持；
- 所选模型仍可解析为支持图片输入的模型绑定。

`execution_snapshot_json` 至少冻结：

- MarkItDown parser、converter 和 wrapper 版本；
- Anydoc parser、converter 和 wrapper 版本；
- renderer、LibreOffice 和 PyMuPDF 版本；
- 渲染 DPI `100`、最大页面数 `6`；
- Markdown 裁判字符上限 `40000`；
- 裁判 provider、模型名、模型绑定指纹和创建者身份；
- judge prompt 版本 `parser-eval-judge-v1`；
- 应用构建版本和任务的 parser 执行并发值 `1`。

启动后任务从 `DRAFT` 原子转换为 `QUEUED`。模型配置或 parser 版本变化不得静默改变已启动任务；绑定不可用时记录错误，不切换到其他模型。

### 5.3 每个文档的执行顺序

1. 真值渲染；失败不阻止双解析，只会使质量评分不可执行。
2. 根据文档 SHA-256 的固定奇偶规则决定 `markitdown -> anydoc` 或 `anydoc -> markitdown`。
3. 两个 parser 对同一文件相邻且串行执行；一期不并发跑两个 parser。
4. parser bundle 被现有 parse 层转换为 `ParsedDocument`，不设置 Page/Image Transcriber，不调用 DocumentAI 或 enrichment。
5. 使用与生产 artifact 相同的 canonical unit join 规则生成完整 `NormalizedMarkdown`。实现时应将该 join 规则提取成 `document` 包共享 helper，避免评测与生产行为漂移。
6. 保存 Markdown、解析版本、警告、结构统计和延迟。
7. 当真值和两个 Markdown 均成功时执行两次裁判。
8. 更新文档 JSON 槽位；最后重新计算批次汇总。

### 5.4 串行与运行隔离

一期只启动一个 parser-evaluation worker，并一次处理一个文档的一个 parser 调用。Docker 单机部署中 API 与 worker 可以在同一 Go 进程。多 pod 部署只能在一个 pod 启用 `parserEvaluation.workerEnabled`，其他 pod 只提供 API；这样无需为全局解析信号量增加第三张协调表。

## 6. 延迟定义

每个 parser 结果记录两个可空延迟：

### 6.1 `parseDurationMs`

主对比指标。起点是 sidecar 已经接收、保存并校验完整原文件之后；终点是 parser worker、wrapper 后处理和 bundle 校验全部完成，结果可以开始流式返回之前。

该指标：

- 包含隔离 worker 启动；
- 包含 converter 执行、wrapper 结构处理和 bundle 构建；
- 不包含上传文件到 sidecar；
- 不包含 tar 响应下载和 Go 解码；
- 不包含排队、真值渲染和 LLM 评分。

现有 `/v1/office/convert` 响应增加受限整数头 `X-BkCrab-Parse-Duration-Ms`。`rag-parser/v2` manifest 不变。Go sidecar client 验证该头为非负且不超过请求超时；缺失或非法头使本次评测缺少主延迟，但不破坏 bundle 的生产兼容性。

### 6.2 `endToEndDurationMs`

辅助诊断指标。Go Runner 在调用 parser client 前开始计时，在完整 tar 下载、校验并解码为 bundle handle 后停止。

解析失败时仍可记录端到端失败耗时；如果 sidecar 没有成功响应，就不伪造 `parseDurationMs`。

### 6.3 汇总

对每个 engine 和每个格式分别计算：

- 成功样本数与成功率；
- 中位数；
- nearest-rank P95。

所有汇总都显示样本数，缺失延迟不补零。

## 7. 真值渲染协议

Renderer 使用独立协议 `parser-eval-renderer/v1`。

### 7.1 健康检查

`GET /healthz` 返回一个封闭对象，字段契约如下：

```text
protocolVersion      string，固定为 parser-eval-renderer/v1
serviceVersion       非空构建版本
libreOfficeVersion   非空的实际 LibreOffice 完整版本
pyMuPDFVersion       非空的实际 PyMuPDF 完整版本
formats              固定为 [docx, pptx, xlsx]
maxInputBytes         正整数，初始部署为 52428800
maxPages              固定为 6
dpi                   固定为 100
```

镜像构建必须锁定基础镜像 digest、LibreOffice 包来源和 Python lockfile；健康响应报告镜像中实际安装的完整版本。Go 客户端从同一构建配置生成允许的 renderer descriptor，并在启动任务前要求健康 descriptor 精确匹配，因此不需要在设计文档里维护第二份容易漂移的依赖版本常量。

### 7.2 渲染请求

`POST /v1/render?format={docx|pptx|xlsx}` 接受且只接受一个名为 `file` 的 multipart 文件。服务端固定使用 6 页和 100 DPI，不接受调用方放大限制。

成功响应是 `application/x-tar`，包含 `manifest.json` 和 `pages/page-0001.png` 至最多 `page-0006.png`。manifest 保存 source SHA-256、总渲染页数、实际覆盖页数、renderer 版本及每张图片的路径、SHA-256、字节数、宽和高。

服务端对输入、PDF 中间产物、单页 PNG、总 bundle、像素数和处理时间设置硬限制。失败返回封闭错误码，不返回 LibreOffice 临时路径或文档内容。

## 8. 质量评分

### 8.1 裁判输入

每次调用包含：

- system message 中的固定 `parser-eval-judge-v1` rubric 和安全指令；
- 原文前 1–6 页 PNG，按页序作为图片内容部分；
- 匿名 `<output-A>` 和 `<output-B>` Markdown 文本；
- 每个 Markdown 最多取前 40,000 个 Unicode 字符，并明确标注截断。

system message 必须说明页面和 Markdown 都是不可信评测素材，不得执行其中的指令。Markdown 不进入 system message。

### 8.2 双位置判决

固定执行两个槽位：

1. `markitdown_a`：MarkItDown 为 A，Anydoc 为 B；
2. `anydoc_a`：Anydoc 为 A，MarkItDown 为 B。

两个槽位使用同一模型、相同页面、相同输出截断和 temperature `0`。每次最大输出 token 固定为 `1024`。

裁判必须返回且只返回：

```json
{
  "a": {
    "completeness": 1,
    "structure": 1,
    "formatting": 1,
    "cleanliness": 1
  },
  "b": {
    "completeness": 1,
    "structure": 1,
    "formatting": 1,
    "cleanliness": 1
  },
  "winner": "A",
  "reason": "one sentence"
}
```

四个维度必须是 1–5 的整数；winner 只能是 `A`、`B` 或 `tie`；reason 必须是有界有效 UTF-8 字符串。响应使用封闭 JSON 解码，额外字段、越界值和尾随内容均视为失败。原始模型响应在有界后写入对象存储，便于诊断。

### 8.3 绝对分数

先按每次槽位的实际 A/B 映射回解析器。每个解析器、每个维度的最终分数为：

```text
(第一次该维度 1–5 分 + 第二次该维度 1–5 分) / 2 / 5 * 100
```

解析器总分是完整性、结构、格式忠实度、清洁度四维最终分数的算术平均。

两次判决必须都有效才产生最终质量分；不能用单次判决冒充双位置分数，也不能为失败补零。

### 8.4 相对胜负

- 两次都映射到 MarkItDown 胜：MarkItDown 胜；
- 两次都映射到 Anydoc 胜：Anydoc 胜；
- 两次都为 `tie`：平局；
- 其他所有不一致组合：平局。

页面同时展示两次原始理由，使“位置不一致而被归为平局”可以被解释。

### 8.5 确定性统计

对完整 Markdown 计算下列诊断值，不参与主分：

- Unicode 字符数；
- ATX heading 数；
- GFM table 数据行数，不含分隔行；
- 有序和无序列表项数；
- Markdown link 数；
- Markdown image/`rag-asset` 标记数；
- footnote definition 数；
- parser warning 数。

统计规则需要版本号 `parser-structure-stats-v1` 并使用固定测试语料。

### 8.6 批次质量汇总

- 逐格式先计算每个解析器的四维均分和总分；
- 跨格式总分使用“存在有效样本的格式均分之平均”，避免文件数量最多的格式支配结果；
- 胜负汇总显示 MarkItDown 胜、Anydoc 胜、平局和评分失败数量；
- 每项显示有效样本数，缺失结果不补零；
- token 与估算费用从两次调用的 provider usage 汇总；模型规格没有价格时费用为 `null`，不得显示为零费用。

## 9. 最小数据模型

一期只新增两张表。完整 Markdown、PNG 和裁判原始响应不进入 SQL。

### 9.1 `parser_eval_runs`

主要字段：

```text
id                       primary key
status                   DRAFT/QUEUED/RUNNING/SUCCEEDED/PARTIAL/FAILED/CANCELLED
stage                    UPLOADING/VALIDATING/RENDERING/PARSING/SCORING/AGGREGATING
progress_json            有界封闭进度对象
execution_snapshot_json  启动后不可变
summary_json             当前汇总，失败时允许部分存在
created_by               超级管理员用户 ID
error_code               有界稳定错误码
error_message            有界安全错误信息
created_at
updated_at
started_at               nullable
finished_at              nullable
expires_at               默认 created_at + 90 天
lease_owner
lease_until              nullable
fence_token
cancel_requested_at      nullable
```

### 9.2 `parser_eval_documents`

主要字段：

```text
id                       primary key
run_id
ordinal                  任务内稳定顺序，UNIQUE(run_id, ordinal)
file_name
format                   docx/pptx/xlsx
media_type
size_bytes
sha256
source_object_key
status                   UPLOADED/RUNNING/SUCCEEDED/PARTIAL/FAILED
stage                    VALIDATING/RENDERING/PARSING/SCORING
truth_json
markitdown_result_json
anydoc_result_json
judge_result_json
error_code
error_message
created_at
updated_at
```

JSON 槽位必须有闭合的 Go DTO 和大小上限，不接受任意 map 作为持久化协议。

`truth_json` 保存 renderer 版本、总页数、覆盖页数、页面对象键、渲染耗时和错误。parser result JSON 保存状态、parser descriptor、执行顺序、两类延迟、Markdown 对象键/SHA-256/字节数、结构统计、warning 数和错误。judge result JSON 保存两个槽位、最终四维分、总分、胜负、usage、费用和错误。

不保存 attempt 历史。自动重试或人工“重试失败项”覆盖最后一次失败字段；已经成功的槽位不可覆盖。

## 10. 对象存储布局

对象键由服务端生成，不包含用户提供的路径：

```text
parser-eval/runs/{runID}/documents/{documentID}/source.bin
parser-eval/runs/{runID}/documents/{documentID}/truth/page-0001.png
parser-eval/runs/{runID}/documents/{documentID}/truth/page-0002.png
parser-eval/runs/{runID}/documents/{documentID}/outputs/markitdown.md
parser-eval/runs/{runID}/documents/{documentID}/outputs/anydoc.md
parser-eval/runs/{runID}/documents/{documentID}/judge/markitdown-a.json
parser-eval/runs/{runID}/documents/{documentID}/judge/anydoc-a.json
```

数据库保存原始文件名供展示和下载响应使用。下载时使用安全的 `Content-Disposition` 编码，不把文件名拼入对象键或本地临时路径。

## 11. 状态、失败与重试

### 11.1 任务状态

```text
DRAFT -> QUEUED -> RUNNING -> SUCCEEDED
                            -> PARTIAL
                            -> FAILED
                            -> CANCELLED
```

显式点击“重试失败项”是唯一的反向边：`PARTIAL/FAILED -> QUEUED`。该转换不修改执行快照和成功槽位。

- `SUCCEEDED`：每个文件双解析和双判决全部成功；
- `PARTIAL`：至少存在一个可用解析或质量结果，但并非所有文档完整成功；
- `FAILED`：没有任何文档形成可用双解析对比；
- `CANCELLED`：收到取消请求且 worker 已停止安排后续步骤。

### 11.2 文档状态

```text
UPLOADED -> RUNNING -> SUCCEEDED/PARTIAL/FAILED
```

文档阶段用于进度展示，不增加更多中间状态。

### 11.3 失败规则

- 真值渲染失败：继续双解析，记录延迟，评分跳过，文档为 `PARTIAL`；
- 一个 parser 失败：保留另一个结果，不做双路评分；
- 一个 judge 槽位失败：保留双解析结果，不生成最终质量分；
- provider 绑定失效：不自动切换模型；
- 确定性错误（损坏、格式错误、资源超限）不自动重试；
- 网络、超时前的连接中断和可恢复上游错误最多自动重试两次。

取消应通过 context 尽快传递给 renderer、parser 和 provider，但只在步骤边界保证可中止。已完成 artifact 和结果保留。

### 11.4 人工重试

`POST .../retry` 只允许终态 `PARTIAL` 或 `FAILED`。它清除失败或缺失槽位的终态标记，将任务重新排队，并保留所有成功槽位。任务的文件列表、执行快照、裁判模型和成功结果不得变化。

## 12. API 契约

所有路径仅超级管理员可访问：

```text
GET    /api/admin/parser-evals/capabilities
GET    /api/admin/parser-evals/runs
POST   /api/admin/parser-evals/runs
GET    /api/admin/parser-evals/runs/{runId}
POST   /api/admin/parser-evals/runs/{runId}/documents
DELETE /api/admin/parser-evals/runs/{runId}/documents/{documentId}
POST   /api/admin/parser-evals/runs/{runId}/start
POST   /api/admin/parser-evals/runs/{runId}/cancel
POST   /api/admin/parser-evals/runs/{runId}/retry
DELETE /api/admin/parser-evals/runs/{runId}
GET    /api/admin/parser-evals/runs/{runId}/documents/{documentId}/artifacts/{kind}
```

### 12.1 Capabilities

返回：

- 功能是否启用及不可用原因；
- renderer 和两个 parser 的缓存健康快照与版本；
- 支持格式；
- 最大文件数、单文件字节和批次总字节；
- 固定评分覆盖；
- 当前超级管理员可使用且支持图片输入的模型绑定列表。

请求路径不得同步探测 sidecar；健康由后台探测缓存提供。

### 12.2 创建、上传和开始

- `POST /runs` 接受 `judgeModelBindingId`，创建 `DRAFT`；
- `POST /documents` 接受一个 multipart 文件；
- `DELETE /documents/{id}` 仅在 `DRAFT` 可用；
- `POST /start` 验证全部条件并原子进入 `QUEUED`。

创建、上传、开始、取消和重试支持 8–128 字符的 `Idempotency-Key`。同一 actor、资源和 key 必须返回相同结果。

### 12.3 查询与 artifact

任务最多 50 个文件，因此 `GET /runs/{id}` 一次返回 run、documents 和 summary，不增加文档分页接口。运行列表使用与现有后台一致的 cursor/limit。

Artifact `kind` 是封闭枚举：`source`、`truth-page`、`markdown`、`judge-raw`。附加参数分别只能是有界 `page`、`engine` 或 `order`。服务端从数据库 DTO 查找对象键，禁止把 URL 路径或查询值直接转换成对象存储路径。

### 12.4 删除

活动任务必须先取消。删除顺序是：删除任务对象前缀成功后，再在事务中删除 documents 和 run。对象删除失败时保留 SQL 记录并返回可重试错误，避免数据库消失后遗留不可追踪原文。

## 13. 前端设计

### 13.1 路由和导航

- `/admin/parser-evals/`：新建任务与历史任务列表；
- `/admin/parser-evals/[id]/`：任务进度和结果详情。

侧边栏在“RAG 测评”旁新增“解析测评”。仅复用通用 UI 基础组件，不复用旧 RAG 测评的 dataset/run 业务状态。

### 13.2 新建任务

- 支持拖拽或选择 `.docx/.pptx/.xlsx`；
- 最多 50 个文件；
- 显示逐文件校验、上传进度、失败重试和移除；
- 必须选择一个 capabilities 返回的视觉模型；
- 明确显示“质量评分覆盖前 6 个渲染页和每个 Markdown 前 40,000 字符；延迟覆盖完整解析”；
- 全部上传成功后才能开始。

### 13.3 任务列表

展示创建时间、文件数、裁判模型、状态、阶段、进度、两个解析器的中位延迟、总分和胜负概况。操作包含查看、取消、重试失败项和删除。

### 13.4 详情页

顶部批次摘要展示：

- 两个 parser 的 `parseDurationMs` 和 `endToEndDurationMs` 中位数、P95、成功率；
- 四维分和总分；
- 胜负数量；
- `.docx/.pptx/.xlsx` 分格式表；
- token、可用时的估算费用和失败数。

文件列表展示双解析状态、两类延迟、双方总分、胜负和失败原因。

“查看证据”抽屉展示：

- 前 6 页真值图；
- MarkItDown 与 Anydoc 的安全 Markdown 并排预览；
- 原始 Markdown；
- 四维分、结构统计和 parser warnings；
- 两次位置交换判决与理由；
- 原文和两个 Markdown 下载。

Markdown 预览复用/扩展现有安全渲染策略：禁止 raw HTML、脚本、远程图片和危险 scheme；`rag-asset` 标记显示为不可交互占位。前端不接收对象键。

### 13.5 轮询

运行中页面轮询 `GET /runs/{id}`。终态停止轮询；后台标签页使用较慢间隔。WebSocket/SSE 不在一期范围内。

## 14. 配置与 Docker 部署

新增顶层 `parserEvaluation` 配置块，包含：

- `enabled`；
- `workerEnabled`；
- renderer、MarkItDown、Anydoc 内部 endpoint；
- 三类调用超时；
- 单文件最大字节；
- 批次最大文件数，默认且最大为 `50`；
- 批次最大总字节，默认 `500 MiB`；
- 保留期，默认 `90` 天；
- 固定 `maxPages=6`、`renderDPI=100`、`markdownJudgeChars=40000`。

单文件默认上限与现有 parser sidecar 的有效最小输入限制一致，capabilities 返回实际值。固定评测方法参数只能通过部署配置变更；每次任务仍写入快照。

Docker Compose 增加 `parser-evaluation` profile：

- `parser-eval-markitdown`；
- `parser-eval-anydoc`；
- `parser-eval-renderer`。

三个容器只加入内部网络，不发布宿主机端口。parser 两个实例复用现有 rag-parser image。renderer 使用固定依赖版本的新 Dockerfile。

renderer 与 parser 容器必须：

- 非 root；
- `read_only: true`；
- request 临时目录使用有大小限制的 tmpfs；
- drop Linux capabilities；
- 设置 CPU、内存、PID 和超时限制；
- 不挂载对象存储凭证；
- 不允许公网访问。

## 15. 安全与隐私

1. API 与 artifact 每次请求都执行超级管理员鉴权。
2. 上传同时校验扩展名、MIME、OOXML 结构、大小和 SHA-256。
3. ZIP entry 数量、单 entry、解压总量和压缩比都受限，防止 ZIP bomb。
4. LibreOffice 每次请求使用独立 profile 和请求目录；容器无网络，不能加载外部链接。
5. 文件名只用于有界元数据和下载头，不用于对象键或本地路径。
6. 对象键、临时路径、文档正文、Markdown、页面图和模型输入不进入普通日志或 telemetry。
7. 日志只记录任务 ID、文档 ID、格式、阶段、版本、计数、耗时、状态和封闭错误码。
8. Judge system message 把源页面和 Markdown 定义为不可信数据；文档中的指令不能改变 rubric 或输出协议。
9. 原始 judge 响应有字节上限，JSON 解析有深度、字段和字符串长度限制。
10. Artifact 响应设置正确 Content-Type、`nosniff`、安全 Content-Disposition 和禁止缓存的敏感响应策略。

## 16. 保留与清理

每个 run 创建时设置 `expires_at = created_at + 90 days`。清理 worker 定期扫描过期终态任务：

1. 删除整个任务对象前缀；
2. 对象删除成功后在事务中删除 documents 和 run；
3. 失败时保留 SQL 并在下次清理重试。

活动任务超过运行超时后由租约恢复逻辑转为可继续或终态失败；清理器不删除活动任务。普通应用日志遵循现有日志保留策略，但不得包含文档内容。

## 17. 可观测性

增加低基数事件/指标：

- run created/started/finished/cancelled/deleted；
- document render/parse/judge outcome；
- 按 engine、format、outcome 的 parse 和 end-to-end duration；
- renderer duration 和页面数；
- judge duration、token 和 error code；
- cleanup outcome。

不把 run ID、document ID、文件名、模型输入或对象键作为 metrics label。它们只在有访问控制的结构化日志中以允许字段出现，文件名仍禁止记录。

## 18. 测试策略

### 18.1 Go 单元测试

- 两张表的 SQLite/MySQL migration 与 CRUD；
- 状态转换、租约、fence、取消和恢复；
- 上传校验、批次限制和幂等；
- A/B 映射、位置不一致平局和 0–100 公式；
- 中位数、nearest-rank P95、按格式和跨格式 macro average；
- 封闭 judge JSON、截断边界和不可信内容 prompt；
- artifact kind/engine/page 验证和路径穿越拒绝；
- 90 天清理顺序。

### 18.2 Renderer 测试

- 最小 `.docx`、`.pptx`、`.xlsx` fixture；
- 多于 6 页时只返回前 6 页且报告总页数；
- 损坏 ZIP、伪扩展名、ZIP bomb、超时、像素和 bundle 限制；
- manifest 与每张 PNG 的 hash/尺寸一致；
- 请求临时目录在成功、失败和取消后都被清理。

### 18.3 Parser 与 provider 集成测试

- 同一 fixture 分别通过两个专用 endpoint；
- 最终输出使用生产相同的 canonical Markdown join；
- 未调用 Page/Image Transcriber 和 enrichment；
- parse duration header 与 end-to-end duration 被正确区分；
- fake vision provider 覆盖 A/B、B/A、平局、非法 JSON、超时和模型失效；
- 两次调用使用同一冻结模型绑定。

### 18.4 API 与前端测试

- 非超级管理员全部返回拒绝；
- DRAFT 创建、逐文件上传、移除、开始、取消、重试和删除；
- 部分 render/parser/judge 失败仍返回已有结果；
- artifact 不泄露对象键且拒绝越权参数；
- 上传进度、任务轮询、终态停止轮询、部分失败和结果映射；
- Markdown 安全渲染不执行 HTML、脚本、远程图片或危险链接。

### 18.5 Docker 端到端测试

在 `parser-evaluation` profile 中上传三个格式各一个 fixture，验证：

- 每个文件产生 MarkItDown 和 Anydoc 两个最终 Markdown；
- 每个成功 parser 有主延迟和端到端延迟；
- 每个可评分文件有两份位置交换判决和四维最终分；
- 批次有逐格式与整体汇总；
- 中途重启 Go 服务后只补齐缺失步骤；
- 取消停止后续步骤；
- 删除后所有 artifact 均不可访问；
- 测试不需要 Milvus、Embedding 或 RAG evaluator，且没有创建 chunk/index 数据。

## 19. 验收标准

1. 超级管理员可以在独立界面批量上传 1–50 个受支持文件并选择视觉裁判模型。
2. 每个成功文件只运行解析阶段，分别生成 MarkItDown 与 Anydoc 的分块前最终 Markdown。
3. 每个成功 parser 记录符合本设计定义的 `parseDurationMs` 和 `endToEndDurationMs`。
4. 每个可评分文件使用前 6 个真值页和双方前 40,000 字符完成两次位置交换判决。
5. 页面展示四维分、总分、胜负、结构统计、真值页、完整 Markdown 和原始判决证据。
6. 混合格式批次按格式汇总并使用 macro average 计算跨格式质量分。
7. 单文件失败不会丢失其他文件结果，失败项可以重试，服务重启后可以恢复。
8. 普通用户和非超级管理员无法访问 API、页面或 artifact。
9. 任务删除或 90 天到期后，SQL 记录和对象存储 artifact 均被清理。
10. Docker 端到端证明确实没有运行 chunker、Embedding、Milvus、检索或回答流程。

## 20. 实施边界

该设计可以作为一个实施计划完成，但计划应按可独立验证的顺序分解：

1. 数据模型、配置和对象布局；
2. renderer 协议与容器；
3. parser timing 与标准化 Markdown 复用；
4. runner、评分和汇总；
5. admin API；
6. 独立前端；
7. Docker 集成、安全和端到端验证。

每一阶段都必须保持现有生产 RAG pipeline 和旧 RAG 测评行为不变。
