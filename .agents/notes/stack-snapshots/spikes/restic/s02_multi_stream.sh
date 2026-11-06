#!/bin/bash
# Spike 2: can several unknown-length streams land in ONE restic snapshot?
. "$(dirname "$0")/env.sh"
REPO=$W/s02-repo
fresh_repo "$REPO"
TO=/opt/homebrew/bin/timeout

summ() { grep -E '"message_type":"(summary|error)"' "$1" | head -5; }
lsnodes() { "$RESTIC" -r "$REPO" ls --json "$1" | grep '"struct_type":"node"' | sed -E 's/.*"name":"([^"]*)","type":"([^"]*)","path":"([^"]*)".*"size":?([0-9]*).*/  \3 type=\2/' ; }

echo "== 2a: directory with two FIFOs, writers attached (each writes 1 MiB then exits)"
DIR=$W/s02-fifos; rm -rf "$DIR"; mkdir -p "$DIR"
mkfifo "$DIR/db1.pgdump" "$DIR/db2.pgdump"
echo "regular" > "$DIR/regular.txt"
( head -c 1048576 /dev/urandom > "$DIR/db1.pgdump"; echo "writer1 finished rc=$?" ) &
W1=$!
( head -c 1048576 /dev/urandom > "$DIR/db2.pgdump"; echo "writer2 finished rc=$?" ) &
W2=$!
t0=$(date +%s.%N 2>/dev/null || date +%s)
$TO 30 "$RESTIC" -r "$REPO" backup "$DIR" --host h --json > $W/s02a.json 2>$W/s02a.err
rc=$?
echo "[restic exit=$rc] (124 = killed by 30s timeout)"
summ $W/s02a.json; head -c 600 $W/s02a.err
SNAP=$(grep '"message_type":"summary"' $W/s02a.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
[ -n "$SNAP" ] && "$RESTIC" -r "$REPO" ls --json "$SNAP" | grep '"struct_type":"node"' | grep -E 'pgdump|regular'
sleep 1
echo "writers still blocked? jobs:"; jobs -l
kill $W1 $W2 2>/dev/null; wait 2>/dev/null

echo "== 2a': FIFO given directly as a file argument, writer attached"
mkfifo "$DIR/direct.pgdump" 2>/dev/null
( head -c 1048576 /dev/urandom > "$DIR/direct.pgdump"; echo "writer-direct finished rc=$?" ) &
W3=$!
$TO 30 "$RESTIC" -r "$REPO" backup "$DIR/direct.pgdump" --host h --json > $W/s02a2.json 2>$W/s02a2.err
rc=$?
echo "[restic exit=$rc]"
summ $W/s02a2.json; head -c 600 $W/s02a2.err
SNAP=$(grep '"message_type":"summary"' $W/s02a2.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
[ -n "$SNAP" ] && "$RESTIC" -r "$REPO" ls --json "$SNAP" | grep '"struct_type":"node"' | grep pgdump
sleep 1; jobs -l; kill $W3 2>/dev/null; wait 2>/dev/null

echo "== 2a'': /dev/fd/N of a process-substitution pipe as a file argument"
$TO 30 "$RESTIC" -r "$REPO" backup /dev/fd/3 --host h --json 3< <(head -c 1048576 /dev/urandom) > $W/s02a3.json 2>$W/s02a3.err
rc=$?
echo "[restic exit=$rc]"
summ $W/s02a3.json; head -c 600 $W/s02a3.err
SNAP=$(grep '"message_type":"summary"' $W/s02a3.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
[ -n "$SNAP" ] && "$RESTIC" -r "$REPO" ls --json "$SNAP" | grep '"struct_type":"node"'

echo "== 2b: --stdin twice / --stdin-filename twice"
printf 'AAAA' | "$RESTIC" -r "$REPO" backup --stdin --stdin --stdin-filename one.dump --stdin-filename two.dump --host h --json > $W/s02b.json 2>$W/s02b.err
echo "[exit=$?]"; summ $W/s02b.json; head -c 400 $W/s02b.err
SNAP=$(grep '"message_type":"summary"' $W/s02b.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
[ -n "$SNAP" ] && "$RESTIC" -r "$REPO" ls --json "$SNAP" | grep '"struct_type":"node"' | sed -E 's/.*"path":"([^"]*)".*/  path=\1/'

echo "== 2b': --stdin plus file args"
printf 'AAAA' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename one.dump "$DIR/regular.txt" --host h --json > $W/s02b2.json 2>$W/s02b2.err
echo "[exit=$?]"; summ $W/s02b2.json; head -c 400 $W/s02b2.err

echo "== 2b'': --stdin-from-command with two commands separated"
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename x.dump --host h --json -- sh -c 'echo one' -- sh -c 'echo two' > $W/s02b3.json 2>$W/s02b3.err
echo "[exit=$?]"; summ $W/s02b3.json; head -c 400 $W/s02b3.err
SNAP=$(grep '"message_type":"summary"' $W/s02b3.json | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/')
[ -n "$SNAP" ] && "$RESTIC" -r "$REPO" dump "$SNAP" /x.dump

echo "== 2b''': --stdin-from-command plus --files-from"
echo "$DIR/regular.txt" > $W/s02-filelist
"$RESTIC" -r "$REPO" backup --stdin-from-command --stdin-filename y.dump --files-from $W/s02-filelist --host h --json -- sh -c 'echo one' > $W/s02b4.json 2>$W/s02b4.err
echo "[exit=$?]"; summ $W/s02b4.json; head -c 400 $W/s02b4.err

echo "== 2c: rewrite cannot add files; check help for any merge/add capability"
"$RESTIC" rewrite --help | grep -iE 'add|merge|include|exclude' | head
"$RESTIC" copy --help | grep -iE 'merge|combine' | head
echo "(no output above = no merge/add option)"
