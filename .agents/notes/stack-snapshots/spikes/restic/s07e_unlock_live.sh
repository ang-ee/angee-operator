#!/bin/bash
# Spike 7 addendum: does `restic unlock` spare a LIVE process's lock while removing a stale one?
. "$(dirname "$0")/env.sh"
REPO=$W/s07e-repo
fresh_repo "$REPO"
locks() { "$RESTIC" -r "$REPO" list locks --no-lock 2>/dev/null; }
nlocks() { locks | grep -c . ; }
lockpids() { for l in $(locks); do "$RESTIC" -r "$REPO" cat lock "$l" --no-lock 2>/dev/null | python3 -I -c 'import json,sys; d=json.load(sys.stdin); print("pid=%s exclusive=%s time=%s" % (d["pid"], d["exclusive"], d["time"][11:19]))'; done; }

# live: 40 MiB at 2 MiB/s => ~20 s
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename live.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 41943040 /dev/urandom' > $W/s07e-live.out 2> $W/s07e-live.err &
PL=$!
echo "live backup pid=$PL"
for i in $(seq 1 100); do [ "$(nlocks)" -ge 1 ] && break; sleep 0.1; done
lockpids
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename doomed.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 41943040 /dev/urandom' > /dev/null 2>&1 &
PD=$!
echo "doomed backup pid=$PD"
for i in $(seq 1 100); do [ "$(nlocks)" -ge 2 ] && break; sleep 0.1; done
echo "locks before kill: $(nlocks)"; lockpids
kill -9 $PD; wait $PD 2>/dev/null
echo "doomed killed; alive? $(kill -0 $PD 2>/dev/null && echo yes || echo no); live alive? $(kill -0 $PL 2>/dev/null && echo yes || echo no)"
echo "locks after kill: $(nlocks)"; lockpids
"$RESTIC" -r "$REPO" unlock; echo "unlock [exit=$?]"
echo "locks after unlock: $(nlocks)"; lockpids
wait $PL; echo "live backup exit=$?"; tr -d '\r' < $W/s07e-live.err | grep -v '^$' | tail -3 | cut -c1-300
grep -E '"message_type":"summary"' $W/s07e-live.out | cut -c1-120
echo "-- unlock --remove-all while a live backup runs:"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename live2.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 20971520 /dev/urandom' > $W/s07e-live2.out 2> $W/s07e-live2.err &
PL=$!
for i in $(seq 1 100); do [ "$(nlocks)" -ge 1 ] && break; sleep 0.1; done
"$RESTIC" -r "$REPO" unlock --remove-all; echo "unlock --remove-all [exit=$?] locks now: $(nlocks)"
wait $PL; echo "live backup exit=$?"; tr -d '\r' < $W/s07e-live2.err | grep -v '^$' | tail -2 | cut -c1-300
grep -E '"message_type":"summary"' $W/s07e-live2.out | cut -c1-120
