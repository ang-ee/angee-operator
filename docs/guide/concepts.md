# Concepts

Angee is a **self-managed stack manager**. The `angee` CLI and the
`angee-operator` HTTP daemon — both written in Go — pull a set of source
repositories, render them into a working stack, and run that stack on
docker-compose or process-compose. The same primitives drive both
**development workspaces** and **production stacks**, so a feature branch
you develop in a workspace can be promoted to production by pointing the
same Sources at a different Stack.

## What "self-managed" means

Angee is the deployment plane *and* the development plane for the same
codebase, configured with the same `angee.yaml`. There is no separate
CI/CD system that knows how to build your app:

1. **GitOps over Sources** — your code is declared as Sources (git
   repositories or local paths) in `angee.yaml`. Angee fetches, caches,
   and (when needed) worktrees them.
2. **Workspaces compose Sources for development** — render a Copier
   template that materializes a chosen set of Sources on a feature
   branch and allocates ports. Workspaces produce only files; if the
   template chains an inner Stack, it's rendered as files and you bring
   it up with a Stack operation against the inner root.
3. **Stacks compose Sources for deployment** — the same `angee.yaml`
   compiles to runtime files (Docker Compose or process-compose) and is
   driven by the operator.
4. **The operator promotes between environments** — the REST and GraphQL
   surfaces let CI, agents, or another tool drive the same lifecycle.

```text
        ┌────────────────┐
git ──► │    Sources     │ ─────────┐
        └────────────────┘          │
                ▼                   ▼
        ┌────────────────┐   ┌────────────────┐
        │   Workspaces   │   │     Stack      │
        │  (dev / agent) │   │  (production)  │
        └────────────────┘   └────────────────┘
                ▼                   ▼
        ┌────────────────────────────────────┐
        │ docker compose / process-compose   │
        └────────────────────────────────────┘
```

## The engine boundary

Everything below is implemented by the Go engine in this repository
(`angee-go`). It is intentionally generic: it knows nothing about Django,
React, or any specific framework.

| Concept | Role | Where it lives |
| --- | --- | --- |
| **Stack** | One `ANGEE_ROOT` containing `angee.yaml` plus generated runtime files. Materialized from a Stack template. | `internal/manifest/`, `internal/service/` |
| **Service** | A long-running workload. `runtime: container` → Docker Compose; `runtime: local` → process-compose. | `internal/runtime/` |
| **Job** | An explicitly invoked command with the same env, mount, and workdir handling as a Service. | `internal/service/` |
| **Source** | Reusable source material. Implemented kinds: `git` (cached and optionally worktreed) and `local` (path-mounted). | `internal/git/`, `internal/service/` |
| **Workspace** | A rendered Copier template at `$ANGEE_ROOT/workspaces/<name>` with materialized Sources and allocated ports. **Pure file primitive** — it never starts services. | `internal/copierx/`, `internal/service/` |
| **Workspace source slot** | A single git materialization inside a Workspace (`workspace.sources.<slot>`). Carries its own branch/ref/mode/subpath and has slot-level fetch/pull/push/merge/rebase/diff. | `internal/service/`, `internal/git/` |
| **GitOps topology** | A derived, read-only view over Sources × Workspace slots — what's clean, dirty, ahead, behind, diverged, branch-mismatched. Available as snapshot query and live subscription. | `internal/service/gitops*.go` |
| **Operator** | The REST + GraphQL control-plane server for one Stack root. | `internal/operator/` |
| **Connection token** | Short-lived HS256 JWT minted by the operator, scoped to one actor. Issued from the admin bearer; verifiable without shared state. | `internal/operator/tokens.go` |
| **Secrets backend** | env-file by default; OpenBao for production. Resolved values land in `run/secrets.env`. | `internal/secrets/` |
| **Port pool** | Named ranges (`workspace`, `django`, `acp`, …) with leases, so workspaces don't collide. | `internal/ports/` |
| **Stack template** | A Copier template with `_angee.kind: stack` that produces an `angee.yaml`. | `internal/copierx/` |
| **Workspace template** | A Copier template with `_angee.kind: workspace` that produces a workspace tree, declares Sources to materialize, and may chain an inner Stack template **as files**. | `internal/copierx/` |

Every concept above has a `service.Platform` method and at least one of
{CLI, REST, GraphQL}. The full classification is tracked in
[Surface parity](/reference/surfaces) and enforced by
`internal/service/surface_matrix_test.go`.

## What the operator owns

One operator runs against exactly one Stack root. Everything reachable
from that root is what a client (CLI, Django host, agent, custom UI)
can read and drive over REST + GraphQL.

