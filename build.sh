#!/usr/bin/env bash
# build.sh — cross-compile self-contained dozzle-shim binaries for macOS, Linux,
# and Windows. The Dozzle UI is embedded (go:embed), so each binary is fully
# standalone: recipients need NO Go, Node, bun, Docker, or the UI dist — just
# the one file.
#
# Usage:
#   ./build.sh              # build all targets into dist/
#   ./build.sh darwin/arm64 # build a single GOOS/GOARCH
#   VERSION=1.2.3 ./build.sh
#
# Output: dist/dozzle-shim-<os>-<arch>[.exe]
set -euo pipefail

cd "$(dirname "$0")"

OUT_DIR="dist"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

# Default target matrix. Covers Intel + Apple Silicon Macs, x86-64 + ARM Linux,
# and 64-bit Windows. Override by passing one or more GOOS/GOARCH args.
TARGETS=(
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
)
if [ "$#" -gt 0 ]; then
  TARGETS=("$@")
fi

mkdir -p "$OUT_DIR"
echo "Building dozzle-shim ${VERSION}"

for target in "${TARGETS[@]}"; do
  os="${target%%/*}"
  arch="${target##*/}"
  out="${OUT_DIR}/dozzle-shim-${os}-${arch}"
  [ "$os" = "windows" ] && out="${out}.exe"

  echo "  → ${out}"
  # CGO disabled → a static binary with no libc dependency, so it runs on any
  # machine of that OS/arch without extra runtime.
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$out" .
done

echo "Done. Binaries in ${OUT_DIR}/:"
ls -lh "$OUT_DIR"
