#!/usr/bin/env bash
# Spike 6: failure semantics for both runtimes.
#  a) producer exits non-zero midway through stdout
#  b) consumer of stdout dies early (shell pipe into head; Go writer error)
#  c) restore job exits non-zero after reading part of stdin
#  d) cancel mid-stream: SIGKILL (compose backend: exec.CommandContext default)
#     and SIGINT (proccompose backend: cmd.Cancel = os.Interrupt) to the client
#  e) the restore source fails mid-stream (Go reader error)
# Compose config rides on fd 3 (spike 3) so stdin is free.
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
G="$D/bin/gorun"
FAILCFG="$CFG/compose-fail.yaml"
C=(docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T)
priv=$(mktemp -d "$OUT/angee-process-job-XXXXXX"); install -m 0600 "$CFG/pc-extra.yaml" "$priv/process-compose.yaml"
PC=(process-compose --no-server --log-file "$priv/process-compose.log" --log-no-color run --no-deps --config "$priv/process-compose.yaml")
TO=(timeout 90)
cstate() { docker ps -a --filter "name=^$1\$" --format '{{.Status}}' | head -1; }
pcleft() { pgrep -f "$1" >/dev/null && echo "RUNNING($(pgrep -f "$1" | wc -l | tr -d ' ') procs)" || echo gone; }

echo "######## compose ########"
n=angee-spike-s6-$(rid)
hr "6a compose: ${C[*]} --name N fail-mid-save 3<cfg >out"
t0; "${TO[@]}" "${C[@]}" --name "$n" fail-mid-save 3<"$FAILCFG" </dev/null >"$OUT/s6-ca.out" 2>"$OUT/s6-ca.err"; rc=$?
echo "exit=$rc time=$(elapsed) stdout_bytes=$(wc -c <"$OUT/s6-ca.out" | tr -d ' ') container_after=$(cstate "$n")"; grep -v Container "$OUT/s6-ca.err"

n=angee-spike-s6-$(rid)
hr "6b1 compose: ${C[*]} --name N cat-only 3<cfg | head -c 1000 >/dev/null   (512 MiB producer)"
t0; "${TO[@]}" "${C[@]}" --name "$n" cat-only 3<"$FAILCFG" </dev/null 2>"$OUT/s6-cb1.err" | head -c 1000 >/dev/null; rc=("${PIPESTATUS[@]}")
echo "client_exit=${rc[0]} head_exit=${rc[1]} time=$(elapsed) container_after=$(cstate "$n")"; grep -v Container "$OUT/s6-cb1.err" | head -3
for i in $(seq 1 180); do [ -z "$(cstate "$n")" ] && break; sleep 1; done; echo "finite 512 MiB container lingered ~${i}s after the client died, then: $(cstate "$n" || true)${i:+}$([ -z "$(cstate "$n")" ] && echo gone)"
docker rm -f "$n" >/dev/null 2>&1

n=angee-spike-s6-$(rid)
hr "6b2 compose: endless producer, ... --name N stream 3<cfg | head -c 1000 >/dev/null"
t0; "${TO[@]}" "${C[@]}" --name "$n" stream 3<"$FAILCFG" </dev/null 2>"$OUT/s6-cb2.err" | head -c 1000 >/dev/null; rc=("${PIPESTATUS[@]}")
echo "client_exit=${rc[0]} head_exit=${rc[1]} time=$(elapsed) container_after=$(cstate "$n")"; sleep 5; echo "container_after_5s=$(cstate "$n")"
docker rm -f "$n" >/dev/null 2>&1 && echo "docker rm -f $n: removed"

n=angee-spike-s6-$(rid)
hr "6b3 compose: Go writer fails after 1 MiB (os/exec closes the pipe): gorun -wrap -out-fail-after 1048576 -- ${C[*]} --name N stream"
t0; "${TO[@]}" "$G" -wrap -out-fail-after 1048576 -config "$FAILCFG" -- "${C[@]}" --name "$n" stream </dev/null >/dev/null 2>"$OUT/s6-cb3.err"; rc=$?
echo "exit=$rc time=$(elapsed) container_after=$(cstate "$n")"; grep -E 'gorun:' "$OUT/s6-cb3.err"; sleep 5; echo "container_after_5s=$(cstate "$n")"
docker rm -f "$n" >/dev/null 2>&1 && echo "docker rm -f $n: removed"

