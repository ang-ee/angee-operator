#!/bin/bash
# Spike 5 addendum: where does the ~0.75 s zero-latency floor come from?
. "$(dirname "$0")/env.sh"
REPO=$W/s03-repo   # any existing local repo
t() { local s e; s=$(python3 -I -c 'import time;print(time.time())'); "$@" >/dev/null 2>&1; e=$(python3 -I -c 'import time;print(time.time())'); python3 -I -c "print('%6.3fs' % ($e-$s))"; }
avg() { for i in 1 2 3; do t "$@"; done | tr '\n' ' '; echo; }
echo "python3 startup overhead (timer itself):      $(avg true)"
echo "restic version (process start only):          $(avg "$RESTIC" version)"
echo "restic cat config (open repo = key KDF):      $(avg "$RESTIC" -r "$REPO" cat config --no-lock)"
echo "restic key list --no-lock:                    $(avg "$RESTIC" -r "$REPO" key list --no-lock)"
echo "restic snapshots --no-lock:                   $(avg "$RESTIC" -r "$REPO" snapshots --no-lock --json)"
echo "restic snapshots (with lock):                 $(avg "$RESTIC" -r "$REPO" snapshots --json)"
echo "restic backup --stdin 1 KiB (local repo):     $(for i in 1 2 3; do s=$(python3 -I -c 'import time;print(time.time())'); head -c 1024 /dev/urandom | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename f --host h -q >/dev/null; e=$(python3 -I -c 'import time;print(time.time())'); python3 -I -c "print('%6.3fs' % ($e-$s))"; done | tr '\n' ' ')"
echo "key KDF params (cat key of first key, metadata only):"
K=$("$RESTIC" -r "$REPO" list keys --no-lock | head -1)
"$RESTIC" -r "$REPO" cat key "$K" --no-lock | python3 -I -c 'import json,sys; d=json.load(sys.stdin); print("  kdf=%s N=%s r=%s p=%s" % (d["kdf"], d["N"], d["r"], d["p"]))'
echo "user/sys CPU of one cat config:"
/usr/bin/time -p "$RESTIC" -r "$REPO" cat config --no-lock 2>&1 >/dev/null | tail -3 | tr '\n' ' '; echo
