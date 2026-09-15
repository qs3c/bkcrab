# Prometheus 与 Grafana 监控设计

用户已确认本设计范围：新增应用指标、Prometheus 采集、Grafana 中文面板、告警规则、Compose 和 Helm 配置及排障文档。

## 架构与边界

应用使用独立 Prometheus registry；默认不启用，由 BKCRAB_METRICS_ENABLED 开启。独立监听地址 BKCRAB_METRICS_ADDR 默认为 127.0.0.1:18954，仅提供 /metrics。Compose 在容器网络监听 :18954，不映射到宿主机；Helm 使用独立 ClusterIP Service 和可选 ServiceMonitor，不经业务 Ingress。指标服务与进程共同启动、退出，监听失败明确报错。

HTTP 使用 method、route、status、mode 标签；route 必须为 ServeMux 注册模板，未知路径统一归类，SSE/WebSocket 与普通请求分开。包装器保留 Flush、Hijack 和 ResponseController 的透传。模型指标覆盖 OpenAI 和 Anthropic 的实际调用，流式耗时直到读取结束，首文本延迟单独统计。模型仅标记协议和调用模式，不使用用户自定义模型名称。工具按固定内置名称或外部工具类别聚合。RAG 保留 slog，附加 Prometheus recorder；公平队列将事件次数、耗时和采样值分开，避免重复采样累加成积压。

所有指标禁止使用用户、会话、文档、任务 ID、原始 URL、正文、错误消息或凭据作为标签。全局队列采样按实例展示，跨副本用 max 而非 sum。采样时间暴露给面板，未启用组件显示无数据，不能伪造为健康。

## 交付

独立监控 Compose overlay：固定版本的 Prometheus 和 Grafana、持久卷、自动数据源与中文面板、15 秒采集、15 天保留；管理界面默认仅绑定宿主机回环地址，Grafana 管理密码必填。Helm 提供指标配置、ServiceMonitor 与 PrometheusRule 开关，不强制安装 Operator。

面板包括 HTTP/Go 运行状态、模型/工具、RAG/公平队列。告警包含离线、低流量保护的错误率、普通请求高延迟、队列积压、依赖异常；阈值为初始运维配置，非已测 SLO。通知渠道由部署者配置。本次不新增日志聚合或分布式追踪平台。

## 验收

Go 测试覆盖指标输出、低基数标签、流式与升级连接、模型流中断、工具失败、遥测适配与生命周期。检查 Compose、Helm、Prometheus 配置及告警规则，并在隔离监控环境验证真实抓取和 Grafana 数据查询；不可执行的验证必须明确说明。不得重启现有业务容器或调用付费模型作为冒烟测试。
