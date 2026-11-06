#!/usr/bin/env bash
# Spike 3: pg_restore error handling when the archive's owner/grantee roles are missing.
source "$(dirname "$0")/lib.sh"
T=angee-spike-db-dst3
DUMP="$W/dumps/s1-a-none.dump"   # pg_dump -Fc, no flags: has OWNER + ACL entries

banner "fresh target $T (no appowner / reader roles)"
rm_c "$T"
docker run -d --name "$T" -e POSTGRES_PASSWORD=dstpw "$PGV" >/dev/null
wait_pg "$T" postgres || exit 1
docker cp -q "$DUMP" "$T:/tmp/app.dump"
docker exec "$T" psql -U postgres -X -At -c "select string_agg(rolname, ',' order by rolname) from pg_roles where rolname !~ '^pg_'"

state() {
  local db="$1"
  echo "    state of $db:"
  docker exec "$T" psql -U postgres -X -At -d "$db" -c "
    select '      extensions: ' || coalesce(string_agg(extname, ','), '') from pg_extension;
    select '      tables: ' || coalesce(string_agg(tablename || '(owner=' || tableowner || ')', ','), '(none)') from pg_tables where schemaname='public';"
  echo "      items rows: $(rowcount "$T" postgres "$db" items)"
}

try() {
  local db="$1"; shift
  banner "pg_restore $* -> $db"
  docker exec "$T" createdb -U postgres "$db"
  docker exec "$T" pg_restore -U postgres -d "$db" "$@" /tmp/app.dump
  echo "    exit=$?"
  state "$db"
}

try t_default
try t_exit --exit-on-error
try t_single --single-transaction
try t_noowner_only --no-owner
try t_clean --no-owner --no-acl --single-transaction --exit-on-error
echo "    vectors in t_clean:"; docker exec "$T" psql -U postgres -X -At -d t_clean -c "select id, name, embedding from items order by id"

banner "extra: restore as a NON-superuser database owner (clean flags)"
docker exec "$T" psql -U postgres -X -q -c "create role appuser login password 'x'" -c "create database t_nonsu owner appuser" -c "create database t_nonsu_ext owner appuser"
echo "--- t_nonsu: extension not pre-created"
docker exec "$T" pg_restore -U appuser -d t_nonsu --no-owner --no-acl --single-transaction --exit-on-error /tmp/app.dump; echo "    exit=$?"
echo "--- t_nonsu_ext: superuser pre-created the extension, restore as appuser"
docker exec "$T" psql -U postgres -X -q -d t_nonsu_ext -c "create extension vector"
docker exec "$T" pg_restore -U appuser -d t_nonsu_ext --no-owner --no-acl --single-transaction --exit-on-error /tmp/app.dump; echo "    exit=$?"
echo "--- t_nonsu_ext again, default error handling (no -1/-e)"
docker exec "$T" psql -U postgres -X -q -c "drop database t_nonsu_ext" -c "create database t_nonsu_ext owner appuser"
docker exec "$T" psql -U postgres -X -q -d t_nonsu_ext -c "create extension vector"
docker exec "$T" pg_restore -U appuser -d t_nonsu_ext --no-owner --no-acl /tmp/app.dump; echo "    exit=$?"
echo "      items rows: $(rowcount "$T" postgres t_nonsu_ext items)"

banner "what the archive emits for the extension"
docker exec "$T" sh -c "pg_restore -f - /tmp/app.dump | grep -iE 'EXTENSION'"
