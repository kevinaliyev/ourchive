#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:-0.3.0-local}"
BASE_URL="${2:-https://github.com/OWNER/ourchive/releases/download/v0.3.0}"
OUT_DIR="$(cd "$(dirname "$0")/.." && pwd)/dist"
mkdir -p "$OUT_DIR"

TARGETS=("windows/amd64" "darwin/arm64" "darwin/amd64" "linux/amd64" "linux/arm64")
LDFLAGS="-s -w -X github.com/ourchive/ourchive/internal/updater.Version=${VERSION}"

echo "{" > "$OUT_DIR/manifest.json"
echo "  \"version\": \"${VERSION}\"," >> "$OUT_DIR/manifest.json"
echo "  \"published_at\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"," >> "$OUT_DIR/manifest.json"
echo "  \"assets\": {" >> "$OUT_DIR/manifest.json"

FIRST=1
for t in "${TARGETS[@]}"; do
  GOOS="${t%/*}"
  GOARCH="${t#*/}"
  EXT=""
  [ "$GOOS" = "windows" ] && EXT=".exe"
  OUT="ourchive-agent-${GOOS}-${GOARCH}${EXT}"
  echo "Building ${OUT}..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags "$LDFLAGS" -o "$OUT_DIR/$OUT" ./cmd/agent
  HASH=$(sha256sum "$OUT_DIR/$OUT" | awk '{print $1}')
  [ "$FIRST" -eq 0 ] && echo "," >> "$OUT_DIR/manifest.json"
  FIRST=0
  printf '    "%s": { "url": "%s/%s", "sha256": "%s" }' "$t" "$BASE_URL" "$OUT" "$HASH" >> "$OUT_DIR/manifest.json"
done

echo "" >> "$OUT_DIR/manifest.json"
echo "  }" >> "$OUT_DIR/manifest.json"
echo "}" >> "$OUT_DIR/manifest.json"
echo "Done! Release binaries and manifest.json written to $OUT_DIR"
