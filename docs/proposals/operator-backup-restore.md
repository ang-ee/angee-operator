# Proposal: operator backup/restore — snapshot a stack's data plane

**Status:** Draft · **Area:** backup, stacks, data lifecycle, runtime backends · **Surfaces:** CLI + REST (operator) + Angee provisioning

## Summary

Give the operator a **generic snapshot/restore mechanism** for a stack's *data*
— the stateful bytes a stack accumulates (a Postgres database, uploaded files, a
SQLite db) — as a first-class backend plugin alongside `secrets.Backend`,
`store.Store`, and `runtime.Backend`. A snapshot captures a stack's declared
`persist:` subpaths plus each service's declared **backup hook** (e.g. Postgres'
`pg_dump`), writes it to a pluggable backend (localfs tarball first, restic/S3
later), and catalogs it by id. Restore reverses it into a target stack.

The load-bearing separation: **git owns the control plane (code, config, the
manifest — desired state, reconciled); snapshots own the data plane (out of
band).** Templates declare *shape*; this plugin owns the *data lifecycle*. No
template ever bakes data.

## Motivation

Three needs, one mechanism:

- **Bootstrap a workspace with real data.** A workspace that opts into Postgres
  (for RAG / working against realistic data) needs to be seeded — but the seed
  must not live in the stack/project template. It belongs to a restore, pulled at
  create time.
- **Experiment safety.** Snapshot `main`'s data before a risky forward migration;
  restore if the experiment's migration is wrong. Note the asymmetry the whole
  workspace flow depends on: **code merges via git; data does not "merge"** —
  migrations run *forward* against the target's data, and the snapshot is the
  rollback net.
- **Move data between instances/machines** without shipping it through git (dumps
  are large and binary; git is the wrong store for them).

## Current behavior — the machinery to reuse

- **`persist:` already enumerates the stateful surface.** A stack declares its
  durable state as named **bind subpaths** under the stack root (e.g. arpee:
  `persist: { app-data: { subpath: ./data }, chrome-profile: { subpath: ./data/chrome } }`),
  materialized on every bring-up in `StackPrepare`
  ([`internal/service/platform.go`](../../internal/service/platform.go)).
  Deliberately **bind subpaths, not Docker named volumes** — which is also why a
  Compose project rename (see `compose-project-isolation.md`) never loses data.
  The operator already knows *what* is stateful.
- **The backend-plugin pattern is established.** `secrets.Backend`
  (env-file/openbao), `store.Store` (localfs, registry-dispatched), and
  `runtime.Backend` (compose/proccompose) are all interface + registry seams.
  Backup is a fourth of the same shape.
- **The kind-free philosophy is set** (`global-source-registry.md`): the operator
  is a generic, deterministic mechanism; *meaning* and *policy* live above it in
  Angee/Django, REBAC-gated. Backup follows suit — it runs declared commands and
  copies declared paths; it never learns the word "Postgres."

What is missing: nothing snapshots or restores the data plane. `persist:` paths
are materialized (created if absent) but never captured; a live database has no
consistent-dump path at all.

## Proposal

### 1. `backup.Backend` seam + registry

```
backup.Backend:
    Snapshot(ctx, stack) (SnapshotID, error)   // capture → backend, cataloged
    Restore(ctx, stack, SnapshotID) error       // materialize into the stack
    List(ctx) ([]SnapshotMeta, error)
    Delete(ctx, SnapshotID) error
```

Impls, registry-dispatched like `store`: **`localfs-tarball`** (default; a tar
under a stack-root/object path), then **`restic`** (dedup/incremental,
remote-capable) and **`s3`**. Selected by a `manifest.BackupBackend.Type` field,
mirroring `SecretsBackend`.

### 2. Snapshot scope = `persist:` subpaths + declared service hooks

A `Snapshot` is the union of two deterministic inputs, both already (or newly)
declared in the manifest — the operator hardcodes neither:

- **Every `persist:` subpath** — tar'd as-is. Covers SQLite, uploaded files,
  caches a stack chooses to include.
