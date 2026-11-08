#!/usr/bin/env bash
# Spike 1: docker compose run -T, container stdout is the artifact.
# Config goes on stdin exactly as Angee's runTransientJob does (-f -).
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
want=$(sha "$DATA/p64")
echo "payload p64 sha256=$want"

case_run() { # label svc [extra run flags...]
  local label=$1 svc=$2; shift 2
  local name="angee-spike-s1-$(rid)"
  hr "$label: docker compose -f - run --rm --no-deps -T $* --name $name $svc  <cfg >out 2>err"
  t0
  "${COMPOSE_RUN[@]}" "$@" --name "$name" "$svc" <"$CFG/compose.yaml" >"$OUT/s1-$label.out" 2>"$OUT/s1-$label.err"
  local rc=$?
  local got; got=$(sha "$OUT/s1-$label.out")
  echo "exit=$rc time=$(elapsed) stdout_bytes=$(wc -c <"$OUT/s1-$label.out" | tr -d ' ') stdout_sha=$got match=$([ "$got" = "$want" ] && echo YES || echo NO)"
  echo "stderr:"; show_err "$OUT/s1-$label.err"
}

# a. Image present, network absent (first run creates the project network).
docker network rm angee-spike-rt_default >/dev/null 2>&1
case_run a-present-fresh-network dump
# b. Image present, network present.
case_run b-present dump
# c. Image present, --quiet-pull.
case_run c-present-quiet-pull dump --quiet-pull
# d. Image absent, default progress.
docker rmi busybox:1.36.1 >/dev/null 2>&1
case_run d-pull-default pulled
# e. Image absent, --quiet-pull.
docker rmi busybox:1.36.1 >/dev/null 2>&1
case_run e-pull-quiet pulled --quiet-pull
# f. Does run -q (\"Don't print anything to STDOUT\") swallow the container's stdout?
case_run f-run-q dump -q
# g. Global --progress plain with image absent (what CI/non-TTY would get anyway).
docker rmi busybox:1.36.1 >/dev/null 2>&1
hr "g-pull-progress-plain: docker compose --progress plain -f - run ... pulled"
name="angee-spike-s1-$(rid)"
docker compose --progress plain -f - run --rm --no-deps -T --name "$name" pulled <"$CFG/compose.yaml" >"$OUT/s1-g.out" 2>"$OUT/s1-g.err"
rc=$?; got=$(sha "$OUT/s1-g.out")
echo "exit=$rc stdout_sha=$got match=$([ "$got" = "$want" ] && echo YES || echo NO)"; echo "stderr:"; show_err "$OUT/s1-g.err"

# h. Leftover containers?
hr "leftover containers"
docker ps -a --filter name=angee-spike-s1 --format '{{.Names}} {{.Status}}'
echo "(end)"
