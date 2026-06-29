#!/bin/sh
set -e

# Build web
cd web && pnpm build && cd ..

# Copy web output to embed dir
rm -rf internal/setup/web
cp -r web/out internal/setup/web

# Build Go binary. Stamp the same version/commit/date that the release
# Makefile sets — so dev binaries report a real `git describe` tag (e.g.
# v0.32.0-3-gabc1234-dirty) instead of the hard-coded "dev" default.
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO=github.com/qs3c/bkcrab/internal/buildinfo
LDFLAGS="-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE \
 -X $BUILDINFO.Version=$VERSION -X $BUILDINFO.Commit=$COMMIT -X $BUILDINFO.Date=$DATE"
go build -ldflags "$LDFLAGS" -o tmp/bkcrab ./cmd/bkcrab
