#!/usr/bin/env bash
# Spike 4: restore from STDIN into the empty DB the postgres image entrypoint creates.
source "$(dirname "$0")/lib.sh"
S=angee-spike-db-src
T=angee-spike-db-dst4
D="$W/dumps"
RFLAGS="--no-owner --no-acl --single-transaction --exit-on-error"

banner "piped -Fc dump of source app DB (no pg_dump flags, as a backup job would stream it)"
docker exec "$S" pg_dump -U postgres -d app -Fc > "$D/app-pipe.dump"; echo "pg_dump exit=$? size=$(stat -f %z "$D/app-pipe.dump")"

banner "fresh target $T: POSTGRES_DB=app POSTGRES_USER=angee (source used postgres/appowner)"
rm_c "$T"
docker run -d --name "$T" -e POSTGRES_DB=app -e POSTGRES_USER=angee -e POSTGRES_PASSWORD=angeepw "$PGV" >/dev/null
wait_pg "$T" angee app || exit 1
docker exec "$T" psql -U angee -d app -X -At -c "
  select 'role angee rolsuper=' || rolsuper from pg_roles where rolname = 'angee';
  select 'databases: ' || string_agg(datname || '(owner=' || pg_get_userbyid(datdba) || ')', ', ' order by datname) from pg_database;
  select 'extensions in app: ' || string_agg(extname, ',') from pg_extension;
  select 'relations in app public: ' || count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where n.nspname = 'public';"

banner "restore: cat dump | docker exec -i $T pg_restore $RFLAGS -U angee -d app"
# shellcheck disable=SC2086
cat "$D/app-pipe.dump" | docker exec -i "$T" pg_restore $RFLAGS -U angee -d app
echo "exit=$?"

banner "verify"
q="COPY (select id, name, embedding::text from items order by id) TO STDOUT"
src=$(docker exec "$S" psql -U postgres -d app -X -At -c "$q" | shasum -a 256)
dst=$(docker exec "$T" psql -U angee -d app -X -At -c "$q" | shasum -a 256)
echo "src items sha256: $src"; echo "dst items sha256: $dst"
[ "$src" = "$dst" ] && echo "DATA EQUAL" || echo "DATA DIFFERS"
docker exec "$T" psql -U angee -d app -X -At -c "
  select 'rows=' || count(*) from items;
  select 'extension vector ' || extversion || ' owner=' || pg_get_userbyid(extowner) from pg_extension where extname='vector';
  select 'table owner=' || tableowner from pg_tables where tablename='items';
  select 'indexes: ' || string_agg(indexname, ',') from pg_indexes where tablename='items';
  select 'seq last_value=' || last_value from items_id_seq;
  select 'nearest to [1,2,3.1]: ' || name from items order by embedding <-> '[1,2,3.1]' limit 1;"
docker exec "$S" psql -U postgres -d app -X -At -c "select 'src seq last_value=' || last_value from items_id_seq"

banner "restore again into the now non-empty app DB (same flags)"
# shellcheck disable=SC2086
cat "$D/app-pipe.dump" | docker exec -i "$T" pg_restore $RFLAGS -U angee -d app; echo "exit=$?"
docker exec "$T" psql -U angee -d app -X -At -c "select 'rows after failed 2nd restore=' || count(*) from items"

banner "-j from stdin"
docker exec "$T" createdb -U angee app_j
cat "$D/app-pipe.dump" | docker exec -i "$T" pg_restore -j 4 --no-owner --no-acl --exit-on-error -U angee -d app_j; echo "exit=$?"
echo "--- -j with --single-transaction"
docker exec "$T" pg_restore -j 4 --single-transaction -U angee -d app_j /dev/null; echo "exit=$?"

banner "-j from a FILE: seekable file dump vs piped (offset-less) dump, big DB (in source container)"
docker exec "$S" createdb -U postgres r_j_file
docker exec "$S" createdb -U postgres r_j_pipe
TIMEFORMAT='    real %3Rs'
echo "--- -j 4 on file dump (has data offsets)"
time docker exec "$S" pg_restore -j 4 --no-owner --no-acl --exit-on-error -U postgres -d r_j_file /tmp/big.dump; echo "    exit=$?"
echo "--- -j 4 on piped dump saved to a file (no data offsets)"
time docker exec "$S" pg_restore -j 4 --no-owner --no-acl --exit-on-error -U postgres -d r_j_pipe /tmp/big-pipe.dump; echo "    exit=$?"
echo "    r_j_pipe big rows: $(rowcount "$S" postgres r_j_pipe big)"

banner "--clean / --create against the image-created DB"
docker exec "$T" createdb -U angee app_clean
echo "--- --clean (no --if-exists) into empty DB, strict flags"
# shellcheck disable=SC2086
cat "$D/app-pipe.dump" | docker exec -i "$T" pg_restore --clean $RFLAGS -U angee -d app_clean; echo "exit=$?"
echo "--- --clean --if-exists into empty DB, strict flags"
# shellcheck disable=SC2086
cat "$D/app-pipe.dump" | docker exec -i "$T" pg_restore --clean --if-exists $RFLAGS -U angee -d app_clean; echo "exit=$?"
echo "    app_clean rows: $(rowcount "$T" angee app_clean items)"
echo "--- --create with --single-transaction"
cat "$D/app-pipe.dump" | docker exec -i "$T" pg_restore --create --single-transaction -U angee -d app; echo "exit=$?"
echo "--- --create (no -1) with -d app while app exists"
cat "$D/app-pipe.dump" | docker exec -i "$T" pg_restore --create --no-owner --no-acl --exit-on-error -U angee -d postgres; echo "exit=$?"
echo "--- what --create emits"
docker exec "$S" sh -c "pg_restore --create -f - /tmp/app.dump 2>/dev/null | grep -E '^(CREATE|ALTER) DATABASE|connect' || pg_dump -U postgres -d app -Fc | pg_restore --create -f - | grep -E '^(CREATE|ALTER) DATABASE|connect'"
