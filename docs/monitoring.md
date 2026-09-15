# Prometheus 与 Grafana 监控

应用指标由独立 HTTP 端口提供，Prometheus 每 15 秒采集，Grafana 自动加载 3 张中文面板（38 个图表）。本功能默认关闭；增加部署配置并发布包含本功能的二进制后才会生效。

## Docker Compose 启用

在 `deploy/docker/.env` 中设置独立的强密码：

```dotenv
GRAFANA_ADMIN_USER=admin
GRAFANA_ADMIN_PASSWORD=替换为独立的随机强密码
GRAFANA_PORT=13000
PROMETHEUS_PORT=19090
```

从仓库根目录执行；若当前部署还使用 RAG、公平队列或其他 overlay，保留原有全部 `-f` 参数，再追加 monitoring 文件。

```bash
docker compose --env-file deploy/docker/.env \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.monitoring.yml \
  up -d --build bkcrab prometheus grafana
```

该命令会更新业务容器，在线环境请在正常发布窗口执行。本次代码实现的验证使用隔离测试环境，不会自动更新已有业务部署。

- Grafana：<http://localhost:13000>，使用上面的管理员账号。
- Prometheus：<http://localhost:19090>，在 Targets 确认 `bkcrab` 为 UP，在 Alerts 查看告警。
- Grafana 数据源 UID 为 `bkcrab-prometheus`，面板在 Bkcrab 文件夹，默认首页为服务总览。
- 镜像固定为 Prometheus `v3.14.0`、Grafana `13.2.1`；更新版本后重跑验证。Prometheus 保留 15 天、最多约 5GB 数据（以先达到的限制为准）；额外预留 WAL 等磁盘空间。两个服务均有持久卷，`down -v` 会删除历史监控数据。
- 管理界面只绑定宿主回环地址。远程查看可使用 SSH 转发 `ssh -L 13000:127.0.0.1:13000 -L 19090:127.0.0.1:19090 <host>`；公网入口需自行配置认证与 TLS。
- 应用指标端口 `18954` 仅在 Compose 网络中开放，不映射到宿主机。使用自定义网络时，确保 Prometheus 能解析、连接 `bkcrab:18954`。

Grafana 面板和数据源通过文件管理；修改 `deploy/monitoring/grafana/` 中的源文件，不依赖 UI 临时修改。已有 Grafana 数据卷中的管理员密码不会因环境变量改变而自动重置。

## 二进制 / systemd

```dotenv
BKCRAB_METRICS_ENABLED=true
BKCRAB_METRICS_ADDR=127.0.0.1:18954
```

默认监听地址为 `127.0.0.1:18954`。Prometheus 在另一台机器或容器时，需要显式配置可达的内网监听地址和采集目标；Docker 中的 `localhost` 是容器自身。支持 IPv6 地址如 `[::1]:18954`。非法布尔值、端口和监听冲突会明确报错，不会悄悄失去监控。

指标端口只提供 `GET /metrics`，不共用业务登录态。该端口无内置认证，应限制在可信监控网络。抓取只读取内存指标、缓存的队列健康状态和 SQL 连接池统计，不会在每次抓取时扫描业务表或调用 Redis/RabbitMQ。

## Kubernetes / Helm

沿用已有发布参数，追加如下 values。ServiceMonitor/PrometheusRule 需要集群已安装 Prometheus Operator CRD；默认均关闭，普通 Helm 安装不依赖 Operator。

```yaml
metrics:
  enabled: true
  port: 18954
  serviceMonitor:
    enabled: true
    interval: 15s
    scrapeTimeout: 10s
    additionalLabels:
      release: kube-prometheus-stack
  prometheusRule:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack
```

`release` 必须匹配你已有 Prometheus 的选择器；还需允许其发现 Bkcrab 所在命名空间。独立 metrics Service 不出现在业务 Ingress 中。ServiceMonitor 按 Pod endpoint 采集，`jobLabel: metrics-job` 固定生成 `job="bkcrab"`，防止只抓到一个负载均衡后端。

