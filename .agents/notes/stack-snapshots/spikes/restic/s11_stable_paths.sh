#!/bin/bash
# Spike 11: stable (host-location independent) in-snapshot paths for FILE members.
. "$(dirname "$0")/env.sh"
REPO=$W/s11-repo
fresh_repo "$REPO"

# Two different "stack roots" with identical staged content.
ROOT1=$W/s11-rootA
ROOT2=$W/s11-rootB/deeper
rm -rf "$W/s11-rootA" "$W/s11-rootB"
for R in "$ROOT1" "$ROOT2"; do
  ST=$R/state/snapshots/snap1
  mkdir -p "$ST/databases/app" "$R/data/media/img"
  head -c 2097152 /dev/urandom > "$W/s11-blob.bin.$$" 2>/dev/null
done
# identical content in both roots
head -c 3145728 /dev/urandom > "$W/s11-content.bin"
for R in "$ROOT1" "$ROOT2"; do
  cp "$W/s11-content.bin" "$R/state/snapshots/snap1/databases/app/app.pgdump"
  echo "meta" > "$R/state/snapshots/snap1/databases/app/meta.json"
  cp "$W/s11-content.bin" "$R/data/media/img/photo.jpg"
done
rm -f "$W"/s11-blob.bin.*

sid() { grep '"message_type":"summary"' "$1" | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/'; }
paths_of() { "$RESTIC" -r "$REPO" snapshots --json "$1" | python3 -I -c 'import json,sys; s=json.load(sys.stdin)[0]; print("  snapshot.paths =", s["paths"])'; }
nodes_of() { "$RESTIC" -r "$REPO" ls --json "$1" | grep '"struct_type":"node"' | sed -E 's/.*"path":"([^"]*)".*/  node \1/'; }
summ() { grep '"message_type":"summary"' "$1" | python3 -I -c 'import json,sys
d=json.loads(sys.stdin.read()); print("  summary: files_new=%s files_unmodified=%s dirs_new=%s data_added=%s total_bytes_processed=%s" % (d["files_new"],d["files_unmodified"],d["dirs_new"],d["data_added"],d["total_bytes_processed"]))'; }
parent_of() { "$RESTIC" -r "$REPO" cat snapshot "$1" | python3 -I -c 'import json,sys; s=json.load(sys.stdin); print("  parent =", (s.get("parent") or "none")[:8])'; }

echo "== 11a: absolute argument"
"$RESTIC" -r "$REPO" backup "$ROOT1/state/snapshots/snap1/databases/app" --host h --tag case=11a --json > $W/s11a.json 2>&1; echo "[exit=$?]"
S=$(sid $W/s11a.json); paths_of $S; nodes_of $S

echo "== 11b: cwd=staging, relative argument databases/app"
( cd "$ROOT1/state/snapshots/snap1" && "$RESTIC" -r "$REPO" backup databases/app --host h --tag case=11b --json > $W/s11b.json 2>&1; echo "[exit=$?]" )
S11b=$(sid $W/s11b.json); paths_of $S11b; nodes_of $S11b; summ $W/s11b.json; parent_of $S11b

echo "== 11b': cwd=staging, argument ."
( cd "$ROOT1/state/snapshots/snap1" && "$RESTIC" -r "$REPO" backup . --host h --tag case=11b2 --json > $W/s11b2.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11b2.json); paths_of $S; nodes_of $S

echo "== 11b'': cwd=staging/databases/app, argument . (member dir itself)"
( cd "$ROOT1/state/snapshots/snap1/databases/app" && "$RESTIC" -r "$REPO" backup . --host h --tag case=11b3 --json > $W/s11b3.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11b3.json); paths_of $S; nodes_of $S

echo "== 11b''': cwd=stack root, relative persist path data/media + staged member in one call"
( cd "$ROOT1" && "$RESTIC" -r "$REPO" backup data/media --host h --tag case=11b4 --json > $W/s11b4.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11b4.json); paths_of $S; nodes_of $S

echo "== 11b4: relative arg with ../ (does it escape to absolute?)"
( cd "$ROOT1/state" && "$RESTIC" -r "$REPO" backup ../data/media --host h --tag case=11b5 --json > $W/s11b5.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11b5.json); paths_of $S; nodes_of $S

echo "== 11c: path-rewrite flags in 0.19.1 help?"
"$RESTIC" backup --help | grep -iE 'path|prefix|strip|root|relative|chroot' | grep -viE 'exclude|files-from|cache|cacert|tls|password|repository-file' || echo "  (no path-rewriting flag)"
"$RESTIC" rewrite --help | grep -iE 'path|prefix|strip|relocat' || echo "  (rewrite has no path-relocation flag)"

echo "== 11d: restore subtree into different location (snapshot:subfolder) from 11b snapshot"
NEW=$W/s11-newroot/state/restore-target
rm -rf "$W/s11-newroot"; mkdir -p "$NEW"
"$RESTIC" -r "$REPO" restore "$S11b:databases/app" --target "$NEW" --json 2>&1 | tail -1; echo "[exit=${PIPESTATUS[0]}]"
( cd "$W/s11-newroot" && find . -type f | sort )
echo "-- restore whole 11b snapshot into new target"
rm -rf "$W/s11-newroot2"; "$RESTIC" -r "$REPO" restore "$S11b" --target "$W/s11-newroot2" --json 2>&1 | tail -1
( cd "$W/s11-newroot2" && find . -type f | sort )
echo "-- restore 11a (absolute) snapshot via snap:<abs subpath> into new target"
S11a=$(sid $W/s11a.json)
rm -rf "$W/s11-newroot3"; "$RESTIC" -r "$REPO" restore "$S11a:$ROOT1/state/snapshots/snap1/databases/app" --target "$W/s11-newroot3" --json 2>&1 | tail -1; echo "[exit=${PIPESTATUS[0]}]"
( cd "$W/s11-newroot3" && find . -type f | sort )
echo "-- restore 11a (absolute) without subfolder: full host path recreated under target"
rm -rf "$W/s11-newroot4"; "$RESTIC" -r "$REPO" restore "$S11a" --target "$W/s11-newroot4" --json 2>&1 | tail -1
( cd "$W/s11-newroot4" && find . -type f | sort | sed 's/^/  /' | cut -c1-200 )

echo "== 11d': dump -a tar of a subpath"
"$RESTIC" -r "$REPO" dump -a tar "$S11b" /databases/app | tar -tvf -; echo "[exit=${PIPESTATUS[0]}]"
echo "-- dump -a tar snap:subfolder /"
"$RESTIC" -r "$REPO" dump -a tar "$S11b:databases/app" / | tar -tvf -; echo "[exit=${PIPESTATUS[0]}]"
echo "-- dump -a tar of 11a at its absolute subpath"
"$RESTIC" -r "$REPO" dump -a tar "$S11a" "$ROOT1/state/snapshots/snap1/databases/app" | tar -tf - | cut -c1-200; echo "[exit=${PIPESTATUS[0]}]"

echo "== 11e: same relative staging from a DIFFERENT absolute root: parent detection & dedup"
( cd "$ROOT2/state/snapshots/snap1" && "$RESTIC" -r "$REPO" backup databases/app --host h --tag case=11e --json > $W/s11e.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11e.json); paths_of $S; nodes_of $S; summ $W/s11e.json; parent_of $S
echo "-- same again from ROOT1 (expect parent = 11b, files_unmodified)"
( cd "$ROOT1/state/snapshots/snap1" && "$RESTIC" -r "$REPO" backup databases/app --host h --tag case=11e2 --json > $W/s11e2.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11e2.json); summ $W/s11e2.json; parent_of $S
echo "-- from ROOT2 with --group-by host (paths ignored for parent selection)"
( cd "$ROOT2/state/snapshots/snap1" && "$RESTIC" -r "$REPO" backup databases/app --host h --group-by host --tag case=11e3 --json > $W/s11e3.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11e3.json); summ $W/s11e3.json; parent_of $S
echo "-- from ROOT2 with explicit --parent of 11b snapshot"
( cd "$ROOT2/state/snapshots/snap1" && "$RESTIC" -r "$REPO" backup databases/app --host h --parent "$S11b" --tag case=11e4 --json > $W/s11e4.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11e4.json); summ $W/s11e4.json; parent_of $S
echo "-- per-attempt staging dir (snap2) with same content, same root: parent? dedup?"
mkdir -p "$ROOT1/state/snapshots/snap2/databases/app"; cp -p "$ROOT1/state/snapshots/snap1/databases/app/"* "$ROOT1/state/snapshots/snap2/databases/app/"
( cd "$ROOT1/state/snapshots/snap2" && "$RESTIC" -r "$REPO" backup databases/app --host h --tag case=11e5 --json > $W/s11e5.json 2>&1; echo "[exit=$?]" )
S=$(sid $W/s11e5.json); paths_of $S; summ $W/s11e5.json; parent_of $S
