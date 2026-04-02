#!/bin/sh
set -eu

REPO="paulbellamy/mcp"
INSTALL_DIR="${HOME}/.local/bin"

# Skip if already installed
if command -v mcp >/dev/null 2>&1; then
  exit 0
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  arm64)   ARCH="arm64" ;;
  *)       echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

if [ -z "${VERSION:-}" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed 's/.*"tag_name": *"//;s/".*//')
  if [ -z "$VERSION" ]; then
    echo "Failed to determine latest version" >&2
    exit 1
  fi
fi

URL="https://github.com/${REPO}/releases/download/${VERSION}/mcp_${VERSION}_${OS}_${ARCH}.tar.gz"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading mcp ${VERSION} for ${OS}/${ARCH}..." >&2
curl -fsSL "$URL" -o "${TMPDIR}/mcp.tar.gz"
tar xzf "${TMPDIR}/mcp.tar.gz" -C "$TMPDIR"

mkdir -p "$INSTALL_DIR"
mv "${TMPDIR}/mcp" "${INSTALL_DIR}/mcp"
chmod +x "${INSTALL_DIR}/mcp"

echo "Installed mcp to ${INSTALL_DIR}/mcp" >&2

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) echo "WARNING: ${INSTALL_DIR} is not in your PATH. Add it with:" >&2
     echo "  export PATH=\"${INSTALL_DIR}:\$PATH\"" >&2 ;;
esac
