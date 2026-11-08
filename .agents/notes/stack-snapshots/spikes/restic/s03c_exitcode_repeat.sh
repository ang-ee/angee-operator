#!/bin/bash
# Spike 3 addendum: is the failed-producer exit code deterministic (1 vs 130)?
. "$(dirname "$0")/env.sh"
REPO=$W/s03c-repo
fresh_repo "$REPO"
for size in 1048576 10485760 33554432 83886080; do
  for i in 1 2 3; do
    "$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename db/x.dump --host h --json -- sh -c "head -c $size /dev/urandom; exit 1" > /dev/null 2>$W/s03c.err
    rc=$?
    echo "size=$size run=$i exit=$rc last=$(tail -1 $W/s03c.err)"
  done
done
echo "snapshots: $("$RESTIC" -r "$REPO" snapshots --json)"
echo "orphan data: $(du -sk "$REPO"/data | cut -f1) KiB in $(find "$REPO"/data -type f | wc -l) packs"
