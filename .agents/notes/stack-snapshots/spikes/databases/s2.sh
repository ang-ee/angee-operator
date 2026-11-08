#!/usr/bin/env bash
# Spike 2: what pg_restore --list validates; cheap real verification and its cost.
source "$(dirname "$0")/lib.sh"
S=angee-spike-db-src
D="$W/dumps"
TIMEFORMAT='    real %3Rs'

psqlc() { docker exec "$S" psql -U postgres -X -At "$@"; }
counts() {
  psqlc -d "$1" -c "select 'big=' || coalesce((select count(*)::text from big), 'n/a') where to_regclass('big') is not null
                    union all select 'big=MISSING' where to_regclass('big') is null
                    union all select 'small=' || (select count(*) from small) where to_regclass('small') is not null
                    union all select 'small=MISSING' where to_regclass('small') is null" | tr '\n' ' '
  echo
}

banner "generate ~200 MB of table data (2,000,000 rows) in bigdb"
time docker exec -i "$S" psql -U postgres -X -q < "$W/s2-big.sql"
echo "COPY text size of big (bytes):"
docker exec "$S" sh -c "psql -U postgres -X -d bigdb -c 'COPY big TO STDOUT' | wc -c"

banner "dumps of bigdb"
echo "pg_dump -Fc (gzip default) to file:"
time docker exec "$S" pg_dump -U postgres -d bigdb -Fc -f /tmp/big.dump
echo "pg_dump -Fc -Z0 (uncompressed) to file:"
time docker exec "$S" pg_dump -U postgres -d bigdb -Fc -Z0 -f /tmp/big-z0.dump
echo "pg_dump -Fc to stdout (pipe, non-seekable) -> host file:"
time docker exec "$S" pg_dump -U postgres -d bigdb -Fc > "$D/big-pipe.dump"
docker cp -q "$S:/tmp/big.dump" "$D/big.dump"
docker cp -q "$S:/tmp/big-z0.dump" "$D/big-z0.dump"
ls -l "$D"/big*.dump

banner "costs on the intact gzip dump"
echo "pg_restore --list:"; time docker exec "$S" sh -c 'pg_restore --list /tmp/big.dump >/dev/null'; echo "    exit=$?"
echo "pg_restore -f /dev/null:"; time docker exec "$S" pg_restore -f /dev/null /tmp/big.dump; echo "    exit=$?"
echo "pg_restore -f /dev/null (-Z0 dump):"; time docker exec "$S" pg_restore -f /dev/null /tmp/big-z0.dump; echo "    exit=$?"
echo "pg_restore -f - | wc -c (bytes of rendered SQL):"; docker exec "$S" sh -c 'pg_restore -f - /tmp/big.dump | wc -c'
echo "sha256sum of the dump file (checksum-only alternative):"; time docker exec "$S" sha256sum /tmp/big.dump
echo "real restore into empty DB r_ok (--no-owner --no-acl --single-transaction --exit-on-error):"
psqlc -c 'drop database if exists r_ok' -c 'create database r_ok' >/dev/null
time docker exec "$S" pg_restore -U postgres -d r_ok --no-owner --no-acl --single-transaction --exit-on-error /tmp/big.dump
echo "    exit=$?"; counts r_ok

banner "TOC offsets: file dump vs piped dump"
docker cp -q "$D/big-pipe.dump" "$S:/tmp/big-pipe.dump"
echo "file dump  TOC header:"; docker exec "$S" sh -c 'pg_restore -l /tmp/big.dump | head -12 | tail -9'
echo "piped dump pg_restore -l exit:"; docker exec "$S" sh -c 'pg_restore -l /tmp/big-pipe.dump > /dev/null'; echo "    exit=$?"
echo "piped dump -f /dev/null:"; time docker exec "$S" pg_restore -f /dev/null /tmp/big-pipe.dump; echo "    exit=$?"

banner "make corrupted copies"
cp "$D/big.dump" "$D/big-flip16.dump"; python3 -I "$W/flip.py" "$D/big-flip16.dump" 0.5 16
cp "$D/big.dump" "$D/big-flip1.dump";  python3 -I "$W/flip.py" "$D/big-flip1.dump" 0.5 1
SZ=$(stat -f %z "$D/big.dump"); head -c $((SZ * 60 / 100)) "$D/big.dump" > "$D/big-trunc60.dump"
echo "truncated to $((SZ * 60 / 100)) of $SZ bytes"
SZP=$(stat -f %z "$D/big-pipe.dump"); head -c $((SZP * 60 / 100)) "$D/big-pipe.dump" > "$D/big-pipe-trunc60.dump"
echo "piped dump truncated to $((SZP * 60 / 100)) of $SZP bytes"
cp "$D/big-z0.dump" "$D/big-z0-hex.dump"; python3 -I "$W/flip_hex.py" "$D/big-z0-hex.dump" 0.5
cp "$D/big-z0.dump" "$D/big-z0-flip16.dump"; python3 -I "$W/flip.py" "$D/big-z0-flip16.dump" 0.5 16

for k in big-flip16 big-flip1 big-trunc60 big-pipe-trunc60 big-z0-flip16 big-z0-hex; do
  banner "corrupted: $k"
  docker cp -q "$D/$k.dump" "$S:/tmp/$k.dump"
  echo "--- pg_restore --list"
  docker exec "$S" sh -c "pg_restore --list /tmp/$k.dump > /dev/null"; echo "    exit=$?"
  echo "--- pg_restore -f /dev/null"
  docker exec "$S" pg_restore -f /dev/null "/tmp/$k.dump"; echo "    exit=$?"
  db="r_$(echo "$k" | tr -c 'a-z0-9\n' '_')"
  echo "--- real restore, default error handling (--no-owner --no-acl) into empty $db"
  psqlc -c "drop database if exists $db" -c "create database $db" >/dev/null
  docker exec "$S" pg_restore -U postgres -d "$db" --no-owner --no-acl "/tmp/$k.dump"; echo "    exit=$?"
  counts "$db"
  echo "--- real restore, --single-transaction --exit-on-error into empty ${db}_st"
  psqlc -c "drop database if exists ${db}_st" -c "create database ${db}_st" >/dev/null
  docker exec "$S" pg_restore -U postgres -d "${db}_st" --no-owner --no-acl --single-transaction --exit-on-error "/tmp/$k.dump"; echo "    exit=$?"
  counts "${db}_st"
done

banner "silent corruption check for big-z0-hex: compare restored data with source"
for db in bigdb r_big_z0_hex; do
  printf '%s md5(all big rows) = ' "$db"
  psqlc -d "$db" -c "select md5(string_agg(id || ',' || a || ',' || b, ';' order by id)) from big"
done
echo "rows that differ:"
docker exec "$S" sh -c "psql -U postgres -X -At -d bigdb -c 'COPY (select id,a,b from big order by id) TO STDOUT' > /tmp/src.tsv;
  psql -U postgres -X -At -d r_big_z0_hex -c 'COPY (select id,a,b from big order by id) TO STDOUT' > /tmp/dst.tsv;
  diff /tmp/src.tsv /tmp/dst.tsv; rm -f /tmp/src.tsv /tmp/dst.tsv"
