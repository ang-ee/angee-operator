# Proposal: operator workspace file tools (read/edit)

**Status:** proposal — for the angee-operator team to implement. Not built here.
**Asked by:** the Angee marketplace (install/uninstall addons). **Consumer:** the
platform console board (TS) + `platform_integrate_vcs`.

## Why

The addon marketplace lets an operator-user install/uninstall addons from a board.
"Install an addon" = add its root to the deployment's `settings.yaml`
`INSTALLED_APPS`, then rebuild + restart. The deployment **is a workspace/source**
the operator already owns (the `app` source), and the operator already owns the
**rebuild lifecycle** (`/stack/build`, `/stack/up`, `/stack/dev`). The one missing
capability is **reading and editing files inside a workspace's source over the
API** — so the console edits `settings.yaml` *through the operator* rather than the
Django app touching its own config. Keep it **generic** (read/edit any file in a
source); `settings.yaml` is just the first consumer.

This keeps the Django app a normal Django app (reads `settings.yaml` at boot, no
DB-driven settings-load), and puts config ownership where it belongs — with the
operator that already owns the stack files and the lifecycle.

## Scope

In scope: a generic, scoped **file read / write** API on the operator daemon,
modeled 1:1 on the existing `secrets` API.

Out of scope (the client/other systems own these):
- **No YAML logic.** The operator reads/writes raw bytes. The `INSTALLED_APPS`
  edit (comment-preserving) is done by the **board** (the `yaml` npm package) on
  the content it read back. Don't put settings.yaml/INSTALLED_APPS knowledge in
  the operator.
- **No rebuild here.** The board calls the existing `/stack/build` (+ restart)
  after writing. The file API does not trigger builds.

## Proposed API (mirror `secrets`)

Model the shapes, routes, client, and GraphQL on the existing secrets path
(`api/types.go` `SecretSetRequest`/`SecretRef`; `internal/operator/operator.go`
route registration with `s.auth(...)`; `internal/platformclient/client.go`
`SecretGet`/`SecretSet`; `internal/operator/schema.graphql` + resolvers + the gql
codegen).

### REST (auth-gated like `/secrets/*`)
- `GET  /files?source=app&path=<relpath>` → `{ path, source, content, etag }`
  (etag = content hash, e.g. sha256, for optimistic concurrency).
