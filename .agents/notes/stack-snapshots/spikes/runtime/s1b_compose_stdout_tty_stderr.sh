#!/usr/bin/env bash
# Spike 1b: same as spike 1, but compose's stderr is a real pseudo-terminal
# (an interactive `angee ... > dump` from a shell). Does compose's TTY progress
# UI ever leak onto stdout when stdout is redirected to a file?
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
want=$(sha "$DATA/p64")
docker rmi busybox:1.36.1 >/dev/null 2>&1
for svc in pulled dump; do
  hr "script -q /dev/null bash -c 'docker compose -f - run --rm --no-deps -T $svc <cfg >out'  (stderr is a pty)"
  script -q /dev/null bash -c "docker compose -f - run --rm --no-deps -T --name angee-spike-s1b-$(rid) $svc <'$CFG/compose.yaml' >'$OUT/s1b-$svc.out'" >"$OUT/s1b-$svc.tty" 2>&1 </dev/null
  rc=$?
  got=$(sha "$OUT/s1b-$svc.out")
  echo "exit=$rc stdout_bytes=$(wc -c <"$OUT/s1b-$svc.out" | tr -d ' ') match=$([ "$got" = "$want" ] && echo YES || echo NO)"
  echo "pty (compose stderr) bytes=$(wc -c <"$OUT/s1b-$svc.tty" | tr -d ' '), first 600 chars escaped:"
  LC_ALL=C cat -v "$OUT/s1b-$svc.tty" | head -c 600; echo
done
