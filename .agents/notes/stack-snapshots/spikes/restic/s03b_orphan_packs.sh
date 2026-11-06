#!/bin/bash
# Spike 3 addendum: does a failed --stdin-from-command leave uploaded packs behind?
. "$(dirname "$0")/env.sh"
REPO=$W/s03b-repo
fresh_repo "$REPO"
echo "packs before: $(ls "$REPO"/data/*/ 2>/dev/null | wc -l)"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename db/big.dump --host h --json -- sh -c 'head -c 83886080 /dev/urandom; exit 1' > /dev/null 2>$W/s03b.err
echo "[restic exit=$?]"; tail -1 $W/s03b.err
echo "packs after failed run: $(find "$REPO"/data -type f | wc -l), bytes: $(du -sk "$REPO"/data | cut -f1) KiB"
echo "snapshots: $("$RESTIC" -r "$REPO" snapshots --json)"
echo "index files: $(find "$REPO"/index -type f | wc -l)"
"$RESTIC" -r "$REPO" prune --dry-run 2>&1 | grep -iE 'unused|to delete|unreferenced|this removes|packs' | head
echo "== check"
"$RESTIC" -r "$REPO" check 2>&1 | tail -4
