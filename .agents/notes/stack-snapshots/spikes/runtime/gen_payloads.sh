#!/usr/bin/env bash
# Regenerate the random payloads the spikes read (deleted after the run to
# free ~576 MiB), and rebuild the Go launcher. Hashes will differ from
# RESULTS.md; every script compares against the payload's own sha256.
set -eu
D=<scratchpad>/spikes/runtime
mkdir -p "$D/data" "$D/bin"
head -c 67108864 /dev/urandom >"$D/data/p64"
head -c 536870912 /dev/urandom >"$D/data/p512"
(cd "$D/gorun" && go build -o ../bin/gorun . && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../bin/gorun-linux .)
shasum -a 256 "$D/data/p64" "$D/data/p512"
