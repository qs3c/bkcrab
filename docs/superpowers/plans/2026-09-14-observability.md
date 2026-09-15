# Prometheus / Grafana Implementation Plan

**Goal:** 为 bkcrab 提供可运行、可排障的指标监控闭环。

**Architecture:** 独立 registry 与指标端口；应用调用点直接埋点，RAG/FairQueue 遥测适配；Prometheus 拉取，Grafana 自动加载面板。

**Tech Stack:** Go client_golang、Docker Compose、Prometheus、Grafana、Helm。

**Spec:** ../specs/2026-09-14-observability-design.md（用户已确认；本会话直接实施）

## Global Constraints

- 指标默认关闭，默认地址 127.0.0.1:18954。
- 禁止用户、会话、文档、任务 ID、正文、URL、凭据作为指标标签。
- 不修改现有部署运行状态；不提交、合并或推送，保留可审查分支。

## Task 1: 指标核心与进程生命周期

- [x] 新建 internal/observability/{metrics,http,server}.go，提供 New、Current、SetDefault、Middleware、StartServer。
- [x] 新建 internal/config/metrics.go，接入 env.go；在 cmd/bkcrab/main.go 配置 registry/端口，setup/server.go 包装 HTTP。
- [x] 测试成功/失败计数、路由归并、SSE Flush、WebSocket Hijack、端口占用与关闭，运行 go test ./internal/observability ./internal/config。

## Task 2: 业务埋点和遥测适配

- [x] internal/provider/{openai,anthropic}.go：实际请求、完整流生命周期、Token 和首文本延迟；模拟 SSE 服务验证取消和失败。
- [x] internal/agent/tools/registry.go：统一工具执行次数/耗时/错误；internal/rag/search.go：检索入口耗时和失败。
- [x] internal/gateway/observability.go：保留 RAG 日志、附加 metrics recorder；FairQueue sink 适配并接入 RAG/imagegen 装配处。
- [x] 验证 gauge Set 与 counter Inc 区分，禁止把关联字段带入标签；运行相关包测试。

## Task 3: 部署、面板和告警

- [x] deploy/monitoring/：Prometheus 配置、规则、规则测试、Grafana provisioning 和中文 JSON 面板。
- [x] deploy/docker/docker-compose.monitoring.yml：独立 overlay，强制 Grafana 密码、持久卷、宿主回环绑定。
- [x] deploy/helm/bkcrab/：可选独立 Service、ServiceMonitor、PrometheusRule；不启用时不依赖 CRD。
- [x] docs/monitoring.md：启用命令、指标语义、排障流程、多副本聚合与限制；链接部署 README。
- [x] promtool check/test、Compose config、Helm lint/template；隔离启动 Prometheus/Grafana 与模拟应用，验证 up、查询、告警、数据源和面板；清理测试容器。

## Verification results (2026-09-14)

- 相关 Go 包测试和 race 检查通过；额外验证 internal/agent 集成测试通过。
- internal/setup 完整测试通过。修正已有部署测试将可选 context-cache Redis 变量误判为必需 fairqueue 依赖的问题；修改前测试源码已复现同一失败。
- Prometheus 配置、7 条规则及正负场景告警测试通过。
- Compose 基础与 RAG/fairqueue 组合配置通过；缺少 Grafana 密码会拒绝启动配置。
- Helm lint、启用/关闭渲染与非法开关/端口组合检查通过；未执行真实 Kubernetes 集群发布。
- 隔离 Prometheus/Grafana 冒烟验证采集 UP、3 张面板自动导入、38 个查询均有数据；浏览器检查三张面板并确认 15 秒自动刷新可用。
- 验证数据为合成数据；未重启、发布或改变已有业务容器，未进行负载/容量测试。告警通知渠道仍由部署环境配置。
