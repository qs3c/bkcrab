# 2026-09-14 单机可靠性部署记录

服务器：`100.93.173.10`；继续 Docker Compose，保留已有 RAG、fairqueue、models、serial-eval overlays。分支 `codex/sandbox-reliability` 包含原服务器 `04c499f` 上下文缓存改动。

## 已启用

- 全局最多 4 个存活沙箱，每用户 2 个；最多 32 个等待调用，等待超时 60 秒。
- 每沙箱 1 CPU、2 GiB RAM、256 PID；网关 2 CPU/2 GiB；DinD 6 CPU/9 GiB。
- 工作区独立 40 GiB ext4 project-quota 文件系统，每用户 10 GiB/100000 inode，技能目录计入同一用户。工作区 MinIO 对象独立限制为每用户 10 GiB/100000 对象。
- DinD 镜像和可写层使用独立 20 GiB 文件系统。
- 保留默认 seccomp 限制并阻止项目配额修改 ioctl；拒绝内部网络访问 DinD 管理端口。
- 流式持久化、同大小修改检测、执行期间不回收、按需回收空闲名额、旧实例清理、输出上限。
- watchdog 每 30 秒检查；备份每天北京时间 03:30，保留 3 份。

网关镜像 `bkcrab/bkcrab:reliability-20260914` 的二进制版本为 `175b699`；后续提交是部署脚本、集成测试及文档，未改变应用二进制。旧镜像保留为 `bkcrab/bkcrab:before-reliability-20260914`，原 named volumes 未删除。

## 验证

- Windows 单元测试：sandbox、workspace、gateway、config 通过。
- Linux `go test -race ./internal/sandbox ./internal/workspace` 通过。
- 在真实网关环境执行 `TestDockerMinioQuotaIntegration` 两次通过：实际创建 Docker 沙箱、MinIO 往返、同大小修改、EDQUOT 拒写、项目编号修改拒绝。
- DinD 内真实容器检查：1 CPU、2 GiB、256 PID 生效；管理端口连接被拒绝，网关调用仍正常。
- `/livez`、`/readyz` 和 Docker healthcheck 通过；原有其他服务保持运行。
- 首份备份 `/srv/bkcrab-backups/20260914T041253Z` 约 730 MiB；gzip 与 tar 完整性检查通过。
- 将 SQL 备份导入网络隔离、tmpfs 存储的临时 MySQL，成功恢复并确认 `bkcrab` 数据库含 63 张表；测试实例随后清理。

部署前数据库备份另存于 `/home/xavier/bkcrab-backups/reliability-20260913-230201`。定时备份保存在服务器本机，尚未配置异地复制；在线文件备份并非跨数据库/文件的原子快照。详细恢复和限制说明见 [sandbox-reliability.md](../sandbox-reliability.md)。
