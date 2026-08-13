#!/usr/bin/env sh
# Regenerate the vendored process-compose Homebrew formula from upstream.
#
# Usage: scripts/update-process-compose-formula.sh [output-path]
#
#   output-path  where to write the formula (default: ./process-compose.rb)
#
# Why we vendor rather than `depends_on "f1bonacc1/tap/process-compose"`:
# Homebrew 6 refuses to load a formula from an untrusted tap, and only the
# formula the user explicitly asked for is auto-trusted. A cross-tap dependency
# therefore makes every install stop with a trust prompt for a third-party tap
# the user never named. Carrying our own copy keeps that prompt to our own tap.
#
# This reads the version and checksums out of upstream's formula and emits a
# clean one rather than copying it: upstream's is GoReleaser output that fails
# `brew audit --strict` (methods defined inside blocks, nested conditionals).
#
# Upstream is Apache-2.0; the header written below preserves attribution.
# Run this by hand when process-compose cuts a release worth picking up — the
# angee release workflow does not touch this formula.
set -eu

UPSTREAM="F1bonacc1/homebrew-tap"
PROJECT="F1bonacc1/process-compose"
OUT="${1:-process-compose.rb}"

BODY="$(gh api "repos/${UPSTREAM}/contents/process-compose.rb" --jq '.content' | base64 -d)"
if [ -z "$BODY" ]; then
  echo "error: could not read process-compose.rb from ${UPSTREAM}" >&2
  exit 1
fi

VERSION="$(printf '%s\n' "$BODY" | awk -F'"' '/^  version /{ print $2; exit }')"
if [ -z "$VERSION" ]; then
  echo "error: could not parse a version out of the upstream formula" >&2
  exit 1
fi

# Upstream pairs each `url` with the `sha256` on the following line. Flatten
# those into "<asset-filename> <sha256>" so we can look up the platforms we
# ship without depending on their block ordering.
PAIRS="$(printf '%s\n' "$BODY" | awk -F'"' '
  /^ *url "/    { n = split($2, p, "/"); asset = p[n]; next }
  /^ *sha256 "/ { if (asset != "") { print asset, $2; asset = "" } }
')"

sha_for() {
  sum="$(printf '%s\n' "$PAIRS" | awk -v name="$1" '$1 == name { print $2; exit }')"
  if [ -z "$sum" ]; then
    echo "error: upstream formula has no checksum for $1" >&2
    exit 1
  fi
  printf '%s' "$sum"
}

# Only the platforms angee itself ships. Upstream also builds 32-bit linux/arm,
# which Homebrew does not support.
DARWIN_AMD64="$(sha_for "process-compose_darwin_amd64.tar.gz")"
DARWIN_ARM64="$(sha_for "process-compose_darwin_arm64.tar.gz")"
LINUX_AMD64="$(sha_for "process-compose_linux_amd64.tar.gz")"
LINUX_ARM64="$(sha_for "process-compose_linux_arm64.tar.gz")"

BASE="https://github.com/${PROJECT}/releases/download/v${VERSION}"

mkdir -p "$(dirname "$OUT")"
cat > "$OUT" <<FORMULA
# typed: false
# frozen_string_literal: true

# Vendored from https://github.com/${UPSTREAM} (Apache-2.0), reformatted to
# pass \`brew audit --strict\`.
#
# Why a copy rather than \`depends_on "f1bonacc1/tap/process-compose"\`:
# Homebrew 6 refuses to load a formula from an untrusted tap, and auto-trusts
# only the formula named on the command line. A cross-tap dependency would make
# every install stop for a trust prompt on a tap the user never named.
#
# Regenerate with scripts/update-process-compose-formula.sh in
# ang-ee/angee-operator. Do not edit by hand.
class ProcessCompose < Formula
  desc "Scheduler and orchestrator for non-containerized applications"
  homepage "https://github.com/${PROJECT}"
  license "Apache-2.0"

  on_macos do
    on_intel do
      url "${BASE}/process-compose_darwin_amd64.tar.gz"
      sha256 "${DARWIN_AMD64}"
    end
    on_arm do
      url "${BASE}/process-compose_darwin_arm64.tar.gz"
      sha256 "${DARWIN_ARM64}"
    end
  end

  on_linux do
    on_intel do
      url "${BASE}/process-compose_linux_amd64.tar.gz"
      sha256 "${LINUX_AMD64}"
    end
    on_arm do
      url "${BASE}/process-compose_linux_arm64.tar.gz"
      sha256 "${LINUX_ARM64}"
    end
  end

  def install
    bin.install "process-compose"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/process-compose version")
  end
end
FORMULA

echo "wrote $OUT for process-compose ${VERSION}"
