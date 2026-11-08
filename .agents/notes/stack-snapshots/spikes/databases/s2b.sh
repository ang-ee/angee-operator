#!/usr/bin/env bash
# Spike 2b: does pg_restore -f /dev/null detect a 1-byte flip with zstd / lz4 archive compression?
source "$(dirname "$0")/lib.sh"
S=angee-spike-db-src
D="$W/dumps"
TIMEFORMAT='    real %3Rs'
for z in zstd lz4 gzip:9; do
  n="big-$(echo "$z" | tr ':' '_')"
  banner "pg_dump -Fc -Z $z"
  time docker exec "$S" pg_dump -U postgres -d bigdb -Fc -Z "$z" -f "/tmp/$n.dump"
  docker cp -q "$S:/tmp/$n.dump" "$D/$n.dump"
  ls -l "$D/$n.dump" | awk '{print "    size", $5}'
  echo "--- intact: pg_restore -f /dev/null"
  time docker exec "$S" pg_restore -f /dev/null "/tmp/$n.dump"; echo "    exit=$?"
  cp "$D/$n.dump" "$D/$n-flip1.dump"; python3 -I "$W/flip.py" "$D/$n-flip1.dump" 0.5 1
  docker cp -q "$D/$n-flip1.dump" "$S:/tmp/$n-flip1.dump"
  echo "--- 1-byte flip: pg_restore --list"
  docker exec "$S" sh -c "pg_restore --list /tmp/$n-flip1.dump >/dev/null"; echo "    exit=$?"
  echo "--- 1-byte flip: pg_restore -f /dev/null"
  docker exec "$S" pg_restore -f /dev/null "/tmp/$n-flip1.dump"; echo "    exit=$?"
done
