#!/usr/bin/env bash
# Spike 7: 512 MiB through each path, each direction. Transfer timed with
# /usr/bin/time -l (wall, user+sys CPU and max RSS of the client process);
# hashes checked outside the timed section (save) or inside the job (restore-sha).
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
G="$D/bin/gorun"
want=$(sha "$DATA/p512")
FAILCFG="$CFG/compose-fail.yaml"
C=(docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T)
priv=$(mktemp -d "$OUT/angee-process-job-XXXXXX"); install -m 0600 "$CFG/pc-extra.yaml" "$priv/process-compose.yaml"
PC=(process-compose --no-server --log-file "$priv/process-compose.log" --log-no-color run --no-deps --config "$priv/process-compose.yaml")
TITLE=43
echo "p512 sha=$want"

tl() { # parse /usr/bin/time -l output in $1
  awk '/real/{w=$1; u=$3; s=$5} /maximum resident set size/{r=$1} END{printf "wall=%ss user=%ss sys=%ss client_maxrss=%.0fMiB", w, u, s, r/1048576}' "$1"
}
save() { # label strip-bytes cmd...
  local label=$1 strip=$2; shift 2
  hr "save $label: $*  >out"
  /usr/bin/time -l "$@" </dev/null >"$OUT/s7.out" 2>"$OUT/s7-$label.err" 3<"$FAILCFG"
  local rc=$? got; got=$(tail -c +$((strip+1)) "$OUT/s7.out" | shasum -a 256 | cut -d' ' -f1)
  echo "exit=$rc $(tl "$OUT/s7-$label.err") bytes=$(wc -c <"$OUT/s7.out" | tr -d ' ') match=$([ "$got" = "$want" ] && echo YES || echo NO)$([ "$strip" -gt 0 ] && echo " (after stripping $strip-byte title)")"
  rm -f "$OUT/s7.out"
}
restore() { # label cmd...
  local label=$1; shift
  hr "restore $label: $*  <p512"
  /usr/bin/time -l "$@" <"$DATA/p512" >/dev/null 2>"$OUT/s7-$label.err" 3<"$FAILCFG"
  local rc=$? seen; seen=$(grep -Eo '^[0-9a-f]{64}' "$OUT/s7-$label.err" | head -1)
  echo "exit=$rc $(tl "$OUT/s7-$label.err")$([ -n "$seen" ] && echo " job_sha_match=$([ "$seen" = "$want" ] && echo YES || echo NO)")$(grep -Eo 'sink-eof' "$OUT/s7-$label.err" | head -1 | sed 's/^/ /')"
}

hr "baselines"
/usr/bin/time -l cat "$DATA/p512" >/dev/null 2>"$OUT/s7-base-cat.err"; echo "host cat p512 >/dev/null: $(tl "$OUT/s7-base-cat.err")"
/usr/bin/time -l shasum -a 256 "$DATA/p512" >/dev/null 2>"$OUT/s7-base-sha.err"; echo "host shasum -a 256 p512: $(tl "$OUT/s7-base-sha.err")"
/usr/bin/time -l docker run --rm -v "$DATA:/data:ro" alpine:3 sh -c 'cat /data/p512 >/dev/null' 2>"$OUT/s7-base-bind.err"; echo "container reads bind-mounted p512 internally (no stream): $(tl "$OUT/s7-base-bind.err")"
/usr/bin/time -l docker run --rm -v "$DATA:/data:ro" alpine:3 sha256sum /data/p512 >/dev/null 2>"$OUT/s7-base-csha.err"; echo "container sha256sum on bind mount: $(tl "$OUT/s7-base-csha.err")"
/usr/bin/time -l docker run --rm -i alpine:3 sh -c 'cat >/dev/null' <"$DATA/p512" 2>"$OUT/s7-base-drun-in.err"; echo "plain docker run -i, stdin -> container: $(tl "$OUT/s7-base-drun-in.err")"
/usr/bin/time -l docker run --rm -v "$DATA:/data:ro" alpine:3 cat /data/p512 >"$OUT/s7.out" 2>"$OUT/s7-base-drun-out.err"; echo "plain docker run, container stdout -> host: $(tl "$OUT/s7-base-drun-out.err") match=$([ "$(sha "$OUT/s7.out")" = "$want" ] && echo YES || echo NO)"; rm -f "$OUT/s7.out"

echo; echo "######## compose ########"
save compose-json-log 0 "${C[@]}" --name "angee-spike-s7-$(rid)" dump512
save compose-log-none 0 "${C[@]}" --name "angee-spike-s7-$(rid)" dump512-nolog
save compose-go-wrapped 0 "$G" -wrap -config "$FAILCFG" -- "${C[@]}" --name "angee-spike-s7-$(rid)" dump512-nolog
restore compose-sink "${C[@]}" --name "angee-spike-s7-$(rid)" sink
restore compose-sha "${C[@]}" --name "angee-spike-s7-$(rid)" restore-sha
restore compose-go-wrapped-sink "$G" -wrap -config "$FAILCFG" -- "${C[@]}" --name "angee-spike-s7-$(rid)" sink

hr "docker json-file log of a 512 MiB stdout job (no --rm, then inspect)"
n="angee-spike-s7-log-$(rid)"
docker compose --project-directory "$D" -f /dev/fd/3 run --no-deps -T --name "$n" dump512 3<"$FAILCFG" </dev/null >/dev/null 2>&1
echo "log driver=$(docker inspect -f '{{.HostConfig.LogConfig.Type}}' "$n") AutoRemove(with --rm it is set by compose? this run had none)=$(docker inspect -f '{{.HostConfig.AutoRemove}}' "$n")"
lp=$(docker inspect -f '{{.LogPath}}' "$n")
echo "LogPath=$lp"
echo "docker logs replays stdout bytes=$(docker logs "$n" 2>/dev/null | wc -c | tr -d ' ')"
echo "on-disk json log size in the VM=$(docker run --rm --privileged --pid=host alpine:3 nsenter -t 1 -m -- stat -c %s "$lp" 2>&1 | tail -1)"
docker rm -f "$n" >/dev/null
n="angee-spike-s7-log-$(rid)"
docker compose --project-directory "$D" -f /dev/fd/3 run --no-deps -T --name "$n" dump512-nolog 3<"$FAILCFG" </dev/null >/dev/null 2>&1
echo "logging: driver none -> docker logs: $(docker logs "$n" 2>&1 | head -c 200)"
docker rm -f "$n" >/dev/null
hr "does compose run --rm set AutoRemove (daemon-side removal)?"
n="angee-spike-s7-ar-$(rid)"
docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T --name "$n" sink 3<"$FAILCFG" < <(sleep 3) >/dev/null 2>&1 &
sleep 2; echo "AutoRemove=$(docker inspect -f '{{.HostConfig.AutoRemove}}' "$n" 2>&1)"; wait

echo; echo "######## process-compose ########"
save pc-file "$TITLE" "${PC[@]}" dump512
save pc-go-wrapped-pgid "$TITLE" "$G" -wrap -pgid -- "${PC[@]}" dump512
restore pc-sink "${PC[@]}" sink
restore pc-sha "${PC[@]}" restore-sha
restore pc-go-wrapped-pgid-sink "$G" -wrap -pgid -- "${PC[@]}" sink
echo "log_location/--log-file after the 512 MiB runs: native log bytes=$(wc -c <"$priv/process-compose.log" | tr -d ' ')"
rm -rf "$priv"
hr "leftovers"; docker ps -a --filter 'name=angee-spike-(s[0-9]|lx|rt)' --format '{{.Names}} {{.Status}}'; echo "(end)"
