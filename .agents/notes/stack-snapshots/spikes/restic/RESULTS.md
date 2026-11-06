# restic 0.19.1 spikes for Angee snapshot storage

Environment: macOS 26 (Darwin 25.4.0) arm64, restic 0.19.1 (go1.26.4), rest-server 0.14.0.
All runs were real commands against local repos (or a local rest-server) under
`spikes/restic/work/`. Password supplied with `RESTIC_PASSWORD_FILE` (or `--password-command`), cache in
`spikes/restic/cache` (`RESTIC_CACHE_DIR`). Scripts: `s01_*.sh` … `s11_*.sh`, `s05_rest_cost.py`,
`latency_proxy.py`; raw outputs: `sNN.out`, `s05.results.json`, `s05.summary.txt`.

Abbreviations: `$R` = `restic -r <repo>`; `H` = `--host`; IDs truncated to 8 hex chars.

## At a glance

| # | Question | Verdict |
|---|---|---|
| 1 | nested `--stdin-filename` | PASS. The path becomes a real tree path. Addressable by dump, restore `--include` and `snap:sub`. Names are silently path-cleaned. |
| 2 | many streams in one snapshot | Impossible. One snapshot per stream is forced. FIFOs are stored as empty special nodes and their writers hang. |
| 3 | failed producer | `--stdin` pipe: FAIL (commits a partial file, exit 0). `--stdin-from-command`: refused, no snapshot, exit **1 or 130 (racy)**. An empty stream gives exit 3 and a snapshot *without* the file. |
| 4 | file changed while reading | Not detected. Torn or truncated content is stored with exit 0. |
| 5 | small invocations over REST | 13 requests and ≈11–12 sequential RTTs per backup. Local floor 0.75–0.95 s (scrypt ≈0.3 s + lock wait ≈0.2 s). Backup ≈1.26 / 1.52 / 2.12 s at 20 / 50 / 100 ms. Cold cache costs O(#index files). |
| 6 | retention over tagged sets | PASS when the policy runs on manifests and members are then forgotten by explicit IDs. `--tag a,b` = AND, `--tag a --tag b` = OR. |
| 7 | locks | Concurrent backups OK. Backup during prune gives exit 11; `--retry-lock` waits and succeeds. A SIGKILL leaves a stale lock that blocks prune and forget until `unlock`. |
| 8 | exit codes / password | 10 / 11 / 12 / 130 confirmed. One JSON `exit_error` on stderr. `--password-command` is word-split, not a shell. An unreachable REST server retries for ≈15 min and then exits **1** (not 10). |
| 9 | dump zip/tar | PASS for stdin and directory snapshots. One snapshot per call. A single file is dumped raw. |
| 10 | JSON shapes | Recorded. Watch for: `--dry-run` summaries carry a `snapshot_id`; `tag` changes the snapshot ID; some stderr is plain text. |
| 11 | stable file-member paths | Use cwd + relative args (the tree becomes `/<group>/<name>/…`). No path-rewrite flag exists. `snapshot.paths` stays absolute. |

---

## 1. Nested `--stdin-filename` paths — PASS

Commands (`s01_nested_stdin.sh`):

```sh
$R backup --stdin --stdin-filename databases/app/app.pgdump --host stackA \
   --tag angee.snapshot=s1,angee.kind=member,angee.service=app --json < src.bin
$R ls --json <snap>
$R dump <snap> /databases/app/app.pgdump      # also without the leading slash
$R dump <snap>:databases/app /app.pgdump       # snapshot:subfolder syntax
$R restore <snap> --target X --include /databases/app/app.pgdump
$R restore <snap>:databases/app --target X2
$R backup --stdin-from-command --stdin-filename databases/app2/app2.pgdump --json -- cat src.bin
```

Key output:

- backup exit 0, `snapshot.paths = ["/databases/app/app.pgdump"]`. `ls --json` nodes:
  `/databases` (dir), `/databases/app` (dir), `/databases/app/app.pgdump` (file, size 300000).
  The intermediate directories are synthesized (mtime = backup time, mode `drwxr-xr-x`).
- `dump` with and without a leading `/`, and `dump <snap>:databases/app /app.pgdump`, all print
  bytes with the same SHA-256 as the source. `dump` of a missing path: exit 1,
  `Fatal: cannot dump file: path "/databases/app/nope.pgdump" not found in snapshot`.
- `restore --include /databases/app/app.pgdump` → `X/databases/app/app.pgdump` (hash matches;
  summary `{"total_files":1,"files_restored":3,...}`, so `files_restored` counts the directories too).
  `restore <snap>:databases/app --target X2` → `X2/app.pgdump`.
- `--stdin-from-command` with the same nested name behaves identically (exit 0, same tree shape;
  `data_blobs:0` because the content deduplicated).
- restic **normalizes stdin filenames silently** (all exit 0):
  `/abs/leading.dump`→`/abs/leading.dump`, `../escape.dump`→`/escape.dump`,
  `a/../b.dump`→`/b.dump`, `trailing/`→ a *file* `/trailing`, `a//b.dump`→`/a/b.dump`.

Verdict: PASS. A nested stdin filename becomes a real path in the snapshot tree. `dump`,
`restore --include` and `snap:subfolder` can all address it.

**Implication for Angee:** use `<group>/<name>/<file>` stdin filenames freely, but validate and
canonicalize member names before calling restic. restic cleans `..` and `//` silently, so two
distinct Angee names could collide.

---

## 2. Several streams in ONE snapshot — not possible; one snapshot per stream is forced

Commands (`s02_multi_stream.sh`). `restic backup --help` and `restic options` have no
`--read-special`-style flag, and no option for reading FIFOs or devices.

| Attempt | Result |
|---|---|
| (a) dir containing 2 FIFOs, writers attached (`head -c 1MiB > fifo &`), `restic backup DIR` | exit 0 in 0.74 s. Both FIFOs stored as `"type":"fifo"` nodes with **no content**. restic never opens them, so the **writers stay blocked forever** (they had to be killed by hand). |
| (a') FIFO passed directly as an argument | exit 0, stored as a `fifo` node, content not read, writer blocked. |
| (a'') `restic backup /dev/fd/3 3< <(producer)` | exit 3: `{"message_type":"error","error":{"message":"incomplete metadata for /dev/fd/3: xattr.list /dev/fd/3: operation not permitted"}}`; stored as a `fifo` node with no content. |
| (b) `--stdin --stdin --stdin-filename one.dump --stdin-filename two.dump` | exit 0; only `/two.dump` exists (the last flag wins). |
| (b') `--stdin` plus file args | exit 1 `Fatal: --stdin was specified and files/dirs were listed as arguments`. |
| (b'') `--stdin-from-command -- sh -c 'echo one' -- sh -c 'echo two'` | exit 0; everything after the first `--` is ONE argv. Only "one" is stored. |
| (b''') `--stdin-from-command` + `--files-from` | exit 1 `Fatal: --stdin and --files-from cannot be used together`. |
| (c) `rewrite` / `copy` | rewrite can only `--exclude`/`--include` and change metadata. Neither can add or merge files into a snapshot. |

Verdict: forced. restic 0.19.1 offers no supported way to put more than one unknown-length
stream into a single snapshot. The only routes are buffering to disk first, or wrapping all
streams in one container stream (for example a tar, which needs each member's size up front).
Either way restic sees a single file.

**Implication for Angee:** "one restic snapshot per streamed member" is the only design.
Never place FIFOs in a file-member path. restic records them as empty special nodes and the
producer hangs forever.

---

## 3. Failed, truncated or empty producers — plain `--stdin` cannot detect failure; `--stdin-from-command` can

Commands (`s03_failed_producer.sh`, `s03b_orphan_packs.sh`, `s03c_exitcode_repeat.sh`):

| Case | Command | restic exit | Snapshot? | Key output |
|---|---|---|---|---|
| 3a | `sh -c 'head -c 10485760 /dev/urandom; exit 1' \| $R backup --stdin ...` | **0** (producer 1) | **YES**, 10 MiB | normal summary, `total_bytes_processed:10485760`, no error |
| 3a' | same with `set -o pipefail` | pipeline 1, restic 0 | **YES** | the shell sees the failure, but restic has already committed |
| 3b | `$R backup --stdin ... < /dev/null` (empty) | **3** | **YES, but the file node is MISSING** (tree has only `/db`) | `{"message_type":"error","error":{"message":"failed to save /db/empty.dump: read empty.dump: no data read"},"during":"archival","item":"/db/empty.dump"}` then `{"message_type":"exit_error","code":3,"message":"Warning: at least one source file could not be read"}` |
| 3c | `$R backup --stdin-from-command ... -- sh -c 'head -c 10485760 /dev/urandom; exit 1'` | **1 or 130** (racy) | NO | `{"message_type":"error","error":{"message":"failed to save /db/fail3.dump: Fatal: command failed: exit status 1"},...}`, `{"message_type":"error","error":{"message":"no result"},...}`, `{"message_type":"exit_error","code":1,"message":"Fatal: unable to save snapshot: failed to save /db/fail3.dump: Fatal: command failed: exit status 1"}` |
| 3c' | producer `echo boom >&2; exit 2` | 1 | NO | the producer's stderr is passed through, prefixed: `subprocess sh: boom` |
| 3c'' | producer SIGKILLs itself after 1 MiB | 1 | NO | `... command failed: signal: killed` |
| 3d | `--stdin-from-command -- sh -c 'exit 0'` (empty, success) | **3** | **YES, file node MISSING** | same "no data read" error as 3b |
| 3e | `--stdin-from-command`, 5 MiB, exit 0 | 0 | yes | normal |

Exit-code race (`s03c`, 12 runs of a failing producer with 1–80 MiB): exit 1 in 7 runs and
**exit 130** (`{"message_type":"exit_error","code":130,"message":"Fatal: unable to save snapshot: context canceled"}`)
in 5 runs, at every size. In the 130 case the stream still contains the
`"error":{"message":"failed to save ...: Fatal: command failed: exit status 1"}` event first.
No snapshot was ever created.

Orphan data: a failed 80 MiB `--stdin-from-command` run left **4 packs, 71 MiB** in `data/` with
no index and no snapshot. 12 failed runs left 14 packs / 241 MiB. `check` reports no errors;
`prune --dry-run` shows `to delete: 0 blobs / 71.321 MiB`, so prune reclaims them.

Verdict: plain `--stdin`: FAIL for integrity. restic commits whatever arrived before EOF and
exits 0. `--stdin-from-command`: PASS. A non-zero exit or signal from the producer is refused
with no snapshot, but the exit code is 1 *or* 130. Both modes treat a zero-byte stream as an
error (exit 3) and still commit a snapshot without the file.

**Implication for Angee:** run producers through `--stdin-from-command -- <save-job argv>`, never
`producer | restic --stdin`. Classify outcomes from the JSON `error`/`exit_error` events plus
"was a `summary.snapshot_id` emitted", not from the exit code alone (1 vs 130 is racy). Treat
exit 3 as "member failed" and forget the incomplete snapshot. A legitimately empty dump cannot
be stored over stdin, so wrap it (for example a 1-byte header or a tar envelope) or record
"empty" in the manifest. Schedule `prune` to reclaim packs from failed attempts.

---

## 4. File changed while reading — restic does NOT detect it (exit 0, no warning)

Command (`s04_file_changed.sh`): 512 MiB of fresh random content per case; upload throttled so the
read takes ~10 s; the mutation fires at ~41–46 % read progress (per the JSON `status` events).

```sh
$R backup <dir> --host h --force --read-concurrency 1 --limit-upload 51200 --json
```

| Case (at ~45 % read) | exit | JSON errors | Stored content |
|---|---|---|---|
| append 1 MiB | 0 | none | size 537919488 (original + 1 MiB); content = the final file (the reader reached the new EOF) |
| in-place overwrite of the first 4 MiB (behind the reader) | 0 | none | **the original** (the change was missed) |
| in-place overwrite at 500 MiB (ahead of the reader) | 0 | none | the final file |
| **torn**: overwrite behind AND ahead | 0 | none | **matches neither the original nor the final file** |
| **truncate to 0** | 0 | none | **a 224 MB prefix** (`size:224395264`). Silently truncated; `total_bytes_processed` 224395270 < the scanner's `total_bytes` 536870918 |
| delete (`rm`) | 0 | none | the original (the open fd keeps the inode) |
| replace via rename (`mv new big.bin`) | 0 | none | the original (old inode) |
| small file deleted after it was read, before its xattrs were read | **3** | `incomplete metadata for …/small.txt: xattr.list …: no such file or directory` | content stored |

(A first run without fresh content per case was invalid: dedup skipped the throttled upload and
the read finished before the mutation. It is kept as `s04-run1-dedup-artifact.out`.)

Verdict: FAIL (no detection). restic 0.19.1 does not re-stat after reading. Concurrent writes
yield silently torn or truncated files with exit 0. Only metadata reads on a vanished path
raise exit 3.

**Implication for Angee:** file members must be quiescent while restic reads them. Back up from a
frozen staging copy, a filesystem snapshot, or a stopped or paused service. Angee cannot rely on
restic to flag a live-written file.

---

## 5. Cost of many small invocations against a REST backend

Setup (`s05_rest_cost.py`). Port 8000 was already taken on this machine, so I used
`rest-server --path work/s05/data --listen 127.0.0.1:18000 --no-auth --log work/s05/rest-access.log`.
restic talked to it through `latency_proxy.py` (127.0.0.1:18001). The proxy logs every request
with monotonic timestamps and in-flight count, and injects a fixed per-request delay to emulate
RTT. Request counts from the proxy matched the rest-server access log line counts in every
invocation. Both servers were stopped at the end.

Phase A: 20 sequential `backup --stdin --stdin-filename databases/app/app.pgdump --host stackA
--tag angee.snapshot=…,angee.kind=member,angee.service=app` with payloads of 1 KiB…1 MiB, 0 ms injected:

- wall mean **0.762 s** (min 0.747, max 0.791). **13 requests per invocation, every time**:
  `HEAD config`, `GET keys/` (list), `GET keys/<id>`, `GET config`, `GET locks/`, `POST locks/<id>`,
  `GET locks/`, `GET index/` (list), `POST data/<id>` ×2 (data pack + tree pack, in parallel), `POST index/<id>`,
  `POST snapshots/<id>`, `DELETE locks/<id>`. There is no `GET snapshots/`: stdin backups do
  not look up a parent.
- Request timeline of one backup (0 ms injected):
  ```
  +0ms HEAD /config; +0 GET /keys/; +1 GET /keys/<id>
  +463ms GET /config          <- ~0.3-0.46 s gap = scrypt key derivation (N=32768,r=8,p=6)
  +466 GET /locks/; +467 POST /locks/<id>
  +678ms GET /locks/          <- ~210 ms fixed wait before the lock re-check
  +678 GET /index/; +681 POST /data ×2 (inflight=2); +703 POST /index; +712 POST /snapshots; +720 DELETE /locks
  ```
- Local floor, measured on a local repo (`s05b_local_floor.sh`): process start 0.03 s;
  `cat config --no-lock` 0.31 s (user CPU 0.25 s = KDF); `snapshots --no-lock` 0.28 s;
  `snapshots` with a lock 0.49 s; `backup --stdin` 1 KiB 0.54 s.

Phase B: repo populated to **200 snapshots** (45 sets × 3 members + manifest, plus phase A),
which produced **200 index files** (one per backup; they are consolidated only by `prune`).

Phase C: each op at 0/20/50/100 ms injected per-request delay (mean of 3 runs for backup).
The slope gives the number of effectively sequential round trips:

| Operation (200-snapshot repo, warm cache) | requests | max in-flight | 0 ms | 20 ms | 50 ms | 100 ms | sequential RTTs |
|---|---|---|---|---|---|---|---|
| `backup --stdin` 64 KiB member | 13 | 2 | 0.945 s | 1.264 s | 1.521 s | 2.115 s | 11.4 |
| `backup --stdin` manifest | 12–13 | 2 | 0.947 | 1.239 | 1.490 | 2.065 | 10.9 |
| `snapshots --json --host H --tag angee.kind=manifest` | 9–10 | 1 | 0.957 | 1.208 | 1.342 | 1.806 | 8.1 |
| same with `--no-lock` | 5 | 1 | 0.766 | 0.831 | 0.867 | 1.152 | 3.8 |
| `dump <member> <path>` (4 KiB) | 10 | 1 | 1.074 | 1.163 | 1.361 | 1.838 | 7.8 |
| `forget --dry-run --json --host H --tag angee.kind=manifest --keep-last 3` | 9 | 1 | 1.026 | 1.156 | 1.325 | 1.742 | 7.1 |
| `backup --stdin` with a **COLD cache** (new `--cache-dir`) | 217→235 (204–222 `GET index/<id>`) | 5 | 1.068 | 2.208 | 3.887 | 6.794 | 57.2 |
| `backup --stdin -o rest.connections=1` | 14 | 1 | 1.006 | 1.211 | 1.677 | 2.352 | 13.7 |

Parallelism: restic is almost fully sequential for small operations. Only the two pack uploads
overlap (in-flight 2). Parallel fan-out (in-flight 5 = `rest.connections` default) appears only
when downloading many index files on a cold cache. The zero-latency floor also grows with the
number of index files: 0.76 s at ~20 snapshots versus 0.95 s at 200 snapshots, from decrypting
200 cached index files.

Estimate, per invocation = local floor (~0.75–0.95 s) + sequential RTTs × RTT:

| RTT | backup (≈12 RTT) | snapshots locked (≈8) / `--no-lock` (≈4) | dump (≈8) | forget --dry-run (≈7) | cold-cache backup, 200 index files (≈57) |
|---|---|---|---|---|---|
| 20 ms | ≈ 1.2 s (measured 1.26) | ≈ 1.1 / 0.85 s | ≈ 1.2 s | ≈ 1.15 s | ≈ 2.2 s |
| 50 ms | ≈ 1.5 s (1.52) | ≈ 1.35 / 0.95 s | ≈ 1.4 s | ≈ 1.35 s | ≈ 3.9 s |
| 100 ms | ≈ 2.1 s (2.12) | ≈ 1.8 / 1.15 s | ≈ 1.85 s | ≈ 1.75 s | ≈ 6.8 s |

Example: one stack snapshot of 3 streamed members + a manifest costs 4 backups ≈ 8.5 s
serially at 100 ms RTT. Members can run concurrently (see spike 7a), which gives ≈ 2 × 2.1 s
plus the producers' own time.

**These are lower bounds.** The proxy adds one RTT per request on an already-open keep-alive
connection. It does not model TCP or TLS handshakes (1–3 extra RTTs per new connection; restic
opens up to `rest.connections`=5), bandwidth limits, server-side storage latency, or a real
object store such as S3/B2, which has its own per-request latency and needs more round trips
per lock. Everything ran on localhost.

**Implication for Angee:** each restic invocation has a fixed cost of ~0.3 s KDF + ~0.2 s lock
wait + ~12 RTTs per backup. Keep the per-stack-snapshot invocation count low: run members in
parallel, use `--no-lock` for read-only listing, and batch forgets into one call. Keep a
persistent `--cache-dir` per destination, because a cold cache costs O(#index files) requests.
Run `prune` periodically, since it consolidates index files (one is added per backup).

---

## 6. Retention over tagged sets

Setup (`s06_retention.sh`): 10 stack snapshots `s01`…`s10`, one per day via `--time`, each with
3 members (`databases/app/app.pgdump`, `databases/redis/dump.rdb`, `files/media/media.tar`, tags
`angee.snapshot=sNN,angee.kind=member,angee.service=…`), then the manifest (`angee/manifest.json`,
`angee.kind=manifest`) last. Set s05's redis member was then removed, giving 39 snapshots.

**6a.** `restic forget --dry-run --json --host stackA --tag angee.kind=manifest --keep-last 3` → exit 0.
The JSON is an **array of groups**, each `{"tags":null,"host":"stackA","paths":["/angee/manifest.json"],"keep":[<snapshot>…],"remove":[<snapshot>…],"reasons":[{"snapshot":<snapshot>,"matches":["last snapshot"]}…]}`.
Each `<snapshot>` has the same shape as a `snapshots --json` element (`time, tree, paths, hostname,
username, uid, gid, tags, program_version, summary, id, short_id`). Here there is 1 group;
keep = s10, s09, s08; remove = s07…s01. (`remove` is `null` rather than `[]` when nothing is
removed; seen in 6c.)

**6b.** `--keep-daily 3 --keep-weekly 2` → keep s10 (`daily snapshot`, `weekly snapshot`), s09, s08
(`daily`), s04 (`weekly`). The `reasons[].matches` strings are human labels.

**6c. Grouping.** Default `--group-by host,paths`: manifests share one path, so they form 1 group,
which is what we want. Members form **one group per path** (3 groups), so a policy applied to
members directly keeps the last 3 *per member path*. That drifts from the sets whenever a set is
partial: the redis group kept s10, s09, s08 but removed 6, not 7. `--group-by host` over members
puts all of them in one group, and `--keep-last 3` then keeps only 3 member snapshots in total
(all from s10). `--group-by tags` over manifests gives 10 groups (one per unique
`angee.snapshot` tag), so nothing is removed. Both are wrong for this layout.

**6d. Tag filter semantics** (via `snapshots --json`; `forget`, `dump` and `restore` use the same filter):

| Filter | Matches | Meaning |
|---|---|---|
| `--tag angee.snapshot=s01,angee.kind=manifest` | 1 | comma = **AND** |
| `--tag angee.snapshot=s01 --tag angee.kind=manifest` | 13 | repeated flag = **OR** |
| `--tag angee.snapshot=s01 --tag angee.snapshot=s02` | 8 | OR (both sets) |
| `--tag angee.snapshot=s01,angee.snapshot=s02` | 0 | AND of two different ids |
| `--tag angee.snapshot=s0`, `--tag 'angee.snapshot=s0*'` | 0 | exact match only, no prefix or glob |

**6e. Forgetting the members of dropped sets:**

- `forget --tag angee.snapshot=s01` (no policy, no IDs) → exit 1 `Fatal: no policy was specified, no snapshots will be removed`. Same with `--keep-last 0`.
- `forget --tag … --group-by '' --keep-tag nonexistent` → exit 1 `refusing to delete last snapshot of snapshot group ""`.
- `forget --dry-run --tag angee.snapshot=s01 --unsafe-allow-remove-all --group-by '' --keep-last 0` → would remove all 4. This works, but it is a foot-gun.
- **Working approach:** resolve IDs with `snapshots --json --tag angee.snapshot=<id>,angee.kind=manifest`
  (and `…,angee.kind=member`), then `forget --json <manifest ids…>` and then `forget --json <member ids…>`
  (7 + 20 IDs, both exit 0, **no stdout output**). Afterwards: 12 snapshots left (s08–s10 × 4).
- `forget deadbeef` (unknown ID) → **exit 0**, stderr `Ignoring "deadbeef": no matching ID found for prefix "deadbeef"`.
  Forgetting an already-forgotten full ID → **exit 0**, with `Load(<snapshot/…>) failed … Ignoring …` on stderr.

Verdict: PASS with a caveat. Apply the policy to manifests only, with default grouping or
`--group-by host`, both of which give one group here. Then forget by explicit IDs. `--tag a,b` is
AND and `--tag a --tag b` is OR.

**Implication for Angee:** compute retention on manifest snapshots, then map the kept and removed
manifests to member IDs by `angee.snapshot` tag, and call `forget` with explicit IDs. Forget the
manifest first (de-commit), then the members, so a crash leaves orphaned members rather than a
manifest pointing at missing data. Never apply keep-policies to members. `forget` returns exit 0
on unknown IDs, so verify by re-listing. Never build tag filters by repeating `--tag` when AND is
intended.

---

## 7. Locks

Scripts: `s07_locks.sh`, `s07d_kill_prune.sh`, `s07e_unlock_live.sh`, `s07f_repro.sh`. The test repo had 451 MiB
of half-used packs so that `prune --max-unused 0` has to repack; it was throttled with `--limit-download/--limit-upload`.
`restic cat lock <id>` shows `{"time","exclusive","hostname","username","pid","uid","gid"}`.

**(a) Two concurrent `backup`s** (60 MiB `--stdin-from-command` each, throttled): both held non-exclusive locks at
the same time (2 locks listed), **both exit 0**, wall 5.9 s for both together, and two snapshots were created. PASS.

**(b) `prune` (exclusive) running, then a backup:**

| Concurrent command | Result |
|---|---|
| `backup --stdin` (no retry) | **exit 11** after 0.5 s: `{"message_type":"exit_error","code":11,"message":"unable to create lock in backend: repository is already locked exclusively by PID 88278 on <host> by user (UID 501, GID 20)\nlock was created at 2026-11-06 11:04:03 (3.172547s ago)\nstorage ID cf086e8a\nthe `unlock` command can be used to remove stale locks"}` |
| `snapshots --json` (non-exclusive lock) | exit 11 (same message) |
| `snapshots --json --no-lock` | **exit 0** |
| `backup --retry-lock 5s` | exit 11 after 5.6 s (gave up) |
| `backup --retry-lock 5m` | **waited 15.8 s and then succeeded, exit 0.** Prune itself took 26.4 s, exit 0 (`rebuilding index … 5 / 5 old indexes deleted`, `removing 20 old packs`) |

Reverse order: with a backup running, `prune --dry-run` → **exit 11** (even dry-run prune takes the exclusive lock),
`prune` → exit 11, and `forget --keep-last 100` → **exit 11** (forget is exclusive too). The backup finished with exit 0.
In non-JSON mode the message is `repo already locked, waiting up to 0s for the lock`.

**(c) Stale locks after `kill -9`:**

- SIGKILL of a running backup leaves its lock file in place (`list locks` shows 1; `cat lock` → `"exclusive": false, "pid": 91081`).
- A new **backup succeeds** (exit 0) next to the stale non-exclusive lock.
- **`prune` fails with exit 11** (`repository is already locked by PID 91081 …`). A stale lock from a dead PID on the
  *same host* still blocks exclusive operations: restic does not auto-ignore it when it acquires a lock.
- `restic unlock` → `successfully removed 2 locks`, exit 0; `list locks` is then empty.
- SIGKILL of a **prune mid-repack** (s07d) leaves a stale **exclusive** lock. `backup` → exit 11, `backup --retry-lock 10s`
  → exit 11, and `check` → exit 11, until `restic unlock`. Afterwards `check` and `check --read-data` both pass
  (`no errors were found`). Packs went from 22 to 28 (half-written repack output). Re-running `prune` removed 14 old packs, and `check` passed.
- `unlock` (no flags) removes **only stale locks**: its own host and dead PID. Over 3 runs with one live and one SIGKILLed backup,
  it removed 1 lock each time; the live backup kept its lock and finished with exit 0.
  `unlock --remove-all` also deletes a **live** lock. The live backup does not notice: it finished with exit 0, then printed
  `Remove(<lock/…>) failed … error while unlocking`, so it was unprotected against a concurrent prune.
- SIGINT and SIGTERM are graceful: exit 130, the lock is removed, and no snapshot is created (spike 8).
- Not tested here (single host): according to restic's lock code, a lock from a *different hostname* is only considered
  stale after 30 minutes without refresh (live locks refresh every 5 min). `unlock` would therefore not remove a lock
  left by a SIGKILLed restic in a container with another hostname until 30 minutes have passed.
- Test artifact (documented so it is not misread): one s07d run showed a "live" backup exiting 130. The cause was the
  script's cleanup `pkill -f 'head -c …'`. It matched the live restic itself, because its argv contains the producer
  command, and macOS `pkill -P` did not narrow the match. That sent SIGTERM to the live restic. s07f reproduced this
  and showed `unlock` is innocent.

Verdict: concurrent backups PASS. Backup during prune fails fast with exit 11, and `--retry-lock <d>` makes it
wait and succeed (PASS). A SIGKILLed process leaves a stale lock that blocks prune, forget and (if exclusive) all
locking commands until `restic unlock`. Plain `unlock` is safe to run while live processes exist on the same host.

**Implication for Angee:** run members of one stack snapshot concurrently. Always pass `--retry-lock`
(longer than the longest prune) on backups. Use `--no-lock` for read-only listing. Serialize
forget and prune per repository inside Angee. After any SIGKILL or crash, run plain `restic unlock`
(never `--remove-all`) before forget or prune. Run all restic processes for a repository under one stable hostname,
so their stale locks stay detectable.

---

## 8. Exit codes and password command

Script: `s08_exit_codes.sh` (stdout and stderr captured separately). The unreachable-server case is in `s08b_unreachable_rest.sh`.

| Case | Exit | stderr (stdout was empty in every failure case) |
|---|---|---|
| wrong password (`RESTIC_PASSWORD_FILE`), `--json` | **12** | `{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}` (the only stderr line) |
| same, without `--json` | 12 | `Fatal: wrong password or no key found` |
| missing local repository | **10** | `{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file: stat …/config: no such file or directory…"}` |
| missing repository on a running rest-server (`rest:http://127.0.0.1:18002/nope/`) | **10** | `{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file: <config/> does not exist\nIs there a repository at the following location?\nrest:http://127.0.0.1:18002/nope/"}` |
| **unreachable** rest-server (connection refused) | **1 after ≈15 min** | plain-text lines `Stat(<config/>) returned error, retrying after 1.28s / 1.84s / 4.8s / 11.3s / 16.5s …` with exponential backoff capped at about 1.5 min, then exit 1 after 892 s (s08b below) |
| `--password-command "cat <file>"` | 0 | — |
| `--password-command "sh -c 'cat <file> \| tr -d x'"` | 0 | — |
| `--password-command "cat <file> \| tr -d x"` (bare pipe) | 1 | `cat: \|: No such file or directory …` then `Fatal: Resolving password failed: exit status 1`. The command is **word-split, not run by a shell** |
| `RESTIC_PASSWORD_COMMAND="cat <file>"` | 0 | — |
| `--password-command "echo nope"` (wrong password) | 12 | same as the wrong-password case |
| `--password-command` exits 1 (prints `vault-down` on stderr) | **1** | `vault-down` (passed through as plain text), then `{"message_type":"exit_error","code":1,"message":"Fatal: Resolving password failed: exit status 1"}` |
| `--password-command true` (empty output) | 1 | `Fatal: an empty password is not allowed by default. Pass the flag --insecure-no-password …` |
| no password source, stdin `/dev/null` | 1 | same "empty password" message |
| password file AND command | 1 | `Fatal: Resolving password failed: Fatal: Password file and command are mutually exclusive options` |
| `init` on an existing repo | 1 | `… failed: config file already exists` |
| `dump deadbeef …` | 1 | `Fatal: failed to find snapshot: no matching ID found for prefix "deadbeef"` |
| SIGINT during backup | **130** | `signal interrupt received, cleaning up` (plain text) then `{"message_type":"exit_error","code":130,"message":"Fatal: unable to save snapshot: error flushing repository: context canceled"}`. No snapshot; lock removed |
| SIGTERM during backup | **130** | `signal terminated received, cleaning up`, then the same JSON. No snapshot; lock removed |
| unknown flag | 1 | `{"message_type":"exit_error","code":1,"message":"unknown flag: --bogus-flag"}` |
| repository locked (spike 7) | 11 | see 7b |
| dump consumer closed the pipe (spike 9) | 141 | (SIGPIPE) |

s08b, unreachable REST backend measured to give-up: `restic -r rest:http://127.0.0.1:18999/ snapshots --json`
(nothing listening) ran from 11:08:54 to 11:23:46, i.e. **892 s ≈ 14.9 min**, with **21 retries**. It ended with **exit 1, not 10**:
`Stat(<config/>) failed: … connection refused`, then
`{"message_type":"exit_error","code":1,"message":"Fatal: unable to open config file: Head \"http://:***@127.0.0.1:18999/config\": dial tcp 127.0.0.1:18999: connect: connection refused\nrest:http://127.0.0.1:18999/"}`.
A timeout-killed run (SIGTERM via `timeout 20`) printed `signal terminated received, cleaning up`.

Verdict: PASS. The documented codes hold: 0, 1, 3, 10, 11, 12, 130. With `--json` the final error is always one
`exit_error` object on stderr (`{"message_type":"exit_error","code":N,"message":"…"}`). Other plain-text lines can
precede it: retry notices, signal notices, the password command's or producer's own stderr, and `Ignoring …`.
`--password-command` works but is **not** run through a shell.

**Implication for Angee:** pass the password through `RESTIC_PASSWORD_COMMAND` or `--password-command` as an argv
string (wrap it in `sh -c` only if shell features are needed), or through `RESTIC_PASSWORD_FILE` on a 0600 tmpfs file.
Map exit codes 10/11/12/130 to typed errors. Treat 1 as "read the `exit_error` message" and 3 as "incomplete
snapshot". **Always bound every restic invocation with Angee's own deadline** (send SIGTERM, which restic handles gracefully),
because an unreachable backend makes restic retry for ≈15 min and then exit with a generic 1 instead of 10.

---

## 9. `dump -a zip` / `tar` — PASS; one snapshot per `dump` call

Commands (`s09_dump_archive.sh`). A stdin file and a directory cannot share one snapshot
(spike 2), so this used one stdin snapshot (`databases/app/app.pgdump`) and one directory snapshot
(`files/media` backed up with cwd = root, containing nested dirs and a symlink).

- `dump -a zip <stdin snap> / > out.zip` → exit 0; `unzip -l`: `databases/`, `databases/app/`, `databases/app/app.pgdump` (200000).
- `dump -a zip <dir snap> /` → `files/`, `files/media/`, `files/media/img/a.jpg`, `files/media/img/sub/b.jpg`, `files/media/link.txt`, `files/media/readme.txt`.
  Entries are made by Unix with modes preserved (`drwxr-xr-x`, `-rw-r--r--`). The symlink is a real zip symlink entry
  (`lrwxr-xr-x … link.txt`) and extracts as `link.txt -> readme.txt`.
- `dump -a zip <dir snap>:files/media /` → re-rooted: `img/…`, `link.txt`, `readme.txt`.
- `dump -a tar <dir snap> /` → same tree; the symlink is kept (`link.txt -> readme.txt`).
- `dump -a zip <snap> /databases/app/app.pgdump` (a single file) → **raw bytes, not a zip** (identical to the source; `file` says `data`).
- `dump -a tar <snap1> <snap2> /` → exit 1 `Fatal: no file and no snapshot ID specified`. The message is misleading, but **one snapshot per call** is confirmed.
- `dump --host h latest /databases/app/app.pgdump` → `latest` resolves to the newest snapshot of the host, which here was the dir snapshot → exit 1 `path "/databases" not found`.
- `dump --target file <snap> <path>` → writes the file (identical).
- `dump <snap> <30 MB file> | head -c 10` → restic **exit 141** (SIGPIPE) when the consumer stops reading.

Verdict: PASS. zip and tar both work for stdin and directory snapshots, and `snap:subfolder`
re-roots the archive. A single-file dump is always raw. One snapshot per `dump` invocation.

**Implication for Angee:** a "download whole stack snapshot" archive needs N `dump` calls (one per
member) stitched together by Angee, or a restore to a temp dir. Always use explicit snapshot IDs
rather than `latest`. Treat exit 141 as a consumer-side failure.

---

## 10. JSON shapes Angee will decode

From `s10_json_shapes.sh` (stdout unless noted):

- `init --json` → `{"message_type":"initialized","id":"<repo id>","repository":"<location>"}`
- `backup --json` streams `status` lines then one `summary`:
  - `status`: `{"message_type":"status","seconds_elapsed"?,"seconds_remaining","percent_done","total_files","files_done"?,"total_bytes"?,"bytes_done","current_files":[…]}`.
    For stdin the size is unknown, so `percent_done` is 0 and `seconds_remaining` is garbage (75527225026).
  - `summary`: `{"message_type":"summary","files_new","files_changed","files_unmodified","dirs_new","dirs_changed","dirs_unmodified","data_blobs","tree_blobs","data_added","data_added_packed","total_files_processed","total_bytes_processed","total_duration","backup_start","backup_end","snapshot_id"}`
  - with `--skip-if-unchanged` and no change: the summary has **no `snapshot_id`**, and no snapshot is created.
  - with `--dry-run`: the summary **has a `snapshot_id` and `"dry_run": true`**, but nothing is written.
  - `-v` adds `verbose_status` events: `{"message_type":"verbose_status","action":"scan_finished"|"new"|"unchanged"|"modified",…,"item","duration","data_size","data_size_in_repo","metadata_size","metadata_size_in_repo","total_files"}`.
  - errors go to **stderr**: `{"message_type":"error","error":{"message":…},"during":"archival","item":"<path>"}` and finally `{"message_type":"exit_error","code":N,"message":…}`.
- `snapshots --json` → a JSON **array** of `{"time","parent"?,"tree","paths":[…],"hostname","username","uid","gid","tags":[…]?,"program_version","summary":{backup_start,backup_end,files_*,dirs_*,data_blobs,tree_blobs,data_added,data_added_packed,total_files_processed,total_bytes_processed},"id","short_id"}`.
  `tags` is omitted when there are none. An empty result is `[]`. In 0.19 the `summary` is stored in the snapshot itself.
  - `--group-by host` → `[{"group_key":{"hostname":"stackA","paths":null,"tags":null},"snapshots":[…]}]`.
  - `--latest 1` → the latest **per group** (2 items here, not 1).
- `ls --json` → the first line is the snapshot (`"message_type":"snapshot","struct_type":"snapshot"`), then one line per node `{"name","type","path","uid","gid","size"?,"mode","permissions","mtime","atime","ctime","inode"?,"message_type":"node","struct_type":"node"}`.
- `restore --json` → `{"message_type":"summary","total_files","files_restored","total_bytes","bytes_restored"}` (counts include directories).
- `forget --dry-run --json` → see 6a. `forget --json <ids>` prints **nothing** on success.
- `tag --add … --json` → `{"message_type":"changed","old_snapshot_id","new_snapshot_id"}` plus `{"message_type":"summary","changed_snapshots":1}`. **Tagging rewrites the snapshot and changes its ID.**
- `stats --json` → `{"total_size","total_file_count","snapshots_count"}`; `--mode raw-data` adds `total_uncompressed_size, compression_ratio, compression_progress, compression_space_saving, total_blob_count`. These have no `message_type`.
- `check --json` → `{"message_type":"summary","num_errors","broken_packs","suggest_repair_index","suggest_prune"}`.
- `find --json` → `[{"matches":[{path,permissions,type,mode,mtime,atime,ctime,uid,gid,size}],"hits","snapshot"}]`.
- `version --json` → `{"message_type":"version","version":"0.19.1","go_version","go_os","go_arch"}`.
- Some warnings are **plain text even with `--json`**, for example forget's `Ignoring "…": no matching ID found` and REST retry lines `Stat(<config/>) returned error, retrying after …`.

**Implication for Angee:** decode line-delimited JSON on stdout (`status`/`summary`) and on
stderr (`error`/`exit_error`), and tolerate non-JSON stderr lines. Commit a member only on a
`summary` with a `snapshot_id` (and no `dry_run`). Never tag after the fact as a commit
mechanism, because it changes IDs. Put every tag on the original `backup` call.

---

## 11. Stable in-snapshot paths for FILE members

Commands (`s11_stable_paths.sh`). Two stack roots, `…/s11-rootA` and `…/s11-rootB/deeper`, with
identical content under `state/snapshots/snap1/databases/app/{app.pgdump,meta.json}` and
`data/media/img/photo.jpg`.

| Case | Stored tree (`ls --json`) | `snapshot.paths` |
|---|---|---|
| (a) `restic backup /abs/…/s11-rootA/state/snapshots/snap1/databases/app` | **full absolute host path**: `/private`, `/private/tmp`, … (16 dirs) …`/snap1/databases/app/app.pgdump` | absolute |
| (b) cwd = `…/snap1`, `restic backup databases/app` | **`/databases`, `/databases/app`, `/databases/app/app.pgdump`, `/databases/app/meta.json`** (relative to cwd) | still absolute: `[…/s11-rootA/state/snapshots/snap1/databases/app]` |
| (b') cwd = `…/snap1`, `restic backup .` | `/databases/…` (cwd contents at the root) | `[…/snap1]` |
| (b'') cwd = `…/snap1/databases/app`, `restic backup .` | `/app.pgdump`, `/meta.json` | `[…/snap1/databases/app]` |
| (b''') cwd = stack root, `restic backup data/media` | `/data/media/img/photo.jpg` | absolute |
| (b4) cwd = `…/state`, `restic backup ../data/media` | `/data/media/img/photo.jpg` (`..` is resolved and the leading `..` dropped) | absolute |

(c) restic 0.19.1 has **no path-rewriting flag** for `backup` (no strip-prefix or "as path" option
for directories; `--stdin-filename` exists only for stdin), and `rewrite` cannot relocate paths
(only exclude/include and metadata). **The tree layout is controlled only by cwd plus relative
arguments.** `snapshot.paths` always records the absolute host path, so it still leaks the host
location as metadata, though not into the tree.

(d) Restoring to a different location works:
- `restore <snap11b>:databases/app --target <newroot>/state/restore-target` → `restore-target/app.pgdump`, `restore-target/meta.json`.
- `restore <snap11b> --target <newroot2>` → `newroot2/databases/app/{app.pgdump,meta.json}` (no host prefix).
- For the absolute snapshot (11a), `restore <snap11a>:<abs subpath> --target X` also works (`X/app.pgdump`), but **Angee has to know the original absolute path**. A plain restore recreates `X/private/tmp/…/s11-rootA/state/snapshots/snap1/databases/app/…`.
- `dump -a tar <snap11b> /databases/app` → `databases/app/app.pgdump`, `databases/app/meta.json`. `dump -a tar <snap11b>:databases/app /` → `app.pgdump`, `meta.json`. For 11a the tar entries carry the whole `private/tmp/…` prefix.

(e) Parent detection and dedup across roots and attempts:
- Parent selection uses `--group-by host,paths` and compares the **absolute** `snapshot.paths`. From rootB
  (different absolute path): `parent = none`, `files_new=2`, so the files are fully re-read. **Dedup still works**:
  `data_added=2048` (tree metadata only).
- A **per-attempt staging dir** (`…/snap2/databases/app`, same root): `paths` differ, so `parent = none` and the
  files are re-read; dedup holds (`data_added=2054`).
- Pitfall: snapshots whose `paths` match but whose layouts differ (11a absolute vs 11b relative vs 11b'' `.`) are
  still chosen as parents. Nothing matches by path, so every file shows as `files_new`. This is harmless for
  dedup but wastes the parent.
- `--group-by host` or an explicit `--parent <id>` from rootB → parent is used, but `files_changed=2`, because inode, ctime and
  (with plain `cp`) mtime differ. With mtime preserved (`touch -r`) plus `--group-by host --ignore-inode`:
  **`files_unmodified=2`**, so the parent fully short-circuits re-reading.

Verdict: a host-independent layout is achievable. Run restic with `cwd` set to the member's base
directory and pass relative arguments. Restore anywhere with `restore <snap>[:<subpath>] --target <dir>`
or `dump -a tar <snap>:<subpath> /`. Absolute arguments embed the host path in the tree and must
be avoided.

**Recommendation for Angee file members:**
1. Run each file-member backup with `cmd.Dir = <member base>` and a *relative* argument that equals the
   in-snapshot path. For staged members, set cwd to `<stack-root>/state/snapshots/<id>` and pass
   `<group>/<name>`. For persist paths, set cwd to `<stack-root>` and pass `data/media`. The tree then holds
   `/<group>/<name>/…` or `/data/media/…`, the same convention as stdin members' `--stdin-filename <group>/<name>/<file>`.
2. Restore with `restore <snap>:<group>/<name> --target <dest>` (or `dump -a tar <snap>:<group>/<name> /`),
   which is independent of the original root. Never decode `snapshot.paths` for restore. Store the
   in-snapshot path in the Angee manifest instead.
3. Per-attempt staging directories defeat parent detection (paths change every attempt). Content
   dedup is unaffected, so the cost is only re-reading and re-hashing staged files, which are freshly
   written anyway. For large stable persist paths (`data/media`), keep cwd and argument stable so
   `host,paths` parent detection works. If Angee needs cross-root moves to stay incremental, pass
   `--parent <previous member id>` (and `--ignore-inode`) explicitly.
4. Do not mix absolute and relative invocations for the same path: they share `paths` but not the layout.
