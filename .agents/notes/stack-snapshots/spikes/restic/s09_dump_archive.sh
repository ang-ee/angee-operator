#!/bin/bash
# Spike 9: dump -a zip / tar for stdin and directory members.
. "$(dirname "$0")/env.sh"
REPO=$W/s09-repo
fresh_repo "$REPO"
sid() { grep '"message_type":"summary"' "$1" | sed -E 's/.*"snapshot_id":"([0-9a-f]+)".*/\1/'; }
head -c 200000 /dev/urandom > $W/s09-db.bin
"$RESTIC" -r "$REPO" backup --stdin --stdin-filename databases/app/app.pgdump --host h --json < $W/s09-db.bin > $W/s09a.json; S1=$(sid $W/s09a.json)
MEDIA=$W/s09-root/files/media; rm -rf $W/s09-root; mkdir -p "$MEDIA/img/sub"
echo hello > "$MEDIA/readme.txt"; head -c 50000 /dev/urandom > "$MEDIA/img/a.jpg"; head -c 70000 /dev/urandom > "$MEDIA/img/sub/b.jpg"; ln -s readme.txt "$MEDIA/link.txt"
( cd $W/s09-root && "$RESTIC" -r "$REPO" backup files/media --host h --json > $W/s09b.json ); S2=$(sid $W/s09b.json)
echo "stdin snapshot=$S1 dir snapshot=$S2"

echo "== 9a: dump -a zip <stdin snap> /"
"$RESTIC" -r "$REPO" dump -a zip "$S1" / > $W/s09-1.zip; echo "[exit=$?]"; unzip -l $W/s09-1.zip
echo "== 9b: dump -a zip <dir snap> /"
"$RESTIC" -r "$REPO" dump -a zip "$S2" / > $W/s09-2.zip; echo "[exit=$?]"; unzip -l $W/s09-2.zip
unzip -Z -v $W/s09-2.zip | grep -E 'file system or operating system of origin|Unix file attributes|^Central directory entry' | head -12
echo "== 9b': dump -a zip <dir snap>:files/media / (re-rooted)"
"$RESTIC" -r "$REPO" dump -a zip "$S2:files/media" / > $W/s09-3.zip; echo "[exit=$?]"; unzip -l $W/s09-3.zip
echo "== 9c: dump -a tar <dir snap> / (symlink preserved?)"
"$RESTIC" -r "$REPO" dump -a tar "$S2" / | tar -tvf -; echo "[exit=${PIPESTATUS[0]}]"
echo "== 9d: dump of a single FILE with -a zip: raw bytes or zip?"
"$RESTIC" -r "$REPO" dump -a zip "$S1" /databases/app/app.pgdump > $W/s09-single.out; echo "[exit=$?] size=$(wc -c < $W/s09-single.out) same-as-source=$(cmp -s $W/s09-single.out $W/s09-db.bin && echo yes || echo no) file-type=$(file -b $W/s09-single.out | cut -c1-40)"
echo "== 9e: dump with TWO snapshot ids in one call"
"$RESTIC" -r "$REPO" dump -a tar "$S1" "$S2" / > /dev/null 2> $W/s09e.err; echo "[exit=$?] $(cat $W/s09e.err | head -3)"
echo "== 9e': dump latest --tag filter"
"$RESTIC" -r "$REPO" dump --host h latest /databases/app/app.pgdump > /dev/null 2>$W/s09e2.err; echo "[exit=$?] (latest = newest snapshot of host; here the dir snapshot, which lacks the path) $(head -2 $W/s09e2.err)"
echo "== 9f: dump --target writes file"
"$RESTIC" -r "$REPO" dump --target $W/s09-target.pgdump "$S1" /databases/app/app.pgdump; echo "[exit=$?] same=$(cmp -s $W/s09-target.pgdump $W/s09-db.bin && echo yes || echo no)"
echo "== 9g: dump | consumer that fails early (EPIPE) -> restic exit?"
"$RESTIC" -r "$REPO" dump "$S1" /databases/app/app.pgdump | head -c 10 > /dev/null; echo "[restic exit=${PIPESTATUS[0]}]"
head -c 30000000 /dev/urandom | "$RESTIC" -r "$REPO" backup --stdin --stdin-filename big.bin --host h -q --json > $W/s09g.json; S3=$(sid $W/s09g.json)
"$RESTIC" -r "$REPO" dump "$S3" /big.bin 2>$W/s09g.err | head -c 10 > /dev/null; echo "[restic exit (30 MB file, consumer reads 10 B)=${PIPESTATUS[0]}] $(head -c 300 $W/s09g.err)"
