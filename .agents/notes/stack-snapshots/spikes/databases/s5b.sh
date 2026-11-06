#!/usr/bin/env bash
# Spike 5b: (1) dedup with a LOCALISED change, gzip vs -Z0; (2) a pg_dump killed mid-stream
# under a pipe vs --stdin-from-command, and what pg_restore --list says about the result.
source "$(dirname "$0")/lib.sh"
S=angee-spike-db-src
T=angee-spike-db-dst5
RESTIC="$W/../../bin/restic"
export RESTIC_REPOSITORY="$W/restic-repo"
export RESTIC_PASSWORD_FILE="$W/restic-pass"
export RESTIC_CACHE_DIR="$W/restic-cache"
TIMEFORMAT='    real %3Rs'

added() { grep -E 'Added to the repository|snapshot .* saved|Warning|error|Fatal'; }
backup() { # backup <tag> [pg_dump flags...]
  local tag="$1"; shift
  docker exec "$S" pg_dump -U postgres -d bigdb -Fc "$@" |
    "$RESTIC" backup --stdin --stdin-filename databases/bigdb/bigdb.pgdump --host stackB --tag "angee.snapshot=$tag" 2>&1 | added
}

banner "dedup with a localised change (1000 contiguous rows updated, host stackB)"
echo "--- gzip baseline g1"; backup g1
echo "--- -Z0 baseline n1";  backup n1 -Z0
docker exec "$S" psql -U postgres -d bigdb -X -q -c "update big set a = md5(a) where id between 1000001 and 1001000"
echo "--- gzip after change g2"; backup g2
echo "--- -Z0 after change n2";  backup n2 -Z0
docker exec "$S" psql -U postgres -d bigdb -X -q -c "insert into big select g, md5(g::text), md5(g::text), now(), '[1,2,3]' from generate_series(2000001, 2001000) g"
echo "--- gzip after 1000-row append g3"; backup g3
echo "--- -Z0 after 1000-row append n3";  backup n3 -Z0

banner "pg_dump killed mid-stream (SIGTERM after ~1s) through a pipe"
( sleep 1; docker exec "$S" pkill -TERM -x pg_dump ) &
docker exec "$S" pg_dump -U postgres -d bigdb -Fc |
  "$RESTIC" backup --stdin --stdin-filename databases/bigdb/bigdb.pgdump --host stackB --tag angee.snapshot=killed-pipe 2>&1 | added
echo "    PIPESTATUS: docker-exec/pg_dump=${PIPESTATUS[0]} restic=${PIPESTATUS[1]}"
wait
"$RESTIC" snapshots --host stackB --tag angee.snapshot=killed-pipe | grep -E 'killed|snapshots'
echo "--- restic dump killed-pipe | pg_restore --list"
"$RESTIC" dump --host stackB --tag angee.snapshot=killed-pipe latest /databases/bigdb/bigdb.pgdump | docker exec -i "$T" sh -c 'pg_restore --list > /dev/null'
echo "    exit=$?"
echo "--- restic dump killed-pipe | pg_restore -f /dev/null"
"$RESTIC" dump --host stackB --tag angee.snapshot=killed-pipe latest /databases/bigdb/bigdb.pgdump | docker exec -i "$T" pg_restore -f /dev/null
echo "    exit=$?"
echo "--- restic dump killed-pipe | pg_restore --no-owner --no-acl -1 -e into empty DB"
docker exec "$T" createdb -U angee killed
"$RESTIC" dump --host stackB --tag angee.snapshot=killed-pipe latest /databases/bigdb/bigdb.pgdump |
  docker exec -i "$T" pg_restore --no-owner --no-acl --single-transaction --exit-on-error -U angee -d killed
echo "    exit=$?  big rows: $(rowcount "$T" angee killed big)"

banner "same kill under --stdin-from-command"
( sleep 1; docker exec "$S" pkill -TERM -x pg_dump ) &
"$RESTIC" backup --stdin-filename databases/bigdb/bigdb.pgdump --host stackB --tag angee.snapshot=killed-cmd \
  --stdin-from-command -- docker exec "$S" pg_dump -U postgres -d bigdb -Fc 2>&1 | added
echo "    restic exit=${PIPESTATUS[0]}"
wait
"$RESTIC" snapshots --host stackB --tag angee.snapshot=killed-cmd | tail -1

banner "repo totals"
du -sh "$RESTIC_REPOSITORY"
"$RESTIC" snapshots --host stackB