n=angee-spike-s6-$(rid)
hr "6c compose: cat p512 | ${C[*]} --name N fail-mid-restore 3<cfg"
t0; cat "$DATA/p512" | "${TO[@]}" "${C[@]}" --name "$n" fail-mid-restore 3<"$FAILCFG" >"$OUT/s6-cc.out" 2>"$OUT/s6-cc.err"; rc=("${PIPESTATUS[@]}")
echo "producer_exit=${rc[0]} client_exit=${rc[1]} time=$(elapsed) container_after=$(cstate "$n")"; grep -v Container "$OUT/s6-cc.err"

for mode in kill sigint; do
  n=angee-spike-s6-$(rid)
  flag=""; [ "$mode" = sigint ] && flag="-cancel-sigint"
  hr "6d-save compose ($mode): gorun -cancel-after 3s $flag -config cfg -- ${C[*]} --name N stream >/dev/null"
  t0; "${TO[@]}" "$G" -cancel-after 3s $flag -config "$FAILCFG" -- "${C[@]}" --name "$n" stream </dev/null >/dev/null 2>"$OUT/s6-cd-$mode.err"; rc=$?
  echo "exit=$rc time=$(elapsed) container_now=$(cstate "$n")"; grep -vE 'Container' "$OUT/s6-cd-$mode.err" | head -4
  sleep 5; echo "container_after_5s=$(cstate "$n")"
  docker rm -f "$n" >/dev/null 2>&1 && echo "docker rm -f $n: removed"

  n=angee-spike-s6-$(rid)
  hr "6d-restore compose ($mode): cat /dev/zero | gorun -cancel-after 3s $flag -- ${C[*]} --name N sink"
  t0; cat /dev/zero | "${TO[@]}" "$G" -cancel-after 3s $flag -config "$FAILCFG" -- "${C[@]}" --name "$n" sink >/dev/null 2>"$OUT/s6-cdr-$mode.err"; rc=("${PIPESTATUS[@]}")
  echo "producer_exit=${rc[0]} exit=${rc[1]} time=$(elapsed) container_now=$(cstate "$n")"; grep -vE 'Container' "$OUT/s6-cdr-$mode.err" | head -4
  sleep 5; echo "container_after_5s=$(cstate "$n")"
  docker rm -f "$n" >/dev/null 2>&1 && echo "docker rm -f $n: removed"
done

n=angee-spike-s6-$(rid)
hr "6e compose: restore source errors after 1 MiB: gorun -wrap -in-fail-after 1048576 -- ${C[*]} --name N restore-count < p64"
t0; "${TO[@]}" "$G" -wrap -in-fail-after 1048576 -config "$FAILCFG" -- "${C[@]}" --name "$n" restore-count <"$DATA/p64" >/dev/null 2>"$OUT/s6-ce.err"; rc=$?
echo "exit=$rc time=$(elapsed)"; grep -vE 'Container' "$OUT/s6-ce.err"

echo; echo "######## process-compose ########"
hr "6a pc: ${PC[*]} fail-mid-save >out"
t0; "${TO[@]}" "${PC[@]}" fail-mid-save </dev/null >"$OUT/s6-pa.out" 2>"$OUT/s6-pa.err"; rc=$?
echo "exit=$rc time=$(elapsed) stdout_bytes=$(wc -c <"$OUT/s6-pa.out" | tr -d ' ') (32 MiB + 43 title bytes expected)"; cat "$OUT/s6-pa.err"

hr "6b1 pc: ${PC[*]} cat-only | head -c 1000 >/dev/null"
t0; "${TO[@]}" "${PC[@]}" cat-only </dev/null 2>"$OUT/s6-pb1.err" | head -c 1000 >/dev/null; rc=("${PIPESTATUS[@]}")
echo "client_exit=${rc[0]} head_exit=${rc[1]} time=$(elapsed) job=$(pcleft "$DATA/p512")"; cat "$OUT/s6-pb1.err"; tail -n 4 "$priv/process-compose.log"

hr "6b3 pc: Go writer fails after 1 MiB: gorun -wrap -pgid -out-fail-after 1048576 -- ${PC[*]} cat-only"
t0; "${TO[@]}" "$G" -wrap -pgid -out-fail-after 1048576 -- "${PC[@]}" cat-only </dev/null >/dev/null 2>"$OUT/s6-pb3.err"; rc=$?
echo "exit=$rc time=$(elapsed) job=$(pcleft "$DATA/p512")"; cat "$OUT/s6-pb3.err"

