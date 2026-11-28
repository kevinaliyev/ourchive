#!/usr/bin/env bash
# Generates client/build/ (platform Taskfiles, Info.plist, NSIS script, .desktop file, icons) with
# the Wails CLI, using build/config.ourchive.yml for product metadata and the ourchive:// scheme.
# Run once after cloning, and again after upgrading Wails. Commit the result.
#
#   go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0
#   ./scripts/bootstrap-build-assets.sh   (Windows PowerShell: .\scripts\bootstrap-build-assets.ps1)
set -euo pipefail
cd "$(dirname "$0")/.."

command -v wails3 >/dev/null || { echo "wails3 not found: go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0" >&2; exit 1; }

wails3 generate build-assets \
  -dir build \
  -name ourchive \
  -binaryname ourchive \
  -productname "Ourchive" \
  -productcompany "Ourchive" \
  -productidentifier com.ourchive.client \
  -productdescription "Ourchive desktop client" \
  -productversion 0.4.0 \
  -productcopyright "(c) Ourchive contributors" \
  -silent

cp build/config.ourchive.yml build/config.yml
wails3 update build-assets -dir build -config build/config.yml -silent
echo "Build assets generated in client/build (ourchive:// registered). Review and commit them."
