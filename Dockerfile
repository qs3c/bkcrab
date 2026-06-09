# --- Stage 1: Build web UI ---
FROM node:22-alpine AS web-builder
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
FROM golang:1.25-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Embed the built web UI
COPY --from=web-builder /src/web/out internal/setup/web
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
# Stamp BOTH symbol sets — `main.*` for the legacy `fastclaw version` CLI
# consumer and `internal/buildinfo.*` for the agent runtime + the About
# page in the web UI. Mirrors the Makefile / scripts/release.sh ldflags
# so a docker-built image identifies itself the same way the released
# binary does; without the buildinfo line the About page silently shows
# "dev" on every published image (the symptom that triggered this fix).
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w \
      -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE} \
      -X github.com/fastclaw-ai/fastclaw/internal/buildinfo.Version=${VERSION} \
      -X github.com/fastclaw-ai/fastclaw/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/fastclaw-ai/fastclaw/internal/buildinfo.Date=${DATE}" \
    -o /fastclaw ./cmd/fastclaw
RUN CGO_ENABLED=0 go build -o /fastclaw-migrate-storage ./tools/migrate-storage

# --- Stage 3: Runtime ---
FROM alpine:3.21
RUN apk add --no-cache ca-certificates docker-cli tzdata
COPY --from=go-builder /fastclaw /usr/local/bin/fastclaw
COPY --from=go-builder /fastclaw-migrate-storage /usr/local/bin/fastclaw-migrate-storage

# Default data directory. Override at runtime with FASTCLAW_HOME, but the
# default value here lets `docker run fastclaw/fastclaw` work with no env.
ENV FASTCLAW_HOME=/data/.fastclaw \
    HOME=/data
RUN mkdir -p /data/.fastclaw /data/.fastclaw/skills
VOLUME /data/.fastclaw

# Bundle built-in skills
COPY skills/ /data/.fastclaw/skills/

EXPOSE 18953
ENTRYPOINT ["fastclaw"]
CMD ["gateway"]