没有 Operator 时可以只开启 `metrics.enabled`，在已有 Prometheus 配置 Kubernetes endpoint discovery，逐个 Pod 采集 metrics 端口；不要用业务 ClusterIP 的单一随机后端代替逐 Pod 采集。Grafana 导入 `deploy/monitoring/grafana/dashboards/*.json`，并创建 UID 为 `bkcrab-prometheus` 的数据源，指向实际 Prometheus 地址。

同一 Prometheus 采集多个独立 Bkcrab 安装时，应增加 cluster/deployment 标签，并同步调整面板过滤和规则的聚合维度，避免不同安装的共享队列混合。已有 NetworkPolicy 需允许监控组件访问指标端口。

Helm 的 `files/monitoring-alerts.yml` 与 `deploy/monitoring/alerts.yml` 内容一致；修改规则后同步两处并运行 `cmp`。阈值默认针对单个 Bkcrab 安装，不代表经过测量的线上 SLO。

## 指标口径

| 指标 | 含义与边界 |
|---|---|
| `up{job="bkcrab"}` | 指标端口是否采集成功，不能单独证明数据库或模型健康 |
| `bkcrab_http_requests_total` | HTTP handler 完成次数，按 method、注册路由模板、状态码、mode 统计；动态 ID 和 query 不进入标签 |
| `bkcrab_http_request_duration_seconds` | handler 生命周期。`request` 为普通请求；`chat` 包含聊天接口；`stream` 为请求声明的 SSE；`websocket` 为升级连接。聊天/流/升级连接不参与普通接口延迟告警 |
| `bkcrab_http_requests_inflight` | 当前 handler 数；WebSocket 统计到 handler 返回，不是独立连接注册表 |
| `bkcrab_operations_total` / `bkcrab_operation_duration_seconds` / `bkcrab_operations_inflight` | 模型实际请求、统一工具执行、RAG 检索和索引执行；每次上游重试单独统计。结果为 ok/error/timeout/canceled |
| `bkcrab_llm_first_text_seconds` | 从发起流式请求到首个非空文本 delta；不包含仅工具/思考的响应样本，不等于首字节或首个 reasoning token |
| `bkcrab_llm_tokens_total` | OpenAI/Anthropic provider 实际返回的 input/output/cache_read/cache_write；不估算缺失 usage，遵循协议各字段含义，不盲目求和。费用与配额以原有 usage 记录为准 |
| `bkcrab_telemetry_events_total` | RAG、公平队列的事件次数；一次任务有多个生命周期事件，不等于任务总数 |
| `bkcrab_telemetry_duration_seconds` | 遥测已有的正耗时，包括解析、DocumentAI、索引阶段、队列执行/恢复等；并非每个内部阶段都有单独指标 |
| `bkcrab_task_queue_wait_seconds` | 创建到首次执行的等待，排除 RetryCount 非零的任务；依赖应用与数据库时钟同步。RAG 覆盖 legacy/fair；图片批次覆盖 fair |
| `bkcrab_fairqueue_sample` | 最近一次成功采样的共享队列值；跨副本用 max 而非 sum。Rabbit ready depth 是消息数，不是 SQL 全部待处理任务数 |
| `bkcrab_fairqueue_sample_timestamp_seconds` | 采样时间；面板与积压告警排除超过 120 秒的样本。失败采样不把队列伪装成 0 |
| `bkcrab_fairqueue_enabled/healthy/dependency_up/gate_open` | 缓存的启用、整体健康、依赖状态和调度门；未启用不导出伪造的健康值 |
| `bkcrab_fairqueue_loop_lag_seconds` | 距该循环上次成功的秒数，首次成功前不产生该样本 |
| `go_*` / `process_*` / `go_sql_*` | Go 运行时、进程及主 SQL 连接池；不代表宿主机、容器限制、MySQL 服务端或整个 Redis 集群的指标 |

模型按协议聚合，不按用户自定义模型/提供者名展开。工具固定分类之外统一为 `other`，MCP/插件的动态工具名不生成无限时序。指标不包含用户、会话、文档、任务 ID、正文、原始错误、路径或凭据。需要具体任务关联时，在相应时间窗和实例的现有日志中排查。

