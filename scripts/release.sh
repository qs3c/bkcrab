#!/bin/bash
set -euo pipefail

# BkClaw Release Script
# Usage: ./scripts/release.sh v0.1.0

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version>"
    echo "Example: $0 v0.1.0"
    exit 1
fi

# Strip 'v' prefix for directory names
VER="${VERSION#v}"
BINARY="bkclaw"
DIST_DIR="dist"
MODULE="github.com/qs3c/bkclaw"

echo "⚡ Building BkClaw ${VERSION}"
echo ""

# Clean
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

# Build matrix. linux/armv7 was dropped because a transitive lark/feishu
# SDK assigns math.MaxInt64 into an `int` field, which overflows on
# 32-bit. Keep this list aligned with what actually ships on Releases.
PLATFORMS=(
    "darwin/amd64"
    "darwin/arm64"
    "linux/amd64"
    "linux/arm64"
    "windows/amd64"
    "windows/arm64"
)

# Stamp the same ldflags the Makefile uses so both main.* and
# buildinfo.* read the real release tag — without this the About page
# (which reads buildinfo.Version) would show "dev" even after upgrade.
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO="${MODULE}/internal/buildinfo"
LDFLAGS="-s -w \
 -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE} \
 -X ${BUILDINFO}.Version=${VERSION} -X ${BUILDINFO}.Commit=${COMMIT} -X ${BUILDINFO}.Date=${DATE}"

for platform in "${PLATFORMS[@]}"; do
    os="${platform%/*}"
    arch="${platform#*/}"

    goarch="$arch"
    goarm=""

    output="${BINARY}"
    [ "$os" = "windows" ] && output="${BINARY}.exe"

    echo "  Building ${os}/${arch}..."

    env GOOS="$os" GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0 \
        go build -ldflags="${LDFLAGS}" \
        -o "${DIST_DIR}/${output}" \
        "./cmd/${BINARY}"

    # Asset name is intentionally version-less (e.g. bkclaw_linux_amd64.tar.gz)
    # so the in-binary `bkclaw upgrade` command (cmd_version.go) can find
    # it via a stable suffix without parsing release tags.
    pkg_name="${BINARY}_${os}_${arch}"
    pkg_dir="${DIST_DIR}/${pkg_name}"
    mkdir -p "$pkg_dir"

    cp "${DIST_DIR}/${output}" "$pkg_dir/"
    cp LICENSE "$pkg_dir/" 2>/dev/null || true
    cp README.md "$pkg_dir/"

    cd "$DIST_DIR"
    if [ "$os" = "windows" ]; then
        zip -q "${pkg_name}.zip" -r "${pkg_name}/"
    else
        tar czf "${pkg_name}.tar.gz" "${pkg_name}/"
    fi
    cd ..

    # Cleanup
    rm -rf "$pkg_dir" "${DIST_DIR}/${output}"
done

# Generate checksums
cd "$DIST_DIR"
shasum -a 256 *.tar.gz *.zip 2>/dev/null > checksums.txt
cd ..

echo ""
echo "✓ Release artifacts in ${DIST_DIR}/:"
ls -lh "$DIST_DIR"/*.tar.gz "$DIST_DIR"/*.zip 2>/dev/null
echo ""
echo "Checksums:"
cat "$DIST_DIR/checksums.txt"
echo ""
echo "Next steps:"
echo "  1. git tag ${VERSION}"
echo "  2. git push origin ${VERSION}"
echo "  3. Create GitHub release and upload ${DIST_DIR}/* files"
echo ""
echo "Or use gh CLI:"
echo "  gh release create ${VERSION} ${DIST_DIR}/*.tar.gz ${DIST_DIR}/*.zip ${DIST_DIR}/checksums.txt --title \"BkClaw ${VERSION}\" --generate-notes"
