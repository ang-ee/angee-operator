# Stack snapshots: design and refactor plan

**Status:** Draft v3.2 (P0 inventory and P1 stream role corrected after the third Codex review; §22 lists the findings that gate P3–P7). v3 followed two Codex reviews and added portability across
configurations: Alexis asked on 2026-11-06 that databases, code and workspaces be able to
move between stack-hosted and external Postgres, and between runtimes and architectures.
v3.1 records Alexis's answers to Q3–Q16 (§16).
**Lane:** jj workspace `angee-operator--stack-snapshots` (`~/Work/angee-operator-stack-snapshots`)

**Evidence** (under [`../notes/stack-snapshots/`](../notes/stack-snapshots/)):

| File | What it holds |
|---|---|
| `prior-art.md` | research on about 25 systems |
| `spikes/runtime/RESULTS.md` | R1–R7: job stdin/stdout through Compose and process-compose |
| `spikes/restic/RESULTS.md` | S1–S11: restic 0.19.1 and rest-server 0.14.0 |
| `spikes/restic/s12_copy.md` | S12: `copy` changes snapshot IDs; passwords on file descriptors |
| `spikes/restic/s13_symlinks_and_copy_ids.md` | S13: symlink handling; copy by explicit IDs |
| `spikes/databases/RESULTS.md` | D1–D10: Postgres/pgvector, SQLite, Redis |
| `codex-review.md` | first review: 24 findings on v1 |
| `codex-review-v2.md` | second review: status of those 24 findings against v2.1, plus 12 new findings (B1–B12) |

§21 maps every finding to this version.

## Summary

- **Three owners.**
  - Service templates own the commands that save, verify, check, reset and restore
    their state.
  - restic owns the bytes: repository, encryption, dedup, backends, retention
    arithmetic, integrity and file restore.
  - Docker and process-compose own processes and readiness.
  - Angee owns the order: claims, attempts, quiesce and release, the commit point, the
    restore journal, secrets first, apps last. It dumps no database, writes no archive
    format, runs no scheduler and adds no polling loop.
- **Portability (§5).** A *member* is a logical dataset with a stable key
  (`databases/app`) and an opaque *format* (`postgres-custom/1`). Each configuration
  declares how *it* saves and restores that key. A snapshot taken in one configuration
  restores into another when key and format match:
  - stack-hosted Postgres ↔ external Postgres;
  - container ↔ local runtime;
  - amd64 ↔ arm64;
  - git worktree ↔ jj slot.

  A restore always runs the **target** configuration's jobs.
- **Vocabulary (Q12).**
  - A *snapshot* is a committed set in the stack's snapshot repository.
  - A *backup* is a snapshot copied to a named *backup destination*.
  - "Backup" names nothing else.
- **Coordination (§9).**
  - A *namespace* is claimed by exactly one stack root through claim records in the
    repository.
  - Every create writes an *attempt* record first, and an *abandoned* record when it
    fails.
  - Only abandoned or de-committed attempts are swept.
- **Create (§10).** The order is quiesce → members → **release** → verify → commit
  manifest → receipt. Receipt reconciliation accepts only one exact manifest whose
  request digest and members match.
- **Restore (§11).** Portable data restore runs by default. Running the snapshot's own
  configuration is a separate mode, `--restore-config`. A non-destructive preflight runs
  the template's `check` jobs. A
  journal records an observable postcondition and a replay rule for every step. The
  receipt commits before any aside cleanup.
- **Delivery (§17).** A greenfield rebuild with no backward compatibility; v0.23.0
  backups were never used (Alexis, Q1). P0 removes them outright and keeps only the
  reusable internals, renamed. Every phase after that builds the new feature on a clean
  base.

## 1. Why change v0.23.0

| Problem | Evidence |
|---|---|
| No restore, no init-from-backup; the archive is only a ZIP | `docs/proposals/operator-backup-restore.md` |
| Self-made archive, verifier and cleanup (`internal/service/backups.go`, 1,315 lines) | prior-art §restic |
| Plaintext `recovery/secrets.env` (`backups.go:936–959`); no retention, remote or encryption | first review, verified claims |
| The template's `pg_dump -Fc --no-owner --no-acl`: owners stay in the dump and GRANTs are lost | D1 |
| `pg_restore --list` passes corrupted archives | D2 |
| A live per-file copy of SQLite WAL corrupts the copy; a stale `-wal` on restore silently reverts data | D7, D8 |
| Redis with AOF on ignores a restored `dump.rdb`, then overwrites it | D9 |
| Captures are tied to one configuration's jobs: nothing can move between stack-hosted and external Postgres | Alexis, 2026-11-06 |

## 2. Facts that shape the design

**Runtimes**
- **R1.** `docker compose run -T` stdout is byte-identical, and compose's own output goes
  to stderr.
- **R2, R3.** Today's `-f -` hands a restore job empty input, and it exits 0. Moving the
  YAML to `-f /dev/fd/3` (`ExtraFiles`) with `--project-directory <root>` works on
  macOS and Linux.
- **R4.** process-compose writes a title escape at the start of stdout. A job writing to
  fd 3 avoids it; the injected shim is not yet proven.
- **R5.** process-compose forwards stdin. A TTY plus a separate process group stops the
  job.
- **R6.**
  - Compose: when the consumer dies, the container keeps running.
  - process-compose: SIGKILL orphans the job.
  - Both: a broken restore source looks like EOF.
- **R7.** Saving with the default `json-file` log driver is 6× slower and copies the dump
  into docker's logs.

**restic 0.19.1**
- **S1.** Nested `--stdin-filename` paths work, but restic silently cleans `..`.
- **S2.** One snapshot per stream is forced.
- **S3.** A failed producer piped into `--stdin` still yields a snapshot and exit 0. An
  empty stream exits 3. Failed runs leave orphan packs.
- **S4.** Files changing mid-read are stored torn, with exit 0.
- **S5.** About 13 requests and 1–2 s per call on a remote backend.
- **S6.** `--tag a,b` is AND. Forgetting an unknown ID exits 0.
- **S7.** Backups run concurrently. `forget` and `prune` take an exclusive lock. A
  `kill -9` leaves a stale lock.
- **S8.** Exit codes 10, 11, 12 and 130. An unreachable REST server is retried for 892 s.
  A password on an inherited fd works.
- **S9.** `dump -a` takes one snapshot per call.
- **S10.** Retagging changes the snapshot ID.
- **S11.** No path rewrite exists; snapshot paths come from the working directory plus
  relative arguments.
- **S12.** `copy` gives new IDs with `original` set. Two passwords can come in on fd 3
  and fd 4. A tag-filtered member-only copy works.
- **S13.** Symlinks inside a tree are stored as links. **An ancestor component that is a
  symlink is followed**, so content from outside the root was captured. `copy <id>` copies
  exactly that ID.

**Databases**
- **D3.** Only `pg_restore --single-transaction` rolls back cleanly.
- **D4.** A stdin restore into the image-created database with
  `--no-owner --no-acl --single-transaction --exit-on-error` works, including
  `CREATE EXTENSION vector`. Restoring into a non-empty database fails.
- **D5.** A killed `pg_dump` piped in is saved, truncated, with exit 0. `-Z0` is 3×
  faster.
- **D6.** Restore runs forward only across Postgres majors.
- **D7.** `VACUUM INTO` is consistent.
- **D10.** Redis loads with AOF off, then `CONFIG SET appendonly yes`.

**Current code**
- `run_on` is validated (`manifest.go:1068`) and never executed.
- `applyService` skips dependencies (`jobs.go:924`).
- `waitServiceReady` polls every 250 ms (`jobs.go:936`).
- `removeCanceledJob` re-runs `docker rm -f` on a ticker (`compose/backend.go:239–265`).
- Allocated ports live in `ports.<n>.value` (`manifest.go:173`) plus `port_leases`
  (`manifest.go:74`), and are allocated by `allocateServicePorts` /
  `allocateWorkspacePorts` (`service_create.go:355`), not by `StackPrepare`.