- `PUT  /files?source=app&path=<relpath>` body `{ content, etag? }` →
  `{ path, source, etag }`. If `etag` is supplied and the on-disk file has
  changed, return `409 Conflict` (don't clobber concurrent edits).
- (optional, later) `GET /files/list?source=app&path=<dir>` → directory entries.

Use query params (not a path segment) since `path` contains slashes —
`/secrets/{name}` uses a single segment, files don't.

### GraphQL (mirror `secretSet`)
- query `file(source: String!, path: String!): FileContent!`
  (`{ path, source, content, etag }`).
- mutation `fileWrite(source: String!, path: String!, content: String!, etag: String): FileRef!`.

### Typed client (mirror `SecretGet`/`SecretSet` in `platformclient/client.go`)
- `FileRead(ctx, source, path) (api.FileContent, error)`
- `FileWrite(ctx, source, path, content, etag string) (api.FileRef, error)`

## Scoping & security (the important part)

- **Resolve the source root** via the existing source-path resolution
  (`workspaceSourcePath(...)` / `source.Path` in `internal/service`). `source` is
  one of the stack's declared sources (`app`, `framework`, …) — reject unknown
  source keys.
- **Confine to the source root.** Clean the requested `path` and verify the
  resolved absolute path is **inside** the source root (reject `..` traversal,
  absolute paths, and symlink escapes — resolve symlinks then re-check the prefix).
- **Auth:** the same bearer gate as `/secrets/*` and `/stack/*` (`s.auth`).
- **Optional allowlist:** if you want to be conservative, gate writes to a
  configured set of editable paths (e.g. `settings.yaml`) declared in the stack
  manifest — see Open Questions. Reads can stay broad (within the source).

## Workspace targeting

The daemon runs for one stack (`--root`), so `source` selects among *that stack's*
sources and the marketplace only needs `source=app`. If/when one daemon serves
multiple workspaces, add an optional `workspace` param resolved via
`workspaceSourcePath(workspaceName, slot, source)` — the resolution helper already
exists; the API param is the only addition.

## How the console uses it (for context, not to implement)

1. Board reads `app/settings.yaml` via `GET /files?source=app&path=settings.yaml`
   (keeps the `etag`).
2. On install/uninstall, the board edits `INSTALLED_APPS` in the YAML
   (comment-preserving, client-side) and `PUT`s it back with the `etag`.
3. Board calls the existing `/stack/build` (+ restart). On success the Django
   `platform.Addon` reflection updates on the next boot; on failure the old
   runtime keeps serving and the board shows the build error.

## Open questions for the implementer

1. **Write allowlist vs any-file-in-source?** Broad (any file under the source) is
   simplest and matches "file tools in the workspace"; an allowlist (declared in
   the manifest) is safer for an internet-exposed daemon. Recommend: broad read,
   manifest-allowlisted write — but your call given the operator's threat model.
2. **etag/concurrency:** content-hash etag with `409` on mismatch (proposed) vs
   last-write-wins. Recommend the etag — two console tabs shouldn't clobber.
3. **List/delete:** include `GET /files/list` and `DELETE /files` now, or defer
   until a consumer needs them? The marketplace needs only read + write.
4. **Binary/size limits:** cap file size and treat content as UTF-8 text (config
   files); reject binary/oversize.

## Implementation notes (operator side) — finalized architecture

This is the architecture the angee-operator team settled on after a prior-art
review. The driving idea: a *store* (where bytes live) is a different axis from
an *object* (secret vs file). Generalize the store; keep the objects distinct.

### Store vs object

- **Store** = a generic `key → bytes` backend with **zero domain knowledge**.
  `localfs`, `env-file`, and OpenBao are all stores.
- **Object** = a domain layer (secrets, files) that sits *on* a store and owns
  its own validation / codec / semantics. Secrets and files are **siblings**
  over a shared store substrate.
- Unifying the *object* layers is the anti-pattern; sharing the lower *store*
  primitive is the goal.

### The `internal/store` substrate

- **Minimal core interface** (`Get/Set/Delete/List` over a `Blob{Bytes, Etag}`)
  with **no secret-domain verbs** — modeled on Vault's `physical.Backend`
  (exactly four generic methods).
- **Capability composition** via *optional* interfaces discovered by type
  assertion (`Lister`, `Deleter`, `Versioned`/CAS) — the Go stdlib idiom
  (`io.WriterTo`, `http.Flusher`, `database/sql/driver` optionals) and Vault's
  `Transactional`/`HABackend` pattern. The core stays tiny; do not widen it.
- **Backend registry** (`store.Register(kind, factory)` / `store.Open(kind,
  cfg)`), the `database/sql` / Go CDK structure — one construction path,
  backends self-register. Adding S3/git/remote later is one registration.
- `localfs` implements `Versioned` (sha256 etag + CAS) and owns the **single
  path-containment resolver** (clean, reject `..`/absolute, resolve symlinks
  then re-verify the prefix). Files *require* `Versioned`; env-file/OpenBao do
  not implement it (last-write-wins; files never run on them).

### The discipline (the one rule from prior art)

Secret-specific actions (rotation, leases/TTLs, dynamic creds, transit
encrypt/decrypt, no-readback) must live in the **secrets object layer** with its
own request-oriented interface (à la Vault `logical.Backend`) — **never** as
`Store` methods, **never** via a gocloud-style `As()` escape hatch (the Go CDK
explicitly discourages it), **never** as storage capability interfaces. Today
the operator's secrets are pure `key → bytes` (no leases/rotation), so a simple
key→value secret object suffices; we just **reserve** (document, not build) the
engine seam so `Store` is never overloaded. There is also a security rationale:
Vault keeps its storage layer free of secret semantics on purpose — it is
"completely untrusted," with encryption handled in a barrier layer above it.

### Convergence

env-file and OpenBao become **registered store backends now**, while
`internal/secrets`' exported API is preserved so callers compile unchanged and
the existing secrets test suite stays green (the regression gate). `localfs` is
the new backend for files.

### Prior art (primary sources)

- Vault/OpenBao `physical.Backend` (4-method generic store; optional
  `Transactional`/`HABackend` capability interfaces; untrusted storage layer):
  `github.com/hashicorp/vault/sdk/physical`, `.../sdk/logical`.
- Go CDK / gocloud.dev deliberate separation (`blob.Bucket` vs `secrets.Keeper`
  vs `runtimevar`), portable-type-over-minimal-driver, and the discouraged
  `As()` escape hatch: `gocloud.dev/concepts/structure/`,
  `gocloud.dev/concepts/as/`, `google/go-cloud/internal/docs/design.md`.
- Go small-core + optional-interface idiom: `database/sql/driver`, `io`,
  `net/http` (and "the bigger the interface, the weaker the abstraction").