hr "6b4 pc: Go writer fails after 1 MiB on the endless shell loop (each cat dies of SIGPIPE, the loop goes on)"
t0; "${TO[@]}" "$G" -wrap -pgid -out-fail-after 1048576 -wait-delay 11s -cancel-after 8s -cancel-sigint -- "${PC[@]}" stream </dev/null >/dev/null 2>"$OUT/s6-pb4.err"; rc=$?
echo "exit=$rc time=$(elapsed) job=$(pcleft angee-spike-stream)"; tail -n 3 "$OUT/s6-pb4.err"

hr "6c pc: cat p512 | ${PC[*]} fail-mid-restore"
t0; cat "$DATA/p512" | "${TO[@]}" "${PC[@]}" fail-mid-restore >/dev/null 2>"$OUT/s6-pc.err"; rc=("${PIPESTATUS[@]}")
echo "producer_exit=${rc[0]} client_exit=${rc[1]} time=$(elapsed)"; cat "$OUT/s6-pc.err"

for mode in kill sigint; do
  flag=""; [ "$mode" = sigint ] && flag="-cancel-sigint"
  hr "6d-save pc ($mode): gorun -pgid -wait-delay 11s -cancel-after 3s $flag -- ${PC[*]} stream >/dev/null"
  t0; "${TO[@]}" "$G" -pgid -wait-delay 11s -cancel-after 3s $flag -- "${PC[@]}" stream </dev/null >/dev/null 2>"$OUT/s6-pd-$mode.err"; rc=$?
  echo "exit=$rc time=$(elapsed) job_now=$(pcleft angee-spike-stream)"; sleep 2; echo "job_after_2s=$(pcleft angee-spike-stream)"; tail -n 2 "$OUT/s6-pd-$mode.err"
  grep -E "Caught|Shutting|terminat|Exited" "$priv/process-compose.log" | tail -n 3
  pkill -f angee-spike-stream 2>/dev/null && echo "pkill: cleaned leftover stream job"

  hr "6d-restore pc ($mode): cat /dev/zero | gorun -pgid -wait-delay 11s -cancel-after 3s $flag -- ${PC[*]} sink"
  # After a SIGKILL of process-compose the orphaned sink job keeps reading the
  # pipe, so `cat /dev/zero` never gets SIGPIPE and the pipeline would never
  # end (run 1 blocked until the orphan was killed by hand). A watchdog
  # records whether the orphan is still alive at +8s and kills its group.
  ( sleep 8; p=$(pgrep -f angee-spike-sink | head -1); if [ -n "$p" ]; then g=$(ps -o pgid= -p "$p" | tr -d ' '); echo "watchdog@8s: orphan sink pid=$p ppid=$(ps -o ppid= -p "$p" | tr -d ' ') pgid=$g still reading; kill -9 -- -$g" >"$OUT/s6-watchdog-$mode"; kill -9 -- "-$g"; fi ) &
  wd=$!
  t0; cat /dev/zero | "${TO[@]}" "$G" -pgid -wait-delay 11s -cancel-after 3s $flag -- "${PC[@]}" sink >/dev/null 2>"$OUT/s6-pdr-$mode.err"; rc=("${PIPESTATUS[@]}")
  echo "producer_exit=${rc[0]} exit=${rc[1]} time=$(elapsed) job_now=$(pcleft angee-spike-sink)"; tail -n 2 "$OUT/s6-pdr-$mode.err"
  wait "$wd"; cat "$OUT/s6-watchdog-$mode" 2>/dev/null; rm -f "$OUT/s6-watchdog-$mode"
done

hr "6e pc: restore source errors after 1 MiB: gorun -wrap -pgid -in-fail-after 1048576 -- ${PC[*]} restore-count < p64"
t0; "${TO[@]}" "$G" -wrap -pgid -in-fail-after 1048576 -- "${PC[@]}" restore-count <"$DATA/p64" >/dev/null 2>"$OUT/s6-pe.err"; rc=$?
echo "exit=$rc time=$(elapsed)"; cat "$OUT/s6-pe.err"

rm -rf "$priv"
hr "leftovers"
docker ps -a --filter 'name=angee-spike-(s[0-9]|lx|rt)' --format '{{.Names}} {{.Status}}'
pgrep -fl "angee-spike-|angee-process-job" || echo "no leftover processes"
echo "(end)"
