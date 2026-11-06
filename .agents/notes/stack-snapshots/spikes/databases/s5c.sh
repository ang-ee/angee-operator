#!/usr/bin/env bash
# Spike 5c: a pg_dump killed mid-stream, under a pipe vs --stdin-from-command.
source "$(dirname "$0")/lib.sh"
S=angee-spike-db-src
T=angee-spike-db-dst5
RESTIC="$W/../../bin/restic"
export RESTIC_REPOSITORY="$W/restic-repo"
export RESTIC_PASSWORD_FILE="$W/restic-pass"
export RESTIC_CACHE_DIR="$W/restic-cache"
docker cp -q "$W/kill_pgdump.sh" "$S:/tmp/kill_pgdump.sh"
added() { grep -E 'Added to the repository|processed|snapshot .* saved|Warning|error|Fatal|killed'; }

banner "pg_dump -Fc -Z0 bigdb killed mid-stream (SIGTERM after 0.7s) through a pipe"
( sleep 0.7; docker exec "$S" sh /tmp/kill_pgdump.sh ) &
docker exec "$S" pg_dump -U postgres -d bigdb -Fc -Z0 |
  "$RESTIC" backup --stdin --stdin-filename databases/bigdb/bigdb.pgdump --host stackC --tag angee.snapshot=killed-pipe 2>&1 | added
echo "    PIPESTATUS: docker-exec/pg_dump=${PIPESTATUS[0]} restic=${PIPESTATUS[1]}"
wait
"$RESTIC" snapshots --host stackC --tag angee.snapshot=killed-pipe | grep -E 'killed|snapshots'
dumpk() { "$RESTIC" dump --host stackC --tag angee.snapshot=killed-pipe latest /databases/bigdb/bigdb.pgdump; }
echo "--- restic dump killed-pipe | pg_restore --list"
dumpk | docker exec -i "$T" sh -c 'pg_restore --list > /dev/null'; echo "    exit=$?"
echo "--- restic dump killed-pipe | pg_restore -f /dev/null"
dumpk | docker exec -i "$T" pg_restore -f /dev/null; echo "    exit=$?"
echo "--- restic dump killed-pipe | pg_restore --no-owner --no-acl -1 -e into empty DB"
docker exec "$T" dropdb -U angee --if-exists killed; docker exec "$T" createdb -U angee killed
dumpk | docker exec -i "$T" pg_restore --no-owner --no-acl --single-transaction --exit-on-error -U angee -d killed
echo "    exit=$?  big rows: $(rowcount "$T" angee killed big)"
echo "--- same partial snapshot, default pg_restore (no -1/-e)"
docker exec "$T" createdb -U angee killed_default
dumpk | docker exec -i "$T" pg_restore --no-owner --no-acl -U angee -d killed_default
echo "    exit=$?  big rows: $(rowcount "$T" angee killed_default big)  small rows: $(rowcount "$T" angee killed_default small)"

banner "same kill under --stdin-from-command"
( sleep 0.7; docker exec "$S" sh /tmp/kill_pgdump.sh ) &
"$RESTIC" backup --stdin-filename databases/bigdb/bigdb.pgdump --host stackC --tag angee.snapshot=killed-cmd \
  --stdin-from-command -- docker exec "$S" pg_dump -U postgres -d bigdb -Fc -Z0 2>&1 | added
echo "    restic exit=${PIPESTATUS[0]}"
wait
"$RESTIC" snapshots --host stackC --tag angee.snapshot=killed-cmd | grep -E 'killed|snapshots'
