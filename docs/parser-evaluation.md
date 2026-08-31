# 文档解析测评

文档解析测评是独立于 RAG benchmark 的超级管理员功能。它只处理用户上传的
`.docx`、`.pptx`、`.xlsx`：同一份文件依次交给 MarkItDown 和 AnyDoc，记录解析延迟，
再以 LibreOffice 渲染出的页面 PNG 为视觉真值，由任务创建时选择的视觉模型进行双位置盲评。
流程在生成标准化 Markdown 后结束，不执行 DocumentAI、增强、分块、Embedding、Milvus、
检索或回答生成。

前端入口为 `/admin/parser-evals/`。该页面与旧 RAG 测评界面分离，只有通过浏览器 Cookie
登录的超级管理员可见和可调用；管理员 API Key、`act-as` 会话及普通用户均会被拒绝。

## 一次任务如何运行

1. 创建任务时选择一个已配置且支持图片输入的裁判模型。
2. 上传 1–50 个 Office 文件；单文件和批次总大小受部署配置限制。
3. 启动任务后，系统冻结裁判 provider/model/fingerprint、两个 parser 版本、renderer 版本和测评限制。
4. 全局单 worker、单文档串行执行；每份文档的两个 parser 顺序由文件 SHA-256 确定性打散，减少先后顺序偏差。
5. renderer 以 LibreOffice headless 转为 PDF，再用 PyMuPDF 输出前若干页 PNG 作为视觉真值。
6. MarkItDown 和 AnyDoc 都通过现有 `parse.LocalParser` 的 Office standard 路径产出标准化 Markdown。
7. 裁判执行两次：第一次 MarkItDown 为 A，第二次 AnyDoc 为 A。每次分别给完整性、结构、格式、整洁度打 1–5 分。
8. 两次分数按解析器归位后相加并乘 10，得到每维 20–100 分，总分是四维平均；两次胜负不一致时总体结果记为平局。

任务状态是 `DRAFT → QUEUED → RUNNING → SUCCEEDED/PARTIAL/FAILED/CANCELLED`。
失败或部分成功的任务可以重试，但已经成功的渲染、解析或裁判槽位不会被覆盖。运行中取消是协作式的，
worker 在步骤边界和租约心跳处停止。页面展示总体、按格式的质量与延迟汇总，以及逐文档证据。

## 延迟口径

- `parseDurationMs`：parser sidecar 报告的核心转换/解析时间。
- `endToEndDurationMs`：BkCrab parser 客户端从请求开始到完整校验解析 bundle 的总时间，包含上传、sidecar 排队/处理、下载和 bundle 校验。
- renderer 的 `renderDurationMs` 单独保留，仅用于解释视觉真值生成成本，不混入两个 parser 的延迟对比。
- 汇总展示成功样本的中位数和 P95；缺失或失败样本不伪造为 0。

## 数据与证据

关系数据库只新增两张表：`parser_eval_runs` 和 `parser_eval_documents`。运行快照、步骤结果、
用量和汇总使用有大小限制的封闭 JSON 字段，不增加 attempt、judge、usage 或 aggregate 表。

以下大对象存入现有对象存储的独立 `parser-evals/` 前缀：

- 用户上传的原始 Office 文件；
- 视觉真值页面 PNG；
- 两个 parser 的完整 Markdown；
- 两次裁判的原始 JSON 响应。

API 响应不会暴露 object key；证据只能通过受保护的 artifact API 按需读取。前端 Markdown
预览禁用原始 HTML、远程图片和不安全协议。任务默认保留 90 天；清理先删除对象前缀，成功后再删除两张表中的记录，
从而允许失败后重试。

## Docker 部署

该能力默认关闭。它位于 RAG Compose overlay 中，但使用独立的内部网络和三个专用 sidecar；
sidecar 无宿主机端口、无外网路由、无模型密钥或对象存储凭据，并使用只读根文件系统、受限 tmpfs、
非 root 用户、全部 capability 丢弃及 CPU/内存/PID 限制。

在 `deploy/docker/.env` 中至少设置：

```dotenv
PARSER_EVAL_ENABLED=true
PARSER_EVAL_WORKER_ENABLED=true
```

然后从仓库根目录启动：

```bash
docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  --profile parser-evaluation \
  up -d --build
```

检查状态和日志：

```bash
docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  --profile parser-evaluation \
  ps

docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  logs --tail=200 parser-eval-renderer parser-eval-markitdown parser-eval-anydoc bkcrab
```

