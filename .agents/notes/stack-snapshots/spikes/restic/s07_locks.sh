#!/bin/bash
# Spike 7: lock behaviour (concurrent backups, prune vs backup, stale locks after kill -9).
. "$(dirname "$0")/env.sh"
REPO=$W/s07-repo
fresh_repo "$REPO"
now() { python3 -I -c 'import time;print("%.2f"%time.time())'; }
locks() { "$RESTIC" -r "$REPO" list locks --no-lock 2>/dev/null; }
nlocks() { locks | grep -c . ; }
wait_lock() { for i in $(seq 1 200); do [ "$(nlocks)" -gt 0 ] && return 0; sleep 0.1; done; return 1; }
lockjson() { for l in $(locks); do "$RESTIC" -r "$REPO" cat lock "$l" --no-lock 2>/dev/null | tr -d '\n' | cut -c1-260; echo; done; }

echo "== setup: 300 x 1 MiB files, backup, replace half, backup, forget first => half-used packs for prune to repack"
SRC=$W/s07-src; rm -rf "$SRC"; mkdir -p "$SRC"
for i in $(seq 1 300); do head -c 1048576 /dev/urandom > "$SRC/f$i"; done
"$RESTIC" -r "$REPO" backup "$SRC" --host h -q --json > /dev/null
for i in $(seq 1 2 300); do head -c 1048576 /dev/urandom > "$SRC/f$i"; done
"$RESTIC" -r "$REPO" backup "$SRC" --host h -q --json > /dev/null
FIRST=$("$RESTIC" -r "$REPO" snapshots --json | python3 -I -c 'import json,sys; print(sorted(json.load(sys.stdin), key=lambda s:s["time"])[0]["id"])')
"$RESTIC" -r "$REPO" forget "$FIRST" -q
echo "repo data: $(du -sm "$REPO/data" | cut -f1) MiB"

echo
echo "== 7a: two concurrent backups (each ~6 s, throttled)"
t0=$(now)
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename a.dump --host h --limit-upload 10240 --json -- sh -c 'head -c 62914560 /dev/urandom' > $W/s07a1.json 2>&1 &
P1=$!
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename b.dump --host h --limit-upload 10240 --json -- sh -c 'head -c 62914560 /dev/urandom' > $W/s07a2.json 2>&1 &
P2=$!
sleep 2; echo "locks while both run: $(nlocks)"; lockjson
wait $P1; r1=$?; wait $P2; r2=$?
echo "backup1 exit=$r1 backup2 exit=$r2 wall=$(python3 -I -c "print(round($(now)-$t0,1))")s"
grep -h '"message_type":"summary"' $W/s07a1.json $W/s07a2.json | sed -E 's/.*"snapshot_id":"([0-9a-f]{8}).*/  snapshot \1/'

echo
echo "== 7b: prune (exclusive) running, then backup without and with --retry-lock"
t0=$(now)
"$RESTIC" -r "$REPO" prune --max-unused 0 --limit-download 8192 --limit-upload 8192 > $W/s07b-prune.out 2>&1 &
PP=$!
wait_lock && echo "prune holds lock after $(python3 -I -c "print(round($(now)-$t0,1))")s:"; lockjson
sleep 1
echo "-- backup without --retry-lock:"
t1=$(now)
printf 'hello' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename x.dump --host h --json > $W/s07b-bk1.json 2>&1
echo "  [exit=$?] took $(python3 -I -c "print(round($(now)-$t1,1))")s"; cat $W/s07b-bk1.json | cut -c1-400
echo "-- snapshots (read-only, takes non-exclusive lock) without --retry-lock:"
"$RESTIC" -r "$REPO" snapshots --json > /dev/null 2>$W/s07b-snap.err; echo "  [exit=$?] $(cut -c1-300 $W/s07b-snap.err)"
echo "-- snapshots --no-lock during prune:"
"$RESTIC" -r "$REPO" snapshots --json --no-lock > /dev/null 2>$W/s07b-snap2.err; echo "  [exit=$?] $(cut -c1-300 $W/s07b-snap2.err)"
echo "-- backup with --retry-lock 5s (expect to give up if prune still running):"
t1=$(now)
printf 'hello' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename x.dump --host h --retry-lock 5s --json > $W/s07b-bk2.json 2>&1
echo "  [exit=$?] took $(python3 -I -c "print(round($(now)-$t1,1))")s"; cut -c1-400 $W/s07b-bk2.json
echo "  prune still running? $(kill -0 $PP 2>/dev/null && echo yes || echo no)"
echo "-- backup with --retry-lock 5m (expect to wait for prune and succeed):"
t1=$(now)
printf 'hello2' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename x.dump --host h --retry-lock 5m --json > $W/s07b-bk3.json 2>&1
r=$?; t2=$(now)
wait $PP; rp=$?; tp=$(now)
echo "  [exit=$r] took $(python3 -I -c "print(round($t2-$t1,1))")s; prune exit=$rp prune total=$(python3 -I -c "print(round($tp-$t0,1))")s"
grep -E '"message_type":"(summary|exit_error)"' $W/s07b-bk3.json | cut -c1-300
grep -v '^{' $W/s07b-bk3.json | head -3
echo "  prune output tail:"; tail -6 $W/s07b-prune.out

