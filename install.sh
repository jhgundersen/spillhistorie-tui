#!/bin/sh
set -e

REPO="jhgundersen/spillhistorie-tui"
BIN="spillhistorie-tui"

# Detect OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  linux)  GOOS="linux" ;;
  darwin) GOOS="darwin" ;;
  *)      echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)   GOARCH="amd64" ;;
  arm64|aarch64)  GOARCH="arm64" ;;
  *)              echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

# Resolve latest release tag
TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep '"tag_name"' \
  | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"

if [ -z "$TAG" ]; then
  echo "Could not determine latest release version" >&2
  exit 1
fi

FILENAME="${BIN}-${TAG}-${GOOS}-${GOARCH}"
URL="https://github.com/${REPO}/releases/download/${TAG}/${FILENAME}"

# Pick install directory — prefer /usr/local/bin, fall back to ~/.local/bin
if [ -w "/usr/local/bin" ]; then
  INSTALL_DIR="/usr/local/bin"
else
  INSTALL_DIR="${HOME}/.local/bin"
  mkdir -p "$INSTALL_DIR"
fi

echo "Installing ${BIN} ${TAG} (${GOOS}/${GOARCH}) to ${INSTALL_DIR}…"
curl -fsSL "$URL" -o "${INSTALL_DIR}/${BIN}"
chmod +x "${INSTALL_DIR}/${BIN}"
echo "Done. Run: ${BIN}"
