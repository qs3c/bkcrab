#!/usr/bin/env bash
# 构建 agent 执行沙箱使用的 bkclaw-sandbox 运行时镜像。
# 捆绑 Python + Node + Camoufox（反检测 Firefox），
# 使 camoufox-cli 技能在第一回合无需任何 pip/npm 往返即可工作。
#
# 用法：
#   deploy/docker/sandbox/build.sh                      # 本地构建，标记 latest
#   deploy/docker/sandbox/build.sh -t v1                # 自定义标签
#   deploy/docker/sandbox/build.sh --push               # 构建 + 推送
#   deploy/docker/sandbox/build.sh --platform linux/amd64,linux/arm64 --push
#                                                       # 多架构 buildx
#
# 构建后，通过设置 → 沙箱 → 镜像或在引导期间将网关指向它。
# 默认：thinkany/bkclaw-sandbox:latest。

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/../../.." && pwd)

IMAGE_NAME=${IMAGE_NAME:-thinkany/bkclaw-sandbox}
TAG=latest
PUSH=0
PLATFORM=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -t|--tag)
      TAG="$2"; shift 2 ;;
    -i|--image)
      IMAGE_NAME="$2"; shift 2 ;;
    --push)
      PUSH=1; shift ;;
    --platform)
      PLATFORM="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,18p' "$0"; exit 0 ;;
    *)
      echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

REF="${IMAGE_NAME}:${TAG}"

echo "==> building ${REF}"
echo "    context: ${SCRIPT_DIR}"

if [[ -n "$PLATFORM" ]]; then
  # buildx 路径 — 多架构和 --push 推送到注册表时需要，无需先加载到本地守护进程。
  docker buildx build \
    --platform "$PLATFORM" \
    $([[ $PUSH -eq 1 ]] && echo --push || echo --load) \
    -t "$REF" \
    "$SCRIPT_DIR"
else
  docker build -t "$REF" "$SCRIPT_DIR"
  if [[ $PUSH -eq 1 ]]; then
    echo "==> pushing ${REF}"
    docker push "$REF"
  fi
fi

echo "==> done: ${REF}"
echo
echo "Use it via Settings → Sandbox → Image, or set during onboard:"
echo "    ${REF}"
