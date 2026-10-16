#!/usr/bin/env sh
# install.sh — download the prebuilt dozzle-shim binary for this OS/arch from a
# GitHub Release and install it. No clone, no Go, no Docker.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lyminhtanh/dozzle-shim/main/install.sh | sh
#   # pin a version:    VERSION=v0.3.0 sh install.sh
#   # custom location:  BINDIR=/usr/local/bin sh install.sh
set -eu

REPO="lyminhtanh/dozzle-shim"
BINDIR="${BINDIR:-$HOME/.local/bin}"
VERSION="${VERSION:-latest}"

# ---- detect OS/arch and map to the release asset naming (build.sh) ----
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux)  goos=linux ;;
  darwin) goos=darwin ;;
  *) echo "unsupported OS: $os (use the Windows .exe asset manually)" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) goarch=amd64 ;;
  arm64|aarch64) goarch=arm64 ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

asset="dozzle-shim-${goos}-${goarch}"

# ---- resolve the download URL ----
if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

echo "Downloading ${asset} (${VERSION}) ..."
mkdir -p "$BINDIR"
dest="${BINDIR}/dozzle-shim"

if command -v curl >/dev/null 2>&1; then
  curl -fSL "$url" -o "$dest"
elif command -v wget >/dev/null 2>&1; then
  wget -O "$dest" "$url"
else
  echo "need curl or wget" >&2; exit 1
fi

chmod +x "$dest"
echo "Installed: $dest"

# ---- PATH hint ----
case ":$PATH:" in
  *":$BINDIR:"*) : ;;
  *) echo "Note: $BINDIR is not on your PATH. Add it, e.g.:"
     echo "  echo 'export PATH=\"$BINDIR:\$PATH\"' >> ~/.profile" ;;
esac

echo
echo "Run it:"
echo "  dozzle-shim -logs ./logs -glob '*.log' -listen :8080   # files only"
echo "  dozzle-shim -logs ./logs -docker unix:///var/run/docker.sock -listen :8080  # + Docker"
echo "then open http://localhost:8080"
