# Shared helpers for the database spikes. Source from a spike script.
W="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export W
PGV=pgvector/pgvector:pg17

# wait_pg <container> <user> [db]: wait until the server accepts connections
# AND the image entrypoint has finished its init (it restarts the server once).
wait_pg() {
  local c="$1" u="$2" db="${3:-postgres}" i
  for i in $(seq 1 120); do
    if docker logs "$c" 2>&1 | grep -q 'database system is ready to accept connections' &&
       docker exec "$c" pg_isready -U "$u" -d "$db" -q 2>/dev/null &&
       docker exec "$c" psql -U "$u" -d "$db" -Atc 'select 1' >/dev/null 2>&1; then
      # entrypoint init prints "PostgreSQL init process complete" before final start
      if docker logs "$c" 2>&1 | grep -q 'PostgreSQL init process complete' ||
         docker logs "$c" 2>&1 | grep -q 'Skipping initialization'; then
        return 0
      fi
    fi
    sleep 0.5
  done
  echo "timeout waiting for $c" >&2
  docker logs "$c" 2>&1 | tail -20 >&2
  return 1
}

rm_c() { docker rm -f "$@" >/dev/null 2>&1 || true; }

banner() { printf '\n===== %s =====\n' "$*"; }

# rowcount <container> <user> <db> <table>: prints count or MISSING
rowcount() {
  local c="$1" u="$2" db="$3" t="$4"
  if [ "$(docker exec "$c" psql -U "$u" -X -At -d "$db" -c "select to_regclass('$t') is not null")" = t ]; then
    docker exec "$c" psql -U "$u" -X -At -d "$db" -c "select count(*) from $t"
  else
    echo MISSING
  fi
}

# run_rc <label> <cmd...>: run, print exit code
run_rc() {
  local label="$1"; shift
  "$@"
  local rc=$?
  echo "[$label] exit=$rc"
  return 0
}