- The answers file sits at the render target, which can be above `ANGEE_ROOT`
  (`stack_update.go:323–328`, `locateStackAnswers`).
- `BackupIdentity` and `BackupEvidence` live in `internal/vcs/jjdriver/backup.go`.

## 3. Ownership

| Concern | Owner | Angee's part |
|---|---|---|
| Save, verify, check, reset and restore of a dataset in this configuration | the template's jobs | runs them in order, with the right channel and precondition |
| Format of an artifact | the template (an opaque format string, §5.2) | compares formats for equality only |
| Bytes, encryption, dedup, backends, retention arithmetic, integrity, file restore | restic | typed arguments, exact IDs, redacted owner errors |
| Container lifecycle and readiness | Docker (`compose up --wait`, `run --rm`) | starts, cancels, removes once |
| Local process lifecycle and readiness | process-compose (its state stream) | as above |
| Which paths are owned, live-copied or excluded | the manifest | enforces it (§6, §10 step 1) |
| External databases | their operator (a DBA, a managed service) | never drops one; only the template's `reset` job runs, and only with `--force` |
| Source revisions and checkouts | vcs drivers | asks them to materialise in the target's slot mode |
| Secrets storage | the secrets backend (`Set`) | writes recovered values by name |
| Template answers and render target | copier, via `locateStackAnswers` | restores through those rules |
| Ports | the port allocator (`internal/ports`, `allocateServicePorts`) | re-allocates on init |
| Scheduling, catalog UI, who may restore what | Django or cron | admin-bearer API only |

## 4. Terms, commands and vocabulary

| Word | Meaning |
|---|---|
| **snapshot** | a committed, immutable stack snapshot in a repository |
| **snapshot repository** | the stack's primary restic repository, usually on-host |
| **member** | one tagged restic snapshot that is part of a stack snapshot, identified by a key such as `databases/app` |
| **format** | an opaque string naming a member's artifact contract, such as `postgres-custom/1` |
| **namespace** | a stack's partition of a repository; restic `--host`; claimed by one root |
| **attempt** | one create, identified by its `request_id` |
| **backup destination** | a named restic repository that receives copies |
| **backup** | a snapshot copied to a backup destination |
| **restore** | rebuilding state from a snapshot, wherever it is read from |

restic's own `restic backup` command name appears only inside the adapter.

| Command | Effect |
|---|---|
| `angee snapshot init [--backup <dest>]` | create the snapshot repository (or a destination), claim the namespace, print an offline recovery key |
| `angee snapshot preview [--select …]` | members and status (included, excluded, unsupported, refused); complete or partial |
| `angee snapshot create [--select …] [--hold svc,…]` | §10 |
| `angee snapshot list [--at <dest>]`, `show <id>` | committed snapshots of this namespace |
| `angee snapshot restore <id> --force [--from <dest>] [--only k,…] [--map old=new] [--partial] [--keep-old] [--restore-config]` | in place, §11 |
| `angee snapshot restore --resume\|--rollback\|--cleanup <restore-id>` | journal operations |
| `angee init [path] --from-snapshot <repo>#<id> --restore-config` | new stack with the **snapshot's** configuration |
| `angee init [path] --template <t> [--input k=v …] [--answers-from-snapshot] --from-snapshot <repo>#<id>` | new stack with a **new** configuration rendered by copier, data restored by key and format |
| `angee snapshot forget <id>… \| --policy [--at <dest>] \| --attempt <id>` | §12 |
| `angee snapshot prune [--at <dest>]`, `check [--read-data-subset N%] [--at <dest>]` | §12 |
| `angee snapshot backup [<id>… \| --latest] [--to <dest>…]` | §13 |
| `angee snapshot export <id> --plain <dir>` | decrypted members into a directory, flagged sensitive |

## 5. Portability model

**5.1 Member identity.**
- Keys are `<group>/<name>`. Templates declare `databases/…` and `service_state/…`.
  Angee reserves `stack/…`, `storage/…`, `workspaces/…` and `sources/…`.
- A key names *what* the data is, not *where* it lives. Config A (stack-hosted Postgres)
  and config B (external Postgres) both declare `databases/app`.
- `--map old=new` renames keys at restore.

**5.2 Format contract.**
- A member declares `format: <name>/<n>`, an opaque string that Angee only compares for
  equality. Angee stays kind-free and never learns "postgres".
- The format promises the artifact's channel (stream, or files with fixed names) and
  what a matching restore expects.
- Conventional names, documented and not enforced: `postgres-custom/1` (pg_dump custom
  format), `mysql-sql/1`, `sqlite-db/1` (a file produced by `VACUUM INTO`),
  `redis-rdb/1`, `files/1`.
- Formats must be **logical**. A physical database directory is tied to engine version,
  CPU architecture and configuration, so it is never a portable format. Angee keeps
  refusing raw `pgdata` copies.

**5.3 Where a member lives in this configuration: `restore.requires`.**

| Value | State lives in | Angee's precondition | Emptiness comes from |
|---|---|---|---|
| `stopped` | files under persist paths owned by a hosting `service` | service down | owned paths moved aside |
| `running` | a server the stack hosts (`service`) | service started on empty owned paths, ready through its runtime | owned paths moved aside, then the image initialises them |
| `external` | outside the stack (managed or shared server) | none; reachable per the `check` job | the template's `reset` job, run only with `--force` |

**5.4 Compatibility at restore.** All of this runs before any change. For each snapshot
member selected:
1. The (mapped) key must be declared in the target configuration.
2. The formats must be equal.
3. The target's optional `check` job, fed the member on stdin, must exit 0. This is
   where the template refuses an older server, for example archive version 17 into a
   16 server (D6).
4. Target members missing from the snapshot need `--partial`. Snapshot members missing
   from the target need `--only` or are reported.

Restore runs the **target's** restore jobs, so no executable config travels with the
data (trust, §11).

**5.5 What moves how.**

| Move | Mechanism |
|---|---|
| Stack Postgres → external Postgres | Source: `databases/app` with `requires: running`. Target declares the same key and format with `requires: external` and a `reset`. The target streams into the external server |
| External → stack Postgres | Target `requires: running`: start `postgres` on empty `pgdata`, wait for Docker's readiness, stream |
| Container ↔ local runtime | Each configuration's jobs carry their own runtime. Files members restore into the target's persist paths |
| amd64 ↔ arm64 | Logical formats are portable (dumps, SQLite files, RDB). Images resolve per platform from the target config |
| git worktree ↔ jj slot | `sources/<id>` members record revisions (git commit; jj change and commit IDs) and remotes. The target's driver materialises the revision in **its** slot mode. v1 covers `fetch_published`; P10 adds portable source bundles (§5.6) |
| A workspace to another stack | `workspaces/<name>` restores into the target's workspace of that name, outside its slots. Slots come from `sources/…` members |
| Secrets | By name: every secret the target declares and the snapshot holds is written, so SECRET_KEY is preserved. Others are generated as usual. Snapshot secrets the target doesn't declare are reported, not written |

**5.6 Code with uncommitted work (P10).**
- v0.23.0 copies raw `.git` and `.jj` metadata. That is not portable and needs jj
  pointer remapping.
- P10 instead makes the drivers produce a **portable source bundle**: a `git bundle` of
  commits the remotes lack (git owns that format), plus an overlay of uncommitted and
  untracked files relative to the checkout.
- Restore lets the target driver create its own checkout (worktree or jj) at the
  revision, fetch the bundle, then apply the overlay.
- The jj operation log does not travel; that is documented.

**5.7 What does not move:** physical database directories, named volumes, undeclared
state, process memory.

## 6. Manifest schema

