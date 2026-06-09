FROM thinkany/fastclaw-sandbox:latest

WORKDIR /workspace

# Override the base image's `CMD ["node"]` (inherited from node:22-bookworm-slim).
# The default `node` REPL waits on stdin and prevents E2B's provisioning step
# from snapshotting the booted sandbox — provisioning hangs indefinitely
# without this. envd is the real main process; `sleep infinity` just keeps
# PID 1 alive so the snapshot can complete.
CMD ["sleep", "infinity"]
