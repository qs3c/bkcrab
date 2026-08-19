# Docker Compose 部署

`docker-compose.yml` 是基础部署，包含 BkCrab、MySQL、MinIO 和 agent 沙箱。RAG 是可选能力；启用时再叠加 `docker-compose.rag.yml`。该 overlay 新增 Milvus Standalone 和 etcd，并复用基础部署已有的 MinIO。公平队列依赖位于独立的 `docker-compose.fairqueue.yml`；该共享 overlay 不会自动启用 RAG 或 imagegen，资源模式由各自 rollout 独立决定。

## 基础部署

从仓库根目录执行：

```bash
cp deploy/docker/.env.example deploy/docker/.env
# 编辑 deploy/docker/.env，替换所有示例密码。

docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  up -d --build
```

本地 `.env` 已被 Git 忽略，不要把真实密码或 API Key 提交到仓库。

## 启用 RAG

先在 `deploy/docker/.env` 中填写：

- `RAG_EMBEDDING_ENDPOINT`：OpenAI 兼容接口的基础 URL，通常以 `/v1` 结尾；BkCrab 会在其后请求 `/embeddings`。
- `RAG_EMBEDDING_MODEL`。
- `RAG_EMBEDDING_DIMS`：必须与该模型实际返回的向量维度一致。
- `RAG_EMBEDDING_API_KEY`：仅在 embedding 服务要求鉴权时填写。
- `MILVUS_USERNAME`、`MILVUS_PASSWORD`：默认内部 standalone 未启用鉴权，可留空；若启用 Milvus 鉴权，两项必须与服务端一致。

RAG overlay 同时部署固定版本的 `rag-parser`。它只连接专用的
`rag-parser-internal` 内部网络；BkCrab 同时连接默认网络和该内部网络。
parser 没有宿主机端口、没有外网出口，也不会收到 embedding、DocumentAI、
MinIO 或对象存储凭据。容器以 UID/GID 65532 运行，根文件系统只读，丢弃
全部 Linux capabilities，并只获得有大小上限的 `/tmp` tmpfs。挂载整个临时目录可让
LibreOffice 在只读根文件系统下创建 EMF 转换所需的本地 IPC 管道。

`.env.example` 中的 parser 限制与主服务 RAG 限制使用同一组规范值。parser
直接接收 `BKCRAB_RAG_LIMITS_MAX_FILE_MB`、
`BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES` 和
`BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS`，并在启动时换算 byte/second：

```text
maxInputBytes = RAG_MAX_FILE_MB * 1048576
parseTimeoutSeconds = RAG_PARSE_TIMEOUT_MS / 1000
```

因此 Compose 不再暴露可独立漂移的 `RAG_MAX_INPUT_BYTES` 或
`RAG_PARSE_TIMEOUT_SECONDS`。旧 parser-specific 环境变量仍可用于独立运行
sidecar；若同时设置规范值且两者不一致，parser 会启动失败。

`RAG_MAX_EXTRACTED_BYTES`、`RAG_MAX_ASSET_BYTES`、页数、图片数、像素和
PDF DPI 则原样同时注入两端。`/healthz` 回显 parser 实际使用的
`maxInputBytes/maxOutputBytes`；主服务对 Office/PDF 上传采用自身与 health
快照的较小值，不能通过只调高一端绕过限制。

高级解析、Office 与表格/代码增强是三个独立发布开关，默认均为 false：

- `RAG_ADVANCED_ENABLED`
- `RAG_OFFICE_ENABLED`
- `RAG_ENRICHMENT_ENABLED`

DocumentAI 配置只进入 BkCrab。填写 `RAG_DOCUMENT_AI_ENDPOINT`、模型和
`RAG_DOCUMENT_AI_API_KEY` 前，先配置精确的
`RAG_DOCUMENT_AI_ALLOWED_ENDPOINT_HOSTS`；HTTP/私网地址还必须显式设置
`RAG_DOCUMENT_AI_ALLOW_PRIVATE_ENDPOINT=true`。不要把真实 key 写进 Compose
文件、README 或日志。

