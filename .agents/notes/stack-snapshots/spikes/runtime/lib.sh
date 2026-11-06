# Shared helpers for the runtime stream spikes. Source, do not execute.
D=<scratchpad>/spikes/runtime
DATA="$D/data"
OUT="$D/out"
CFG="$D/cfg"
mkdir -p "$OUT"

# sha FILE -> hex digest
sha() { shasum -a 256 "$1" | cut -d' ' -f1; }

# t0 / elapsed: wall clock seconds since t0
t0() { T0=$EPOCHREALTIME; }
elapsed() { awk -v a="$T0" -v b="$EPOCHREALTIME" 'BEGIN{printf "%.2fs", b-a}'; }

# rid: short random id for container names
rid() { od -An -N4 -tx1 /dev/urandom | tr -d ' \n'; }

# compose_run NAME [extra run args...] -- the exact Angee transient invocation,
# config on stdin is the caller's job.
COMPOSE_RUN=(docker compose -f - run --rm --no-deps -T)

hr() { printf -- '---- %s\n' "$*"; }

# show_err FILE: print a file that should be text, escaping non-printables
show_err() { if [ -s "$1" ]; then LC_ALL=C cat -v "$1" | head -c 2000; echo; else echo "(empty)"; fi; }