- **Every service that declares a `backup:` hook** — the operator execs the
  service's `dump` command (into the snapshot) and, on restore, its `restore`
  command. A live Postgres data dir cannot be safely file-copied, so it is *not*
  captured as a `persist:` tar; its **`backup: { dump: "pg_dump …", restore:
  "pg_restore …" }`** hook is. The operator runs the declared command inside the
  service via the active `runtime.Backend` (compose `exec` / proccompose).

The three owners this creates (none of which is "the stack template baking
data"):

| Fact | Owner |
| --- | --- |
| Stack shape (which services, which `persist:` paths) | stack/project template |
| A service's **backup protocol** (how it dumps/restores *itself*) | the reusable **service template** (e.g. a `postgres` service ships its own hooks) |
| The **data snapshot** (the bytes) | this plugin's backend + catalog |
| Catalog, retention, who-may-restore-what | Angee (`angee.operator_backup` addon), REBAC-gated |

### 3. Snapshot catalog

Snapshot metadata (`{id, stack, createdAt, sizeBytes, sourceRef, backend}`) lives
in the `store` layer (localfs today), keyed by id. The **bytes** live in the
backup backend; the **catalog** is the index. Snapshots are object artifacts —
they are **not** committed to git.

### 4. Surfaces + the bootstrap restore

- **CLI/REST:** `angee stack backup [--name <label>]`, `angee stack restore <id>`,
  `angee stack backup ls` → the corresponding REST routes on the daemon.
- **Bootstrap-with-data as a restore Job, not a template concern:**
  `angee workspace create X --input db=postgres --restore <id>` renders a
  workspace whose stack carries a **restore Job** — `depends_on: [postgres]` →
  run the service `restore` hook → `migrate` forward if the schema drifted. The
  snapshot is pulled by the *job*; the template holds no data.
- **Automated:** scheduled periodic backups of a durable stack; "seed a new
  experiment from the latest `main` snapshot" = create + restore-from-latest. RAG
  embeddings ride along for free — they are just rows in the dump.

## Design options

- **A. Service-declared `backup:` hooks (recommended).** Keeps the operator
  kind-free: it execs declared commands and copies declared paths. New service
  kinds (a different DB, a search index) need no operator change — they ship their
  own hooks. Postgres is simply the first service template to carry one.
- **B. A built-in Postgres handler in the operator.** Faster to ship, but couples
  the operator to a specific data system and reopens the "does the operator know
  what a service *is*" question the `global-source-registry` proposal closed.
  Rejected as the primary path; acceptable only as a temporary shim behind the
  same `backup.Backend` interface.

## Security

- **Secrets are excluded from snapshots by default** and regenerated on restore
  (env-file secrets are already `generate`-on-demand). An opt-in "seal and
  include" mode (for exact clones) is future work, gated behind an explicit flag
  and an encrypted backend.
- Snapshot access is **policy in Angee** (who may `restore` which snapshot into
  which stack), REBAC-gated — the operator mechanism is unprivileged w.r.t.
  meaning, consistent with sources.

## Backward compatibility & migration

Purely additive. Stacks without a `backup:` backend or service hooks are
unaffected; `persist:` semantics are unchanged. No existing file or field
changes meaning.

## Out of scope

- The **git → running-state reconciler** (the control plane). This proposal is
  the *data* plane only; see `local-platform-instance.md` for the control side.
- Cross-service referential consistency (app-consistent multi-DB snapshots) —
  v1 is per-stack, best-effort quiesce via the service `dump` hook.

## Acceptance

- `stack backup` on a Postgres+files stack produces a snapshot whose restore into
  a fresh stack yields an identical database (row + pgvector parity) and identical
  `persist:` files.
- A `workspace create --restore <id>` boots with the restored data after its
  restore Job completes.
- Unit: snapshot scope is exactly `persist:` paths ∪ services-with-`backup:`;
  a service without a hook contributes nothing; secrets never appear in a default
  snapshot.

## See also

- [`global-source-registry.md`](./global-source-registry.md) — the kind-free
  mechanism/policy split this mirrors.
- [`compose-project-isolation.md`](./compose-project-isolation.md) — why
  `persist:` uses bind subpaths (rename-safe), which makes them tar-able.
- [`local-platform-instance.md`](./local-platform-instance.md) — the control
  plane and the workspace circle this data plane serves.
