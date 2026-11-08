# Database save/restore spikes: results

Run on 2026-11-06. macOS arm64, Docker 29.8.2. Images: `pgvector/pgvector:pg17` (PostgreSQL 17.10, pgvector 0.8.5),
`postgres:16` (16.14), `postgres:17` (17.10), `redis:7-alpine` (Redis 7.4.9), `alpine:3`. Host `/usr/bin/sqlite3` 3.51.0
and Python 3.14 `sqlite3` module (SQLite 3.53.4). restic 0.19.1.

Everything here was run for real. Each spike has a script in this directory and its raw output in `out-s*.txt`.
Scripts were written to files and run with bash or `python3 -I`, with no heredocs. All `angee-spike-db-*` containers,
their anonymous and named volumes, and the network were removed at the end. The large dumps (about 1.8 GB) and the
restic repository (541 MB) were deleted after the runs and can be rebuilt by re-running `s2.sh` and `s5*.sh`. The
small dumps are still in `dumps/`.

Shared helpers are in `lib.sh` (`wait_pg`, `rowcount`, `banner`).

## Verdicts on the research report's claims

| # | Claim | Verdict |
|---|---|---|
| 1 | pg_dump ignores `--no-owner` and `--no-acl` for archive formats | **Half right.** pg_dump ignores `--no-owner`: the archive is byte-identical. pg_dump **honours** `--no-acl`: the ACL entries are dropped from the archive and can never be restored. |
| 2 | `pg_restore --list` reads only the TOC and does not validate data | **Confirmed.** Exit 0 on archives with flipped bytes and on truncated archives. |
| 3 | pg_restore continues past errors by default | **Confirmed, with a nuance.** It continues, but still exits **1** (`warning: errors ignored on restore: N`), and it leaves a half-restored database. |
| 4 | Restoring from stdin into the image-created DB with `--no-owner --no-acl -1 -e` works, including `CREATE EXTENSION vector` | **Confirmed.** It works when the restoring user is superuser, which `POSTGRES_USER` is. As a non-superuser it fails. |
| 5 | Copying a live WAL SQLite file by file loses or corrupts data; `.backup` and `VACUUM INTO` are safe | **Confirmed, and corruption was reproduced.** `.backup` is consistent but can block for as long as the writer keeps writing. |
| 6 | A `dump.rdb` in the data dir is ignored when AOF is on | **Confirmed, and it is worse than claimed.** Redis creates an empty AOF and then overwrites the restored `dump.rdb` on the first clean stop. |

---

## Spike 1: pg_dump ownership/ACL flags in custom format (`s1.sh`, `s1-setup.sql`, `out-s1.txt`)

**Setup.** A pgvector pg17 container `angee-spike-db-src`. Roles `appowner` (LOGIN) and `reader`. Database `app`
owned by `appowner`. `CREATE EXTENSION vector`. Table `items(id serial, name text, embedding vector(3))` with 4 rows
and an HNSW index, created as `appowner`. `GRANT SELECT ON items TO reader`. Resulting ACL:
`{appowner=arwdDxtm/appowner,reader=r/appowner}`.

**Commands.** `pg_dump -U postgres --dbname app --format=custom [flags] --file /tmp/X.dump` was run for
(a) no flags, (b) `--no-owner`, (c) `--no-acl` and (d) both. Each archive was then checked with
`pg_restore -f - X.dump | grep -E 'OWNER TO|GRANT|REVOKE'` and `pg_restore -l X.dump`.

**Key output.**

```
size  sha256(prefix)  dump
3903  cb2607ca701f…   a-none
3903  cb2607ca701f…   b-noowner     <- byte-identical to a-none
3720  f442217313f4…   c-noacl
3720  f442217313f4…   d-both        <- byte-identical to c-noacl
```

| archive | rendered `OWNER TO` | rendered `GRANT` | `pg_restore -l` ACL entry | TOC owner column |
|---|---|---|---|---|
| a none | `ALTER TABLE public.items OWNER TO appowner;` and `ALTER SEQUENCE … OWNER TO appowner;` | `GRANT SELECT ON TABLE public.items TO reader;` | `3606; 0 0 ACL public TABLE items appowner` | appowner |
| b `--no-owner` | same as a | same as a | present | appowner |
| c `--no-acl` | same as a | **none** | **(no ACL entries)** | appowner |
| d both | same as a | **none** | **none** | appowner |

