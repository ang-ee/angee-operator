#!/bin/bash
# Spike 6: retention over tagged sets (3 members + 1 manifest per stack snapshot).
. "$(dirname "$0")/env.sh"
REPO=$W/s06-repo
fresh_repo "$REPO"
H=stackA
py() { python3 -I -c "$1" "${@:2}"; }

echo "== create 10 stack snapshots, one per day (via --time), each 3 members + manifest written LAST"
for i in $(seq -w 1 10); do
  T="2026-10-$i 03:00:00"
  for m in "app databases/app/app.pgdump" "redis databases/redis/dump.rdb" "media files/media/media.tar"; do
    set -- $m
    head -c 2048 /dev/urandom | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename "$2" --host $H --time "$T" \
      --tag "angee.snapshot=s$i,angee.kind=member,angee.service=$1" -q >/dev/null || echo "member backup failed"
  done
  # Set s05 is "partial": its redis member is missing (simulates a set where one member was skipped)
  printf '{"id":"s%s"}' "$i" | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename angee/manifest.json --host $H --time "$T" \
    --tag "angee.snapshot=s$i,angee.kind=manifest" -q >/dev/null
done
# remove s05's redis member to make set sizes unequal
X=$("$RESTIC" -r "$REPO" snapshots --json --tag angee.snapshot=s05,angee.service=redis | py 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
"$RESTIC" -r "$REPO" forget "$X" -q
echo "total snapshots: $("$RESTIC" -r "$REPO" snapshots --json | py 'import json,sys; print(len(json.load(sys.stdin)))')"

echo
echo "== 6a: forget --dry-run --json over MANIFESTS, keep-last 3"
"$RESTIC" -r "$REPO" forget --dry-run --json --host $H --tag angee.kind=manifest --keep-last 3 > $W/s06a.json; echo "[exit=$?]"
py 'import json,sys
d=json.load(open(sys.argv[1]))
print("top-level type:", type(d).__name__, "groups:", len(d))
g=d[0]
print("group keys:", list(g.keys()))
print("group tags/host/paths:", g["tags"], g["host"], g["paths"])
print("keep:", [ (s["short_id"], [t for t in s["tags"] if t.startswith("angee.snapshot")]) for s in g["keep"]])
print("remove:", [ (s["short_id"], [t for t in s["tags"] if t.startswith("angee.snapshot")]) for s in (g["remove"] or [])])
print("reasons[0]:", json.dumps(g["reasons"][0])[:400])
print("keep[0] keys:", list(g["keep"][0].keys()))
' $W/s06a.json 2>&1 || cat $W/s06a.json | head -c 1500
echo "-- raw JSON excerpt:"; head -c 900 $W/s06a.json; echo

echo
echo "== 6b: keep-daily 3 + keep-weekly 2 over manifests (calendar policy)"
"$RESTIC" -r "$REPO" forget --dry-run --json --host $H --tag angee.kind=manifest --keep-daily 3 --keep-weekly 2 | py 'import json,sys
for g in json.load(sys.stdin):
  print("keep:", [ [t for t in s["tags"] if t.startswith("angee.snapshot")][0] for s in g["keep"]])
  print("reasons:", [ (r["snapshot"]["tags"][0], r["matches"]) for r in g["reasons"]])'

echo
echo "== 6c: grouping behaviour over MEMBERS (paths differ per member)"
echo "-- default group-by host,paths, members only, keep-last 3:"
"$RESTIC" -r "$REPO" forget --dry-run --json --host $H --tag angee.kind=member --keep-last 3 | py 'import json,sys
for g in json.load(sys.stdin):
  print("  group paths=%s keep=%s remove=%d" % (g["paths"], [ [t for t in s["tags"] if t.startswith("angee.snapshot")][0] for s in g["keep"]], len(g["remove"] or [])))'
echo "-- --group-by host, members only, keep-last 3 (all members in ONE group => keeps only 3 member snapshots total):"
"$RESTIC" -r "$REPO" forget --dry-run --json --host $H --tag angee.kind=member --group-by host --keep-last 3 | py 'import json,sys
for g in json.load(sys.stdin):
  print("  group host=%s paths=%s keep=%s remove=%d" % (g["host"], g["paths"], [ ([t for t in s["tags"] if t.startswith("angee.snapshot")][0], s["paths"][0]) for s in g["keep"]], len(g["remove"] or [])))'
echo "-- --group-by tags over manifests (each set has a unique angee.snapshot tag => every group keeps its one snapshot):"
"$RESTIC" -r "$REPO" forget --dry-run --json --host $H --tag angee.kind=manifest --group-by tags --keep-last 3 | py 'import json,sys
d=json.load(sys.stdin); print("  groups:", len(d), "total kept:", sum(len(g["keep"]) for g in d), "total removed:", sum(len(g["remove"] or []) for g in d))'
echo "-- --group-by host over manifests (same as default here because manifests share one path):"
"$RESTIC" -r "$REPO" forget --dry-run --json --host $H --tag angee.kind=manifest --group-by host --keep-last 3 | py 'import json,sys
d=json.load(sys.stdin); print("  groups:", len(d), "keep:", [ [t for t in s["tags"] if t.startswith("angee.snapshot")][0] for g in d for s in g["keep"]])'

echo
echo "== 6d: tag filter semantics"
cnt() { "$RESTIC" -r "$REPO" snapshots --json "$@" | py 'import json,sys; d=json.load(sys.stdin); print(len(d), sorted(set(t for s in d for t in s["tags"] if t.startswith("angee.snapshot"))))'; }
echo "--tag angee.snapshot=s01,angee.kind=manifest  (comma)  => $(cnt --tag angee.snapshot=s01,angee.kind=manifest)"
echo "--tag angee.snapshot=s01 --tag angee.kind=manifest (repeat) => $(cnt --tag angee.snapshot=s01 --tag angee.kind=manifest)"
echo "--tag angee.snapshot=s01 --tag angee.snapshot=s02 (repeat) => $(cnt --tag angee.snapshot=s01 --tag angee.snapshot=s02)"
echo "--tag angee.snapshot=s01,angee.snapshot=s02 (comma) => $(cnt --tag angee.snapshot=s01,angee.snapshot=s02)"
echo "--tag angee.snapshot=s0 (prefix, no wildcard)  => $(cnt --tag angee.snapshot=s0)"
echo "--tag 'angee.snapshot=s0*' (glob?)  => $(cnt --tag 'angee.snapshot=s0*')"
echo "--tag '' (untagged only) => $(cnt --tag '')"

echo
echo "== 6e: forget a dropped set by tag"
DROP=$(py 'import json,sys
d=json.load(open(sys.argv[1]))
print(" ".join([t.split("=")[1] for s in (d[0]["remove"] or []) for t in s["tags"] if t.startswith("angee.snapshot=")]))' $W/s06a.json)
echo "dropped sets: $DROP"
echo "-- forget --tag angee.snapshot=s01 with NO policy and NO ids:"
"$RESTIC" -r "$REPO" forget --tag angee.snapshot=s01 --json; echo "[exit=$?]"
echo "   remaining with tag s01: $(cnt --tag angee.snapshot=s01)"
echo "-- forget --tag angee.snapshot=s01 --keep-last 0:"
"$RESTIC" -r "$REPO" forget --tag angee.snapshot=s01 --keep-last 0 --json 2>&1 | head -c 400; echo "[exit=${PIPESTATUS[0]}]"
echo "-- forget --tag angee.snapshot=s01 --group-by '' --keep-tag nonexistent (policy that matches nothing):"
"$RESTIC" -r "$REPO" forget --dry-run --tag angee.snapshot=s01 --group-by '' --keep-tag nonexistent.tag --json 2>&1 | head -c 400; echo "[exit=${PIPESTATUS[0]}]"
echo "-- forget --tag angee.snapshot=s01 --unsafe-allow-remove-all --group-by '' --keep-last 0 (dry-run):"
"$RESTIC" -r "$REPO" forget --dry-run --tag angee.snapshot=s01 --unsafe-allow-remove-all --group-by '' --keep-last 0 --json 2>&1 | py 'import json,sys
raw=sys.stdin.read()
try:
  d=json.loads(raw); print("  groups=%d remove=%s keep=%d" % (len(d), [s["short_id"] for g in d for s in (g["remove"] or [])], sum(len(g["keep"] or []) for g in d)))
except Exception: print(raw[:400])'
echo "-- recommended: resolve ids by tag, then forget explicit ids (manifest FIRST, then members)"
IDS_MAN=""; IDS_MEM=""
for s in $DROP; do
  IDS_MAN="$IDS_MAN $("$RESTIC" -r "$REPO" snapshots --json --tag angee.snapshot=$s,angee.kind=manifest | py 'import json,sys; print(" ".join(x["id"] for x in json.load(sys.stdin)))')"
  IDS_MEM="$IDS_MEM $("$RESTIC" -r "$REPO" snapshots --json --tag angee.snapshot=$s,angee.kind=member | py 'import json,sys; print(" ".join(x["id"] for x in json.load(sys.stdin)))')"
done
echo "manifest ids: $(echo $IDS_MAN | wc -w)   member ids: $(echo $IDS_MEM | wc -w)"
"$RESTIC" -r "$REPO" forget --json $IDS_MAN; echo; echo "[exit=$?]"
"$RESTIC" -r "$REPO" forget --json $IDS_MEM; echo; echo "[exit=$?]"
echo "-- forget output with explicit ids (non-dry-run, non-json):"
echo "remaining sets: $("$RESTIC" -r "$REPO" snapshots --json | py 'import json,sys
d=json.load(sys.stdin); import collections
c=collections.Counter([t for s in d for t in s["tags"] if t.startswith("angee.snapshot")]); print(len(d), "snapshots;", dict(sorted(c.items())))')"
echo "-- forget a nonexistent id:"
"$RESTIC" -r "$REPO" forget --json deadbeef; echo "[exit=$?]"
echo "-- forget mix of valid + already-forgotten id:"
ONE=$("$RESTIC" -r "$REPO" snapshots --json --tag angee.snapshot=s08,angee.kind=member | py 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
"$RESTIC" -r "$REPO" forget --json $ONE $(echo $IDS_MAN | cut -d" " -f1); echo "[exit=$?]"
