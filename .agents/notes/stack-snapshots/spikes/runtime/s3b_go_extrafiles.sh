#!/usr/bin/env bash
# Spike 3b: the Go-faithful version of spike 3a. The compose YAML travels on an
# os.Pipe passed as cmd.ExtraFiles[0] (fd 3, nothing on disk); stdin/stdout are
# wrapped io.Reader/io.Writer so os/exec copies through its own pipes, as a
# service handing a restic stream to the backend would. Run on macOS and inside
# a Linux container shaped like Dockerfile.operator (alpine + docker-cli-compose).
set -u
. <scratchpad>/spikes/runtime/lib.sh
cd "$D"
want=$(sha "$DATA/p64")
REL="$CFG/compose-rel.yaml"
ABS="$CFG/compose.yaml"
G="$D/bin/gorun"

hr "mac restore: gorun -config compose-rel.yaml -wrap -- docker compose --project-directory ROOT -f /dev/fd/3 run --rm --no-deps -T --name N restore-rel <p64"
"$G" -config "$REL" -wrap -- docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T --name "angee-spike-s3b-$(rid)" restore-rel <"$DATA/p64" >"$OUT/s3b-r.out" 2>"$OUT/s3b-r.err"
echo "rc=$?"; grep -E 'bind-|^[0-9a-f]{64}|gorun:' "$OUT/s3b-r.err"; echo "match=$(grep -q "^$want" "$OUT/s3b-r.err" && echo YES || echo NO)"

hr "mac save: gorun -config compose.yaml -wrap -- docker compose --project-directory ROOT -f /dev/fd/3 run --rm --no-deps -T --name N dump >out"
"$G" -config "$ABS" -wrap -- docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T --name "angee-spike-s3b-$(rid)" dump </dev/null >"$OUT/s3b-s.out" 2>"$OUT/s3b-s.err"
echo "rc=$? stdout_sha=$(sha "$OUT/s3b-s.out") match=$([ "$(sha "$OUT/s3b-s.out")" = "$want" ] && echo YES || echo NO)"; grep gorun: "$OUT/s3b-s.err"

hr "linux image build (alpine:3 + docker-cli docker-cli-compose)"
docker build -q -t angee-spike-cli:1 "$D/linux-cli" 2>&1 | tail -1
docker run --rm angee-spike-cli:1 sh -c 'docker --version; docker compose version; cat /etc/alpine-release'
LX=(docker run --rm -i --name "angee-spike-lx-$(rid)" -v /var/run/docker.sock:/var/run/docker.sock -v "$D:$D" -w "$D" angee-spike-cli:1)

hr "linux restore, fd 3 = Go os.Pipe: gorun-linux -config compose-rel.yaml -wrap -- docker compose --project-directory ROOT -f /dev/fd/3 run ... restore-rel <p64"
"${LX[@]}" "$D/bin/gorun-linux" -config "$REL" -wrap -- docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T --name "angee-spike-s3b-$(rid)" restore-rel <"$DATA/p64" >"$OUT/s3b-lr.out" 2>"$OUT/s3b-lr.err"
echo "rc=$?"; grep -E 'bind-|^[0-9a-f]{64}|gorun:|rror' "$OUT/s3b-lr.err"; echo "match=$(grep -q "^$want" "$OUT/s3b-lr.err" && echo YES || echo NO)"

hr "linux restore, fd 3 = regular file redirect: sh -c 'docker compose --project-directory ROOT -f /dev/fd/3 run ... restore-rel 3<cfg' <p64"
"${LX[@]}" sh -c "docker compose --project-directory '$D' -f /dev/fd/3 run --rm --no-deps -T --name angee-spike-s3b-$(rid) restore-rel 3<'$REL'" <"$DATA/p64" >"$OUT/s3b-lf.out" 2>"$OUT/s3b-lf.err"
echo "rc=$?"; grep -E 'bind-|^[0-9a-f]{64}|rror' "$OUT/s3b-lf.err"; echo "match=$(grep -q "^$want" "$OUT/s3b-lf.err" && echo YES || echo NO)"

hr "linux save, fd 3 = Go os.Pipe: gorun-linux -config compose.yaml -wrap -- docker compose --project-directory ROOT -f /dev/fd/3 run ... dump >out"
"${LX[@]}" "$D/bin/gorun-linux" -config "$ABS" -wrap -- docker compose --project-directory "$D" -f /dev/fd/3 run --rm --no-deps -T --name "angee-spike-s3b-$(rid)" dump </dev/null >"$OUT/s3b-ls.out" 2>"$OUT/s3b-ls.err"
echo "rc=$? stdout_sha=$(sha "$OUT/s3b-ls.out") match=$([ "$(sha "$OUT/s3b-ls.out")" = "$want" ] && echo YES || echo NO)"; grep -E 'gorun:|rror' "$OUT/s3b-ls.err"

hr "leftover containers"
docker ps -a --filter name=angee-spike-s3b --format '{{.Names}} {{.Status}}'
echo "(end)"
