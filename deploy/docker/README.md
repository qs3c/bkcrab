# Docker Compose 部署

`docker-compose.yml` 是基础部署，包含 BkCrab、MySQL、MinIO 和 agent 沙箱。RAG 是可选能力；启用时再叠加 `docker-compose.rag.yml`。该 overlay 新增 Milvus Standalone 和 etcd，并复用基础部署已有的 MinIO。

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
- `RAG_RERANKER_TIMEOUT_MS`：默认 5000；
- `RAG_RERANKER_CANDIDATE_TOP_K`：默认 20；
- `RAG_RERANKER_MIN_SCORE`：默认 0.5，取值范围 `(0,1]`。

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

## 停止与升级

停止服务但保留数据卷：

```bash
docker compose \
  --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  down
```

`down -v` 会永久删除 MySQL、MinIO、Milvus 和 etcd 数据，只应在明确需要清空环境时使用。

Milvus 固定为 `v2.6.19`，etcd 固定为 `v3.5.18`。升级前应备份命名卷，阅读对应版本的升级说明，并重新执行 `config --quiet` 与真实检索冒烟测试；不要直接改用 `latest`。
