#!/usr/bin/env bash
# Spike 5c: a pg_dump killed mid-stream, under a pipe vs --stdin-from-command.
source "$(dirname "$0")/lib.sh"
S=angee-spike-db-src
T=angee-spike-db-dst5
RESTIC="$W/../../bin/restic"
export RESTIC_REPOSITORY="$W/restic-repo"
export RESTIC_PASSWORD_FILE="$W/restic-pass"
export RESTIC_CACHE_DIR="$W/restic-cache"
added() { grep -E "Added to the repository|processed|snapshot .* saved|Warning|error|Fatal|killed|terminated"; }
banner "kill under --stdin-from-command (gzip dump, SIGTERM after 1.5s)"
( sleep 1.5; docker exec "$S" sh /tmp/kill_pgdump.sh ) &
"$RESTIC" backup --stdin-filename databases/bigdb/bigdb.pgdump --host stackC --tag angee.snapshot=killed-cmd2 --stdin-from-command -- docker exec "$S" pg_dump -U postgres -d bigdb -Fc 2>&1 | added
echo "    restic exit=${PIPESTATUS[0]}"
wait
"$RESTIC" snapshots --host stackC --tag angee.snapshot=killed-cmd2 | grep -E "killed|snapshots"
