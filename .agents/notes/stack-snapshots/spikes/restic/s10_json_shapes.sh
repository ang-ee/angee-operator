#!/bin/bash
# Spike 10: JSON message shapes Angee will decode.
. "$(dirname "$0")/env.sh"
REPO=$W/s10-repo
fresh_repo "$REPO"
keys() { python3 -I -c 'import json,sys
seen={}
for l in sys.stdin:
  l=l.strip()
  if not l.startswith("{") and not l.startswith("["): print("NON-JSON:", l[:150]); continue
  d=json.loads(l)
  items = d if isinstance(d,list) else [d]
  for x in items:
    t=x.get("message_type","<array element>")
    if t in seen: continue
    seen[t]=1
    print(t, "=>", json.dumps(x)[:900])'; }

echo "== 10a: init --json"
rm -rf "$REPO"; "$RESTIC" init -r "$REPO" --json | keys
echo "== 10b: backup --stdin --json (status + summary)"
head -c 40000000 /dev/urandom | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename databases/app/app.pgdump --host stackA --tag angee.snapshot=s1,angee.kind=member,angee.service=app --json | keys
echo "== 10b': backup dir --json -v (verbose_status per file)"
mkdir -p $W/s10-src/sub; echo a > $W/s10-src/a; echo b > $W/s10-src/sub/b
"$RESTIC" -r "$REPO" backup $W/s10-src --host stackA --json -v | keys
echo "== 10b'': same dir again (unchanged, parent used)"
"$RESTIC" -r "$REPO" backup $W/s10-src --host stackA --json | keys
echo "== 10b''': --skip-if-unchanged on unchanged dir"
"$RESTIC" -r "$REPO" backup $W/s10-src --host stackA --skip-if-unchanged --json | keys
echo "== 10b4: --dry-run backup summary"
printf 'abc' | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename x --host stackA --dry-run --json | keys
echo "== 10c: snapshots --json"
"$RESTIC" -r "$REPO" snapshots --json --host stackA --tag angee.kind=member | keys
echo "== 10c': snapshots --json --group-by host"
"$RESTIC" -r "$REPO" snapshots --json --group-by host | python3 -I -c 'import json,sys; d=json.load(sys.stdin); print(type(d).__name__, "of", list(d[0].keys()), "group_key=", d[0]["group_key"])'
echo "== 10c'': snapshots --json --latest 1"
"$RESTIC" -r "$REPO" snapshots --json --latest 1 | python3 -I -c 'import json,sys; d=json.load(sys.stdin); print(len(d), "items")'
echo "== 10c''': snapshots --json on empty result"
"$RESTIC" -r "$REPO" snapshots --json --tag nope; echo
echo "== 10d: ls --json first lines"
"$RESTIC" -r "$REPO" ls --json latest | keys
echo "== 10e: restore --json"
rm -rf $W/s10-restore; "$RESTIC" -r "$REPO" restore latest --target $W/s10-restore --json | keys
echo "== 10f: forget --json (non dry-run) and tag --json"
ID=$("$RESTIC" -r "$REPO" snapshots --json --latest 1 --host stackA | python3 -I -c 'import json,sys; print(json.load(sys.stdin)[-1]["id"])')
"$RESTIC" -r "$REPO" tag --add extra=1 "$ID" --json | keys
"$RESTIC" -r "$REPO" forget "$ID" --json | keys; echo "(forget with ids prints: above)"
echo "== 10g: stats --json, check --json"
"$RESTIC" -r "$REPO" stats --json | keys
"$RESTIC" -r "$REPO" stats --json --mode raw-data | keys
"$RESTIC" -r "$REPO" check --json | keys
echo "== 10h: find --json"
"$RESTIC" -r "$REPO" find --json app.pgdump | keys
echo "== 10i: version --json"
"$RESTIC" version --json | keys
echo "== 10j: cat snapshot (raw snapshot object, includes parent/original fields)"
"$RESTIC" -r "$REPO" cat snapshot "$("$RESTIC" -r "$REPO" snapshots --json | python3 -I -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')" | tr -d '\n' | cut -c1-900; echo