echo
echo "== 7b': backup running, then prune (exclusive) without/with --retry-lock"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename slow.dump --host h --limit-upload 8192 --json -- sh -c 'head -c 52428800 /dev/urandom' > $W/s07b2-bk.json 2>&1 &
PB=$!
wait_lock; sleep 0.5
"$RESTIC" -r "$REPO" prune --dry-run > $W/s07b2-prune.out 2>&1; echo "prune --dry-run during backup: [exit=$?] $(grep -iE 'lock|Fatal' $W/s07b2-prune.out | head -2)"
"$RESTIC" -r "$REPO" prune > $W/s07b2-prune.out 2>&1; echo "prune during backup: [exit=$?] $(grep -iE 'lock|Fatal' $W/s07b2-prune.out | head -3)"
"$RESTIC" -r "$REPO" forget --keep-last 100 --json > $W/s07b2-forget.out 2>&1; echo "forget during backup: [exit=$?] $(grep -iE 'lock|Fatal' $W/s07b2-forget.out | head -2 | cut -c1-300)"
wait $PB; echo "backup exit=$?"

echo
echo "== 7c: kill -9 a backup mid-way -> stale lock"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename killed.dump --host h --limit-upload 4096 --json -- sh -c 'head -c 52428800 /dev/urandom' > $W/s07c-bk.json 2>&1 &
PK=$!
wait_lock; sleep 1.5
kill -9 $PK; wait $PK 2>/dev/null; echo "killed backup (pid $PK) with SIGKILL"
pkill -f 'head -c 52428800' 2>/dev/null
echo "locks after kill: $(nlocks)"; lockjson
echo "-- new backup (non-exclusive) with the stale lock present:"
printf 'after-kill' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename after.dump --host h --json > $W/s07c-bk2.json 2>&1; echo "  [exit=$?] $(grep -E 'exit_error|summary' $W/s07c-bk2.json | cut -c1-200)"; grep -v '^{' $W/s07c-bk2.json | head -3
echo "-- exclusive op (prune --dry-run takes exclusive lock? and real prune) with the stale lock present:"
"$RESTIC" -r "$REPO" prune > $W/s07c-prune.out 2>&1; echo "  prune [exit=$?] $(grep -iE 'lock|stale|Fatal' $W/s07c-prune.out | head -3)"
echo "locks now: $(nlocks)"
echo "-- kill -9 again, then restic unlock"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename killed2.dump --host h --limit-upload 4096 --json -- sh -c 'head -c 52428800 /dev/urandom' > $W/s07c-bk3.json 2>&1 &
PK=$!
wait_lock; sleep 1.5; kill -9 $PK; wait $PK 2>/dev/null; pkill -f 'head -c 52428800' 2>/dev/null
echo "locks after kill: $(nlocks)"
"$RESTIC" -r "$REPO" unlock; echo "  unlock [exit=$?]"
echo "locks after unlock: $(nlocks)"
echo "-- check after kills (orphan packs from killed uploads?)"
"$RESTIC" -r "$REPO" check 2>&1 | tail -3
"$RESTIC" -r "$REPO" prune --dry-run 2>&1 | grep -iE 'unused|to delete|unreferenced' | head -3
echo "-- kill -9 a PRUNE mid-way, then backup"
"$RESTIC" -r "$REPO" forget --keep-last 1 -q
"$RESTIC" -r "$REPO" prune --max-unused 0 --limit-download 2048 --limit-upload 2048 > $W/s07d-prune.out 2>&1 &
PP=$!
wait_lock; sleep 3; kill -9 $PP; wait $PP 2>/dev/null; echo "killed prune"
echo "locks after prune kill: $(nlocks)"; lockjson
printf 'after-prune-kill' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename after2.dump --host h --json > $W/s07d-bk.json 2>&1; echo "  backup after killed prune [exit=$?] $(grep -E 'exit_error|summary' $W/s07d-bk.json | cut -c1-200)"; grep -v '^{' $W/s07d-bk.json | head -3
"$RESTIC" -r "$REPO" check 2>&1 | tail -3
