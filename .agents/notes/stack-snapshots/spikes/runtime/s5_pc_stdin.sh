#!/usr/bin/env bash
# Spike 5: process-compose foreground run, the process's stdin is the artifact.
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
want=$(sha "$DATA/p64")
empty=$(printf '' | shasum -a 256 | cut -d' ' -f1)
priv=$(mktemp -d "$OUT/angee-process-job-XXXXXX"); install -m 0600 "$CFG/pc-extra.yaml" "$priv/process-compose.yaml"
PC=(process-compose --no-server --log-file "$priv/process-compose.log" --log-no-color run --no-deps --config "$priv/process-compose.yaml")
echo "payload sha=$want empty sha=$empty"
echo "PC=${PC[*]}"

report() { # label rc
  local seen; seen=$(grep -Eo '^[0-9a-f]{64}' "$OUT/s5-$1.err" | head -1)
  echo "exit=$2 time=$(elapsed) job_saw_sha=${seen:-none} match=$([ "$seen" = "$want" ] && echo YES || { [ "$seen" = "$empty" ] && echo "NO(empty stdin)" || echo NO; })"
  echo "stderr:"; show_err "$OUT/s5-$1.err"
}

hr "a: pipe: cat p64 | PC restore"
t0; cat "$DATA/p64" | "${PC[@]}" restore >"$OUT/s5-a.out" 2>"$OUT/s5-a.err"; report a "${PIPESTATUS[1]}"

hr "b: file redirect: PC restore < p64"
t0; "${PC[@]}" restore <"$DATA/p64" >"$OUT/s5-b.out" 2>"$OUT/s5-b.err"; report b $?

hr "c: /dev/null: PC restore </dev/null"
t0; "${PC[@]}" restore </dev/null >"$OUT/s5-c.out" 2>"$OUT/s5-c.err"; report c $?

hr "d: Go os/exec with wrapped stdin (own pipe + copy goroutine) and Setpgid like Angee's isolated foreground: gorun -wrap -pgid -- PC restore < p64"
t0; "$D/bin/gorun" -wrap -pgid -- "${PC[@]}" restore <"$DATA/p64" >"$OUT/s5-d.out" 2>"$OUT/s5-d.err"; report d $?

hr "e: Go os/exec, stdin unset (Angee today: cmd.Stdin nil => /dev/null) is case c"

hr "f: stdin is a TTY and the child is in its own process group (Setpgid) -> SIGTTIN? (10s timeout)"
t0; timeout 10 script -q /dev/null "$D/bin/gorun" -pgid -- "${PC[@]}" restore-count >"$OUT/s5-f.tty" 2>&1 </dev/null; rc=$?
echo "exit=$rc time=$(elapsed) (124 = timed out / stopped)"; LC_ALL=C cat -v "$OUT/s5-f.tty" | head -c 400; echo
pgrep -fl "angee-process-job" | head -3; pkill -f "$priv/process-compose.yaml" 2>/dev/null; true

hr "g: stdin is a TTY, same process group (no Setpgid): reads the terminal (expect a wait for input, 5s timeout)"
t0; timeout 5 script -q /dev/null "$D/bin/gorun" -- "${PC[@]}" restore-count >"$OUT/s5-g.tty" 2>&1 </dev/null; rc=$?
echo "exit=$rc time=$(elapsed)"; LC_ALL=C cat -v "$OUT/s5-g.tty" | head -c 400; echo
pkill -f "$priv/process-compose.yaml" 2>/dev/null
rm -rf "$priv"
echo "(end)"
