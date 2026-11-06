#!/usr/bin/env bash
# Spike 1: which of pg_dump --no-owner / --no-acl take effect for -Fc archives.
source "$(dirname "$0")/lib.sh"
S=angee-spike-db-src
mkdir -p "$W/dumps"

banner "start source container $S ($PGV)"
rm_c "$S"
docker run -d --name "$S" -e POSTGRES_PASSWORD=srcpw "$PGV" >/dev/null
wait_pg "$S" postgres || exit 1
docker exec "$S" postgres --version

banner "setup roles/db/extension/table/grant"
docker exec -i "$S" psql -U postgres -X -q < "$W/s1-setup.sql"

banner "dumps"
declare -A FLAGS=( [a-none]="" [b-noowner]="--no-owner" [c-noacl]="--no-acl" [d-both]="--no-owner --no-acl" )
for k in a-none b-noowner c-noacl d-both; do
  # shellcheck disable=SC2086
  docker exec "$S" pg_dump -U postgres --dbname app --format=custom ${FLAGS[$k]} --file "/tmp/$k.dump"
  echo "pg_dump -Fc ${FLAGS[$k]:-(no flags)} -> exit=$?"
  docker cp -q "$S:/tmp/$k.dump" "$W/dumps/s1-$k.dump"
done
ls -l "$W"/dumps/s1-*.dump
shasum -a 256 "$W"/dumps/s1-*.dump

for k in a-none b-noowner c-noacl d-both; do
  banner "$k: pg_dump ${FLAGS[$k]:-(no flags)}"
  echo "--- pg_restore -f - (no restore flags) | grep OWNER TO|GRANT|REVOKE"
  docker exec "$S" sh -c "pg_restore -f - /tmp/$k.dump | grep -E 'OWNER TO|GRANT|REVOKE'"
  echo "--- pg_restore -l | grep ACL (ACL TOC entries)"
  docker exec "$S" sh -c "pg_restore -l /tmp/$k.dump | grep -E ' ACL ' || echo '(no ACL entries)'"
  echo "--- pg_restore -l | TABLE/SEQUENCE/INDEX/EXTENSION entries (last column = owner recorded in TOC)"
  docker exec "$S" sh -c "pg_restore -l /tmp/$k.dump | grep -E ' (TABLE|SEQUENCE|EXTENSION|INDEX) '"
done

banner "full TOC of a-none"
docker exec "$S" pg_restore -l /tmp/a-none.dump

banner "pg_restore-side flags applied to a-none archive"
echo "--- pg_restore --no-owner -f - | grep"
docker exec "$S" sh -c "pg_restore --no-owner -f - /tmp/a-none.dump | grep -E 'OWNER TO|GRANT|REVOKE' || echo '(none)'"
echo "--- pg_restore --no-acl -f - | grep"
docker exec "$S" sh -c "pg_restore --no-acl -f - /tmp/a-none.dump | grep -E 'OWNER TO|GRANT|REVOKE' || echo '(none)'"
echo "--- pg_restore --no-owner --no-acl -f - | grep"
docker exec "$S" sh -c "pg_restore --no-owner --no-acl -f - /tmp/a-none.dump | grep -E 'OWNER TO|GRANT|REVOKE' || echo '(none)'"

banner "plain-text format control: pg_dump -Fp --no-owner (text honours it)"
docker exec "$S" sh -c "pg_dump -U postgres -d app -Fp --no-owner | grep -E 'OWNER TO|GRANT|REVOKE' || echo '(none)'"
