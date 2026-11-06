#!/usr/bin/env bash
# Spike 7b: three repeats of the key 512 MiB paths (wall seconds only, no
# hashing), because another agent's database containers share the machine.
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
FAILCFG="$CFG/compose-fail.yaml"
C=(docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T)
priv=$(mktemp -d "$OUT/angee-process-job-XXXXXX"); install -m 0600 "$CFG/pc-extra.yaml" "$priv/process-compose.yaml"
PC=(process-compose --no-server --log-file "$priv/process-compose.log" --log-no-color run --no-deps --config "$priv/process-compose.yaml")
G="$D/bin/gorun"
w() { local label=$1 mode=$2; shift 2; local out=""; for i in 1 2 3; do
  t0
  if [ "$mode" = save ]; then "$@" </dev/null >/dev/null 2>/dev/null 3<"$FAILCFG"; else "$@" <"$DATA/p512" >/dev/null 2>/dev/null 3<"$FAILCFG"; fi
  local rc=$?
  out="$out $(elapsed)(rc=$rc)"; done; echo "$label:$out"; }
w "compose save, json-file logging   " save "${C[@]}" dump512
w "compose save, logging: none       " save "${C[@]}" dump512-nolog
w "compose save, none, Go-wrapped    " save "$G" -wrap -config "$FAILCFG" -- "${C[@]}" dump512-nolog
w "compose restore -> cat >/dev/null " restore "${C[@]}" sink
w "compose restore, Go-wrapped       " restore "$G" -wrap -config "$FAILCFG" -- "${C[@]}" sink
w "plain docker run -i restore       " restore docker run --rm -i alpine:3 sh -c 'cat >/dev/null'
w "pc save                           " save "${PC[@]}" dump512
w "pc save, Go-wrapped + pgid        " save "$G" -wrap -pgid -- "${PC[@]}" dump512
w "pc restore -> cat >/dev/null      " restore "${PC[@]}" sink
w "pc restore, Go-wrapped + pgid     " restore "$G" -wrap -pgid -- "${PC[@]}" sink
rm -rf "$priv"
echo "(end)"
