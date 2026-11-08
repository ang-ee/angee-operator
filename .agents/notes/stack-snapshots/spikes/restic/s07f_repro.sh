#!/bin/bash
# Spike 7 addendum: reproduce the s07d sub-sequence that ended with the live backup at exit 130.
. "$(dirname "$0")/env.sh"
REPO=$W/s07f-repo
fresh_repo "$REPO"
locks() { "$RESTIC" -r "$REPO" list locks --no-lock 2>/dev/null; }
nlocks() { locks | grep -c . ; }
lockpids() { for l in $(locks); do "$RESTIC" -r "$REPO" cat lock "$l" --no-lock 2>/dev/null | python3 -I -c 'import json,sys; d=json.load(sys.stdin); print("   lock pid=%s exclusive=%s" % (d["pid"], d["exclusive"]))'; done; }
for iter in 1 2; do
  echo "== iteration $iter"
  "$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename live.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 20971520 /dev/urandom' > $W/s07f-live.json 2>&1 &
  PL=$!
  for i in $(seq 1 100); do [ "$(nlocks)" -ge 1 ] && break; sleep 0.1; done; sleep 1
  "$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename doomed.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 20971520 /dev/urandom' > /dev/null 2>&1 &
  PD=$!
  echo "  PL=$PL PD=$PD"
  for i in $(seq 1 50); do [ "$(nlocks)" -ge 2 ] && break; sleep 0.1; done; sleep 1
  echo "  before kill:"; lockpids
  echo "  pgrep matches for pkill -f 'head -c 20971520' -P $PD (before kill): $(pgrep -f 'head -c 20971520' -P $PD | tr '\n' ' ')"
  kill -9 $PD; wait $PD 2>/dev/null
  echo "  pgrep matches for pkill -f 'head -c 20971520' -P $PD (after kill): $(pgrep -f 'head -c 20971520' -P $PD | tr '\n' ' ')"
  echo "  all 'head -c 20971520' procs with parents: $(ps -o pid=,ppid=,command= -ax | grep '[h]ead -c 20971520' | awk '{print $1"<-"$2}' | tr '\n' ' ')"
  echo "  after kill:"; lockpids
  "$RESTIC" -r "$REPO" unlock; echo "  unlock exit=$?"; echo "  after unlock:"; lockpids
  wait $PL; echo "  live exit=$?"; grep -E 'exit_error|"error"' $W/s07f-live.json | cut -c1-200
  pkill -f 'head -c 20971520' 2>/dev/null
done
