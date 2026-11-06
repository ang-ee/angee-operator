#!/usr/bin/env bash
# Spike 5: pg_dump -Fc | restic backup --stdin  ->  restic dump | pg_restore (stdin), end to end.
source "$(dirname "$0")/lib.sh"
S=angee-spike-db-src
T=angee-spike-db-dst5
RESTIC="$W/../../bin/restic"
export RESTIC_REPOSITORY="$W/restic-repo"
export RESTIC_PASSWORD_FILE="$W/restic-pass"
export RESTIC_CACHE_DIR="$W/restic-cache"
RFLAGS="--no-owner --no-acl --single-transaction --exit-on-error"
TIMEFORMAT='    real %3Rs'
set -o pipefail

rm -rf "$RESTIC_REPOSITORY" "$RESTIC_CACHE_DIR"
printf 'spike-password-not-secret\n' > "$RESTIC_PASSWORD_FILE"
banner "restic init"
"$RESTIC" init -q && echo "init ok"

backup() { # backup <db> <snapshot-tag> [pg_dump extra flags...]
  local db="$1" tag="$2"; shift 2
  docker exec "$S" pg_dump -U postgres -d "$db" -Fc "$@" |
    "$RESTIC" backup --stdin --stdin-filename "databases/$db/$db.pgdump" \
      --host stackA --tag "angee.snapshot=$tag" 2>&1 | grep -vE '^\s*$'
}

banner "s1: backup app (small, pgvector) and bigdb (~218 MB COPY data) via pipe"
time backup app s1
time backup bigdb s1
"$RESTIC" snapshots --host stackA
"$RESTIC" ls --host stackA --tag angee.snapshot=s1 latest

banner "fresh target $T (POSTGRES_DB=app POSTGRES_USER=angee)"
rm_c "$T"
docker run -d --name "$T" -e POSTGRES_DB=app -e POSTGRES_USER=angee -e POSTGRES_PASSWORD=angeepw "$PGV" >/dev/null
wait_pg "$T" angee app || exit 1
docker exec "$T" createdb -U angee bigdb

banner "restore app and bigdb: restic dump | docker exec -i pg_restore $RFLAGS"
for db in app bigdb; do
  echo "--- $db"
  # shellcheck disable=SC2086
  time { "$RESTIC" dump --host stackA --tag angee.snapshot=s1 --path "/databases/$db/$db.pgdump" latest "/databases/$db/$db.pgdump" |
    docker exec -i "$T" pg_restore $RFLAGS -U angee -d "$db"; }
  echo "    pipeline exit=$?"
done

banner "verify equality"
cmpq() { # cmpq <db> <query>
  local a b
  a=$(docker exec "$S" psql -U postgres -d "$1" -X -At -c "COPY ($2) TO STDOUT" | shasum -a 256 | cut -c1-16)
  b=$(docker exec "$T" psql -U angee -d "$1" -X -At -c "COPY ($2) TO STDOUT" | shasum -a 256 | cut -c1-16)
  echo "  $1: src=$a dst=$b $([ "$a" = "$b" ] && echo EQUAL || echo DIFFERENT)"
}
cmpq app "select id, name, embedding::text from items order by id"
cmpq bigdb "select id, a, b, ts, embedding::text from big order by id"
cmpq bigdb "select * from small order by id"
echo "  bigdb rows: src=$(rowcount "$S" postgres bigdb big) dst=$(rowcount "$T" angee bigdb big)"

banner "sizes"
docker exec "$S" pg_dump -U postgres -d bigdb -Fc | wc -c | awk '{print "  bigdb -Fc dump bytes:", $1}'
du -sh "$RESTIC_REPOSITORY" | awk '{print "  repo on disk after s1:", $1}'
"$RESTIC" stats --mode raw-data --host stackA 2>&1 | grep -E 'Total|Compression|Snapshots'

banner "dedup: s2 unchanged gzip, then update 1000 rows -> s3 gzip; same with -Z0 (z1, z2)"
time backup bigdb s2
docker exec "$S" psql -U postgres -d bigdb -X -q -c "update big set a = md5(a) where id % 2000 = 0"
time backup bigdb s3
time backup bigdb z1 -Z0
docker exec "$S" psql -U postgres -d bigdb -X -q -c "update big set b = md5(b) where id % 2000 = 1"
time backup bigdb z2 -Z0
du -sh "$RESTIC_REPOSITORY" | awk '{print "  repo on disk now:", $1}'

banner "failure semantics: failing producer in a pipe vs --stdin-from-command"
echo "--- pipe: pg_dump of a missing DB | restic backup --stdin"
docker exec "$S" pg_dump -U postgres -d nosuchdb -Fc |
  "$RESTIC" backup --stdin --stdin-filename databases/nosuchdb/nosuchdb.pgdump --host stackA --tag angee.snapshot=fail1 2>&1 | grep -vE '^\s*$'
echo "    pipeline exit (pipefail)=$?"
echo "--- pipe: producer writes partial output then exits 3"
sh -c 'printf PGDMPpartial; exit 3' |
  "$RESTIC" backup --stdin --stdin-filename databases/partial/partial.pgdump --host stackA --tag angee.snapshot=fail2 2>&1 | grep -vE '^\s*$'
echo "    pipeline exit (pipefail)=$?"
echo "--- --stdin-from-command: producer writes partial output then exits 3"
"$RESTIC" backup --stdin-filename databases/partial/partial.pgdump --host stackA --tag angee.snapshot=fail3 \
  --stdin-from-command -- sh -c 'printf PGDMPpartial; exit 3' 2>&1 | grep -vE '^\s*$'
echo "    restic exit=$?"
echo "--- --stdin-from-command: docker exec pg_dump (success)"
time "$RESTIC" backup --stdin-filename databases/app/app.pgdump --host stackA --tag angee.snapshot=s4 \
  --stdin-from-command -- docker exec "$S" pg_dump -U postgres -d app -Fc 2>&1 | grep -vE '^\s*$'
echo "    restic exit=$?"
echo "--- snapshots tagged fail*:"
"$RESTIC" snapshots --host stackA --tag angee.snapshot=fail1 --tag angee.snapshot=fail2 --tag angee.snapshot=fail3
echo "--- restoring the fail2 snapshot:"
"$RESTIC" dump --tag angee.snapshot=fail2 latest /databases/partial/partial.pgdump | docker exec -i "$T" pg_restore -l >/dev/null
echo "    exit=$?"

banner "restic check --read-data"
time "$RESTIC" check --read-data 2>&1 | tail -3
