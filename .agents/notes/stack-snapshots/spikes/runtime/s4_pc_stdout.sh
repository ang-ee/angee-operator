#!/usr/bin/env bash
# Spike 4: process-compose foreground run, the process's stdout is the artifact.
# Exact Angee flags: --no-server --log-file <dir>/process-compose.log --log-no-color run --no-deps --config <cfg> <name>
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
want=$(sha "$DATA/p64")
echo "payload p64 sha256=$want"

pc_case() { # label stdin-redirect-target wrapper...
  local label=$1 stdin=$2; shift 2
  local priv; priv=$(mktemp -d "$OUT/angee-process-job-XXXXXX")
  install -m 0600 "$CFG/pc.yaml" "$priv/process-compose.yaml"
  rm -rf "$OUT/run/logs"; mkdir -p "$OUT/run/logs"
  hr "$label: $* process-compose --no-server --log-file PRIV/process-compose.log --log-no-color run --no-deps --config PRIV/process-compose.yaml dump <$stdin >out 2>err"
  t0
  "$@" process-compose --no-server --log-file "$priv/process-compose.log" --log-no-color run --no-deps --config "$priv/process-compose.yaml" dump <"$stdin" >"$OUT/s4-$label.out" 2>"$OUT/s4-$label.err"
  local rc=$? got; got=$(sha "$OUT/s4-$label.out")
  echo "exit=$rc time=$(elapsed) stdout_bytes=$(wc -c <"$OUT/s4-$label.out" | tr -d ' ') match=$([ "$got" = "$want" ] && echo YES || echo NO)"
  echo "stderr:"; show_err "$OUT/s4-$label.err"
  echo "native --log-file bytes=$(wc -c <"$priv/process-compose.log" | tr -d ' '); log_location files: $(ls -l "$OUT/run/logs" | awk 'NR>1{printf "%s=%s ", $9, $5}')"
  echo "log_location head:"; head -c 300 "$OUT/run/logs/dump.log" 2>/dev/null | LC_ALL=C cat -v | head -5; echo
  echo "native log tail:"; tail -n 6 "$priv/process-compose.log"
  rm -rf "$priv"
}

pc_case a-stdin-devnull /dev/null
pc_case b-go-wrapped-pgid /dev/null "$D/bin/gorun" -wrap -pgid --

hr "c: stdout is a pipe (| cat >out) instead of a file"
priv=$(mktemp -d "$OUT/angee-process-job-XXXXXX"); install -m 0600 "$CFG/pc.yaml" "$priv/process-compose.yaml"
process-compose --no-server --log-file "$priv/process-compose.log" --log-no-color run --no-deps --config "$priv/process-compose.yaml" dump </dev/null 2>"$OUT/s4-c.err" | cat >"$OUT/s4-c.out"
echo "exit=${PIPESTATUS[0]} match=$([ "$(sha "$OUT/s4-c.out")" = "$want" ] && echo YES || echo NO)"; echo "stderr:"; show_err "$OUT/s4-c.err"; rm -rf "$priv"

hr "d: stderr is a pty (script), stdout to a file"
priv=$(mktemp -d "$OUT/angee-process-job-XXXXXX"); install -m 0600 "$CFG/pc.yaml" "$priv/process-compose.yaml"
script -q /dev/null bash -c "process-compose --no-server --log-file '$priv/process-compose.log' --log-no-color run --no-deps --config '$priv/process-compose.yaml' dump >'$OUT/s4-d.out' </dev/null" >"$OUT/s4-d.tty" 2>&1 </dev/null
echo "exit=$? match=$([ "$(sha "$OUT/s4-d.out")" = "$want" ] && echo YES || echo NO)"; echo "pty:"; LC_ALL=C cat -v "$OUT/s4-d.tty" | head -c 600; echo; rm -rf "$priv"
echo "(end)"
