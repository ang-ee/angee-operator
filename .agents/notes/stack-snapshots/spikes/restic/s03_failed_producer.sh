#!/bin/bash
# Spike 3: failed / truncated / empty producers with --stdin vs --stdin-from-command.
. "$(dirname "$0")/env.sh"
REPO=$W/s03-repo
fresh_repo "$REPO"
count() { "$RESTIC" -r "$REPO" snapshots --json | python3 -I -c 'import json,sys; print(len(json.load(sys.stdin)))'; }
show() { grep -E '"message_type":"(summary|error|exit_error)"' "$1"; echo "--- stderr:"; cat "$2"; }

echo "snapshots before: $(count)"

echo "== 3a: plain --stdin, producer writes 10 MiB then exits 1"
sh -c 'head -c 10485760 /dev/urandom; exit 1' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename db/fail.dump --host h --tag case=3a --json > $W/s03a.json 2>$W/s03a.err
echo "[pipe status: producer=${PIPESTATUS[0]} restic=${PIPESTATUS[1]}]"
show $W/s03a.json $W/s03a.err
echo "snapshots now: $(count)"

echo "== 3a': plain --stdin with set -o pipefail (shell sees producer status; restic still commits)"
set -o pipefail
sh -c 'head -c 1048576 /dev/urandom; exit 1' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename db/fail2.dump --host h --tag case=3a2 --json > $W/s03a2.json 2>$W/s03a2.err
echo "[pipeline exit=$? PIPESTATUS=${PIPESTATUS[*]}]"
set +o pipefail
grep '"message_type":"summary"' $W/s03a2.json | sed -E 's/.*"snapshot_id":"([0-9a-f]{8}).*/snapshot created: \1/'
echo "snapshots now: $(count)"

echo "== 3b: plain --stdin, EMPTY stdin"
"$RESTIC" -r "$REPO" backup --stdin --stdin-filename db/empty.dump --host h --tag case=3b --json < /dev/null > $W/s03b.json 2>$W/s03b.err
echo "[restic exit=$?]"
show $W/s03b.json $W/s03b.err
SNAP=$(grep '"message_type":"summary"' $W/s03b.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
[ -n "$SNAP" ] && "$RESTIC" -r "$REPO" ls --json "$SNAP" | grep '"type":"file"'
echo "snapshots now: $(count)"

echo "== 3c: --stdin-from-command, producer writes 10 MiB then exits 1"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename db/fail3.dump --host h --tag case=3c --json -- sh -c 'head -c 10485760 /dev/urandom; exit 1' > $W/s03c.json 2>$W/s03c.err
echo "[restic exit=$?]"
show $W/s03c.json $W/s03c.err
echo "snapshots now: $(count)"

echo "== 3c': --stdin-from-command, producer exits 2 with no output"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename db/fail4.dump --host h --tag case=3c2 --json -- sh -c 'echo boom >&2; exit 2' > $W/s03c2.json 2>$W/s03c2.err
echo "[restic exit=$?]"
show $W/s03c2.json $W/s03c2.err
echo "snapshots now: $(count)"

echo "== 3c'': --stdin-from-command, producer killed by signal (SIGKILL self after 1 MiB)"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename db/fail5.dump --host h --tag case=3c3 --json -- sh -c 'head -c 1048576 /dev/urandom; kill -9 $$' > $W/s03c3.json 2>$W/s03c3.err
echo "[restic exit=$?]"
show $W/s03c3.json $W/s03c3.err
echo "snapshots now: $(count)"

echo "== 3d: --stdin-from-command, producer succeeds with EMPTY output"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename db/empty2.dump --host h --tag case=3d --json -- sh -c 'exit 0' > $W/s03d.json 2>$W/s03d.err
echo "[restic exit=$?]"
show $W/s03d.json $W/s03d.err
echo "snapshots now: $(count)"

echo "== 3e: --stdin-from-command, producer succeeds with 5 MiB"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename db/ok.dump --host h --tag case=3e --json -- sh -c 'head -c 5242880 /dev/urandom' > $W/s03e.json 2>$W/s03e.err
echo "[restic exit=$?]"
grep '"message_type":"summary"' $W/s03e.json
echo "snapshots now: $(count)"

echo "== 3f: --stdin-from-command, producer stderr passthrough? (writes to stderr, exits 0)"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename db/ok2.dump --host h --tag case=3f --json -- sh -c 'echo producer-diagnostic >&2; echo data' > $W/s03f.json 2>$W/s03f.err
echo "[restic exit=$?]"; echo "--- stderr:"; cat $W/s03f.err

echo "== 3g: were packs from the refused 3c upload left behind? (unreferenced data -> prune would collect)"
"$RESTIC" -r "$REPO" check --json 2>&1 | tail -3
"$RESTIC" -r "$REPO" prune --dry-run 2>&1 | grep -iE 'unused|to delete|to repack|unreferenced|this removes' | head

echo "== final snapshot list"
"$RESTIC" -r "$REPO" snapshots --json | python3 -I -c 'import json,sys
for s in json.load(sys.stdin): print(s["short_id"], s["tags"], s["paths"], s["summary"]["total_bytes_processed"])'
