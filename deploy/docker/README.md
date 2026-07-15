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

启动顺序为 MinIO/etcd 健康后启动 Milvus，Milvus 健康后才启动 BkCrab。Milvus 数据与元数据分别持久化在 `milvus-data`、`milvus-etcd-data` 命名卷。Milvus 复用现有 MinIO 凭据；RAG 上传的原始文档仍使用 BkCrab 的对象存储配置。

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
  logs --tail=200 milvus-etcd milvus-standalone bkcrab
```

除容器均为 healthy 外，还应在 BkCrab 日志中确认出现 `rag service enabled`。`/readyz` 只表示主网关可用，不能单独证明 RAG 已启用；使用登录态请求 `/api/rag/kbs`，不应返回 503。

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
