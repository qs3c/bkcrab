FROM thinkany/bkcrab-sandbox:latest

WORKDIR /workspace

# 覆盖基础镜像的 `CMD ["node"]`（继承自 node:22-bookworm-slim）。
# 默认的 `node` REPL 等待标准输入，阻止 E2B 的预配置步骤
# 对启动的沙箱进行快照 — 没有这个，预配置会无限挂起。
# envd 是真正的主进程；`sleep infinity` 只是保持 PID 1 存活以便快照完成。
CMD ["sleep", "infinity"]
