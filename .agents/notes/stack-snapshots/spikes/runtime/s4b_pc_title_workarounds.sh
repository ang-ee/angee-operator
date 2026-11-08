#!/usr/bin/env bash
# Spike 4b: process-compose prefixes stdout with an OSC 0 terminal-title escape
# (src/util/terminal.go SetTerminalTitle, called unconditionally by runHeadless).
#  a) with `name:` pinned, is the prefix exactly ESC]0;process-compose: <hostname>/<name>BEL ?
#     Strip exactly that and compare hashes.
#  b) does the title depend on cwd when name: is absent? (spike 4 showed <host>/runtime)
#  c) alternative channel: the job writes its artifact to an inherited fd 3.
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
want=$(sha "$DATA/p64")
priv=$(mktemp -d "$OUT/angee-process-job-XXXXXX"); install -m 0600 "$CFG/pc-extra.yaml" "$priv/process-compose.yaml"
PC=(process-compose --no-server --log-file "$priv/process-compose.log" --log-no-color run --no-deps --config "$priv/process-compose.yaml")

hr "a: ${PC[*]} dump </dev/null >out"
"${PC[@]}" dump </dev/null >"$OUT/s4b-a.out" 2>"$OUT/s4b-a.err"; rc=$?
host=$(hostname)
printf '\033]0;process-compose: %s/%s\007' "$host" angee-job >"$OUT/s4b-expected-prefix"
n=$(wc -c <"$OUT/s4b-expected-prefix" | tr -d ' ')
echo "exit=$rc stdout_bytes=$(wc -c <"$OUT/s4b-a.out" | tr -d ' ') expected_prefix_bytes=$n prefix_escaped=$(LC_ALL=C cat -v "$OUT/s4b-expected-prefix")"
if head -c "$n" "$OUT/s4b-a.out" | cmp -s - "$OUT/s4b-expected-prefix"; then echo "prefix matches exactly"; else echo "PREFIX DIFFERS: $(head -c 80 "$OUT/s4b-a.out" | LC_ALL=C cat -v)"; fi
got=$(tail -c +$((n+1)) "$OUT/s4b-a.out" | shasum -a 256 | cut -d' ' -f1)
echo "after stripping prefix: sha=$got match=$([ "$got" = "$want" ] && echo YES || echo NO)"
echo "stderr:"; show_err "$OUT/s4b-a.err"

hr "b: no name:, cwd=/ : title follows cwd basename"
grep -v '^name:' "$CFG/pc-extra.yaml" >"$priv/noname.yaml"
(cd / && process-compose --no-server --log-file "$priv/pc2.log" --log-no-color run --no-deps --config "$priv/noname.yaml" dump </dev/null 2>/dev/null | head -c 60 | LC_ALL=C cat -v); echo

hr "c: artifact on inherited fd 3: ${PC[*]} fd3dump 3>fd3.out >stdout.out"
"${PC[@]}" fd3dump </dev/null 3>"$OUT/s4b-c.fd3" >"$OUT/s4b-c.out" 2>"$OUT/s4b-c.err"; rc=$?
got=$(sha "$OUT/s4b-c.fd3")
echo "exit=$rc fd3_bytes=$(wc -c <"$OUT/s4b-c.fd3" | tr -d ' ') fd3_match=$([ "$got" = "$want" ] && echo YES || echo NO) stdout_bytes=$(wc -c <"$OUT/s4b-c.out" | tr -d ' ') stdout=$(LC_ALL=C cat -v "$OUT/s4b-c.out")"
echo "stderr:"; show_err "$OUT/s4b-c.err"
rm -rf "$priv"
echo "(end)"