Applying the flags on the restore side to archive (a):

- `pg_restore --no-owner -f -` leaves only the GRANT.
- `pg_restore --no-acl -f -` leaves only the two OWNER TO lines.
- `pg_restore --no-owner --no-acl -f -` leaves none of them.

Control check: plain text `pg_dump -Fp --no-owner` does honour the flag, and only the GRANT remains.

**Verdict.** pg_dump ignores `--no-owner` for `-Fc`: the archive is identical byte for byte, and ownership is always
recorded in the TOC. Only `pg_restore --no-owner` can drop it. pg_dump does apply `--no-acl` to archives: the ACL
TOC entries are left out at dump time, so no restore can bring the grants back. `pg_restore --no-acl` can also drop
them at restore time.

**Implication for Angee.** Drop `--no-owner --no-acl` from the save command. The first does nothing, and the second
throws away grants permanently. Keep a faithful archive and pass `--no-owner --no-acl` to `pg_restore` in the restore
procedure.

---

## Spike 2: what `pg_restore --list` validates (`s2.sh`, `s2b.sh`, `s2-big.sql`, `flip.py`, `flip_hex.py`, `out-s2*.txt`)

**Setup.** Database `bigdb` with table `big`: 2,000,000 rows of id, 2×md5 text, timestamptz and `vector(3)`.
`COPY big TO STDOUT` is **218,037,244 bytes**, and the heap is 257 MB. Table `small` has 1,000 rows. Dumps:

- `pg_dump -Fc`, gzip by default: **104,042,452 B**, 11.3 s
- `-Fc -Z0`: 228 MB, 1.9 s
- `-Fc` to stdout through a pipe: 104 MB, 9.6 s

**Corruptions.**

- `flip.py`: XOR of 16 bytes, or of 1 byte, at 50% of the file. That position is inside the `big` data block.
- `head -c` truncation to 60%, applied to both the file dump and the piped dump.
- On the `-Z0` dump: a 16-byte flip, plus `flip_hex.py`, which changes one md5 hex digit and keeps the row valid.

**Key output (exit codes).**

| archive | `pg_restore --list` | `pg_restore -f /dev/null` | real restore, default | real restore, `-1 -e` |
|---|---|---|---|---|
| gzip, 16-byte flip | **0** | 1 `could not uncompress data: incorrect data check` | 1, empty tables left behind | 1, nothing left |
| gzip, 1-byte flip | **0** | 1 (same error) | 1 | 1 |
| gzip, truncated 60% | **0** | 1 `could not read from input file: end of file` | 1, empty tables left | 1, nothing |
| piped dump, truncated 60% | **0** | 1 (same) | 1 | 1 |
| `-Z0`, 16-byte flip | **0** | **0** | 1 `COPY failed for table "big": invalid byte sequence for encoding "UTF8": 0x9d` (with `small` restored) | 1 |
| `-Z0`, 1 hex digit changed | **0** | **0** | **0** | **0**: silent corruption, row 1004915 `45c6…` became `05c6…` |
| `-Z zstd`, 1-byte flip | **0** | **0** | (not run) | (not run) |
| `-Z lz4`, 1-byte flip | **0** | **0** | (not run) | (not run) |
| `-Z gzip:9`, 1-byte flip | **0** | 1 `incorrect data check` | (not run) | (not run) |

For zstd and lz4, `pg_restore -f -` of the flipped archive was compared with the intact one. zstd mangled a row into
`100169d1f7544f57a,283f…`. lz4 dropped a row and injected binary bytes. Both exited 0.

**Cost on the 104 MB gzip archive of 218 MB table data** (inside the container):

| check | time |
|---|---|
| `pg_restore --list` | 0.09 s |
| `sha256sum` of the archive | 0.44 s |
| **`pg_restore -f /dev/null`** (reads and inflates every block) | **1.33 s** (piped dump: 1.28 s) |
| `pg_restore -f /dev/null` on the `-Z0` archive | 0.21 s |
| real restore into an empty DB, `--no-owner --no-acl -1 -e` | 3.68 s |
| real restore, `-j 4`, from a file | 2.00 s |

**Verdict.** Confirmed: `--list` reads only the header and TOC, which sit at the start of the file, and passes every
corruption tested. `pg_restore -f /dev/null` does read every data block. It reliably catches **truncation** and, with
**gzip** compression only, bit flips, because deflate carries a checksum. The archive format has no checksums of its
own, so with `-Z0`, zstd or lz4 the check passes corrupted data. Only a real restore, or a comparison of content,
catches a value-preserving change.

