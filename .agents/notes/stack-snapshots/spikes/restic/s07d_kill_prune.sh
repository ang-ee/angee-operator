#!/bin/bash
# Spike 7 addendum: SIGKILL a prune mid-repack; stale exclusive lock; unlock spares live locks.
. "$(dirname "$0")/env.sh"
REPO=$W/s07d-repo
fresh_repo "$REPO"
locks() { "$RESTIC" -r "$REPO" list locks --no-lock 2>/dev/null; }
nlocks() { locks | grep -c . ; }
wait_lock() { for i in $(seq 1 200); do [ "$(nlocks)" -gt 0 ] && return 0; sleep 0.1; done; return 1; }
lockjson() { for l in $(locks); do "$RESTIC" -r "$REPO" cat lock "$l" --no-lock 2>/dev/null | tr -d '\n' | cut -c1-200; echo; done; }

SRC=$W/s07d-src; rm -rf "$SRC"; mkdir -p "$SRC"
for i in $(seq 1 200); do head -c 1048576 /dev/urandom > "$SRC/f$i"; done
"$RESTIC" -r "$REPO" backup "$SRC" --host h -q
for i in $(seq 1 2 200); do head -c 1048576 /dev/urandom > "$SRC/f$i"; done
"$RESTIC" -r "$REPO" backup "$SRC" --host h -q
FIRST=$("$RESTIC" -r "$REPO" snapshots --json | python3 -I -c 'import json,sys; print(sorted(json.load(sys.stdin), key=lambda s:s["time"])[0]["id"])')
"$RESTIC" -r "$REPO" forget "$FIRST" -q
echo "packs before prune: $(find "$REPO/data" -type f | wc -l | tr -d ' ')  index files: $(ls "$REPO/index" | wc -l | tr -d ' ')"

echo "== 7d: SIGKILL prune mid-repack (throttled to 1 MiB/s)"
"$RESTIC" -r "$REPO" prune --max-unused 0 --limit-download 1024 --limit-upload 1024 > $W/s07d-prune.out 2>&1 &
PP=$!
wait_lock; sleep 6
if kill -0 $PP 2>/dev/null; then kill -9 $PP; wait $PP 2>/dev/null; echo "prune SIGKILLed while running; last output:"; tr '\r' '\n' < $W/s07d-prune.out | grep -v '^$' | tail -3
else echo "WARNING: prune already finished"; fi
echo "locks after prune kill: $(nlocks)"; lockjson
echo "-- backup with a stale EXCLUSIVE lock (dead PID, same host):"
printf 'after-prune-kill' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename after.dump --host h --json > $W/s07d-bk.json 2>&1
echo "  [exit=$?] $(grep exit_error $W/s07d-bk.json | cut -c1-260)"
echo "-- same with --retry-lock 10s:"
printf 'after-prune-kill' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename after.dump --host h --retry-lock 10s --json > $W/s07d-bk2.json 2>&1
echo "  [exit=$?] $(grep exit_error $W/s07d-bk2.json | cut -c1-160)"
echo "-- check with the stale lock (check takes a non-exclusive lock):"
"$RESTIC" -r "$REPO" check > $W/s07d-check0.out 2>&1; echo "  [exit=$?] $(grep -iE 'locked|Fatal' $W/s07d-check0.out | head -1 | cut -c1-200)"
echo "-- unlock while a LIVE backup is running (should remove only the stale lock)"
"$RESTIC" -r "$REPO" unlock; echo "  unlock [exit=$?] locks now: $(nlocks)"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename live.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 20971520 /dev/urandom' > $W/s07d-live.json 2>&1 &
PL=$!
wait_lock; sleep 1
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename doomed.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 20971520 /dev/urandom' > /dev/null 2>&1 &
PD=$!
for i in $(seq 1 50); do [ "$(nlocks)" -ge 2 ] && break; sleep 0.1; done; sleep 1
kill -9 $PD; wait $PD 2>/dev/null; pkill -f 'head -c 20971520' -P $PD 2>/dev/null
echo "  locks (1 live + 1 stale): $(nlocks)"; lockjson
"$RESTIC" -r "$REPO" unlock; echo "  unlock [exit=$?] locks now: $(nlocks)"; lockjson
wait $PL; echo "  live backup exit=$?"
echo "-- integrity after killed prune:"
"$RESTIC" -r "$REPO" check 2>&1 | tail -4
"$RESTIC" -r "$REPO" check --read-data 2>&1 | tail -2
echo "packs now: $(find "$REPO/data" -type f | wc -l | tr -d ' ')  index files: $(ls "$REPO/index" | wc -l | tr -d ' ')"
echo "-- re-run prune to completion:"
"$RESTIC" -r "$REPO" prune --max-unused 0 2>&1 | tail -3
"$RESTIC" -r "$REPO" check 2>&1 | tail -1