```yaml
snapshots:
  namespace: shop-7f3a91c2            # written by `snapshot init`; restic --host
  repository: "${secret.snapshot_repository}"
  password: snapshot_password         # a declared secret; restic reads it from fd 3
  env: {}                             # backend credentials; restic child env only
  hold: [web, worker]                 # quiesced for the whole create
  after_restore: [migrate]            # idempotent jobs, after data, before apps
  retention: {keep_last: 3, keep_daily: 7, keep_weekly: 4, keep_monthly: 6}
  backups:
    offsite:
      repository: "${secret.offsite_repository}"
      password: offsite_password
      env: {B2_ACCOUNT_ID: "${secret.offsite_b2_id}", B2_ACCOUNT_KEY: "${secret.offsite_b2_key}"}
      retention: {keep_daily: 30, keep_monthly: 12}
  members:
    databases/app:                    # config A: the stack hosts Postgres
      format: postgres-custom/1
      service: postgres
      owns: [pgdata]
      save:    {job: db-save, stream: app.pgdump}
      verify:  {job: db-verify}       # required for stream members
      check:   {job: db-check}        # optional target-side probe at restore preflight
      restore: {job: db-restore, requires: running}
    service_state/app-db:
      format: sqlite-db/1
      service: app
      owns: [appdb]
      save:    {job: app-db-save, files: [app.db]}
      verify:  {job: app-db-verify}
      restore: {job: app-db-restore, requires: stopped}

persist:
  pgdata:  {subpath: ./data/pgdata, scope: stack}
  appdb:   {subpath: ./data/app, scope: stack}
  media:   {subpath: ./data/media, scope: stack}                   # unowned: copied live, warned if a running service can write it
  scratch: {subpath: ./tmp, scope: stack, snapshot: exclude}
```

The same key in config B (external Postgres):

```yaml
  members:
    databases/app:
      format: postgres-custom/1
      save:    {job: db-save, stream: app.pgdump}
      verify:  {job: db-verify}
      check:   {job: db-check}
      reset:   {job: db-reset}        # required for external: empties the target database
      restore: {job: db-restore, requires: external}
```

**Rules (strict loader):**
- **Patterns:**
  - `namespace`: `[a-z0-9][a-z0-9-]{0,62}`;
  - member keys: `(databases|service_state)/[a-z0-9][a-z0-9_-]{0,62}`;
  - `format`: `[a-z0-9][a-z0-9.-]{0,62}/[0-9]{1,4}`;
  - destination names: `[a-z0-9][a-z0-9-]{0,31}`.
- **`requires`:** `stopped` and `running` require `service`. `external` forbids `service`
  and `owns`, and requires `reset`.
- **Required jobs:** `save`, `verify` (for stream members) and `restore` are required.
  `check` is optional. `reset` is required for `external` and forbidden otherwise.
- **Channels:** `save` takes exactly one of `stream: <file>` or `files: [<file>…]`, and
  verify, check and restore use the same channel. File names follow today's capture-name
  validation (no `/`, `..` or leading dot).
- **Snapshot jobs:** `save`, `verify`, `check`, `reset`, `restore` and `after_restore` jobs
  are snapshot jobs. Every job entry point refuses them, and they cannot declare `run_on`
  or `holds` or be a dependency. `after_restore` jobs must be idempotent; this is a
  documented contract, because resume reruns them.
- **`owns`:** names declared persist entries. Owned paths must not overlap or nest, and
  each has one owner.
- **`persist.<n>.snapshot`:**
  - `exclude` skips the path.
  - Unset means copy. If a service that is not held mounts the path writable, the copy
    still happens, with a warning in preview, the receipt and the manifest (Q3).
  - A `pgdata` with no owner stays refused, as today.
- **Backup destinations:** each has its own `repository`, a `password` that differs from
  the snapshot repository's, and optional `env` and `retention`. A destination must not
  equal the snapshot repository. Destinations are **named copies**; Angee does not prove
  they are off-host (Q6).
- **No legacy fields:** `jobs.*.capture` and `persist.*.backup` are deleted in P0. The
  strict loader rejects them like any unknown field, with no migration message.

**Job inputs:** files saves get `${inputs.snapshot_output}` (stack-relative) and
`${inputs.snapshot_output_absolute}`. Files verify and restore get
`${inputs.snapshot_input}` and `${inputs.snapshot_input_absolute}`. Streams use
stdin/stdout. `reset` gets nothing.

## 7. Runtime: streaming runner, cleanup and readiness

**Contract.**
- Saves write the artifact to stdout (stream) or to the declared files.
- Verify, check and restore read it from stdin or `snapshot_input`.
- Stderr is diagnostics, redacted into the receipt. Stdout never reaches the receipt.
- Exit 0 is required but proves only transport; formats are proved by `verify`.

**Seam (B18).** `runtime.StreamJobBackend` adds
`RunJobStreams(ctx, target, job, JobStreams{Role; Stdin io.Reader; Stdout io.Writer}) ([]byte, error)`.
`Role` is `save`, `verify`, `check`, `reset` or `restore`. The backend derives
`logging: none` (compose) and the fd-3 stdout routing (process-compose) from
`Role == save`, so P1 never has to guess a job's purpose. P4 sets the role from the
member declaration (third review, finding 11). `JobBackend.RunJob` and the compose
`RunInput` stay for every existing path.

**Compose** (`internal/runtime/compose`):
1. **Invocation.** A new `StreamRunner` runs
   `docker compose -f /dev/fd/3 --project-directory <root> [--env-file F] run --rm --no-deps -T --name <c> <job>`.
   The YAML goes on `ExtraFiles[0]`; stdin and stdout come from `JobStreams`; stderr goes
   to diagnostics.
2. **No-poll cleanup (B11), as built in P1 after two reviews.**
   - **Process group.** The docker client runs in its own process group, which also
     holds the compose plugin. On cancel the group is **SIGKILLed at once**: the client
     could miss SIGINT during startup and keep streaming for a whole `WaitDelay`.
     Docker owns the container's cleanup through the removal below.
   - **Event subscription.** Before the client starts, Angee subscribes to Docker's own
     events for this job's unique container name:
     `docker events --filter type=container --filter container=<c>`. The subscription
     replays from `--since` one minute back, to tolerate clock skew, and is stopped on
     every path.
   - **Removal.** After any non-success exit, Angee makes **exactly one**
     `docker rm -f -v <c>` call with a fresh, bounded context. `-v` matters: our removal
     wins the race against `run --rm`'s auto-remove, so without it anonymous volumes
     would leak. "No such container" and "removal already in progress" count as gone.
     - Only a **killed** client can leave a create in flight. In that case Angee waits
       a bounded window on the event stream for a `create` event before removing, then
       waits a bounded time for the `destroy` event.
     - A client that exited on its own had every create answered, so the removal
       happens at once with no window.
     - If the event stream ends early, the removal still happens, and the receipt
       reports that it could not be confirmed.
   - **What is reported.** A container whose removal was not confirmed, or a create seen
     after the window, is reported in the receipt and never re-polled. This replaces
     `removeCanceledJob`'s ticker for all transient jobs.
3. **Logs.** The compose model gains `Logging`. Save jobs compile with
   `logging: {driver: none}` (R7). Roles are validated, so a mis-tagged save can't skip
   this.

**process-compose** (`internal/runtime/proccompose`):
1. `cmd.Stdin` is set from `JobStreams`, or `/dev/null`; never a TTY.
2. **Job stdout through fd 3.** Whenever `JobStreams.Stdout` is set, the job's stdout
   goes through fd 3, with the command compiled as `exec 1>&3 3>&-; <command>`. This
   gives one `Stdout` contract across both backends.
   - **Proof.** The end-to-end proof (binary payload, early consumer death, producer
     failure, cancel) runs in CI: the ubuntu job installs
     `github.com/f1bonacc1/process-compose@v1.122.0` and sets
     `ANGEE_REQUIRE_PROCESS_COMPOSE=1`. That allows local stream saves.
   - **Bounded copy.** After the process exits, the fd-3 copy has an **idle** bound: the
     read deadline is re-armed before each read. Survivors in the group are then
     killed. A background child holding fd 3 can't hang a save, and a slow consumer
     (restic stalling on a remote repository) can't fail a complete one.
3. Cancel sends SIGINT to the isolated group, and SIGKILL to the group after `WaitDelay`.
   A SIGKILLed operator can still orphan a job; the docs say so.
4. Windows keeps refusing transient local jobs.

