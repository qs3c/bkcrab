# --- Stage 1: Build web UI ---
FROM --platform=$BUILDPLATFORM node:22-alpine AS web-builder
WORKDIR /src/web
# Pin pnpm: `latest` started pulling v11, which made
# pnpm-workspace.yaml's onlyBuiltDependencies allow-list ineffective
# under --frozen-lockfile (v11 wants an interactive `pnpm approve-builds`
# step that has nowhere to run in a non-TTY Docker build), failing the
# image build with ERR_PNPM_IGNORED_BUILDS on msw/sharp/unrs-resolver.
RUN corepack enable && corepack prepare pnpm@10.15.0 --activate
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ .
RUN pnpm build

# --- Stage 2: Build Go binary ---
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Embed the built web UI
COPY --from=web-builder /src/web/out internal/setup/web
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
# Stamp BOTH symbol sets — `main.*` for the legacy `bkclaw version` CLI
# consumer and `internal/buildinfo.*` for the agent runtime + the About
# page in the web UI. Mirrors the Makefile / scripts/release.sh ldflags
# so a docker-built image identifies itself the same way the released
# binary does; without the buildinfo line the About page silently shows
# "dev" on every published image (the symptom that triggered this fix).
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE} \
      -X github.com/qs3c/bkclaw/internal/buildinfo.Version=${VERSION} \
      -X github.com/qs3c/bkclaw/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/qs3c/bkclaw/internal/buildinfo.Date=${DATE}" \
    -o /bkclaw ./cmd/bkclaw
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /bkclaw-migrate-storage ./tools/migrate-storage

# --- Stage 3: Runtime ---
FROM alpine:3.21
RUN apk add --no-cache ca-certificates docker-cli tzdata
COPY --from=go-builder /bkclaw /usr/local/bin/bkclaw
COPY --from=go-builder /bkclaw-migrate-storage /usr/local/bin/bkclaw-migrate-storage

# Default data directory. Database startup still requires an explicit MySQL
# DSN via BKCLAW_STORAGE_DSN.
ENV BKCLAW_HOME=/data/.bkclaw \
    HOME=/data
RUN mkdir -p /data/.bkclaw /data/.bkclaw/skills
VOLUME /data/.bkclaw

# Bundle built-in skills
COPY skills/ /data/.bkclaw/skills/

EXPOSE 18953
ENTRYPOINT ["bkclaw"]
CMD ["gateway"]
