#!/usr/bin/env sh
# Angee installer — installs the angee CLI to /usr/local/bin
# Usage: curl https://angee.ai/install.sh | sh
set -e

ANGEE_VERSION=latest
REPO="ang-ee/angee-operator"
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

# build_from_source clones the repo and builds the CLI with Go. Used only as a
# fallback when no prebuilt binary can be downloaded.
build_from_source() {
  echo ""
  echo "  Falling back to building from source."
  echo ""

  if ! command -v go >/dev/null 2>&1; then
    echo "  ✗ Go is required to build from source."
    echo "    Install Go: https://go.dev/dl/"
    echo "    Or download a release manually: https://github.com/${REPO}/releases"
    exit 1
  fi

  echo "  Building angee CLI..."
  SRCDIR="$(mktemp -d)"
  trap 'rm -rf "${TMP:-}" "$SRCDIR"' EXIT

  git clone --depth 1 "https://github.com/${REPO}.git" "$SRCDIR" 2>/dev/null || {
    echo "  ✗ Failed to clone repository."
    echo "    Build from a local checkout instead: make build && ANGEE_DIST_DIR=dist sh scripts/install.sh"
    exit 1
  }

  BUILD_VERSION="$(cd "$SRCDIR" && git describe --tags --always 2>/dev/null || echo dev)"
  (cd "$SRCDIR" && go build -ldflags="-s -w -X github.com/ang-ee/angee-operator/internal/cli.Version=${BUILD_VERSION}" -o angee ./cmd/angee/) || {
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

FILENAME="angee-${OS}-${ARCH}.tar.gz"

# Resolve the download URL WITHOUT the GitHub REST API. The unauthenticated API
# (api.github.com) is rate limited to 60 requests/hr/IP, so on shared or CI
# networks the old "resolve latest via the API" step returned an empty version
# and the installer silently fell back to a source build (which fails without
# Go). GitHub's /releases/latest/download redirect needs no API call and is not
# rate limited.
if [ "$ANGEE_VERSION" = "latest" ]; then
  URL="https://github.com/${REPO}/releases/latest/download/${FILENAME}"
  # Best-effort: read the resolved tag from the /releases/latest redirect, for
  # the progress message only. Never fatal — the download does not depend on it.
  RESOLVED="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" 2>/dev/null \
    | sed -n -E 's#.*/releases/tag/v?([^/]+)/?$#\1#p')" || true
  if [ -n "$RESOLVED" ]; then VERSION_LABEL="v${RESOLVED}"; else VERSION_LABEL="latest"; fi
else
  URL="https://github.com/${REPO}/releases/download/v${ANGEE_VERSION}/${FILENAME}"
  VERSION_LABEL="v${ANGEE_VERSION}"
fi

echo "Installing angee ${VERSION_LABEL} (${OS}/${ARCH})..."

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if ! curl -fsSL "$URL" -o "${TMP}/${FILENAME}"; then
  echo ""
  echo "  ✗ Download failed: ${URL}"
  echo "    No prebuilt binary for ${OS}/${ARCH}, or GitHub is unreachable."
  build_from_source
fi
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
echo "  ✔ angee ${VERSION_LABEL} installed to ${INSTALL_DIR}/angee"
if [ -f "${INSTALL_DIR}/angee-operator" ]; then
  echo "  ✔ angee-operator ${VERSION_LABEL} installed to ${INSTALL_DIR}/angee-operator"
fi
echo ""
echo "  Get started:"
echo "    angee init --dev --yes"
echo "    angee dev"
echo ""