**Readiness for `requires: running` (B11, #7).** Snapshot code never calls
`waitServiceReady`.
- **Container services:** `docker compose up -d --wait --wait-timeout <s> --no-deps <svc>`.
  Docker owns readiness, and its error surfaces as-is.
- **Local services:** readiness comes from process-compose's pushed state stream, which
  `owner-pushed-live-views.md` delivers. Until that lands, preflight refuses local
  `running` members.

Dependencies start explicitly in topological order (§11 step 10), so `--no-deps` is
correct.

## 8. restic adapter (`internal/restic`)

- **Runner.** An exec runner with `--json`, typed structs that tolerate unknown fields
  and non-JSON stderr lines, and fixtures from the spikes. The floor is 0.19.1
  (`restic version --json`).
- **Exposure (#20).**

  | Item | Where it is | Where it never is |
  |---|---|---|
  | Repository password | fd 3 (`RESTIC_PASSWORD_FILE=/dev/fd/3`) | argv, env, disk |
  | Source password for `copy` | fd 4 (`--from-password-file /dev/fd/4`, S12) | argv, env, disk |
  | Repository URL | child `RESTIC_REPOSITORY` env, redacted in every error, log and receipt | argv |
  | Backend credentials | child env only | Angee's env, receipts |
  | Snapshot job env | as for any job today (process-compose 0600 temp config, `run/secrets.env`) | receipts |
  | Files-mode staging | plaintext in a 0700 per-attempt directory, removed after; documented | stream members (never on disk) |

  P4 adds a secret-injection test across every diagnostic path: receipt output and error,
  logs, REST error bodies, and GraphQL.
- **Deadlines.**
  - Metadata calls get 5 minutes.
  - Data calls follow `ANGEE_JOB_TIMEOUT`, where `0` means none, as for jobs (Q15).
  - Locking calls pass `--retry-lock 5m`.
- **Errors (#22).** A stable Angee code plus restic's own message (redacted) plus a hint:

  | restic exit | Angee code |
  |---|---|
  | 10 | `NOT_FOUND` |
  | 11 | `CONFLICT` |
  | 12 | `INVALID_INPUT` |
  | 3 | write failure |
  | 130 | interrupted |
  | other | failure |
- **Paths.** Each file member runs with `cmd.Dir` at its base and relative arguments
  (S11). `--parent <id>` is the previous committed snapshot's member of the same key and
  base.
- **Operations.**
  - Repository: `Version`, `Init`, `KeyAdd`, `CatConfig`.
  - Writes: `BackupStdin`, `BackupPaths`.
  - Reads: `Snapshots`, `Ls`, `Dump`, `Restore`.
  - Lifecycle: `ForgetDryRunPolicy`, `Forget(ids)`, `Prune`, `Check`, `Copy(ids)`, and
    `Unlock` (stale locks only).
- **Distribution.** `angee doctor` reports `tool.restic`. The process-compose-style
  prompt offers `go install github.com/restic/restic/cmd/restic@v0.19.1`.
  `Dockerfile.operator` copies the binary from `restic/restic:0.19.1`, pinned by digest.

## 9. Coordination: namespace, claims and attempts

This section answers B2, #3 and #17.

- **Root id.** A random 128-bit id in `state/snapshots/root-id` (0600), created on first
  use. `init` never copies it.
- **Claim.**
  - `snapshot init` writes a claim member (`angee.kind=claim`, tags `angee.ns`,
    `angee.root=<root-id>`, plus hostname and root path).
  - The namespace's **effective claim** is its newest claim member.
  - Every writing operation (create, forget, sweep, prune, backup) first reads the
    effective claim, and refuses if it belongs to another root: "namespace owned by root
    … at host …".
- **Takeover.** `init --from-snapshot` claims a **new** namespace. Only
  `--take-over-namespace`, for disaster recovery once the old root is gone, writes a new
  claim for the snapshot's namespace. The old root then refuses to write. Takeover is
  explicit and manual by design; two roots never share a namespace by default.
- **Attempt record.** A create's first repository write is an attempt member
  (`angee.kind=attempt`, `angee.snapshot=<request_id>`, root id, request digest).
- **Abandoned record.** When a create fails in-process, or when the same root's receipt
  recovery finds a non-terminal attempt (that root holds its root lock, so the attempt is
  provably dead), the owner writes an abandoned member (`angee.kind=abandoned`). This is
  the durable proof of inactivity.
- **Sweep.** Only members of attempts that are abandoned, or de-committed by forget, are
  collected. An **unknown** attempt (an attempt record with no manifest and no abandoned
  record) is listed by `forget --attempt --list`, and removed only by an explicit
  `forget --attempt <id>`. No age heuristics are used.

## 10. `snapshot create`

The mutation lease and `run/operator.lock` are held throughout. The receipt is a
`JobRunOperation`, kind `SNAPSHOT`, admin-only, never `latestJobRun`, idempotent by
`request_id` plus selection digest. Members carry the tags `angee.ns`, `angee.snapshot`,
`angee.kind` and `angee.member` and run with `--host <ns>`.

0. **Preflight.** Check the claim is this root's, restic is at the floor, and
   `cat config` succeeds. Then reconcile (B3): look for manifests tagged with this
   namespace and `request_id`.
   - **Exactly one**, whose request digest and root id match the receipt and whose
     member set is complete: mark the receipt succeeded.
   - **Any other match:** `CONFLICT`, as `backupReceiptFor` does today.
   - **None:** continue.
1. **Discover and close the selection.**
   - Unsupported resources are refused.
   - A deselected member leaves its owned paths uncaptured, so the snapshot is
     **partial**.
   - Every unowned persist path that a service which is not held mounts writable is
     **copied live with a warning** (Q3). The warning goes into preview, the receipt and
     the manifest (`live_copies: [{path, writers}]`). `--strict` turns these warnings
     into refusals.
   - **Confinement (security, not consistency).** Every **container** service with a
     writable mount on a strict ancestor of a captured path must be in the hold set, or
     the create is refused (S13, B5). Otherwise a swapped parent directory could pull
     files from outside the stack into the snapshot. Local services run as the operator's
     own user and share its trust domain; documented.
2. **Attempt record** (§9).
3. **Quiesce.** Record the prior state of every hold service in the receipt, then stop
   the running ones.
4. **Secrets member.** Read the declared values and stream them into
   `stack/secrets.env`; nothing is staged on disk.
5. **Stream members** (container runtime; local after Q2 clears).
   - `RunJobStreams` pipes stdout into
     `restic backup --stdin --stdin-filename <key>/<file>`.
   - Transport checks: the job exits 0, restic exits 0, no copy error, bytes > 0, and
     restic's processed bytes equal Angee's count.
   - Record bytes and sha256.
6. **Files-mode members.**
   - Each save writes into a new per-attempt directory,
     `state/snapshots/attempts/<id>/<key>`. It is created by `os.Root.Mkdir(…, 0700)`
     under a 0700 parent; an existing path fails (#23).
   - The directory must hold exactly the declared names, as non-empty regular files.
   - Then `restic backup` runs with `cmd.Dir` at the attempt directory and relative key
     arguments.
   - `restic ls --json` must equal that set.
7. **Stack file trees, by base** (#6, #11, B5).
   - **Bases:**
     - the stack root: `angee.yaml`, unowned and non-excluded persist paths, and
       workspace files outside slots;
     - the answers render target, when above the root: the answers file only;
     - each source store, for bundle policy, until P10 replaces it.
   - **Walk.** The confined walk (`os.Root`, `backups.go:480–645`) produces the include
     list. It Lstats every ancestor component of each captured path, which must be a
     real directory, never a symlink, and records `(dev, inode)` per component and
     `(size, mtime, ctime, inode)` per file.
   - **Run.** `restic backup --files-from-raw` from `cmd.Dir=<base>`.
   - **After the run:**
     - every ancestor component must be unchanged (same `(dev, inode)`, still a
       directory), or the create fails;
     - `restic ls --json` must match the include list, apart from files deleted
       mid-run;
     - per-file metadata changes are **warnings**, recorded in the manifest as
       `changed_during_copy`, and shown by `show` and the receipt. `--strict` makes them
       fail the create.

     This content check is advisory, not a consistency guarantee (Q3). Confinement rests
     on the hold rule in step 1 plus the ancestor checks (S13), and a confinement
     violation always fails.
   - The manifest records base roles (`stack-root`, `answers-target`, `source:<id>`),
     never host paths.
8. **Release (B1, #8).** Restart exactly the hold services that were running, through a
   fresh `context.WithoutCancel` with its own timeout, and record the outcome. If release
   fails, write the abandoned record and fail the receipt, naming the services and the
   recovery command (`angee service start …`). **Nothing is committed.**
9. **Verify gate (#9).** Every stream member's `verify` job, and declared files `verify`
   jobs, read the member back from restic (`dump`, or `restore` into an attempt input
   directory). Any failure leads to the abandoned record and a failed receipt.
10. **Source evidence.** `verifyBackupSource` (renamed `verifySnapshotSource` in P0)
    re-checks revisions.
11. **Commit.** The manifest member (`stack/snapshot.json`, `angee.kind=manifest`)
    holds:
    - manifest version 2, the namespace, root id and request digest;
    - stack name and angee version, start and end times, and `complete`;
    - the quiesce and release record;
    - per member: key, format, mode, in-snapshot path, **exact restic ID**, bytes,
      sha256 and `verified`, plus the job names, `requires`, and the resolved image
      reference and digest;
    - base roles, source evidence, secret names (not values), and the template ref and
      answers relation.
12. **Receipt.** The succeeded receipt is the last durable act. Recovery after a crash
    between 11 and 12 reconciles exactly as in step 0.
13. **Cleanup.** Remove the attempt directory.

**Invariants (#24):**

| Interrupted at | State left | Recovery |
|---|---|---|
| 0–2 | at most an attempt record | the same root's receipt recovery writes the abandoned record |
| 3–7 | members, services stopped | receipt recovery restarts the recorded hold services, writes the abandoned record, and fails the receipt |
| 8 | partly released | as above; the receipt names any service that could not be restarted |
| 9–10 | members complete, no manifest | abandoned and failed |
| 11 | manifest written | reconciliation marks the receipt succeeded |
| 12–13 | succeeded | the next create removes leftover attempt directories |

## 11. Restore

**Modes and trust (#4, B6).**
- **Portable data restore** is the default, in place and for `init --template`. Data is
  restored by key and format (§5.4) using the **target's installed jobs**, so no
  executable config comes from the snapshot. There is no whole-config fingerprint:
  per-member key, format and `check` replace it, because portability *requires*
  configurations to differ.
- **Config restore** (`--restore-config`, in place or with `init --from-snapshot`)
  writes the archived `angee.yaml` and answers, then runs a portable restore under them.
  This executes jobs taken from the snapshot, so it is never a default: `--yes` does not
  imply it, and `init --from-snapshot` without `--template` requires it. Before
  proceeding it prints the archived jobs and images.
- **Data from another repository or namespace.** Naming the source (`--from-snapshot`,
  `--from`) is the consent; no further flag is needed. The docs warn that restoring a
  database dump runs SQL chosen by the dump's author in the target database (for
  example through `pg_restore`), so only restore snapshots you trust.
- Member keys, tags, IDs and stored paths are validated against the manifest on read:
  no `..`, no absolute paths, exact IDs, and one member per key.

**Preflight (non-destructive, #5).** All of the following, then nothing changes until
every item passes:
- the manifest is exact and complete, unless `--partial` or `--only`;
- members resolve by exact ID (or by `original` from a destination) and match their
  sha256;
- key and format compatibility holds (§5.4);
- every `check` job passes against the member;
- the target images are present or pullable;
- `fetch_published` revisions are reachable;
- the target secrets backend is reachable and isolated;
- the answers render target has no collision;
- ports can be allocated;
- local `running` members are refused until pushed readiness exists (§7);
- external targets are listed and require `--force`;
- for in-place restores, a **complete committed** safety snapshot exists (§10),
  tagged `angee.reason=pre-restore`.

**Journal (#12, B4).** `state/snapshots/restores/<restore-id>/journal.json`. Each step
records its intent before it runs, its observable postcondition after, and its undo
material.

| Step | Done when (observable) | Replay rule | Undo |
|---|---|---|---|
| aside-move | source absent and aside present | done: skip. Both present: **stop** for manual recovery | move back |
| config-write (`angee.yaml`, answers) | file sha256 equals the journal's | rewrite | restore from aside |
| secrets-backend-override | the target manifest's `secrets_backend` equals the journal's | rewrite | config undo |
| ports-remap | the manifest's `ports.*.value` and `port_leases` equal the journal's | rewrite | config undo |
| secret-write | the backend value's sha256 equals the journal's | `Set` again | `Set` from the safety snapshot's secrets member |
| path-place | staging absent and target entries equal to the member (`restic ls`) | restore into fresh staging, place again | move to aside |
| service-start | runtime reports running or healthy | start again (idempotent) | stop |
| member-restore job | **not observable** | owned paths: move the partial state aside again and rerun. External: run `reset`, then rerun. Neither available: **stop** for manual recovery | aside or `reset`, then restore the safety snapshot's member |
| after-restore jobs | not observable | rerun (idempotent by contract) | none |
| commit | receipt succeeded | none | none |
| aside-cleanup | asides absent | delete again | none (after commit) |

- `--resume` replays forward with these rules.
- `--rollback` applies the undos in reverse, and treats a step whose state is ambiguous
  as "stop".
- The receipt is reconciled from the journal on read. Undo material (asides, safety
  snapshot) is kept until the **commit** step. Aside cleanup runs *after* commit and is
  itself resumable (`--cleanup`).

**Pipeline:**
1. Preflight (plus the safety snapshot, in place).
2. **Down.** In place: `angee down`. Init: the root must be empty, or `--force`.
3. **Config.**
   - **`init --template`:** copier renders the template with `--input`. With
     `--answers-from-snapshot`, the archived answers file is passed as copier's answers
     and the inputs override it; copier owns the merge.
   - **Config restore:** the archived files.
   - **Then:**
     - **Secrets backend override (B7, #13).** `--secrets-backend-file` replaces
       `secrets_backend` in the target manifest before anything renders. If its address,
       mount or path equals the snapshot's, it is refused unless the namespace was taken
       over.
     - **Port remap (B8, #14).** Clear `port_leases` and every `ports.*.value`, then
       allocate through the existing allocator (`allocateServicePorts` and
       `allocateWorkspacePorts`, generalised to a stack-wide allocate) before the first
       render.
     - **Answers.** Written through an `os.Root` opened at the render target, using
       `locateStackAnswers` rules. The render target is a journal participant.
4. **Aside.** Move the target's owned paths, and selected unowned persist and workspace
   paths, to `restores/<id>/aside/…` (same filesystem, `os.Root` rename).
5. **Secrets.** `Set` every target-declared secret present in the snapshot. Missing ones
   are generated as usual. Undeclared snapshot secrets are reported.
   - Only declared secrets are recovery data. Preview lists undeclared env-file keys as
     not captured, so a value that matters must be declared (B7).
6. **Files.** `restic restore <exact-id>:<path> --target restores/<id>/stage/<base-role>/…`,
   then confined moves into the targets. Single files are handled the same way.
7. **Sources.** The target driver materialises each `fetch_published` revision in the
   target's slot mode. Bundle-captured sources are refused until P10.
8. **Render** with every service down. `run_on` is inert, so nothing else starts.
9. **`stopped` members**, in topological order of the target's dependency graph.
10. **`running` and `external` members**, in topological order.
    - `running`: start the service and its already-restored dependencies with
      `--no-deps` (§7), wait through the owner, then stream `restic dump` into the
      restore job while re-hashing.
    - `external`: run `reset` (`--force`), then stream.
    - A member fails on job failure, a dump or copy error, or a hash or byte mismatch.
    - Restores fail closed, with explicit timeouts.
11. **After restore.** Run optional `verify` jobs against the restored state where the
    template supports it, then the `snapshots.after_restore` jobs.
12. **Up.** Stop the data services, then `angee up`.
13. **Commit:** the receipt, with the source snapshot as provenance.
14. **Cleanup:** remove asides unless `--keep-old`, and close the journal.

## 12. Retention, sweep, prune and check

Every writing operation passes the claim check (§9).

**`forget --policy [--at <dest>]`:**
1. `restic forget --dry-run --json --host <ns> --tag angee.kind=manifest` plus the
   policy gives the manifests to remove.
2. **De-commit:** forget those manifests by exact ID.
3. Forget their exact member IDs, as read from each manifest before step 2.
4. Re-query to verify none remain (S6).

**Other operations:**
- `forget <id>…` and `forget --attempt <id>` work the same way.
- The **sweep** collects only abandoned attempts (§9).
- `prune` is its own operation, with `--retry-lock`. Stale locks are cleared with
  `restic unlock` only.
- `check` results are health evidence, not verification.

## 13. Backups to destinations, and plain export

**`snapshot init --backup <dest>`** initialises the destination with
`--copy-chunker-params --from-repo <snapshot repository>` and writes this root's claim
there too.

**`snapshot backup [<id>… | --latest] [--to <dest>…]`** (B9, #16):
1. Read and validate the **source** manifest's exact member IDs: one per key, sha256
   recorded, set complete.
2. `restic copy --from-repo <src> <member ids…>`, with passwords on fd 3 and fd 4 (S12,
   S13).
3. In the destination, verify **exactly one** snapshot per member whose `original`
   equals the source ID, with matching tags and paths.
4. Copy the manifest by its exact ID, last.
5. A rerun validates whatever exists: a complete set is a no-op; members without a
   manifest are reused when they validate, and the missing ones are copied; anything
   conflicting is reported and left alone.

**Reading from and managing a destination:**
- Restore reads the manifest's member IDs, resolved through `original` in the
  destination.
- Destination retention uses the same de-commit-first procedure. Prune and check run per
  repository.
- `list --at`, `restore --from` and `init --from-snapshot <dest-repo>#<id>` read
  destinations. That is the disaster-recovery path.

**Off-host is recommended, not enforced (B10).** `angee doctor` warns when a destination
is a local path on the same filesystem as the snapshot repository, and when a stack has
no destination at all.

**`export --plain <dir>`** restores every member decrypted, flagged sensitive. There is
no ZIP.

## 14. Surfaces

- **CLI.** `angee snapshot …` goes through `service.API`, locally or against the
  operator. `init --from-snapshot` is local only.
- **REST**, admin bearer only, strict bodies:

  | Route | Purpose |
  |---|---|
  | `POST /snapshots/preview` | preview |
  | `POST /snapshots` | create (202 + receipt) |
  | `GET /snapshots[?at=]` | list |
  | `GET /snapshots/{id}` | show |
  | `POST /snapshots/{id}/restore` | `force`, `from`, `only`, `map`, `partial`, `keep_old`, `restore_config` |
  | `POST /snapshots/restores/{id}/resume`, `/rollback`, `/cleanup` | journal operations |
  | `POST /snapshots/backup` | copy to destinations |
  | `POST /snapshots/forget` | ids, `policy`, `attempt`, `at` |
  | `POST /snapshots/prune`, `/check`, `/{id}/export` | lifecycle |
  | `GET /snapshots/attempts` | unknown and abandoned attempts |

  Receipts stay admin-only (`operator.go:735–747`). `/backups*` is deleted in P0.
- **Generic surfaces (#21).** P4 adds explicit exclusions, with tests, so the file, log
  and GraphQL APIs cannot reach `state/snapshots/**` (attempts, restores, asides, root id,
  restic cache) or `--plain` targets inside the root. Snapshot work adds no route a minted
  token can reach. The existing minted-token scope debt (`operator.go:1105–1124`) is
  unchanged and stays tracked separately.
- **The catalog is an operator function (Q5).** The operator answers which snapshots
  exist, from restic, through REST and through admin-only GraphQL queries:
  `snapshots(at:)`, `snapshot(id:, at:)` and `snapshotAttempts`. Django keeps no Backup
  records; its UI calls these. Lists are read on demand, never polled. Progress of
  create, backup and restore comes from their job-run receipts, which already exist.
- **GraphQL.** Node kind `BACKUP` is deleted in P0, and `SNAPSHOT` arrives in P4.
- **API types** go in `api/snapshots.go`, and **platformclient** methods in
  `snapshots.go`.

## 15. What stays, changes and goes

This is a greenfield rebuild (Q1). P0 deletes the v0.23.0 feature in one buildable
change. The repository lints with `unused` (`.golangci.yml`, `tests: true`), so
unexported service code with no remaining caller cannot be parked for later phases.
P0 therefore **deletes all of `internal/service/backups.go`**, including discovery, the
confined walk and the source-tree planner. P4 rebuilds what it needs, using the code at
`02dd95b` (v0.27.3) as reference. Only exported, tested seams survive, renamed.

**Deleted:**

| Area | Items |
|---|---|
| Service | `internal/service/backups.go` and `backups_test.go` in full; `BackupAPI` and its methods in `service/api.go`; `backupMu`, `backupLayers` and `backupLayer` (`service/platform.go`, `service/vcs.go`); `compileCapture`, the `capture` parameter of `compileRuntime` and the capture skip in `compile` (`platform.go:708–722, 843`); `CaptureJobError`, `refuseCaptureJob` and their call sites, and `job.Capture` in `Transient: isolated \|\| job.Capture != nil` (`jobs.go:51–80, 118, 247, 275–277, 996`); the backup branches of job-receipt recovery and listing, `IsBackupReceipt` and `rememberBackupRun` (`job_receipts.go:116–121, 175`); `CauseBackupRequestConflict` (`operror.go:49`) |
| API | `api/backups.go` except the source evidence type (renamed below); `JobRunOperation.Artifact` (`api/types.go:201`) and its clone (`jobs.go:235`) |
| Operator | `internal/operator/rest_backups.go` and `rest_backups_test.go`; the `/backups*` routes and the backup-receipt admin check (`operator.go:199–201, 737`). The GraphQL enum value `BACKUP` (`schema.graphql:5`), regenerated with `make generate` (`gql/generated.go`, `gql/model/models_gen.go`, resolvers) |
| Client | `internal/platformclient/backups.go` |
| Manifest | `BackupCapture`, `Job.Capture`, `PersistPath.Backup` and their validation (`manifest.go:582–583, 734–749, 1045, 1064–1080, 1124–1126`), with the matching cases in `manifest_test.go` and `jobs_test.go` |
| Docs | `docs/proposals/operator-backup-restore.md`; its ROADMAP row; the references in `local-platform-instance.md` (pointed at "stack snapshots, planned"); the "Manual backups" section of `docs/reference/operator-api.md`; the backup rows in `docs/reference/surfaces.md`; the capture-job clause in `docs/guide/manifest.md:417`; the regenerated `docs/public/angee.graphql` and `angee.schema.json`; a `CHANGELOG.md` entry under `Unreleased` / `Removed` |

**Renamed (exported, tested, reused from P4 on):**

| Before | After |
|---|---|
| `internal/vcs/backup.go`: `BackupPlanner`, `BackupPlan`, `BackupLayer`, `BackupLayerPlan`, `BackupComponent`, `BackupCapabilityError`, `BackupRefusal`, `BackupWithin` | `internal/vcs/snapshot.go`: the same names with `Snapshot` in place of `Backup` |
| `internal/vcs/gitdriver/backup.go` (+ test) | `snapshot.go` (+ test), with methods renamed |
| `internal/vcs/jjdriver/backup.go` (+ test): `BackupIdentity`, `BackupEvidence`, `BackupLayerPlan` | `snapshot.go` (+ test): `SnapshotIdentity`, `SnapshotEvidence`, `SnapshotLayerPlan`; the doc comments in `jjdriver.go` and `revset.go` follow |
| `internal/git/backup.go` (+ test): `PublishedBackup`, `BackupPublicationError` | `snapshot.go` (+ test): `PublishedSnapshot`, `SnapshotPublicationError` |
| `api.BackupSourceEvidence` | `api.SnapshotSourceEvidence`, in a new `api/snapshots.go` |

**Unchanged:**
- Receipts, the mutation lease and redaction.
- `secrets.ReadDeclarations` (exported; reused by P4). Its error text loses
  "recovery capture" wording.
- `decodeStrict` (still used by migrate). Its comment loses the backup example.
- The transient runners in both runtimes, still used by isolated job runs. The Windows
  refusal text says "transient local jobs" instead of "capture jobs".
- process-compose `MaxBackups` and copier's `renderedDocumentBackup`, which are other
  owners' words.
- `removeCanceledJob`'s ticker stays until P1 replaces it.

Existing on-disk `backups/archives/` and `state/backups/` directories are left alone;
none are in use.

## 16. Decisions

- **Q1. Decided (Alexis): greenfield rebuild, no backward compatibility.** v0.23.0
  backups were never used. P0 deletes them, with no translation layer or coexistence
  period.
- **Q2. Decided (Alexis): local stream saves stay gated** until the shim test passes.
  Upstream issue filed: [F1bonacc1/process-compose#583](https://github.com/F1bonacc1/process-compose/issues/583)
  (`runHeadless` calls `SetProjectNameAsTerminalTitle` unconditionally, so the title is
  written even when stdout isn't a terminal). Once a fixed release is the floor, the fd-3
  shim goes.
- **Q3. Decided (Alexis): copy live, with a warning.** Unowned persist paths that a
  service which is not held can write are copied with a warning, and so are per-file
  changes during the copy. `--strict` refuses instead. Confinement (S13) still refuses,
  because it protects against reading outside the stack.
- **Q4 and Q10. Decided (Alexis): portable data restore is the default.** Running the
  snapshot's configuration is `--restore-config`, which `--yes` never implies. Naming a
  source is consent for its data; there is no separate trust flag.
- **Q5. Decided (Alexis): the catalog is an operator function.** REST plus admin GraphQL
  `snapshots`/`snapshot`/`snapshotAttempts`. Django keeps no records.
- **Q6. Decided (Alexis): destinations are named copies.** Off-host is recommended and
  doctor warns; Angee does not prove location.
- **Q7. Keys are `<group>/<name>`**, where the group is the key's first segment. Groups
  beyond `databases` and `service_state` are **deferred** (Alexis).
- **Q8. Verify is required for stream members** and gates the commit.
- **Q9. Decided (Alexis): namespace plus claim plus root id**, with explicit takeover.
- **Q11. Decided (Alexis): v1 restores `fetch_published` sources;** P10 adds portable
  source bundles.
- **Q12. Decided (Alexis): vocabulary** — a snapshot is the committed set; a backup is a
  copy sent to a destination.
- **Q13. Decided (Alexis): formats are opaque strings compared for equality;**
  conventional names are documented.
- **Q14. Decided (Alexis): external targets** need a template `reset` job, `--force` and
  a committed safety snapshot. Angee never drops an external database itself.
- **Q15. Decided (Alexis): `ANGEE_JOB_TIMEOUT=0`** leaves restic data calls unbounded, as
  for jobs.
- **Q16. Decided (Alexis): local `running` members** wait for process-compose's pushed
  readiness from the live-views work, and are refused until it exists.
- **Still open:** the process questions: the next step, pushing the plan as a draft PR,
  and who builds the phases.

## 17. Phases

Every merge builds and passes `make check` and the go-code-reviewer agent. Each PR
carries its CHANGELOG entry. There is no compatibility constraint between phases (Q1).

0. **Remove v0.23.0 backups and rename the reusable internals (§15).**
   - Gate:
     - `make check`, `make check-generated` and golangci-lint are clean;
     - the vcs, git and jj driver tests pass under their renamed names;
     - `grep -rn 'BackupCapture\|/backups\|IsBackupReceipt\|BackupArtifact'` finds
       nothing;
     - the deleted feature's tests go with it.
   - **Ready to start** with the inventory as corrected in §15, after the third review.
1. **Streaming runner and no-poll cleanup.**
   - Changes: `StreamJobBackend`; compose `StreamRunner` (`/dev/fd/3`,
     `--project-directory`, separate stdout); group kill plus one `rm -f` replacing the
     ticker; the `Logging` model; process-compose stdin and group kill; the fd-3 shim
     behind its gate.
   - Tests: R1–R6 as integration tests, skipped without the tool or under `-short`; the
     shim proof; a cancel race with no surviving container.
2. **`internal/restic`**, plus doctor, the install prompt and the operator image.
   - Tests: fixtures; real-binary tests for S1, S3, S6–S8 and S11–S13. A new
     `ANGEE_REQUIRE_RESTIC=1`, mirroring `ANGEE_REQUIRE_JJ`, makes skips fail in CI.
3. **Schema:** `snapshots` (namespace, members, formats, `requires`, check/reset,
   `persist.*.snapshot: exclude`, backups, after_restore), plus the snapshot-job guards.
   `gen:schema`; validation tests for every §6 rule.
4. **Claims and attempts (§9), create, preview, list and show**, with REST, the admin
   GraphQL catalog queries, CLI and platformclient, the file-API exclusions and the
   secret-injection test.
   - Tests: interrupt at every §10 step and check the invariant table; two roots on one
     namespace refused; reconciliation conflicts; live-copy warnings and their
     `--strict` refusal; the ancestor-hold refusal; the S13 symlink-swap attempt refused.
5. **Forget, policy, sweep, prune and check.**
   - Tests: de-commit ordering, abandoned-only sweep, `forget --attempt`, prune racing a
     create.
6. **Portable in-place restore:** preflight, `check` and `reset`, external targets, the
   journal with resume, rollback and cleanup, `after_restore`, `--map`, `--only` and
   `--partial`.
   - Tests:
     - kill at every journal step, then resume and roll back;
     - an ambiguous aside stops the restore;
     - **cross-configuration:** stack pgvector to external Postgres and back (a second,
       unmanaged Postgres container stands in for external); SQLite between container
       and local runtimes;
     - Redis via the template procedure;
     - pg 17 into pg 16 refused by `check`.
7. **`init --from-snapshot`**, both variants: config from the snapshot (trust) and
   `--template` with `--input` and `--answers-from-snapshot`. Also the secrets backend
   override, port remap, answers at the render target, and `fetch_published` sources
   materialised in a different slot mode (git worktree ↔ jj).
   - Tests: a sibling restore on the same host with port conflicts; OpenBao isolation;
     answers above the root; path-attack manifests.
8. **Destinations and plain export:** exact-ID copy, validated reruns, destination
   retention and claims, the doctor warnings, and init from a destination.
9. **Docs and release.**
   - Docs: a new `docs/guide/snapshots.md`, covering the vocabulary, portability,
     formats and templates; `docs/guide/manifest.md`, `docs/guide/commands.md`,
     `docs/reference/operator-api.md` and `docs/reference/surfaces.md`; replace the
     proposal; the ROADMAP row.
   - Release.
10. **Portable source bundles:** driver-owned `git bundle` plus overlay, for git and jj,
    with cross-mode restore tests.

**Cross-repo, after P9:** angee-django's stack template declares `databases/app` as
`postgres-custom/1` in **both** `postgres_mode` variants (§18), so stacks move between
them. Its parked `operator_backup` addon is reworked to mirror `list` and drive create,
backup and restore drills.

## 18. Template guidance

**Postgres/pgvector**, format `postgres-custom/1`. The same key is used whether the stack
hosts the server or not.

| Job | Command |
|---|---|
| save | `pg_dump --format=custom -Z0 --dbname "$DATABASE_URL"`, client major ≥ server major (D5, D6) |
| verify | `pg_restore -f /dev/null` on stdin (D2) |
| check | compares the archive's "Dumped from database version" (`pg_restore -l`) with `psql -tAc 'show server_version_num'`; exits non-zero if the target is older (D6) |
| restore | `pg_restore --no-owner --no-acl --single-transaction --exit-on-error --dbname "$DATABASE_URL"` (D3, D4) |

- **Hosted:** `requires: running`, `owns: [pgdata]`. The image entrypoint creates the
  empty database.
- **External:** `requires: external`, with a `reset` job that empties the target, for
  example `DROP SCHEMA public CASCADE; CREATE SCHEMA public;` plus extensions, or
  `dropdb/createdb` with an admin URL. The template decides, using the privileges it has.
- Add a `pg_dumpall --globals-only` member if roles must move too.

**SQLite**, `sqlite-db/1`:
- save: `sqlite3 <db> "VACUUM INTO '$OUT/app.db'"` (D7);
- verify: `PRAGMA integrity_check`;
- restore (`requires: stopped`): `rm -f <db> <db>-wal <db>-shm`, then copy (D8).

**Redis/Valkey**, `redis-rdb/1`:
- save: `redis-cli --rdb "$OUT/dump.rdb"`;
- restore (`requires: stopped`): place the file, run a private
  `redis-server --appendonly no`, then `CONFIG SET appendonly yes` and `SHUTDOWN`
  (D9, D10).

**MySQL/MariaDB**, `mysql-sql/1`: `mysqldump --single-transaction --quick --routines --events`
out and `mysql` back in. No migrations during a create.

## 19. Risks

- **A lost password means lost data.** Offline keys are issued per repository and per
  destination.
- **Claims, attempts, journal and reconciliation are Angee code.** P4–P7 cover them with
  interruption matrices.
- **restic's format is 0.x.** Members are referenced by exact ID with `original` mapping;
  layouts stay engine-neutral; plain export exists.
- **Prune blocks creates.** Scheduling is Django's; `--retry-lock` absorbs overlaps.
- **Latency:** 1–2 s per restic call remotely (S5), about 5 + N calls per create.
- **External resets are destructive.** They run only with `--force`, after a committed
  safety snapshot.
- **Templates must keep formats honest.** A format string promises compatibility that
  only the template's `verify` and `check` jobs enforce.
- **Local services share the operator's trust domain** (S13 confinement).

## 20. Template-facing contract summary

A template that wants its data in snapshots declares, for each dataset:
- a stable key and a format;
- where the data lives here (`requires`, `service`, `owns`);
- `save`, `verify` and `restore` jobs, plus `check` and (for external targets) `reset`;
- jobs that never touch other datasets.

A stack moves between configurations when both configurations' templates declare the
same keys and formats.

## 21. Review resolution

**Second review, new findings:**

| B | Severity | Resolution | Section |
|---|---|---|---|
| 1 | blocker | Release before verify and commit; a failed release writes the abandoned record and fails the receipt | §10 steps 8–12 |
| 2 | blocker | Claims per namespace; abandoned records as owner proof; unknown attempts swept only explicitly; no age heuristics | §9, §12 |
| 3 | blocker | Reconcile exactly one manifest whose digest, root id and member set match; otherwise `CONFLICT` | §10 steps 0, 12 |
| 4 | blocker | Journal with observable postconditions and replay or undo per step; ambiguity stops; commit before aside cleanup | §11 |
| 5 | blocker | The hold rule for container writers on ancestors, plus ancestor `(dev, inode)` checks before and after (S13); the content check is advisory | §10 steps 1, 7 |
| 6 | major | The config fingerprint is replaced by per-member key, format and `check`, running target jobs; config restore is `--restore-config` | §5.4, §11 |
| 7 | major | Secrets backend override written into the target manifest before render, isolation checked; only declared secrets are recovery data | §11 step 3, step 5 |
| 8 | major | `ports.*.value` and `port_leases` remapped through the allocator before render; the answers render target is journaled | §11 step 3 |
| 9 | major | Copy of exact member IDs; one destination match per key via `original`; manifest last; reruns validate | §13 |
| 10 | major | Destinations are named copies, and doctor warns. Delivery order no longer matters: greenfield, with nothing to cut over (Q1) | §13, §17 |
| 11 | major | Group kill plus one `docker rm -f`, no ticker; readiness from `compose up --wait` and process-compose's pushed stream | §7 |
| 12 | minor | `BackupIdentity`/`BackupEvidence` placed in `jjdriver/backup.go`; full P0 inventory | §15 |

**First-review findings still open after v2.1:**

| # | v3 |
|---|---|
| 2 | release before commit; exact reconciliation (B1, B3) |
| 3, 17 | claims, abandoned records, explicit unknown-attempt forget (B2) |
| 4 | target jobs by default; `--restore-config` for archived config (B6) |
| 5, 12 | observable journal, commit before cleanup (B4) |
| 6 | hold plus ancestor checks; exact include list (B5) |
| 7 | owner readiness; explicit topological starts; `after_restore` |
| 8 | release before commit (B1) |
| 9 | verify required for stream members |
| 10 | copied with a warning recorded in preview, the receipt and the manifest, per Alexis (Q3); `--strict` refuses |
| 11 | exact sets for files and trees; partial status |
| 13 | backend override and isolation (B7) |
| 14 | port values and leases remapped; answers journaled (B8) |
| 16 | exact-ID copy (B9) |
| 20 | exposure table plus the injection test |
| 21 | exclusions plus an explicit statement of the remaining debt |
| 22 | deadlines specified; Q15 for the 0 case |
| 23 | `os.Root.Mkdir` exclusive rule |
| 24 | invariant tables in §10, §11 and §17 |

Findings 15, 18 and 19 were already fixed in v2.1. Finding 1 (buildable cutover) is moot:
the greenfield decision (Q1) deletes the old feature in P0.

## 22. Third review (v3.1): gates on later phases

`codex-review-v3.md` gives P0 a go once its inventory is corrected (done in §15) and P1
a go after P0 lands, with the stream role added (done in §7). Its remaining findings
belong to later phases. Each phase below is **blocked until its plan section is revised
for these findings**, and the revision is reviewed before that phase starts.

| Third-review finding | Gate | What the revision must specify |
|---|---|---|
| 1 (blocker): portable restore can place archived config | P6 | Built-in member roles and an exact restore path map. In portable mode, archived `angee.yaml` and answers are metadata only; data paths are placed without config |
| 2 (blocker): journal cannot undo its first writes | P6 | Record old config, answers, presence and old/new path maps before any copier or config write. Journal copier's file transaction (reusing `template_plan.go:512`'s rollback record). Secret undo uses `Delete` when the value was absent |
| 3 (blocker): safety snapshot unprotected while a restore is open | P5, P6 | `forget` subtracts IDs referenced by open restore journals from restic's removal set, and refuses explicit forget of them |
| 4 (major): claims are observations, not leases | P4 | A claim generation order, revalidation before commit and before each destructive repository call, and fencing of the old root before takeover. State the remaining race honestly where restic offers no atomic claim |
| 5 (major): `--files-from-raw` vs live copy and S13 | P4 | Directory-level restic arguments for live trees, the hold test defined against the arguments restic opens, and an exact post-copy path check |
| 6 (major): format equality is not enough | P3 | Compatibility also compares the channel and exact file names. Hosted state requires `owns`, or an explicit reset/empty-state contract |
| 7 (major): `check` cannot run before a new hosted target exists | P6, P7 | A preflight check contract that needs no running target (archive plus target image), and a separate post-start check that is not called non-destructive |
| 8 (major): source members vs the v1 scope | P4 | v1 `fetch_published` evidence is a manifest record with no restic member; bundle selection is refused until P10 |
| 9 (major): archived answers can redirect init | P7 | Parse answers through the guarded reader, keep only the target template's declared inputs, replace `_src_path` and the root relation, then apply `--input` |
| 10 (major): P0 inventory | P0 | **Done:** §15 |
| 11 (minor): stream role | P1 | **Done:** §7 |
| Partials B2, B3, B4, B6–B8 and first-review #5, #11–#14, #17, #20, #21, #24 | P4–P7 | Addressed by the rows above, plus acceptance rules for plaintext files staging and process-compose's temp config (P4) |