尚未出现调用的 CounterVec/HistogramVec 可能没有时序；无请求时分位数和比率也可能为 NaN。面板保留无数据，不用 0 冒充健康。新部署通常需要两次采集后才能计算 rate。

## 线上排障路径

1. **实例无数据**：检查 Prometheus Targets 的报错、业务镜像是否已更新、指标开关/监听地址、DNS 与网络。UP 但业务不可用时，继续检查 `/readyz` 与依赖状态。
2. **接口报错或变慢**：服务总览按实例和 route 看状态码、P95/P99；检查 Go 堆、goroutine、CPU 与 SQL in-use/wait。对照同一实例日志定位业务错误。
3. **聊天卡住**：模型面板看在途调用、完整耗时与首文本延迟；区分上游无响应、长生成、工具阻塞和主动取消。HTTP 200 后发生的流内失败由模型/业务操作指标体现，不能只看 HTTP 5xx。
4. **RAG 索引慢或失败**：查看 index 操作、RAG 遥测的错误/重试/解析阶段与缓存命中。缓存图表覆盖 RAG 解析、视觉和增强缓存，不包含 Agent context Redis 缓存。
5. **公平队列积压**：先看样本年龄，再看消息深度、首次等待、在途任务、任务执行耗时、调度门和依赖健康。整体未就绪时查看已有受保护的 fairqueue 管理健康接口和恢复日志，不直接修改租约或 Redis 状态。

## 告警与通知

内置 7 条规则：实例采集失败、HTTP 高错误率、普通请求高延迟、业务操作高错误率、就绪消息持续积压、队列依赖异常、队列整体未就绪。错误率/延迟规则设有最低请求数，主动取消不算业务错误。计划维护的 paused 模式可静默队列未就绪告警。

**规则默认在 Prometheus 内求值，不会自动发邮件或飞书消息。** 要实际通知，请将 Prometheus 的 `alerting.alertmanagers` 指向已有 Alertmanager，再在 Alertmanager 配置接收人和通知渠道。Kubernetes 使用已有 kube-prometheus-stack 的 Alertmanager 配置即可。不要把 webhook token 或通知凭据提交到仓库。Grafana 的告警管理与 Prometheus 规则是不同配置入口，不能仅因面板可见就认为通知已接通。

## 验证

```bash
go test -race ./internal/observability ./internal/config ./internal/provider \
  ./internal/gateway ./internal/agent/tools ./internal/rag ./internal/imagegen ./cmd/bkcrab
go test ./internal/setup

docker run --rm --entrypoint promtool \
  -v "$PWD/deploy/monitoring:/etc/prometheus:ro" \
  prom/prometheus:v3.14.0 check config /etc/prometheus/prometheus.yml
docker run --rm --entrypoint promtool \
  -v "$PWD/deploy/monitoring:/etc/prometheus:ro" -w /etc/prometheus \
  prom/prometheus:v3.14.0 test rules alerts.test.yml
cmp deploy/monitoring/alerts.yml deploy/helm/bkcrab/files/monitoring-alerts.yml

python3 scripts/monitoring/smoke.py
```

冒烟脚本只依赖 Go、Docker Compose、Python 3 标准库：编译合成流量 fixture，创建独立随机项目、临时强密码和随机回环端口；验证 Prometheus UP、Grafana 数据源、3 张自动导入面板、全部 38 个查询和规则加载，最后删除该测试项目和卷。测试数据是合成数据，不是线上流量、性能或容量证明。加 `--keep` 可保留测试环境进行浏览器检查，临时目录内有只读凭据文件 `browser.json` 与精确清理命令 `cleanup.json`。

告警测试覆盖持续异常触发，以及低流量、SSE、陈旧采样和未启用资源不误报。实际服务器启用后，还需按本文第一节在真实流量下检查 Targets 与图表；本地冒烟不代表已完成线上部署。

参考：[Prometheus 指标规范](https://prometheus.io/docs/practices/naming/)、[Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/)。