主服务不依赖三个 sidecar 的 Compose 启动条件；因此不启用 profile 或 sidecar 暂时故障时，
BkCrab 其余能力仍可启动。解析测评 capability 接口会明确报告不可用原因，前端会禁止启动新任务。
如需 API-only 实例，显式设置 `PARSER_EVAL_WORKER_ENABLED=false`，并确保同一数据库只有预期的 worker 实例启用。

## 配置

应用读取以下 `BKCRAB_` 环境变量；Compose `.env` 使用去掉 `BKCRAB_` 的同名变量映射：

| 环境变量 | 默认值 | 说明 |
|---|---:|---|
| `BKCRAB_PARSER_EVAL_ENABLED` | `false` | 总开关 |
| `BKCRAB_PARSER_EVAL_WORKER_ENABLED` | `true` | 是否运行后台 worker |
| `BKCRAB_PARSER_EVAL_RENDERER_ENDPOINT` | `http://parser-eval-renderer:8080` | renderer 根地址 |
| `BKCRAB_PARSER_EVAL_MARKITDOWN_ENDPOINT` | `http://parser-eval-markitdown:8080` | MarkItDown sidecar 根地址 |
| `BKCRAB_PARSER_EVAL_ANYDOC_ENDPOINT` | `http://parser-eval-anydoc:8080` | AnyDoc sidecar 根地址 |
| `BKCRAB_PARSER_EVAL_RENDER_TIMEOUT_MS` | `600000` | 单次渲染超时 |
| `BKCRAB_PARSER_EVAL_PARSE_TIMEOUT_MS` | `600000` | 单次解析超时 |
| `BKCRAB_PARSER_EVAL_JUDGE_TIMEOUT_MS` | `240000` | 单次裁判超时 |
| `BKCRAB_PARSER_EVAL_MAX_FILE_BYTES` | `52428800` | 单文件上限（50 MiB） |
| `BKCRAB_PARSER_EVAL_MAX_FILES` | `50` | 单任务文件数上限 |
| `BKCRAB_PARSER_EVAL_MAX_BATCH_BYTES` | `524288000` | 单任务总大小上限（500 MiB） |
| `BKCRAB_PARSER_EVAL_RETENTION_DAYS` | `90` | 终态任务保留天数 |
| `BKCRAB_PARSER_EVAL_MAX_PAGES` | `6` | 交给视觉裁判的最大页数 |
| `BKCRAB_PARSER_EVAL_RENDER_DPI` | `100` | 真值页面 DPI |
| `BKCRAB_PARSER_EVAL_MARKDOWN_JUDGE_CHARS` | `40000` | 每个候选送入单次裁判的最大 Unicode 字符数 |

页面上限和 Markdown 字符上限只裁剪裁判输入；完整 Markdown 与 renderer 的总页数仍保留在结果和证据中。

## 可选 Docker 端到端测试

生产 Compose 不开放 sidecar 端口。测试覆盖文件只在回环地址发布 `18081`–`18083`：

```bash
docker compose \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  -f deploy/docker/docker-compose.parser-eval-test.yml \
  --profile parser-evaluation \
  up -d --build parser-eval-renderer parser-eval-markitdown parser-eval-anydoc

PARSER_EVAL_E2E=1 go test ./internal/parseeval \
  -run TestDockerOfficeParsersEndToEnd -count=1 -timeout=6m
```

该测试生成最小 `.docx/.pptx/.xlsx`，验证真实 renderer、两个 parser、双位置裁判编排、
所有证据和三种格式汇总，并断言 `rag_documents`、`rag_chunks` 行数没有变化。裁判使用测试内的确定性模型，
不会调用外部模型或消耗费用。

## 故障排查

- 页面显示功能关闭：确认 `PARSER_EVAL_ENABLED=true`，重新创建 `bkcrab` 容器。
- renderer/parser 不健康：先看对应 sidecar 日志，再核对 `.env` 的页数、DPI、文件大小和超时是否与主服务一致。
- 无可选裁判模型：配置支持图片输入的模型绑定；文本模型不会出现在任务选择列表中。
- 文档为 `PARTIAL`：查看逐步骤错误码；只要一个 parser 成功，已得到的 Markdown 和延迟仍可查看，裁判前置条件不足时会跳过。
- LibreOffice 处理复杂文件超时或内存不足：在确认文件可信且主机容量允许后，调整 renderer/parser 的 timeout、tmpfs 和 memory limit；不要开放 sidecar 公网端口。
- 任务长期 `RUNNING`：worker 会用租约恢复过期任务。确认只有预期实例启用 worker，并检查数据库时钟与 worker 日志。