**Implication for Angee.** Replace `pg_restore --list` with `pg_restore -f /dev/null`. It costs about 1.3 s per 100 MB
of gzip archive. Treat that check as a test for truncation and framing, and rely on the storage layer for integrity
(restic content hashes and `restic check --read-data`). A real verification is a scratch restore.

---

## Spike 3: restore error handling (`s3.sh`, `out-s3.txt`)

**Setup.** A fresh pgvector pg17 container `angee-spike-db-dst3` whose only role is `postgres`, so `appowner` and
`reader` do not exist. Archive (a) from spike 1 (no flags, so it has OWNER and ACL entries). Each case runs
`pg_restore -U postgres -d <fresh db> [flags] /tmp/app.dump`.

**Key output.**

| flags | exit | errors printed | resulting state |
|---|---|---|---|
| (none) | **1** | `role "appowner" does not exist` ×2, `role "reader" does not exist`, then `pg_restore: warning: errors ignored on restore: 3` | extension and table **owned by postgres**, **4 rows**, no grant (half-right DB) |
| `--exit-on-error` | 1 | first `ALTER TABLE … OWNER TO appowner` only | **partial**: extension and **empty** `items` table left behind |
| `--single-transaction` | 1 | first error only (`-1` implies `-e`) | **rolled back**: no tables, no vector extension |
| `--no-owner` | 1 | `role "reader" does not exist`, `errors ignored on restore: 1` | 4 rows, no grant |
| `--no-owner --no-acl --single-transaction --exit-on-error` | **0** | none | 4 rows, vectors identical (`[1,2,3]`, `[0.5,-1.25,3.75]`, `[0.001,250,-7]`, `[0,0,0]`) |

**Extra: restoring as a non-superuser database owner `appuser`, with the clean flags:**

- No extension present: `ERROR: permission denied to create extension "vector"` with
  `HINT: Must be superuser to create this extension.`, exit 1. pgvector is not a trusted extension.
- Extension pre-created by a superuser: `ERROR: must be owner of extension vector`, from
  `COMMENT ON EXTENSION vector IS '…'`, exit 1. In default mode the rows do load, but the exit code is still 1.
- Extension pre-created **and** `--no-comments` (available in pg_restore 17): exit **0**, 4 rows, owner `appuser`.

**Verdict.** Confirmed. By default pg_restore continues past errors and exits 1, leaving a database that is only
partly correct. `--exit-on-error` alone leaves partial state behind. `--single-transaction` is the only mode that is
all-or-nothing.

