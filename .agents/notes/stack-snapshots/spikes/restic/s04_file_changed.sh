#!/bin/bash
# Spike 4: file modified / deleted / replaced while restic reads it.
. "$(dirname "$0")/env.sh"
REPO=$W/s04-repo
fresh_repo "$REPO"
SRC=$W/s04-src
BIG=$W/s04-big.bin
SIZE_MB=512
if [ ! -f "$BIG" ]; then head -c $((SIZE_MB*1048576)) /dev/urandom > "$BIG"; fi

# case <name> <mutation shell snippet operating on $SRC/data/big.bin>
case_run() {
  name=$1; mut=$2
  # fresh random content per case so dedup cannot short-circuit the throttled upload
  rm -rf "$SRC"; mkdir -p "$SRC/data"; head -c $((SIZE_MB*1048576)) /dev/urandom > "$BIG"; cp "$BIG" "$SRC/data/big.bin"; echo small > "$SRC/data/small.txt"
  echo "== 4-$name: mutation: $mut"
  t0=$(python3 -I -c 'import time;print(time.time())')
  # throttle uploads so the read takes ~10 s; force re-read so no parent shortcut
  "$RESTIC" -r "$REPO" backup "$SRC" --host h --tag case=$name --force --read-concurrency 1 --limit-upload 51200 --json > $W/s04-$name.json 2>$W/s04-$name.err &
  P=$!
  sleep 3
  # show progress at mutation time
  grep '"message_type":"status"' $W/s04-$name.json | tail -1 | python3 -I -c 'import json,sys
l=sys.stdin.read().strip()
if l:
  d=json.loads(l); print("  progress at mutation: percent_done=%.2f bytes_done=%s current=%s" % (d.get("percent_done",0), d.get("bytes_done"), d.get("current_files")))'
  sh -c "$mut"
  wait $P; rc=$?
  t1=$(python3 -I -c 'import time;print(time.time())')
  echo "  [restic exit=$rc] wall=$(python3 -I -c "print(round($t1-$t0,1))")s"
  grep -E '"message_type":"(error|exit_error|verbose_status)"' $W/s04-$name.json $W/s04-$name.err | sed 's/^[^:]*://' | head -5
  grep '"message_type":"summary"' $W/s04-$name.json | python3 -I -c 'import json,sys
l=sys.stdin.read().strip()
if l:
  d=json.loads(l); print("  summary: files_new=%s files_changed=%s total_bytes_processed=%s snapshot=%s" % (d["files_new"],d["files_changed"],d["total_bytes_processed"],d["snapshot_id"][:8]))'
  SNAP=$(grep '"message_type":"summary"' $W/s04-$name.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
  if [ -n "$SNAP" ]; then
    "$RESTIC" -r "$REPO" ls --json "$SNAP" | grep '"name":"big.bin"' | python3 -I -c 'import json,sys
l=sys.stdin.read().strip()
if l:
  d=json.loads(l); print("  stored node: size=%s mtime=%s" % (d.get("size"), d.get("mtime")))
else: print("  stored node: big.bin ABSENT")'
    stored=$("$RESTIC" -r "$REPO" dump "$SNAP" "$SRC/data/big.bin" 2>/dev/null | shasum -a 256 | cut -c1-16)
    orig=$(shasum -a 256 < "$BIG" | cut -c1-16)
    now=$( [ -f "$SRC/data/big.bin" ] && shasum -a 256 < "$SRC/data/big.bin" | cut -c1-16 || echo absent)
    echo "  sha16: stored=$stored original=$orig on-disk-after=$now"
  fi
}

export SRC
case_run append    'head -c 1048576 /dev/urandom >> "$SRC/data/big.bin"'
case_run inplace   'dd if=/dev/urandom of="$SRC/data/big.bin" bs=1048576 count=4 conv=notrunc 2>/dev/null'
case_run inplace_tail 'dd if=/dev/urandom of="$SRC/data/big.bin" bs=1048576 seek=500 count=4 conv=notrunc 2>/dev/null'
case_run truncate  ': > "$SRC/data/big.bin"'
case_run delete    'rm -f "$SRC/data/big.bin"'
case_run rename    'head -c 2097152 /dev/urandom > "$SRC/data/.big.new" && mv "$SRC/data/.big.new" "$SRC/data/big.bin"'
case_run torn      'dd if=/dev/urandom of="$SRC/data/big.bin" bs=1048576 count=4 conv=notrunc 2>/dev/null; dd if=/dev/urandom of="$SRC/data/big.bin" bs=1048576 seek=500 count=4 conv=notrunc 2>/dev/null'
case_run delete_small_before_read 'rm -f "$SRC/data/small.txt"'
