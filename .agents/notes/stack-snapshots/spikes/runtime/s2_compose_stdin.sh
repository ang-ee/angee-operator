#!/usr/bin/env bash
# Spike 2: docker compose run -T, container stdin is the artifact.
# The config cannot also be on stdin, so this spike reads it from a file
# (spike 3 covers the ways to keep it off disk).
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
want=$(sha "$DATA/p64")
empty=$(printf '' | shasum -a 256 | cut -d' ' -f1)
echo "payload p64 sha256=$want   (empty input sha256=$empty)"

report() { # label rc
  local seen; seen=$(grep -Eo '^[0-9a-f]{64}' "$OUT/s2-$1.err" | head -1)
  echo "exit=$2 time=$(elapsed) container_saw_sha=${seen:-none} match=$([ "$seen" = "$want" ] && echo YES || { [ "$seen" = "$empty" ] && echo "NO(empty stdin)" || echo NO; })"
  echo "stdout bytes=$(wc -c <"$OUT/s2-$1.out" | tr -d ' ')"; echo "stderr:"; show_err "$OUT/s2-$1.err"
}

hr "a: cat p64 | docker compose -f cfg/compose.yaml run --rm --no-deps -T --name N restore   (no -i flag)"
t0; cat "$DATA/p64" | docker compose -f "$CFG/compose.yaml" run --rm --no-deps -T --name "angee-spike-s2-$(rid)" restore >"$OUT/s2-a.out" 2>"$OUT/s2-a.err"
report a "${PIPESTATUS[1]}"

hr "b: same with --interactive=false (to prove -i defaults to true)"
t0; cat "$DATA/p64" | docker compose -f "$CFG/compose.yaml" run --rm --no-deps -T --interactive=false --name "angee-spike-s2-$(rid)" restore >"$OUT/s2-b.out" 2>"$OUT/s2-b.err"
report b "${PIPESTATUS[1]}"

hr "c: payload redirected from a file instead of a pipe: docker compose -f cfg run ... restore < p64"
t0; docker compose -f "$CFG/compose.yaml" run --rm --no-deps -T --name "angee-spike-s2-$(rid)" restore <"$DATA/p64" >"$OUT/s2-c.out" 2>"$OUT/s2-c.err"
report c $?

hr "d: TODAY's Angee shape: docker compose -f - run ... restore < cfg  (what does the job's stdin see?)"
t0; docker compose -f - run --rm --no-deps -T --name "angee-spike-s2-$(rid)" restore <"$CFG/compose.yaml" >"$OUT/s2-d.out" 2>"$OUT/s2-d.err"
report d $?

hr "e: config then payload concatenated on one stdin (-f -)"
t0; cat "$CFG/compose.yaml" "$DATA/p64" | docker compose -f - run --rm --no-deps -T --name "angee-spike-s2-$(rid)" restore >"$OUT/s2-e.out" 2>"$OUT/s2-e.err"
report e "${PIPESTATUS[1]}"

hr "leftover containers"
docker ps -a --filter name=angee-spike-s2 --format '{{.Names}} {{.Status}}'
echo "(end)"