Reranker 是可选增强。启用后，混合检索先保留全局候选 20 条，再精排到调用方要求的 TopN（默认 5）；只有精排成功时才应用 `RAG_RERANKER_MIN_SCORE`。服务超时、连接失败或响应非法时会自动退回 RRF 排序，不会用精排阈值过滤 RRF 分数。相关配置：

- `RAG_RERANKER_ENABLED`：是否启用；
- `RAG_RERANKER_ENDPOINT`：llama.cpp/Jina 兼容基础 URL，例如 `http://qwen3-reranker:8080/v1`；
- `RAG_RERANKER_API_KEY`、`RAG_RERANKER_MODEL`；
- `RAG_RERANKER_TIMEOUT_MS`：Compose 默认 60000；本地 CPU reranker 需要覆盖候选队列的完整处理时间；
- `RAG_RERANKER_CANDIDATE_TOP_K`：默认 20；
- `RAG_RERANKER_MIN_SCORE`：默认 0.5，取值范围 `(0,1]`。
- `RAG_RERANKER_BATCH_SIZE`、`RAG_RERANKER_UBATCH_SIZE`：llama.cpp 物理批大小，默认 1024，避免较长 query/context 在 512 tokens 处直接失败；
- `RAG_RERANKER_PARALLEL`：llama.cpp 并行槽位，默认 1；评测的 `RAG_EVAL_CASE_CONCURRENCY` 不应高于可用槽位。

仓库还提供 CPU-only 的 Qwen3 embedding/reranker 服务。模型分别放到 `${RAG_MODEL_ROOT}/embedding` 和 `${RAG_MODEL_ROOT}/reranker` 后，将模型文件名与 `docker-compose.models.yml` 中的路径保持一致，并把该文件加入同一次 Compose 启动：

```bash
docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  -f deploy/docker/docker-compose.models.yml \
  up -d --build
```

此时推荐配置 `RAG_EMBEDDING_ENDPOINT=http://qwen3-embedding:8080/v1`、`RAG_EMBEDDING_MODEL=qwen3-embedding`，以及 `RAG_RERANKER_ENDPOINT=http://qwen3-reranker:8080/v1`、`RAG_RERANKER_MODEL=qwen3-reranker`。

随后同时传入基础文件和 RAG overlay：

```bash
docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  config --quiet

docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  up -d --build
```

overlay 通过映射合并为现有 `bkcrab` 服务追加 RAG 环境变量和 Milvus 健康依赖，不会替换基础服务的构建、端口或原有依赖。不要单独启动 overlay。

启动顺序为 MinIO/etcd 健康后启动 Milvus；BkCrab 等待 Milvus 与
`rag-parser` 都健康后启动。Milvus 数据与元数据分别持久化在
`milvus-data`、`milvus-etcd-data` 命名卷。Milvus 复用现有 MinIO 凭据；
RAG 上传的原始文档仍使用 BkCrab 的对象存储配置。

Milvus gRPC 和 WebUI 默认仅绑定服务器回环地址：

- gRPC：`127.0.0.1:${MILVUS_PORT:-19530}`
- WebUI：`http://127.0.0.1:${MILVUS_WEB_PORT:-19091}/webui/`

容器间的 BkCrab 始终使用 `milvus-standalone:19530`，无需开放公网端口。

如需从可信局域网中的 Attu 查看数据，在服务器的 `deploy/docker/.env` 中设置：

```bash
MILVUS_BIND_ADDRESS=192.168.1.72
```

然后使用上面的两个 Compose 文件重新执行 `up -d`。这会使 Attu 可连接
`192.168.1.72:19530`，并仅在该局域网网卡上监听；不要为了方便改为
`0.0.0.0`。Milvus WebUI 同时可通过 `http://192.168.1.72:19091/webui/` 访问。
若服务器启用了防火墙，还需要仅对可信网段放行 TCP 19530 和 19091。

## 检查状态

```bash
docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  ps

docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  logs --tail=200 milvus-etcd milvus-standalone rag-parser bkcrab
```

