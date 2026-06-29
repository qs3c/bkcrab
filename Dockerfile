# --- 阶段 1：构建 Web UI ---
FROM --platform=$BUILDPLATFORM node:22-alpine AS web-builder
WORKDIR /src/web
# 锁定 pnpm：`latest` 开始拉取 v11，这使得
# pnpm-workspace.yaml 的 onlyBuiltDependencies 允许列表在
# --frozen-lockfile 下失效（v11 需要交互式 `pnpm approve-builds`
# 步骤，但在非 TTY 的 Docker 构建中无法运行），导致
# 镜像构建因 msw/sharp/unrs-resolver 的 ERR_PNPM_IGNORED_BUILDS 而失败。
RUN corepack enable && corepack prepare pnpm@10.15.0 --activate
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ .
RUN pnpm build

# --- 阶段 2：构建 Go 二进制文件 ---
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 嵌入构建好的 Web UI
COPY --from=web-builder /src/web/out internal/setup/web
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
# 标记两个符号集 — `main.*` 用于旧的 `bkcrab version` CLI
# 调用者，`internal/buildinfo.*` 用于 agent 运行时和 Web UI 中的关于
# 页面。镜像 Makefile / scripts/release.sh 的 ldflags，
# 使得 Docker 构建的镜像与发布的二进制文件以相同方式标识自己；
# 没有 buildinfo 行，关于页面会在每个发布的镜像上静默显示
# "dev"（触发此修复的症状）。
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE} \
      -X github.com/qs3c/bkcrab/internal/buildinfo.Version=${VERSION} \
      -X github.com/qs3c/bkcrab/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/qs3c/bkcrab/internal/buildinfo.Date=${DATE}" \
    -o /bkcrab ./cmd/bkcrab
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /bkcrab-migrate-storage ./tools/migrate-storage

# --- 阶段 3：运行时 ---
FROM alpine:3.21
RUN apk add --no-cache ca-certificates docker-cli tzdata
COPY --from=go-builder /bkcrab /usr/local/bin/bkcrab
COPY --from=go-builder /bkcrab-migrate-storage /usr/local/bin/bkcrab-migrate-storage

# 默认数据目录。数据库启动仍需通过 BKCRAB_STORAGE_DSN 提供显式的 MySQL DSN。
ENV BKCRAB_HOME=/data/.bkcrab \
    HOME=/data
RUN mkdir -p /data/.bkcrab /data/.bkcrab/skills
VOLUME /data/.bkcrab

# 捆绑内置技能
COPY skills/ /data/.bkcrab/skills/

EXPOSE 18953
ENTRYPOINT ["bkcrab"]
CMD ["gateway"]