**Implication for Angee.** Restore with `--no-owner --no-acl --single-transaction --exit-on-error`, as a superuser
(the image's `POSTGRES_USER`). For a non-superuser restore, a superuser must create the extension first and the
restore needs `--no-comments`.

---

## Spike 4: stdin restore into the image-created empty DB (`s4.sh`, `out-s4.txt`)

**Setup.** A fresh `pgvector/pgvector:pg17` container `angee-spike-db-dst4` with `POSTGRES_DB=app`,
`POSTGRES_USER=angee` and `POSTGRES_PASSWORD=angeepw`. The source used `postgres`/`appowner`. Before the restore:
`angee rolsuper=true`, `app` contains only `plpgsql` and 0 relations in `public`. The archive is
`docker exec src pg_dump -U postgres -d app -Fc > app-pipe.dump`, a piped dump with no pg_dump flags.

**Command.**

```
cat app-pipe.dump | docker exec -i angee-spike-db-dst4 pg_restore --no-owner --no-acl --single-transaction --exit-on-error -U angee -d app
```

**Key output.**

- exit=0.
- `COPY (select id,name,embedding::text from items order by id)` gives sha256 `75cbef09…` on both sides: **DATA EQUAL**.
- rows=4. `vector 0.8.5`, owner `angee`. The table is owned by `angee`. Indexes `items_pkey` and
  `items_embedding_hnsw` are present. Sequence `last_value=4`, matching the source. The KNN query `<->` works.
- Restoring again into the now non-empty DB: `relation "items" already exists`, exit 1. Rolled back, still 4 rows.
- `-j 4` from stdin: `pg_restore: error: parallel restore from standard input is not supported`, exit 1.
- `-j 4 --single-transaction`: `cannot specify both --single-transaction and multiple jobs`, exit 1.
- `-j 4` from a **file** works for both the seekable dump (2.00 s) and the piped, offset-less dump saved to a file
  (1.87 s, 2,000,000 rows).
- `--clean` without `--if-exists` into the empty DB: `index "items_embedding_hnsw" does not exist`, exit 1.
- `--clean --if-exists`: exit 0. This is a harmless no-op.
- `--create --single-transaction`: `options -C/--create and -1/--single-transaction cannot be used together`.
- `--create`, connecting to `postgres`: `database "app" already exists`, exit 1. It would also emit
  `ALTER DATABASE app OWNER TO appowner`.

**Verdict.** Confirmed. A plain stdin restore into the entrypoint-created DB works, `CREATE EXTENSION vector`
included, because `POSTGRES_USER` is superuser. `--clean` and `--create` are unnecessary and only add failure modes.
`-j` needs a regular file.

**Implication for Angee.** The restore procedure is "fresh volume, image creates DB, pipe archive into `pg_restore
--no-owner --no-acl -1 -e -d $POSTGRES_DB`". Restore must target empty state, because a second run fails and rolls
back. Parallel restore would mean spooling to a file first, and gives up `-1`.

---

## Spike 5: end to end through restic (`s5.sh`, `s5b.sh`, `s5c.sh`, `s5d.sh`, `kill_pgdump.sh`, `out-s5*.txt`)

**Commands.** Local repo, `RESTIC_PASSWORD_FILE`.

```
docker exec src pg_dump -U postgres -d $db -Fc | restic backup --stdin --stdin-filename databases/$db/$db.pgdump --host stackA --tag angee.snapshot=s1
restic dump --host stackA --tag angee.snapshot=s1 --path /databases/$db/$db.pgdump latest /databases/$db/$db.pgdump \
  | docker exec -i angee-spike-db-dst5 pg_restore --no-owner --no-acl --single-transaction --exit-on-error -U angee -d $db
```

**Key output.**

- Snapshot path: `/databases/bigdb/bigdb.pgdump`. The tree is `/databases`, `/databases/bigdb`, then the file.
- Backup: app 3.8 KiB in 0.78 s. **bigdb 99.2 MiB in 4.25 s** (`Added 99.228 MiB (97.794 MiB stored)`, so gzip leaves
  restic almost nothing to compress, ratio 1.01×).
- Restore through `restic dump | pg_restore`: app 0.77 s, **bigdb 2.58 s**. Pipeline exit 0.
- Equality: the sha256 of ordered `COPY` output matches for app `items`, bigdb `big` (2,000,000 rows) and `small`.
- `restic check --read-data`: `no errors were found`, 1.3 s.

**Dedup and size, gzip archive vs `-Z0` archive with restic compression** (`s5b.sh`):

| backup of bigdb | gzip `-Fc` added | `-Z0` added (stored) |
|---|---|---|
| baseline | 99.2 MiB (97.9 stored), 4.2 s | 217.5 MiB → **92.5 MiB stored**, **1.5 s** |
| unchanged re-run | 2.3 MiB | ~0.8 MiB |
| 1,000 contiguous rows updated | **51.7 MiB** | **3.35 MiB (1.4 stored)** |
| 1,000 rows appended | 2.08 MiB | 1.06 MiB (0.4 stored) |
| 1,000 rows updated spread through the table (1 every 2,000) | 99.2 MiB | 217.5 MiB (92.5 stored): no dedup either way |

**Failure semantics when the producer dies** (an important extra):

| case | restic exit | snapshot? | what the snapshot holds |
|---|---|---|---|
| pipe, `pg_dump -d nosuchdb` (no output) | 3 (`no data read`, `Warning: at least one source file could not be read`) | **yes, 0 B** (`dc7736fe`) | nothing |
| pipe, producer prints 12 B then exits 3 | **0** | **yes, 12 B** | garbage. Restore: `unsupported version (112.97) in file header` |
| pipe, real `pg_dump -Fc -Z0` SIGTERMed after 0.7 s | **0**. The `pg_dump` side exited 1 (`pg_dump: terminated by user`) | **yes, 6.85 MiB** | a truncated archive. `pg_restore --list` **exit 0**. `-f /dev/null` exit 1 (`end of file`). Restore `-1 -e` exit 1 with nothing left; default restore exit 1 with empty tables left |
| `--stdin-from-command`, producer exits 3 | **1** (`Fatal: unable to save snapshot: … command failed: exit status 3`) | **no** | n/a |
| `--stdin-from-command -- docker exec src pg_dump …`, SIGTERMed after 1.5 s | **1** (`subprocess docker: pg_dump: terminated by user`, `Fatal: unable to save snapshot`) | **no** | n/a |
| `--stdin-from-command -- docker exec src pg_dump …`, success | 0 | yes | 0.87 s for app |

**Verdict.** The end-to-end pipeline works, and the data comes back identical. Restoring 218 MB of table data from
restic takes about 2.6 s.

**Implication for Angee.**

- Use `restic backup --stdin-from-command -- <save command>`, not a shell pipe. With a pipe, restic saves a partial or
  empty snapshot and exits 0 or 3, and the partial archive passes `pg_restore --list`.
- Prefer `pg_dump -Fc -Z0` and let restic compress. It is about 3× faster to dump, smaller once stored (92.5 vs
  97.8 MiB) and it dedups (3.4 vs 51.7 MiB per small change). The trade-off is the gzip-only check from spike 2.
  restic's own content hashing covers that ground.

---

## Spike 6: version direction (`s6.sh`, `s6-setup.sql`, `out-s6.txt`)

**Setup.** Plain `postgres:16` (16.14) and `postgres:17` (17.10) containers on network `angee-spike-db-net`. Table
`t` with an identity column, text, jsonb and timestamptz (1,000 rows), plus a view and a SQL function. Archive
versions: a pg_dump 17 archive is `Dump Version: 1.16-0`, a pg_dump 16 archive is `1.15-0`. The 17 plain dump
contains `SET transaction_timeout = 0;` and `\restrict` / `\unrestrict`.

**Key output.**

| case | exit | error | rows |
|---|---|---|---|
| A1 17 archive → **pg_restore 16** → 16 server | 1 | `pg_restore: error: unsupported version (1.16) in file header` | n/a |
| A2 17 archive → pg_restore 17 → **16 server**, default | 1 | `unrecognized configuration parameter "transaction_timeout"` / `Command was: SET transaction_timeout = 0;` / `errors ignored on restore: 1` | 1000 (all data) |
| A2b same with `-1 -e` | 1 | same, on the first statement | none (MISSING) |
| A3 17 plain SQL → psql 16, default | **0** | `psql:/tmp/v17.sql:13: ERROR: unrecognized configuration parameter "transaction_timeout"` | 1000 |
| A3b same with `ON_ERROR_STOP=1 --single-transaction` | 3 | same | none |
| A4 `pg_restore 17 -f -` with `^SET transaction_timeout` filtered out → `psql 16 -1 ON_ERROR_STOP=1` | 0 | none | 1000 |
| B1 16 archive → pg_restore 17 → 17 server, `-1 -e` | 0 | none | 1000, `t_count()`=1000 |
| B2 16 archive → pg_restore 16 → 17 server, `-1 -e` | 0 | none | 1000 |
| C1 **pg_dump 16 against a 17 server** | 1 | `pg_dump: error: aborting because of server version mismatch` / `server version: 17.10…; pg_dump version: 16.14…` | n/a |
| C2 pg_dump 17 against a 16 server | 0 | archive is 1.16, `Dumped from 16.14 / by 17.10` | n/a |
| C3 that C2 archive → pg_restore 16 | 1 | `unsupported version (1.16) in file header` | n/a |

**Verdict.** Compatibility runs forward only. Going 16 to 17 works with either client. Going 17 to 16 fails in two
ways: the older client cannot read the newer archive (format 1.16), and the newer client emits
`SET transaction_timeout`, which the 16 server rejects. In psql's default mode that failure is quietly ignored.

**Implication for Angee.**

- Run pg_dump and pg_restore from the **same image and major version as the server**, for example `docker exec` into
  the service container.
- Record the server major version and the archive version in the snapshot metadata.
- Refuse to restore into an older major version rather than guessing.

---

## Spike 7: SQLite live-copy hazard (`sqlite_writer.py`, `sqlite_spike.py`, `sqlite_cli_backup.py`, `sqlite_restore_hazard.py`, `out-s7*.txt`)

**Setup.** `sqlite_writer.py` sets `PRAGMA journal_mode=WAL` and `synchronous=NORMAL`, with a configurable
`wal_autocheckpoint`. Each transaction does `BEGIN IMMEDIATE`, inserts 50 rows sharing one `txn` number (200-byte
random payload, index on `txn`), runs `UPDATE meta SET v=v+50`, then commits, with a short sleep between
transactions. A copy counts as consistent when all of these hold:

- `PRAGMA integrity_check` returns `ok`.
- `count(*) == meta.rows`.
- Every txn has exactly 50 rows.

While the writer runs, `sqlite_spike.py` loops over these methods:

- (a) `cp app.db`, sleep 0.02–0.4 s, then `cp app.db-wal`.
- (a′) The same with the -wal copied first, then the db.
- (b) `cp app.db` only.
- (c) `/usr/bin/sqlite3 app.db ".backup copy.db"`.
- (d) `/usr/bin/sqlite3 app.db "VACUUM INTO 'copy2.db'"`.
- (e) Python `Connection.backup()` (`pages=-1`).

**Phase A: `wal_autocheckpoint=0`, 20 s, 16 iterations** (`out-s7a.txt`).

| method | result |
|---|---|
| cp db then wal | 16/16 consistent. With no checkpoints the db file never changes. |
| cp wal then db | 16/16 consistent |
| cp db only | **16/16 unusable: `no such table: t`**. The schema itself was still only in the WAL, while the live DB held 11,650 to 155,550 rows. |
| `.backup` / `VACUUM INTO` / py backup | 16/16 consistent. `.backup` took up to 6.9 s. |

**Phase B: default `wal_autocheckpoint=1000`, 60 s, 23 iterations, CLI `.backup` taken out of the loop** (`out-s7b1.txt`).

| method | consistent | integrity_check failed | open/query error | notes |
|---|---|---|---|---|
| **cp db, then -wal later** | 6/23 | 7 | 10 | `Tree 3 page 2348: btreeInitPage() returns error code 11 …` and `database disk image is malformed` |
| **cp -wal, then db later** | 18/23 | 4 | 1 | `Tree 2 page 2 cell 4: Rowid 40185 out of order`, `2nd reference to page 2237 …`. One copy had rows=26706 vs meta=22450 with 86 torn txns. |
| **cp db only** | 22/23 | 0 | 1 | 20 of the 22 "ok" copies were **older than the live DB**, missing up to **4,500 rows** (41,900 in total). One copy was torn by a checkpoint mid-copy. |
| `VACUUM INTO` | **23/23** | 0 | 0 | at most 0.9 s |
| Python `Connection.backup()` | **23/23** | 0 | 0 | |

There was also an earlier 4-iteration run of the same phase with CLI `.backup` still in the loop. Its output file was
overwritten by the rerun above, so these figures come from the session log only: cp db then wal gave 3/4 corrupt,
cp wal then db 2/4 corrupt, and `.backup` 4/4 consistent but took up to **56 s** per call.

**CLI `.backup` under a continuous writer** (`sqlite_cli_backup.py`, `out-s7c.txt`).

| writer pace | `.backup` duration | when it returned | copy |
|---|---|---|---|
| a commit every ~2 ms, 228 MiB DB | **23.2 s** | at t+25.2 s, **the same moment the writer exited** | ok, 1,047,500 = meta |
| a commit every 50 ms | 0.3 s | immediately | ok |
| a commit every 500 ms | 0.2 s | immediately | ok |
| `VACUUM INTO` with the same writers | 0.20–0.38 s | immediately | ok |

The CLI backs up 100 pages per step and restarts whenever another process writes, so under a busy writer it does
not finish until writes stop.

**Restore hazard: a backup dropped in place of `app.db` while the old `app.db-wal` and `-shm` remain.** In each
scenario the writer was SIGKILLed so that it left uncheckpointed WAL behind (`sqlite_restore_hazard.py`,
`out-s7r.txt`).

| scenario | intended (backup.db) | old crashed DB | what opening `app.db` gives |
|---|---|---|---|
| R1 page-identical backup (Python backup), checkpoints after the backup, stale WAL kept | 37,900 rows | 76,600 | **integrity_check fails** (`Tree 3 page 4150: btreeInitPage() returns error code 11 …`), rows=37,900 vs meta=76,600. A previous run gave `database disk image is malformed`. |
| R2 page-identical backup, `wal_autocheckpoint=0`, stale WAL kept | 37,550 | 76,050 | **integrity ok, 76,050 rows**. The restore was **silently undone**: SQLite replayed the old WAL over the backup. |
| R3 `VACUUM INTO` backup (pages laid out differently), stale WAL kept | 36,450 | 75,150 | **`database disk image is malformed`**. A previous run gave a missing `meta` row. |
| R4 `VACUUM INTO` backup, `-wal`/`-shm` deleted first | 38,100 | 76,900 | ok, 38,100 rows (correct) |
| R5 page-identical backup, `-wal`/`-shm` deleted first | 37,150 | 76,200 | ok, 37,150 rows (correct) |

**Verdict.** Confirmed, with corruption reproduced. When a checkpoint falls between the two `cp` calls, a
file-by-file copy of a live WAL database is corrupt in most iterations, in either order. With no checkpoint in that
window the pair happens to be consistent. Copying only the db file loses everything that has not been checkpointed,
up to and including the schema. `VACUUM INTO` and the backup API are always consistent. The CLI `.backup` can block
for as long as a busy writer keeps writing. When restoring, a leftover `-wal` either corrupts the restored file or
quietly replaces it with the old data.

**Implication for Angee.**

- Save SQLite with `sqlite3 db "VACUUM INTO '<out>'"` (or the backup API with `pages=-1`), never by copying files.
- Restore only into a stopped service, deleting `<db>-wal` and `<db>-shm` before putting the file in place.
- Avoid the CLI `.backup` for busy databases.

---

## Spike 8: Redis AOF vs RDB on restore (`s8.sh`, `s8b.sh`, `out-s8*.txt`)

**Setup.** `redis:7-alpine` (7.4.9) running `redis-server --appendonly yes`, loaded with 1,001 keys (`key:1..1000`
plus `marker=from-source`), then `BGSAVE`. The source `/data` held `dump.rdb` (15,900 B) and `appendonlydir/`, which
contains `appendonly.aof.1.base.rdb` (88 B), `appendonly.aof.1.incr.aof` (37,852 B) and the manifest. The
`dump.rdb` was then placed alone in a fresh named volume with `docker create` + `docker cp` + `docker start`.

**Key output.**

| case | keys loaded | log / files |
|---|---|---|
| **A**: only `dump.rdb`, `--appendonly yes` | **DBSIZE=0** | `Creating AOF base file appendonly.aof.1.base.rdb on server start` / `Creating AOF incr file … on server start`. The new base is **88 B (empty)**. No "Loading RDB" line at all. |
| A, then `docker stop` (`s8b.sh`) | n/a | `Saving the final RDB snapshot before exiting.` / `DB saved on disk`. **`dump.rdb` drops from 15,900 B to 88 B**, so the restored data is destroyed. Restarting with `--appendonly no` gives DBSIZE=0. |
| **B**: only `dump.rdb`, `--appendonly no` | **1001** | `Loading RDB produced by version 7.4.9` / `Done loading RDB, keys loaded: 1001` |
| **D**: `dump.rdb` copied into a data dir that already has an AOF (target held `other=1`) | **1** (other=1, marker absent) | `Reading RDB base file on AOF loading...` / `keys loaded: 0` / `DB loaded from incr file`. The AOF wins and the RDB is ignored. |
| **E**: load with `--appendonly no`, `CONFIG SET appendonly yes`, restart with `--appendonly yes` | **1001** | the rewrite produced `appendonly.aof.1.base.rdb` of 15,900 B. On restart: `RDB is base AOF` / `keys loaded: 1001` |
| **F**: whole `/data` (dump.rdb + appendonlydir) copied from the stopped source | **1001** | loaded through the AOF manifest (base plus incr) |

**Verdict.** Confirmed, and the outcome is worse than the claim. With AOF on, Redis 7.4 ignores a lone `dump.rdb`.
It does **not** build the AOF from the RDB: it writes an AOF base from the empty in-memory dataset. The first clean
shutdown then overwrites the restored `dump.rdb` with an empty snapshot. If an AOF already exists, it always takes
precedence.

**Implication for Angee.** Pick one of two Redis restore procedures:

1. Put `dump.rdb` into an **empty** data dir, start with `--appendonly no`, run `CONFIG SET appendonly yes` and wait
   for `aof_rewrite_in_progress:0`, then return to the normal config.
2. Save and restore the whole stopped `/data`, including `appendonlydir`.

Never drop an RDB next to an AOF.