除容器均为 healthy 外，还应确认 `rag-parser` 的 health 输出包含
`protocolVersion=rag-parser/v2`、固定 `serviceVersion` 和预期 limits，并在
BkCrab 日志中确认出现 `rag service enabled`。`/readyz` 只表示主网关可用，
不能单独证明 RAG 已启用；使用登录态请求 `/api/rag/capabilities` 和
`/api/rag/kbs`，后者不应返回 503。不要为了检查 health 给 parser 增加公网
端口；可在内部网络中用一次性诊断容器或 `docker compose exec bkcrab`
访问 `http://rag-parser:8080/healthz`。

## Kubernetes / Helm parser 隔离

静态清单先应用主服务，再应用 parser 与网络策略：

```bash
kubectl apply -f deploy/k8s/bkcrab.yaml
kubectl apply -f deploy/k8s/rag-parser.yaml
kubectl apply -f deploy/k8s/rag-parser-networkpolicy.yaml
```

## Selecting the Office parser backend

The RAG overlay starts both `rag-parser` (MarkItDown) and
`rag-parser-anydoc` (Firecrawl anydoc). The upload page selects an engine per
document; the selection is persisted and reused for reindexing. MarkItDown
remains the default for legacy clients. To change only that default:

```text
RAG_PARSER_ENDPOINT=http://rag-parser-anydoc:8080
RAG_PARSER_ENGINE=anydoc
```

For the baseline implementation use:

```text
RAG_PARSER_ENDPOINT=http://rag-parser:8080
RAG_PARSER_ENGINE=markitdown
```

The gateway always receives both internal endpoints from the Compose overlay.
After changing the legacy default, recreate `bkcrab`; the two parser containers
do not need to be stopped. Check both with:

```bash
docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  ps rag-parser rag-parser-anydoc bkcrab
```

The configured engine is verified against health metadata and each returned
bundle. A mismatched endpoint/engine pair is unavailable rather than silently
using the wrong converter.

`rag-parser-networkpolicy.yaml` 对 parser ingress/egress 默认拒绝，仅允许带
`app=bkcrab` 标签的 Pod 访问 TCP/8080，parser egress 始终为空。如果集群
已经用其它 NetworkPolicy 限制 BkCrab egress，还需额外增加一条
BkCrab→parser 的 additive allow；不要单独安装一条只允许 parser 的 gateway
egress 策略，否则会意外阻断数据库、对象存储和 provider。

Helm 中通过 `rag.enabled=true` 部署 RAG 接线，parser 默认随之启用；三个
高级 feature gate 仍保持 false。parser 的 input byte limit 由
`rag.limits.maxFileMB` 自动换算，其它 limit 也直接复用 `rag.limits`：

```bash
helm lint deploy/helm/bkcrab --set rag.enabled=true
helm template bkcrab deploy/helm/bkcrab --set rag.enabled=true >/tmp/bkcrab.yaml
```

生产环境应通过 Secret 管理器提供 `rag.documentAI.apiKey` 等凭据，并把
`rag.parser.image.tag` 固定为已验证的 release tag 或镜像 digest；禁止使用
`latest`/`dev`。只有命名空间已经限制 gateway egress 时才设置
`rag.parser.networkPolicy.allowGatewayEgress=true`。

如需运行真实 Milvus 集成测试，overlay 已把 gRPC 端口限制在本机：

```bash
RAG_TEST_MILVUS_ADDR=127.0.0.1:19530 \
  go test ./internal/rag/vector -run TestMilvusRoundTrip -v
```

## RAG fairqueue 依赖与两阶段切换

基础 Compose 与普通 RAG overlay 都保持
`FAIR_QUEUE_ENABLED=false`、`RAG_INDEX_WORKER_MODE=legacy`，不会要求 Redis /
RabbitMQ 凭据。`docker-compose.fairqueue.yml` 提供持久化 Redis（AOF）和
RabbitMQ management 并启用共享协议，但不改变任何资源的 worker mode；RAG
最终阶段还必须叠加 `docker-compose.rag-fair.yml`。management 只绑定
`127.0.0.1`。使用该 overlay 前，在 `.env` 中为 `REDIS_PASSWORD` 和
`RABBITMQ_PASSWORD` 生成独立随机值，Rabbit 密码必须是 URL-safe。

