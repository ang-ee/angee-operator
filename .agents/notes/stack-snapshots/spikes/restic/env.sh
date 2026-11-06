# Shared environment for the restic spikes. Source it: . ./env.sh
SP=<scratchpad>
D=$SP/spikes/restic
BIN=$SP/bin
RESTIC=$BIN/restic
REST_SERVER=$BIN/rest-server
W=$D/work
mkdir -p "$W"
# Password lives in a file, never on argv.
if [ ! -f "$D/password.txt" ]; then
  printf 'spike-password-123\n' > "$D/password.txt"
  chmod 600 "$D/password.txt"
fi
export RESTIC_PASSWORD_FILE=$D/password.txt
# Keep the restic cache inside the spike dir.
export RESTIC_CACHE_DIR=$D/cache
unset RESTIC_REPOSITORY RESTIC_PASSWORD RESTIC_PASSWORD_COMMAND RESTIC_HOST

# Helper: run a command, print exit code.
run() { echo "+ $*"; "$@"; rc=$?; echo "[exit=$rc]"; return $rc; }

# Fresh local repo at $1.
fresh_repo() { rm -rf "$1"; "$RESTIC" init -r "$1" --json >/dev/null 2>&1 || { echo "init failed for $1"; return 1; }; }
