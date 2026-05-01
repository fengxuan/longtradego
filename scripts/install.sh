#!/usr/bin/env bash
set -euo pipefail

REPO="${LONGTRADEGO_RELEASE_REPO:-jianfengxuan/longtradego}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
REQUESTED_VERSION="${1:-latest}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

if [[ "$OS" != "darwin" ]]; then
  echo "unsupported os: $OS (this installer currently supports macOS only)" >&2
  exit 1
fi

case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

if [[ "$REQUESTED_VERSION" == "latest" ]]; then
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep -m1 '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
else
  if [[ "$REQUESTED_VERSION" == v* ]]; then
    TAG="$REQUESTED_VERSION"
  else
    TAG="v${REQUESTED_VERSION}"
  fi
fi

if [[ -z "$TAG" ]]; then
  echo "failed to resolve release tag" >&2
  exit 1
fi

VERSION="${TAG#v}"
ASSET="longtradego_${VERSION}_darwin_${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
ASSET_URL="${BASE_URL}/${ASSET}"
CHECKSUM_URL="${BASE_URL}/checksums.txt"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

curl -fsSL "$ASSET_URL" -o "$TMP_DIR/$ASSET"
curl -fsSL "$CHECKSUM_URL" -o "$TMP_DIR/checksums.txt"

EXPECTED_SHA="$(grep " $ASSET$" "$TMP_DIR/checksums.txt" | awk '{print $1}')"
if [[ -z "$EXPECTED_SHA" ]]; then
  echo "checksum not found for $ASSET" >&2
  exit 1
fi

ACTUAL_SHA="$(shasum -a 256 "$TMP_DIR/$ASSET" | awk '{print $1}')"
if [[ "$EXPECTED_SHA" != "$ACTUAL_SHA" ]]; then
  echo "checksum mismatch: expected=$EXPECTED_SHA actual=$ACTUAL_SHA" >&2
  exit 1
fi

tar -xzf "$TMP_DIR/$ASSET" -C "$TMP_DIR"
BIN_PATH="$TMP_DIR/longtradego_${VERSION}_darwin_${ARCH}/longtradego"
if [[ ! -f "$BIN_PATH" ]]; then
  echo "binary not found in archive: $BIN_PATH" >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
install -m 0755 "$BIN_PATH" "$INSTALL_DIR/longtradego"

echo "installed longtradego ${TAG} -> ${INSTALL_DIR}/longtradego"
if ! command -v longtradego >/dev/null 2>&1; then
  echo "tip: add ${INSTALL_DIR} to PATH"
fi