正式切换不能做 legacy/fair canary，必须是两个独立的全量部署：

1. 先只用基础 Compose（需要 RAG 时再叠加普通 RAG overlay）部署兼容
   dual-write 镜像。执行以下 dry-run；确认所有旧 writer 已归零后，追加
   `--apply --confirm-all-writers-dual-write` 完成 contract：

   ```bash
   docker compose \
     --env-file deploy/docker/.env \
     -f deploy/docker/docker-compose.yml \
     -f deploy/docker/docker-compose.rag.yml \
     run --rm bkcrab admin fairqueue contract-migrate
   ```

2. 全量设置 `RAG_INDEX_WORKER_MODE=paused`，仍保持
   `FAIR_QUEUE_ENABLED=false`。等待旧容器归零、heartbeat 静止且所有旧
   claimant 退出；无法证明时保持 paused。
3. 第二次全量部署显式叠加共享 fairqueue overlay 和 RAG fair overlay；前者固定
   enabled=true、writer topology=single 并启动 Redis/RabbitMQ，后者只固定
   RAG worker mode=fair：

   ```bash
   docker compose \
     --env-file deploy/docker/.env \
     -f deploy/docker/docker-compose.yml \
     -f deploy/docker/docker-compose.rag.yml \
     -f deploy/docker/docker-compose.fairqueue.yml \
     -f deploy/docker/docker-compose.rag-fair.yml \
     up -d --build
   ```

   检查 Redis、Rabbit health 和 `/readyz`；Rabbit/Redis 暂时 degraded 不应使
   API Pod 失活，但 scheduler 会停止新 claim。
4. 回滚按 `fair -> paused`，排空非终态任务后再到兼容 dual-write 的
   `legacy`。contract 后禁止回到 pre-expand 镜像。

详细的 Redis/Rabbit 灾难恢复、writer rebind 和 journal 对账见
[`docs/rag-fair-queue-operations.md`](../../docs/rag-fair-queue-operations.md)。

### Imagegen batch rollout 与 workspace

`IMAGEGEN_BATCH_MODE` 默认 `legacy`。Imagegen 的正向切换必须是两次独立全量
rollout：先 `legacy -> drain`，确认所有旧 ReplicaSet/容器和同步 `image_gen`
调用归零；再 `drain -> fair`。禁止 legacy/fair canary。回滚按
`fair -> drain`，等待所有非终态 batch 排空后才能 `drain -> legacy`。

`drain` 和 `fair` 都必须叠加同一个共享基础设施 overlay；只修改
`IMAGEGEN_BATCH_MODE`，不要叠加 `docker-compose.rag-fair.yml`，除非 RAG 也已完成
自己的 paused rollout。例如最终 imagegen fair 部署为：

```bash
IMAGEGEN_BATCH_MODE=fair docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.fairqueue.yml \
  up -d --build
```

Compose 默认使用 MinIO（S3-compatible）作为共享 workspace，因此 fair/drain
worker 可以跨实例恢复 artifact。若改为 LocalFS，只允许一个 gateway/worker
实例；LocalFS 不能用于多副本。MySQL 必须是所有 Pod 看到相同
`@@server_uuid + DATABASE()` 的 single writer，Redis 必须是 standalone，Rabbit
和 Redis 暂时不可用时 create 仍先持久化到 MySQL，恢复后再 dispatch。

## 停止与升级

停止服务但保留数据卷：

```bash
docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  down
```

如果当前启用了 fairqueue，停止或更新时也必须传入
`-f deploy/docker/docker-compose.fairqueue.yml`，避免遗漏 Redis/RabbitMQ 服务。

`down -v` 会永久删除 MySQL、MinIO、Milvus 和 etcd 数据，只应在明确需要清空环境时使用。

Milvus 固定为 `v2.6.19`，etcd 固定为 `v3.5.18`。升级前应备份命名卷，阅读对应版本的升级说明，并重新执行 `config --quiet` 与真实检索冒烟测试；不要直接改用 `latest`。
