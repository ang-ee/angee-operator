#!/bin/bash
# Spike 8: exit codes, password command, JSON error shape.
. "$(dirname "$0")/env.sh"
REPO=$W/s08-repo
fresh_repo "$REPO"
printf 'x' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename a.dump --host h -q >/dev/null
printf 'wrong-password\n' > $W/s08-wrong.txt
chmod 600 $W/s08-wrong.txt
T=$W/s08-run.$$
NOPW="env -u RESTIC_PASSWORD_FILE"
# cap <cmd...>: run, report exit code, stdout size and stderr (stderr is NOT merged into stdout)
cap() { "$@" > $T.stdout 2> $T.stderr; rc=$?; echo "  [exit=$rc] stdout_bytes=$(wc -c < $T.stdout | tr -d ' ') stderr:"; tr -d '\r' < $T.stderr | cut -c1-300 | head -6 | sed 's/^/    /'; }

echo "== 8a: wrong password (password file), --json"
cap env RESTIC_PASSWORD_FILE=$W/s08-wrong.txt "$RESTIC" -r "$REPO" snapshots --json
echo "== 8a': wrong password, non-json"
cap env RESTIC_PASSWORD_FILE=$W/s08-wrong.txt "$RESTIC" -r "$REPO" snapshots
echo "== 8b: missing repository (local path)"
cap "$RESTIC" -r "$W/s08-does-not-exist" snapshots --json
echo "== 8b': unreachable rest backend (server not running) -- bounded to 20 s here; full give-up time in s08b"
cap /opt/homebrew/bin/timeout 20 "$RESTIC" -r rest:http://127.0.0.1:18999/ snapshots --json
echo "== 8b'': missing repository on a running rest-server (empty --path)"
mkdir -p $W/s08-rs-empty
"$REST_SERVER" --path $W/s08-rs-empty --listen 127.0.0.1:18002 --no-auth > $W/s08-rs.log 2>&1 &
RSP=$!; sleep 1
cap "$RESTIC" -r rest:http://127.0.0.1:18002/nope/ snapshots --json
kill $RSP; wait $RSP 2>/dev/null; echo "  (rest-server $RSP stopped)"
echo "== 8c: --password-command printing the right password"
cap $NOPW "$RESTIC" -r "$REPO" --password-command "cat $D/password.txt" snapshots --json
echo "== 8c': --password-command via sh -c with a pipe"
cap $NOPW "$RESTIC" -r "$REPO" --password-command "sh -c 'cat $D/password.txt | tr -d x'" snapshots --json
echo "== 8c'': --password-command with a bare pipe (is it run by a shell?)"
cap $NOPW "$RESTIC" -r "$REPO" --password-command "cat $D/password.txt | tr -d x" snapshots --json
echo "== 8c''': RESTIC_PASSWORD_COMMAND env var"
cap $NOPW RESTIC_PASSWORD_COMMAND="cat $D/password.txt" "$RESTIC" -r "$REPO" snapshots --json
echo "== 8d: --password-command printing a wrong password"
cap $NOPW "$RESTIC" -r "$REPO" --password-command "echo nope" snapshots --json
echo "== 8e: --password-command that exits 1"
cap $NOPW "$RESTIC" -r "$REPO" --password-command "sh -c 'echo vault-down >&2; exit 1'" snapshots --json
echo "== 8e': --password-command that prints nothing, exit 0"
cap $NOPW "$RESTIC" -r "$REPO" --password-command "true" snapshots --json
echo "== 8f: no password source at all (stdin = /dev/null)"
cap $NOPW "$RESTIC" -r "$REPO" snapshots --json < /dev/null
echo "== 8f': both password file and password command set"
cap "$RESTIC" -r "$REPO" --password-command "cat $D/password.txt" snapshots --json
echo "== 8g: init on an existing repo"
cap "$RESTIC" -r "$REPO" init --json
echo "== 8h: dump unknown snapshot id"
cap "$RESTIC" -r "$REPO" dump deadbeef /a.dump --json
echo "== 8i: SIGINT during backup"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename int.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 20971520 /dev/urandom' > $T.i.out 2> $T.i.err &
P=$!; sleep 2; kill -INT $P; wait $P; echo "  [exit=$?]"; tr -d '\r' < $T.i.err | grep -v '^$' | tail -2 | cut -c1-300 | sed 's/^/    /'
echo "== 8i': SIGTERM during backup"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename term.dump --host h --limit-upload 2048 --json -- sh -c 'head -c 20971520 /dev/urandom' > $T.j.out 2> $T.j.err &
P=$!; sleep 2; kill -TERM $P; wait $P; echo "  [exit=$?]"; tr -d '\r' < $T.j.err | grep -v '^$' | tail -2 | cut -c1-300 | sed 's/^/    /'
echo "   locks left after SIGINT/SIGTERM: $("$RESTIC" -r "$REPO" list locks --no-lock | grep -c .)"
echo "   snapshots: $("$RESTIC" -r "$REPO" snapshots --json | python3 -I -c 'import json,sys; print(len(json.load(sys.stdin)))') (1 = only the setup snapshot)"
echo "== 8j: invalid flag / usage error"
cap "$RESTIC" -r "$REPO" snapshots --json --bogus-flag
echo "== 8k: non-JSON stderr lines with --json on wrong password"
env RESTIC_PASSWORD_FILE=$W/s08-wrong.txt "$RESTIC" -r "$REPO" snapshots --json 2>&1 >/dev/null | grep -vc '^{' ; echo "  (^ number of non-JSON stderr lines)"
rm -f $T.*
