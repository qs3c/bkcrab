# 2026-09-14 可靠上下文缓存部署记录

服务器 `100.93.173.10`，Docker Compose 项目 `bkcrab`。应用镜像 `bkcrab/bkcrab:context-cache-20260914`，二进制提交 `680d2f0`，Go 1.25.12。该提交已推送 main；后续本记录与讲解稿只改文档。

## 发布内容

- 合入 reliable 缓存及会话/技能快照修复，默认缓存 TTL 1800 秒。
- 保留原来线上 sandbox-reliability 的资源、配额、持久化、watchdog 和备份配置。为兼容技能发布，生命周期包装器在热沙箱上也传播回合版本变更；用户技能对象计入用户配额，全局技能单独计入受限作用域；发布器外层接口和本地工作区定位能力均保留。
- 新增独立 `context-cache-redis`，Redis 7.4，maxmemory 256 MiB、allkeys-lru，容器内存 384 MiB/0.5 CPU，无宿主机监听端口。缓存可丢弃，不启用 AOF/RDB。原调度 Redis 保持原配置。
- 网关开启新版缓存，TTL 1800 秒，Redis 操作超时 200 毫秒；网关原有 2 CPU/2 GiB 与工作区挂载保持一致。

## 备份与构建

升级前备份：`/home/xavier/bkcrab-backups/context-cache-20260914T153056Z`，包含 MySQL 全库、应用数据/MinIO/工作区文件、部署环境与旧镜像标识。两份 gzip 完整性检查通过。备份在本机，在线备份不承诺跨 SQL/文件的原子快照。

旧镜像保留为 `bkcrab/bkcrab:before-context-cache-20260914`。安装缺少 Docker Buildx，原多阶段 Dockerfile 无法直接构建；此次在宿主机用 CGO_ENABLED=0 编译同提交的两个 Go 二进制，再按仓库 Dockerfile 的最终阶段打包。前端源码相对原线上 `175b699` 没有变化，复用已生成的嵌入式前端资源。

## 迁移与启动

首次切换因 MySQL 二进制日志开启、普通应用账号无 SUPER 权限而短暂启动失败（1419）。用一次性迁移程序在 MySQL 容器内通过本地 socket、管理员身份调用仓库 DBStore.Migrate 完成迁移。应用继续使用原普通账号，未扩大其权限；`log_bin_trust_function_creators` 仍为 0。迁移完成后网关自动恢复健康，一次性工具已移除。

已安装 15 个 ctxcache 触发器，包括 sessions UPDATE v2；新增 context_cache_changes、skill_publications。未来同类部署需要先由迁移账号完成建表/触发器安装；不能只依赖普通应用账号首次启动。

## 验证

- 发布源码全仓库编译通过（`go test ./... -run '^$'`，不等于运行全仓库测试）。
- sandbox/workspace/gateway/skills 完整包测试通过 race 检测；原 reliable 的 store/session/contextcache/agent/tools 等测试与真实 MySQL/Redis 故障、并发用例此前已通过。
- 隔离真实 Docker 测试通过：旧/新技能挂载、同版本复用、生命周期包装器热实例换版、切换后工作区文件保留。
- 新网关环境中 `TestDockerMinioQuotaIntegration` 通过：真实 DinD/MinIO 文件往返、同长度修改、磁盘配额拒写、禁止修改 project quota。
- 发布环境的隔离探针记录：资料和完整会话热读各新增 1 次命中、0 次源数据读取；更新资料后 TTL 未过期即读到新值。探针仅使用随机独立作用域，SQL 测试记录、通知记录及临时程序已清理。
- `/livez`、`/readyz`、网关与独立缓存 healthcheck 通过；所有原有服务保持运行；待补偿 dirty 行为 0。
- 无真实业务压力测试和端到端 LLM 延迟对比，不给出加速百分比。生产环境未刻意停 Redis 做故障演练；故障恢复结论来自隔离集成测试。

## 后续运维

本次保留发布工作树 `/home/xavier/Desktop/github/bkcrab-context-cache-release`。Compose 配置文件来自该目录的 `deploy/docker/`：基础配置、rag、fairqueue、rag-fair、models、rag-serial-eval、reliability，最后加 context-cache。项目目录和 `.env` 仍是 `/home/xavier/Desktop/github/bkcrab/deploy/docker`，相对挂载路径因此不变。

未来重新创建网关时需保留 context-cache overlay；只用旧分支的 Compose 配置会漏掉新版缓存开关。变更前可从当前网关的 Compose labels 读取完整配置文件列表。

需要关闭缓存时使用当前兼容版本并设置 BKCRAB_CONTEXT_CACHE_ENABLED=false。已有技能发布记录仍为权威；回退旧二进制前必须先导出新发布技能到旧格式。不要直接恢复整库覆盖升级后的用户数据，也不要清空调度 Redis。旧技能版本和未引用 blob 目前没有自动垃圾回收，应按留存策略清理并避开活跃回合。
