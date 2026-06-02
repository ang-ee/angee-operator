#!/usr/bin/env sh
# Angee installer — installs the angee CLI to /usr/local/bin
# Usage: curl https://angee.ai/install.sh | sh
set -e

ANGEE_VERSION=latest
REPO="ang-ee/angee"
INSTALL_DIR="${ANGEE_INSTALL_DIR:-/usr/local/bin}"

install_bin() {
  src="$1"
  dst="$2"
  # Stage a sibling file in the destination directory, then atomically
  # rename it over the target. This allocates a fresh inode, which
  # invalidates macOS's per-inode code-signature cache — overwriting a
  # signed binary in place would otherwise trigger AMFI's "load code
  # signature error 2" because the kernel keeps stale signature pages
  # for the old contents. Staging in the same directory guarantees the
  # rename is on a single filesystem and therefore atomic.
  stage="${dst}.new.$$"
  if [ -w "$INSTALL_DIR" ]; then
    cp "$src" "$stage"
    chmod +x "$stage"
    mv -f "$stage" "$dst"
  else
    sudo cp "$src" "$stage"
    sudo chmod +x "$stage"
    sudo mv -f "$stage" "$dst"
  fi
}

# Detect OS and arch
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

case "$OS" in
  linux|darwin) ;;
  *)
    echo "Unsupported OS: $OS"
    echo "On Windows, download from: https://github.com/${REPO}/releases"
    exit 1
    ;;
esac

if [ -n "${ANGEE_DIST_DIR:-}" ]; then
  DIST_DIR="${ANGEE_DIST_DIR%/}"
  CLI_BIN="${DIST_DIR}/angee"
  OPERATOR_BIN="${DIST_DIR}/angee-operator"

  if [ ! -f "$CLI_BIN" ]; then
    echo "  ✗ No angee binary found at ${CLI_BIN}"
    echo "    Run: make build"
    exit 1
  fi

  install_bin "$CLI_BIN" "${INSTALL_DIR}/angee"
  if [ -f "$OPERATOR_BIN" ]; then
    install_bin "$OPERATOR_BIN" "${INSTALL_DIR}/angee-operator"
  fi

  echo ""
  echo "  ✔ angee installed to ${INSTALL_DIR}/angee from ${DIST_DIR}"
  if [ -f "$OPERATOR_BIN" ]; then
    echo "  ✔ angee-operator installed to ${INSTALL_DIR}/angee-operator from ${DIST_DIR}"
  fi
  echo ""
  echo "  Get started:"
  echo "    angee init --dev --yes"
  echo "    angee dev"
  echo ""
  exit 0
fi

# Resolve latest version if needed
if [ "$ANGEE_VERSION" = "latest" ]; then
  ANGEE_VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | grep '"tag_name"' | sed -E 's/.*"v([^"]+)".*/\1/')" || true
fi

if [ -z "$ANGEE_VERSION" ]; then
  echo ""
  echo "  No releases found on GitHub. Building from source instead."
  echo ""

  # Check prerequisites
  if ! command -v go >/dev/null 2>&1; then
    echo "  ✗ Go is required to build from source."
    echo "    Install Go: https://go.dev/dl/"
    echo "    Or set ANGEE_VERSION to a specific release tag."
    exit 1
  fi

  echo "  Building angee CLI..."
  SRCDIR="$(mktemp -d)"
  trap 'rm -rf "$SRCDIR"' EXIT

  git clone --depth 1 "https://github.com/${REPO}.git" "$SRCDIR" 2>/dev/null || {
    echo "  ✗ Failed to clone repository. Install from local checkout instead:"
    echo "    make build-cli && sudo cp dist/angee /usr/local/bin/"
    exit 1
  }

  BUILD_VERSION="$(cd "$SRCDIR" && git describe --tags --always 2>/dev/null || echo dev)"
  (cd "$SRCDIR" && go build -ldflags="-s -w -X github.com/fyltr/angee/internal/cli.Version=${BUILD_VERSION}" -o angee ./cmd/angee/) || {
    echo "  ✗ Build failed."
    exit 1
  }

  install_bin "${SRCDIR}/angee" "${INSTALL_DIR}/angee"

  echo ""
  echo "  ✔ angee ${BUILD_VERSION} installed to ${INSTALL_DIR}/angee"
  echo ""
  echo "  Get started:"
  echo "    angee init --dev --yes"
  echo "    angee dev"
  echo ""
  exit 0
fi

FILENAME="angee-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/v${ANGEE_VERSION}/${FILENAME}"

echo "Installing angee v${ANGEE_VERSION} (${OS}/${ARCH})..."

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

curl -fsSL "$URL" -o "${TMP}/${FILENAME}" || {
  echo ""
  echo "  ✗ Download failed: ${URL}"
  echo "    The release may not have a binary for ${OS}/${ARCH}."
  echo "    Build from source: git clone https://github.com/${REPO} && cd angee && make build-cli"
  exit 1
}
tar -xzf "${TMP}/${FILENAME}" -C "$TMP"

# As of v0.2.0+ the tarball contains plain `angee` and `angee-operator`
# binaries. Older releases shipped `angee-${OS}-${ARCH}` instead, so we
# fall back to that layout for backward compatibility.
if [ -f "${TMP}/angee" ]; then
  CLI_BIN="${TMP}/angee"
elif [ -f "${TMP}/angee-${OS}-${ARCH}" ]; then
  CLI_BIN="${TMP}/angee-${OS}-${ARCH}"
else
  echo "  ✗ Unexpected tarball layout — no angee binary found in:"
  ls -1 "${TMP}"
  exit 1
fi

# Install CLI; install operator too when the tarball ships it (>=v0.2.0).
install_bin "$CLI_BIN" "${INSTALL_DIR}/angee"
if [ -f "${TMP}/angee-operator" ]; then
  install_bin "${TMP}/angee-operator" "${INSTALL_DIR}/angee-operator"
fi

echo ""
echo "  ✔ angee v${ANGEE_VERSION} installed to ${INSTALL_DIR}/angee"
if [ -f "${INSTALL_DIR}/angee-operator" ]; then
  echo "  ✔ angee-operator v${ANGEE_VERSION} installed to ${INSTALL_DIR}/angee-operator"
fi
echo ""
echo "  Get started:"
echo "    angee init --dev --yes"
echo "    angee dev"
echo ""
