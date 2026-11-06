#!/usr/bin/env bash
# Spike 3: move the compose YAML off stdin so stdin can carry the payload.
#  a) -f /dev/fd/3 (file redirect and pipe on fd 3)
#  b) 0600 temp file in a private dir
#  c) -f - with --project-directory (stdin still holds the config)
# The config uses a relative bind mount (./data) to show project-dir resolution.
set -u
. <scratchpad>/spikes/runtime/lib.sh
REL="$CFG/compose-rel.yaml"
want=$(sha "$DATA/p64")
empty=$(printf '' | shasum -a 256 | cut -d' ' -f1)
# Run from an unrelated cwd so cwd-based resolution is visible.
mkdir -p "$OUT/elsewhere"; cd "$OUT/elsewhere"
echo "cwd=$PWD  project root=$D  payload sha=$want"

report() { # label rc
  local f="$OUT/s3-$1.err" seen
  seen=$(grep -Eo '^[0-9a-f]{64}' "$f" | head -1)
  echo "exit=$2 bind=$(grep -Eo 'bind-(ok|MISSING)' "$f" | head -1) stdin_sha=${seen:-none} payload_match=$([ "$seen" = "$want" ] && echo YES || { [ "$seen" = "$empty" ] && echo "NO(empty stdin)" || echo NO; })"
  grep -vE '^ Container|^[0-9a-f]{64}|bind-' "$f" | head -5
}
source_of() { grep -E 'source:' | head -1 | sed 's/^ *//'; }

hr "a1: docker compose -f /dev/fd/3 run ... restore-rel 3<cfg <p64   (no --project-directory)"
echo "resolved: $(docker compose -f /dev/fd/3 config 3<"$REL" 2>&1 | source_of)"
docker compose -f /dev/fd/3 run --rm --no-deps -T --name "angee-spike-s3-$(rid)" restore-rel 3<"$REL" <"$DATA/p64" >"$OUT/s3-a1.out" 2>"$OUT/s3-a1.err"
report a1 $?

hr "a2: docker compose --project-directory ROOT -f /dev/fd/3 run ... restore-rel 3<cfg <p64"
echo "resolved: $(docker compose --project-directory "$D" -f /dev/fd/3 config 3<"$REL" 2>&1 | source_of)"
docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T --name "angee-spike-s3-$(rid)" restore-rel 3<"$REL" <"$DATA/p64" >"$OUT/s3-a2.out" 2>"$OUT/s3-a2.err"
report a2 $?

hr "a3: same as a2 but fd 3 is a PIPE: 3< <(cat cfg)"
cat "$DATA/p64" | docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T --name "angee-spike-s3-$(rid)" restore-rel 3< <(cat "$REL") >"$OUT/s3-a3.out" 2>"$OUT/s3-a3.err"
report a3 "${PIPESTATUS[1]}"

hr "a4: project name when the config has no name: and comes from /dev/fd/3"
grep -v '^name:' "$REL" >"$OUT/compose-rel-noname.yaml"
echo "no --project-directory: $(docker compose -f /dev/fd/3 config 3<"$OUT/compose-rel-noname.yaml" 2>&1 | grep -E '^name:')"
echo "with --project-directory ROOT: $(docker compose --project-directory "$D" -f /dev/fd/3 config 3<"$OUT/compose-rel-noname.yaml" 2>&1 | grep -E '^name:')"

PRIV=$(mktemp -d "$OUT/angee-job-XXXXXX"); chmod 700 "$PRIV"
install -m 0600 "$REL" "$PRIV/docker-compose.yaml"
hr "b1: 0600 temp file in private dir, no --project-directory: docker compose -f PRIV/docker-compose.yaml run ... <p64"
echo "resolved: $(docker compose -f "$PRIV/docker-compose.yaml" config 2>&1 | source_of)"
docker compose -f "$PRIV/docker-compose.yaml" run --rm --no-deps -T --name "angee-spike-s3-$(rid)" restore-rel <"$DATA/p64" >"$OUT/s3-b1.out" 2>"$OUT/s3-b1.err"
report b1 $?

hr "b2: 0600 temp file + --project-directory ROOT"
echo "resolved: $(docker compose --project-directory "$D" -f "$PRIV/docker-compose.yaml" config 2>&1 | source_of)"
docker compose --project-directory "$D" -f "$PRIV/docker-compose.yaml" run --rm --no-deps -T --name "angee-spike-s3-$(rid)" restore-rel <"$DATA/p64" >"$OUT/s3-b2.out" 2>"$OUT/s3-b2.err"
report b2 $?
rm -rf "$PRIV"

hr "c1: -f - with --project-directory ROOT, config on stdin (payload cannot ride along)"
echo "resolved: $(docker compose --project-directory "$D" -f - config <"$REL" 2>&1 | source_of)"
docker compose --project-directory "$D" -f - run --rm --no-deps -T --name "angee-spike-s3-$(rid)" restore-rel <"$REL" >"$OUT/s3-c1.out" 2>"$OUT/s3-c1.err"
report c1 $?

hr "c2: -f - without --project-directory (today), cwd=$PWD"
echo "resolved: $(docker compose -f - config <"$REL" 2>&1 | source_of)"
hr "c3: -f - without --project-directory, cwd=ROOT (Angee sets cmd.Dir=root)"
echo "resolved: $(cd "$D" && docker compose -f - config <"$REL" 2>&1 | source_of)"

hr "leftover containers"
docker ps -a --filter name=angee-spike-s3 --format '{{.Names}} {{.Status}}'
echo "(end)"
