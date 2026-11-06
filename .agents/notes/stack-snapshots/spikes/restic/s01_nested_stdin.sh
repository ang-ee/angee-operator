#!/bin/bash
# Spike 1: nested --stdin-filename paths.
. "$(dirname "$0")/env.sh"
REPO=$W/s01-repo
fresh_repo "$REPO"
SRC=$W/s01-src.bin
head -c 300000 /dev/urandom > "$SRC"
echo "src sha: $(shasum -a 256 < "$SRC")"

echo "== 1a: --stdin with nested filename"
"$RESTIC" -r "$REPO" backup --stdin --stdin-filename databases/app/app.pgdump --host stackA --tag angee.snapshot=s1,angee.kind=member,angee.service=app --json < "$SRC" > $W/s01-backup.json 2>$W/s01-backup.err
echo "[exit=$?]"; grep '"message_type":"summary"' $W/s01-backup.json; cat $W/s01-backup.err
SNAP=$(grep '"message_type":"summary"' $W/s01-backup.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
echo "snap=$SNAP"

echo "== ls --json"
"$RESTIC" -r "$REPO" ls --json "$SNAP"; echo "[exit=$?]"

echo "== snapshots --json (paths)"
"$RESTIC" -r "$REPO" snapshots --json "$SNAP"; echo; echo "[exit=$?]"

echo "== dump with leading slash"
"$RESTIC" -r "$REPO" dump "$SNAP" /databases/app/app.pgdump | shasum -a 256; echo "[exit=${PIPESTATUS[0]}]"
echo "== dump without leading slash"
"$RESTIC" -r "$REPO" dump "$SNAP" databases/app/app.pgdump | shasum -a 256; echo "[exit=${PIPESTATUS[0]}]"
echo "== dump nonexistent path"
"$RESTIC" -r "$REPO" dump "$SNAP" /databases/app/nope.pgdump | wc -c; echo "[exit=${PIPESTATUS[0]}]"
echo "== dump subdir as tar"
"$RESTIC" -r "$REPO" dump "$SNAP" /databases | tar -tvf -; echo "[exit=${PIPESTATUS[0]}]"
echo "== dump snap:subfolder syntax"
"$RESTIC" -r "$REPO" dump "$SNAP:databases/app" /app.pgdump | shasum -a 256; echo "[exit=${PIPESTATUS[0]}]"

echo "== restore --include"
rm -rf $W/s01-restore1
"$RESTIC" -r "$REPO" restore "$SNAP" --target $W/s01-restore1 --include /databases/app/app.pgdump --json | tail -1; echo "[exit=${PIPESTATUS[0]}]"
find $W/s01-restore1 -type f -exec shasum -a 256 {} \;
echo "== restore snap:subfolder"
rm -rf $W/s01-restore2
"$RESTIC" -r "$REPO" restore "$SNAP:databases/app" --target $W/s01-restore2 --json | tail -1; echo "[exit=${PIPESTATUS[0]}]"
find $W/s01-restore2 -type f -exec shasum -a 256 {} \;
ls -la $W/s01-restore2

echo "== 1b: --stdin-from-command with nested filename"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename databases/app2/app2.pgdump --host stackA --json -- cat "$SRC" > $W/s01-backup2.json 2>$W/s01-backup2.err
echo "[exit=$?]"; grep '"message_type":"summary"' $W/s01-backup2.json; cat $W/s01-backup2.err
SNAP2=$(grep '"message_type":"summary"' $W/s01-backup2.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
"$RESTIC" -r "$REPO" ls --json "$SNAP2" | grep -v '"message_type":"snapshot"'
"$RESTIC" -r "$REPO" dump "$SNAP2" /databases/app2/app2.pgdump | shasum -a 256

echo "== 1c: edge-case filenames"
for fn in /abs/leading.dump "../escape.dump" "a/../b.dump" "trailing/" "a//b.dump"; do
  printf 'x' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename "$fn" --host stackA --json > $W/s01-edge.json 2>$W/s01-edge.err
  rc=$?
  sid=$(grep '"message_type":"summary"' $W/s01-edge.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
  echo "filename=$fn exit=$rc snap=$sid err=$(head -c 300 $W/s01-edge.err)"
  if [ -n "$sid" ]; then "$RESTIC" -r "$REPO" ls --json "$sid" | grep '"struct_type":"node"' | sed -E 's/.*"path":"([^"]*)".*/  path=\1/'; "$RESTIC" -r "$REPO" snapshots --json "$sid" | sed -E 's/.*"paths":(\[[^]]*\]).*/  paths=\1/'; fi
done
