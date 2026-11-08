#!/usr/bin/env bash
# Spike 8: Redis 7 restore of a dump.rdb with AOF on / off.
source "$(dirname "$0")/lib.sh"
IMG=redis:7-alpine
R="$W/redis"; mkdir -p "$R"
P=angee-spike-db-redis

cleanup() {
  for c in src a b d e f; do rm_c "$P-$c"; docker volume rm "$P-$c" >/dev/null 2>&1; done
}
cleanup

wait_redis() { local i; for i in $(seq 1 60); do docker exec "$1" redis-cli ping 2>/dev/null | grep -q PONG && return 0; sleep 0.3; done; return 1; }
lsdata() { docker run --rm -v "$1:/data" alpine:3 sh -c 'cd /data && find . -type f -exec ls -l {} \; | awk "{print \"      \", \$5, \$9}"'; }
report() { # report <container> <volume>
  echo "    DBSIZE=$(docker exec "$1" redis-cli dbsize)  marker=$(docker exec "$1" redis-cli get marker)  other=$(docker exec "$1" redis-cli get other)  aof_enabled=$(docker exec "$1" redis-cli info persistence | grep -E '^aof_enabled' | tr -d '\r')"
  echo "    log:"; docker logs "$1" 2>&1 | grep -iE 'DB loaded|AOF|RDB|append|Ready|No AOF|Creating|base|loading' | sed 's/^/      /'
  echo "    files in /data:"; lsdata "$2"
}
# start_with_rdb <suffix> <redis args...>: fresh volume holding only dump.rdb, then start
start_with_rdb() {
  local s="$1"; shift
  docker volume create "$P-$s" >/dev/null
  docker create --name "$P-$s" -v "$P-$s:/data" "$IMG" redis-server "$@" >/dev/null
  docker cp -q "$R/dump.rdb" "$P-$s:/data/dump.rdb"
  docker start "$P-$s" >/dev/null
  wait_redis "$P-$s"
}

banner "source: redis-server --appendonly yes"
docker volume create "$P-src" >/dev/null
docker run -d --name "$P-src" -v "$P-src:/data" "$IMG" redis-server --appendonly yes >/dev/null
wait_redis "$P-src"
docker exec "$P-src" redis-server --version
{ for i in $(seq 1 1000); do echo "SET key:$i val$i"; done; echo "SET marker from-source"; } | docker exec -i "$P-src" redis-cli > /dev/null
echo "source DBSIZE=$(docker exec "$P-src" redis-cli dbsize)"
docker exec "$P-src" redis-cli bgsave
for i in $(seq 1 50); do docker exec "$P-src" redis-cli info persistence | grep -q 'rdb_bgsave_in_progress:0' && break; sleep 0.2; done
docker exec "$P-src" redis-cli info persistence | grep -E 'rdb_last_bgsave_status|aof_enabled' | tr -d '\r'
docker cp -q "$P-src:/data/dump.rdb" "$R/dump.rdb"
echo "copied dump.rdb ($(stat -f %z "$R/dump.rdb") bytes); source /data:"; lsdata "$P-src"

banner "A: fresh data dir with ONLY dump.rdb, --appendonly yes"
start_with_rdb a --appendonly yes
report "$P-a" "$P-a"

banner "B: fresh data dir with ONLY dump.rdb, --appendonly no"
start_with_rdb b --appendonly no
report "$P-b" "$P-b"

banner "D: restore INTO an existing AOF data dir (target already ran with AOF, has key other=1)"
docker volume create "$P-d" >/dev/null
docker run -d --name "$P-d" -v "$P-d:/data" "$IMG" redis-server --appendonly yes >/dev/null
wait_redis "$P-d"; docker exec "$P-d" redis-cli set other 1 >/dev/null; docker stop "$P-d" >/dev/null
echo "    target before restore:"; lsdata "$P-d"
docker cp -q "$R/dump.rdb" "$P-d:/data/dump.rdb"
docker start "$P-d" >/dev/null; wait_redis "$P-d"
echo "    (log below includes the first start too)"
report "$P-d" "$P-d"

banner "E: safe AOF restore: load RDB with --appendonly no, CONFIG SET appendonly yes, restart with --appendonly yes"
docker volume create "$P-e" >/dev/null
docker create --name "$P-e" -v "$P-e:/data" "$IMG" redis-server --appendonly no >/dev/null
docker cp -q "$R/dump.rdb" "$P-e:/data/dump.rdb"
docker start "$P-e" >/dev/null; wait_redis "$P-e"
echo "    loaded with AOF off: DBSIZE=$(docker exec "$P-e" redis-cli dbsize)"
docker exec "$P-e" redis-cli config set appendonly yes
for i in $(seq 1 50); do docker exec "$P-e" redis-cli info persistence | grep -q 'aof_rewrite_in_progress:0' && docker exec "$P-e" redis-cli info persistence | grep -q 'aof_enabled:1' && break; sleep 0.2; done
sleep 1
docker exec "$P-e" redis-cli info persistence | grep -E 'aof_enabled|aof_rewrite_in_progress|aof_last_bgrewrite_status' | tr -d '\r' | sed 's/^/    /'
docker stop "$P-e" >/dev/null; docker rm "$P-e" >/dev/null
docker run -d --name "$P-e" -v "$P-e:/data" "$IMG" redis-server --appendonly yes >/dev/null; wait_redis "$P-e"
echo "    after restart with --appendonly yes:"
report "$P-e" "$P-e"

banner "F: dump.rdb + appendonlydir copied together from the source (file-level copy of /data while stopped)"
docker stop "$P-src" >/dev/null
docker volume create "$P-f" >/dev/null
docker run --rm -v "$P-src:/from" -v "$P-f:/to" alpine:3 sh -c 'cp -a /from/. /to/'
docker run -d --name "$P-f" -v "$P-f:/data" "$IMG" redis-server --appendonly yes >/dev/null; wait_redis "$P-f"
report "$P-f" "$P-f"

banner "cleanup"
cleanup; echo "removed redis spike containers and volumes"
