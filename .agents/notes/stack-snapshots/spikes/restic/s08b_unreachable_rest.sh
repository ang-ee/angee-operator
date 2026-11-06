#!/bin/bash
# Spike 8 addendum: how long does restic retry an unreachable REST backend before giving up?
. "$(dirname "$0")/env.sh"
OUT=$D/s08b.out
echo "start $(date '+%H:%M:%S')" > "$OUT"
s=$(date +%s)
/opt/homebrew/bin/timeout 1500 "$RESTIC" -r rest:http://127.0.0.1:18999/ snapshots --json > $W/s08b.stdout 2> $W/s08b.stderr
rc=$?
e=$(date +%s)
echo "end $(date '+%H:%M:%S') exit=$rc elapsed=$((e-s))s (124 = outer 25 min timeout hit)" >> "$OUT"
echo "retries: $(grep -c 'retrying after' $W/s08b.stderr)" >> "$OUT"
grep -v 'retrying after' $W/s08b.stderr | tail -3 >> "$OUT"
