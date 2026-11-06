#!/usr/bin/env bash
# Spike 8b: after case A (dump.rdb ignored because AOF is on), does a clean stop
# overwrite the restored dump.rdb? And does a later --appendonly no start still see the data?
source "$(dirname "$0")/lib.sh"
IMG=redis:7-alpine
R="$W/redis"
C=angee-spike-db-redis-a2
rm_c "$C"; docker volume rm "$C" >/dev/null 2>&1
wait_redis() { local i; for i in $(seq 1 60); do docker exec "$1" redis-cli ping 2>/dev/null | grep -q PONG && return 0; sleep 0.3; done; return 1; }
lsdata() { docker run --rm -v "$C:/data" alpine:3 sh -c 'cd /data && find . -type f -exec ls -l {} \; | awk "{print \"      \", \$5, \$9}"'; }

docker volume create "$C" >/dev/null
docker create --name "$C" -v "$C:/data" "$IMG" redis-server --appendonly yes >/dev/null
docker cp -q "$R/dump.rdb" "$C:/data/dump.rdb"
docker start "$C" >/dev/null; wait_redis "$C"
echo "started with AOF on + restored dump.rdb: DBSIZE=$(docker exec "$C" redis-cli dbsize)"; lsdata
docker stop "$C" >/dev/null
echo "after docker stop (SIGTERM -> shutdown save):"; lsdata
docker logs "$C" 2>&1 | grep -iE 'Saving|DB saved|bye' | sed 's/^/      /'
docker rm "$C" >/dev/null
docker run -d --name "$C" -v "$C:/data" "$IMG" redis-server --appendonly no >/dev/null; wait_redis "$C"
echo "restart same volume with --appendonly no: DBSIZE=$(docker exec "$C" redis-cli dbsize)"
rm_c "$C"; docker volume rm "$C" >/dev/null 2>&1; echo "cleaned up"