| Primitive | What it owns | Read | Write |
| --- | --- | --- | --- |
| **Stack** | The single `angee.yaml` + generated runtime files at `ANGEE_ROOT`. | `stackStatus`, `stackPrepare` (compiled compose + process-compose + resolved secret env), `stackLogs`. | `stackInit`, `stackUpdate`, `stackBuild`, `stackUp`, `stackDev`, `stackDown`, `stackDestroy`. |
| **Service** | Long-running workloads declared in the stack. | `services`, `serviceLogs`. | `serviceInit`, `serviceUpdate`, `serviceStart`, `serviceStop`, `serviceRestart`, `serviceDestroy`. |
| **Job** | Explicitly invoked one-shot commands. | `jobs`. | `jobRun(name, inputs)`. |
| **Source** | Reusable source material (git or local) declared in the stack. | `sources`, `source(name)` (with state). | `sourceFetch`, `sourcePull` (= **update**: fetch + fast-forward), `sourcePush`, `sourceDiff`. |
| **Workspace** | Rendered file tree under `workspaces/<name>`. | `workspaces`, `workspace(name)`, `workspaceStatus(name)`, `workspaceLogs(name)`. | `workspaceCreate`, `workspaceCreatePreflight` (validate without rendering), `workspaceUpdate` (re-render with new inputs / TTL), `workspaceDestroy`. **Never** start/stop — see "Workspaces don't run" below. |
| **Workspace source slot** | One git materialization inside a Workspace. | `workspaceGit(name)` (all slots), `workspaceSourceDiff(workspace, slot, ref)`. | `workspaceSourceFetch`, `workspaceSourcePull` (= **slot-level update**), `workspaceSourcePush`, `workspaceSourceMerge`/`Rebase`/`MergeAbort`/`RebaseAbort`/`RebaseContinue`/`Publish`. |
| **Workspace branch identity** | The cross-slot promise that every git slot in a workspace lives on its declared branch. | `workspaceStatus.sources[].branch` / `currentRef` / `state`. Top-level `state: discrepancy` flags any mismatch. | `workspacePush` (push every slot's branch), `workspaceSyncBase(name, method)` (= **multi-slot update** against each base ref; merge or rebase). |
| **GitOps topology** | Derived view over Sources × Workspace slots. | `gitOpsTopology(withCommits: Int)` (snapshot; `withCommits > 0` opt-in populates `sources[].commits`). Live: `onGitOpsTopologyChange` subscription. | No direct writes — the view recomputes from the underlying state on every read. |
| **Templates** | Workspace and Stack Copier templates discoverable under `<root>/.templates/<kind>/<name>` and `<root>/templates/<kind>/<name>`. | `templates` (list), `template(ref)` (one descriptor with input schema). | No write surface; templates live in the filesystem. |
| **Connection token** | Short-lived JWT for scoped per-actor access. | — (opaque to clients). | `mintConnectionToken(actor, ttl)`. Gated by the admin bearer; clients use the returned token for follow-up requests. |
| **Secrets** | Values referenced as `${secret:name}` in the manifest. | `stackPrepare` returns the resolved env var **names** (not values). | Out-of-band: provisioned via the configured backend (env-file or OpenBao). The operator reads from the backend and writes resolved values into `run/secrets.env`. |
| **Ports** | Named pools declared as `operator.port_pool.*`, leased per workspace. | Lease state lives on the stack manifest under `port_leases:`. | Leases are added/removed implicitly by `workspaceCreate` / `workspaceDestroy`. |

### The three update scopes

"Update" means different things at different scopes, all in the same
family of git operation. The names are deliberately distinct so a client
picks the right one:

| Scope | Op | Meaning |
| --- | --- | --- |
| **Whole source** | `sourcePull(name)` | Fetch from upstream + fast-forward the cached source's tracking ref. Use when the top-level source cache should match its remote. |
| **One workspace slot** | `workspaceSourcePull(workspace, slot)` | Fast-forward this slot's worktree from its tracking ref. The slot is itself a worktree on the **workspace branch**, not the source's main branch. |
| **All slots of a workspace** | `workspaceSyncBase(workspace, method)` | Merge or rebase **each slot's workspace branch** against its declared base ref (typically `origin/main`). Stays on the workspace branch — never switches. This is "stay current with main." |

`sourcePull` is the top-level synonym for "update". `workspaceSyncBase`
is what you reach for when a workspace has fallen behind `main` and you
want to bring its working branches forward without leaving them.

### Workspaces don't run

A Workspace is a file primitive. `workspaceCreate` renders a Copier
template (including any chained inner-stack template **as files**) and
materializes git/local sources. It does **not** start services.

If a Workspace renders an inner Stack and you want it running, drive it
explicitly as a Stack operation against the inner root:

```sh
angee stack up --root workspaces/<name>/.angee
# or expose it as its own HTTP control plane:
angee operator --root workspaces/<name>/.angee --port 9100
```

This boundary keeps Workspace (data) and Stack (runtime) cleanly
separable — a Service in the outer Stack can mount
`workspace://<name>` without anything inside the workspace needing to
"run".

### Live event streams

Snapshots are reachable via plain GraphQL queries; live updates are
GraphQL subscriptions over Server-Sent Events. The transport is
`POST /graphql` with `Accept: text/event-stream` (the gqlgen
single-connection SSE mode).

| Subscription | Fires when |
| --- | --- |
| `onGitOpsTopologyChange` | Polled topology hash changes (2 s tick by default). |
| `onWorkspaceStatusChange(name)` | A specific workspace's polled status hash changes. |
| `onServiceLogs(name)` | New log lines arrive from the runtime backend's `--follow`. |
| `onWorkspaceLogs(name)` | Same, scoped to a workspace's logs. |

No initial snapshot is emitted on connect — clients should issue a
one-shot query for the current state alongside opening the
subscription.

### Auth model

- The operator-wide admin token (`--token` flag or
  `Authorization: Bearer <token>`) gates every protected endpoint.
  Required for non-loopback binds.
- `mintConnectionToken` issues per-actor JWTs from that admin bearer for
  finer-grained client scoping. The signing key resolves in order:
  explicit `--jwt-secret`, `ANGEE_OPERATOR_JWT_SECRET` env var,
  HKDF-derived from the admin bearer, then a per-process random
  fallback for loopback dev.

See the [Operator API reference](/reference/operator-api) for the
detailed REST + GraphQL contract.

## Above the engine

Angee is designed so application frameworks plug in *on top* of the
engine. The engine deploys whatever Services you declare; an
**application runtime** decides what those Services actually do, what
gets composed inside them, and how features are added.

| Term | Meaning | Status in `angee-go` |
| --- | --- | --- |
| **Host** | An application runtime that runs *inside* one or more of a Stack's Services — for example a Django process, a React build, or an MCP server. The Host is what end-user code talks to. | Not a manifest concept. The engine just runs Services. |
| **Block** | A unit of application code that contributes to the Host runtime — for example a Python pip distribution that adds models, GraphQL types, permissions, and React views. | Not a manifest concept. Defined by the Host. |
| **Build** | The Host's own build step (e.g. `manage.py angee build`) that composes Blocks into a deterministic `runtime/` tree before the Service starts. | Not invoked by the engine; usually a Job or a service entrypoint step. |

The engine treats a Host as just another container or local process. It
will mount Sources, set env, allocate ports, and start the Service — what
runs there (Django? Node? a static site? an agent loop?) is entirely up
to the Host.

### `angee-django` — the first default Host

[`angee-django`](https://github.com/fyltr/angee-django) is the first and
currently the default application runtime. It is a **Block compiler**
that produces a working Django + GraphQL + React application:

- Each Block is a pip distribution that contributes abstract models,
  GraphQL fragments, REBAC permissions, and React views.
- `manage.py angee build` composes every installed Block into a
  deterministic `runtime/` tree.
- The output runs as a single Django Service inside an Angee Stack.

`angee-django` ships its own Stack and Workspace Copier templates under
`templates/stacks/dev/` and `templates/workspaces/dev-pr/` — those
templates are what `angee init --dev` and
`angee workspace create <name> --template dev-pr` render when you work on a
Django consumer.

Other Hosts (a Node service, a Go API, a static site, anything that runs
in a container or as a local process) plug in the same way: ship a Stack
template that declares the right Services and Sources, and Angee will
pull, render, and run it.

## What "Self-Building" Looks Like

Putting the pieces together, a typical loop looks like this:

1. **Declare Sources.** Your app repos go into `angee.yaml` under
   `sources:`. Angee fetches them into a shared cache via
   `sourceFetch` / `sourcePull`.
2. **Render a Workspace (files only).**
   `angee workspace create fix-issue-123 --template dev-pr` renders a
   Copier template, materializes each Source as a worktree on
   `workspace/fix-issue-123`, and allocates ports. If the template
   chains an inner Stack template, it's rendered **as files** under
   the workspace tree.
3. **Bring the inner Stack up explicitly.** Workspaces don't manage
   services — start the inner Stack with
   `angee stack up --root workspaces/fix-issue-123/.angee` (or expose
   it over HTTP/GraphQL by running a second operator against that
   root). The same `stack up` / `stack down` / `stack logs` commands
   work on a workspace's inner Stack as on production.
4. **Stay current.** `workspaceSyncBase` merges or rebases each slot's
   workspace branch against its declared base ref so the workspace
   doesn't drift from `main` while you work.
5. **Push.** `workspacePush` pushes every slot's workspace branch
   upstream; `workspaceSourcePublish` is the slot-level
   `--set-upstream` variant for first publication.
6. **Sync the production Stack.** The production root pulls those same
   Sources at the new ref (`sourcePull`) and the operator brings the
   Stack up via `stackUp`.

Stack and Workspace templates are the only place where the deployment
*shape* (which Services, which ports, which Sources) is declared.
Everything else is just running them.

## Where to next

- [Getting started](/guide/getting-started) — install and first commands.
- [Manifest](/guide/manifest) — `angee.yaml` schema and substitutions.
- [Templates](/guide/templates) — how Stack and Workspace templates are
  resolved and what the `_angee` metadata block declares.
- [Commands](/guide/commands) — full CLI surface.
- [Operator API](/reference/operator-api) — REST + GraphQL transports.
