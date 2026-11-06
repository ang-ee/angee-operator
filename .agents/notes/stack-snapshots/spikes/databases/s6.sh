#!/usr/bin/env bash
# Spike 6: dump/restore across major versions (17 -> 16 and 16 -> 17), plain postgres images.
source "$(dirname "$0")/lib.sh"
N=angee-spike-db-net
P16=angee-spike-db-pg16
P17=angee-spike-db-pg17
rm_c "$P16" "$P17"
docker network rm "$N" >/dev/null 2>&1
docker network create "$N" >/dev/null
for v in 16 17; do
  docker run -d --name "angee-spike-db-pg$v" --network "$N" -e POSTGRES_DB=app -e POSTGRES_PASSWORD=pw postgres:$v >/dev/null
done
wait_pg "$P16" postgres app || exit 1
wait_pg "$P17" postgres app || exit 1
for c in "$P16" "$P17"; do
  printf '%s: ' "$c"; docker exec "$c" sh -c 'postgres --version; psql --version' | tr '\n' ' '; echo
  docker exec -i "$c" psql -U postgres -d app -X -q -At < "$W/s6-setup.sql"
done
pwenv=(-e PGPASSWORD=pw)

banner "dumps"
docker exec "$P17" pg_dump -U postgres -d app -Fc -f /tmp/v17.dump && echo "pg_dump17 -Fc ok"
docker exec "$P17" pg_dump -U postgres -d app -Fp -f /tmp/v17.sql && echo "pg_dump17 -Fp ok"
docker exec "$P16" pg_dump -U postgres -d app -Fc -f /tmp/v16.dump && echo "pg_dump16 -Fc ok"
docker cp -q "$P17:/tmp/v17.dump" "$W/dumps/v17.dump"; docker cp -q "$P17:/tmp/v17.sql" "$W/dumps/v17.sql"
docker cp -q "$W/dumps/v17.dump" "$P16:/tmp/v17.dump"; docker cp -q "$W/dumps/v17.sql" "$P16:/tmp/v17.sql"
docker cp -q "$P16:/tmp/v16.dump" "$W/dumps/v16.dump"; docker cp -q "$W/dumps/v16.dump" "$P17:/tmp/v16.dump"
echo "archive versions:"
for f in v17 v16; do docker exec "$P17" sh -c "pg_restore -l /tmp/$f.dump | grep -E 'Dump Version|Dumped (from|by)'"; done
echo "SET lines in the 17 plain dump:"; docker exec "$P17" sh -c "grep -nE '^SET |^\\\\(un)?restrict|^SELECT pg_catalog.set_config' /tmp/v17.sql"

banner "A1: 17 archive -> pg_restore 16 (client 16, server 16)"
docker exec "$P16" createdb -U postgres a1
docker exec "$P16" pg_restore -U postgres -d a1 --no-owner --no-acl /tmp/v17.dump; echo "exit=$?"

banner "A2: 17 archive -> pg_restore 17 client -> 16 server, default error handling"
docker exec "$P16" createdb -U postgres a2
docker exec "${pwenv[@]}" "$P17" pg_restore -h "$P16" -U postgres -d a2 --no-owner --no-acl /tmp/v17.dump; echo "exit=$?"
echo "  rows in 16 a2: $(rowcount "$P16" postgres a2 t)"
banner "A2b: same, --single-transaction --exit-on-error"
docker exec "$P16" createdb -U postgres a2b
docker exec "${pwenv[@]}" "$P17" pg_restore -h "$P16" -U postgres -d a2b --no-owner --no-acl --single-transaction --exit-on-error /tmp/v17.dump; echo "exit=$?"
echo "  rows in 16 a2b: $(rowcount "$P16" postgres a2b t)"

banner "A3: 17 plain SQL -> psql 16"
docker exec "$P16" createdb -U postgres a3
docker exec "$P16" psql -U postgres -d a3 -X -q -f /tmp/v17.sql; echo "exit=$?"
echo "  rows in 16 a3: $(rowcount "$P16" postgres a3 t)"
banner "A3b: 17 plain SQL -> psql 16 with ON_ERROR_STOP=1 --single-transaction"
docker exec "$P16" createdb -U postgres a3b
docker exec "$P16" psql -U postgres -d a3b -X -q -v ON_ERROR_STOP=1 --single-transaction -f /tmp/v17.sql; echo "exit=$?"
echo "  rows in 16 a3b: $(rowcount "$P16" postgres a3b t)"

banner "B1: 16 archive -> pg_restore 17 -> 17 server, strict flags"
docker exec "$P17" createdb -U postgres b1
docker exec "$P17" pg_restore -U postgres -d b1 --no-owner --no-acl --single-transaction --exit-on-error /tmp/v16.dump; echo "exit=$?"
echo "  rows in 17 b1: $(rowcount "$P17" postgres b1 t)  t_count(): $(docker exec "$P17" psql -U postgres -d b1 -X -At -c 'select t_count()')"
banner "B2: 16 archive -> pg_restore 16 client -> 17 server, strict flags"
docker exec "$P17" createdb -U postgres b2
docker exec "${pwenv[@]}" "$P16" pg_restore -h "$P17" -U postgres -d b2 --no-owner --no-acl --single-transaction --exit-on-error /tmp/v16.dump; echo "exit=$?"
echo "  rows in 17 b2: $(rowcount "$P17" postgres b2 t)"

banner "C: client/server mismatch at dump time"
echo "--- pg_dump 16 against 17 server"
docker exec "${pwenv[@]}" "$P16" pg_dump -h "$P17" -U postgres -d app -Fc -f /tmp/x.dump; echo "exit=$?"
echo "--- pg_dump 17 against 16 server"
docker exec "${pwenv[@]}" "$P17" pg_dump -h "$P16" -U postgres -d app -Fc -f /tmp/y.dump; echo "exit=$?"
docker exec "$P17" sh -c "pg_restore -l /tmp/y.dump | grep -E 'Dump Version|Dumped (from|by)'"
echo "--- that 17-client archive of a 16 server -> pg_restore 16 into 16"
docker cp -q "$P17:/tmp/y.dump" "$W/dumps/y.dump"; docker cp -q "$W/dumps/y.dump" "$P16:/tmp/y.dump"
docker exec "$P16" createdb -U postgres c1
docker exec "$P16" pg_restore -U postgres -d c1 --no-owner --no-acl --single-transaction --exit-on-error /tmp/y.dump; echo "exit=$?"
