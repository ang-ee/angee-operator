# Proposal: local platform instance — a central operator that hosts workspaces and agents

**Status:** Draft · **Area:** stacks, workspaces, runtime backends, agent-runtimes, gitops · **Surfaces:** new `stacks/local` template + CLI/REST/GraphQL (operator) + Angee provisioning

## Summary

Stand up a **persistent, central local instance** of Angee — the operator running
under docker-compose from a **non-dev "platform" stack template** — that is *the
control plane you develop against*, not a stack you run inside a framework
checkout. From that instance you **create workspaces from the dev template**
(each an isolated, containerized, testable stack over a git worktree), **run
coding agents** (codex / opencode / claude) as per-workspace container services
that develop on those worktrees, and **reconcile/pull** the resulting branches
back into the main instance — then run that circle again.

The shift: today you *clone the framework, render `.angee/` inside it, and run one
project with process-compose*. This makes the instance a **first-class,
long-lived, container-native platform** that manages *many* workspaces and their
agents, with the framework itself just another tracked source (or a wheel image).

Almost every primitive exists. This proposal names the small set of new pieces
and sequences them into milestones, each independently usable.

## The shift, precisely

| | Today (`stacks/dev`) | This proposal (`stacks/local`) |
| --- | --- | --- |
| Where it runs | inside a framework checkout | a self-contained instance dir |
| Runtime | `local` (process-compose) | `container` (docker-compose) |
| Scope | one in-repo project | a control plane hosting **N** workspaces |
| Framework | a source clone on the host | a wheel **image** (or a mounted source, per workspace) |
| Lifetime | brought up for a session | **persistent** — the thing you drive |
| Agents | run by hand / ad hoc | per-workspace **container services** |

This is `prod`-shape wiring pointed at `localhost`: the manifest's "one
`angee.yaml`, two flavors" already anticipates it — `stacks/local` is the docker,
edge-routed, container-runtime flavor.

## What already exists (the seams to reuse)

This is not green field. The operator already provides:

- **Two runtime backends behind one interface**, selected **per service** —
  `runtime.Backend` with `compose` (docker) and `proccompose`
  ([`internal/runtime/backend.go`](../../internal/runtime/backend.go)). A stack can
  mix `container` (operator, db, agents) and `local` (a hot process) freely;
  `Compile` fans each into `docker-compose.yaml` and/or `process-compose.yaml`.
- **A mount model** — `internal/mount` resolves `source://<name>` into a container
  bind, so "the workspace's code, mounted into a service" is already how
  `workdir: source://app` works.
- **Workspace creation as a chain** — `WorkspaceCreate`
  ([`internal/service/workspaces.go`](../../internal/service/workspaces.go))
  materializes declared sources as **git worktrees** on `workspace/<name>`, leases
  ports, and renders a chained stack. The `workspaces/dev` template already chains
  `stacks/dev` per workspace.
- **Agent-runtime services** — `service_create.go` renders an agent service
  (default name `agent-${workspace.name}`) from a template (`templates/agent-runtime`,
  plus `services/claude-code` / `services/opencode` downstream), mounting a
  workspace and driving it over MCP/GraphQL.
- **The reconcile primitives** — `WorkspaceSourceFetch/Pull/Push/Merge/Rebase`,
  `WorkspacePush`, `WorkspaceSyncBase`
  ([`gitops.go`](../../internal/service/gitops.go),
  [`gitops_merge.go`](../../internal/service/gitops_merge.go)) — everything the
  develop→reconcile circle needs, exposed 1:1 over REST.
- **Operator-as-container is packaged** — `Dockerfile.operator` builds an alpine
  image with `docker-cli` + compose + git on `:9000`/`healthz`; the Caddy edge
  already expects `operator:9000` on the edge network. Running the control plane
  itself as a service needs only the docker socket mounted.

And four sibling proposals are load-bearing dependencies (below):
`compose-project-isolation`, `ephemeral-workspace-pool`, `global-source-registry`,
`stack-update-template-sync` — plus `operator-backup-restore` for the data plane.

## The topology

```
stacks/local  (the instance dir — persistent, git-tracked)
├── operator        (container: the control plane; docker.sock mounted; :9000 + edge)
├── edge            (container: Caddy — fronts each workspace's routed services)
├── platform-db     (container: Postgres/pgvector — the instance's own state, optional)
└── hosts, on demand, N workspaces — each its own isolated compose project:
      workspaces/<name>/
      ├── git worktree of a source (e.g. angee-django) on workspace/<name>
      ├── django   (container: base image + workdir: source://app  [deps baked, code mounted])
      ├── vite      (container: pnpm dev, same mount)
      ├── postgres  (container: pgvector — OPT-IN, bind data, leased port)   ← via operator-backup-restore for seed
      └── agent-<name>-<agent>  (container: codex | opencode | claude, mounts /workspace)
```

The instance is the daemon you talk to (CLI/REST/GraphQL/console); workspaces are
namespaced compose projects inside the shared daemon; agents are workloads that
also mutate git.

## Proposal

### 1. `stacks/local` — the non-dev platform stack template

A new stack template (rendered by `angee stack init --template <ref> <root>`)
that emits a **thin instance root**, not a framework:

- `angee.yaml` with **`runtime: container`** services for `operator`, `edge`, and
  (optionally) `platform-db`; the operator service mounts `/var/run/docker.sock`
  and runs `angee operator --root .`.
- `sources:` and `ensure:` (port pools) for the instance itself; secrets via
  env-file (or openbao).
- **No project code.** The framework and any project arrive later as *sources* or
  *images* declared per workspace.

`angee stack init --template github.com/ang-ee/angee-django/tree/main/templates/stacks/local <dir>`
+ `angee up` boots the instance; the containerized daemon takes over. (The
GitHub-HTTPS template-ref path already works; see Design option on refs.)

### 2. The `ghcr.io/ang-ee/django-angee-base` image (built)

The artifact that unblocks everything: a base image with **deps baked, source
mounted** — the container analogue of the `django-angee` wheel.

- **Built and lean**, directly on `python:3.14-slim` + uv (no intermediate
  `docker-django` base): the framework's dependency closure lives in a venv
  **outside** the app root (`/opt/.venv`), so the worktree bind-mounts over `/app`
  in dev while the baked deps survive. Change code → live; change a lockfile →
  `up --build` rebuilds the deps layer. Non-root, `libmagic1` + `tini` only, the
  strawberry ssh forks cloned via a BuildKit ssh mount. Validated: python 3.14.6,
  django 6.0.6, all framework deps import. (angee-django: `Dockerfile` +
  `.github/workflows/publish-base-image.yml`.)
- **Python-only** — Vite/pnpm is a **separate** node image (a workspace runs
  `django` and `vite` as two services), matching how fyltr splits them.
- The **same image runs a wheel** for a downstream project (no mount, `pip
  install django-angee`, addons/settings mounted) — the mount-vs-image duality is
  a choice the *workspace's* stack template makes, not the operator's concern.
  angee-django's de-sibling + config-seam + wheel-portability work (package-name
  `@angee/app/*` refs, `config/` shipped in the wheel) is what makes the wheel
  path viable.

**Prior art — `fyltr-django` proves the whole model.** Its `docker-compose.dev.yaml`
is operator-generated ("Source of truth: angee.yaml"); it runs the operator as a
container (`ghcr.io/fyltr/angee-operator`), `pgvector/pgvector:pg17` with bind
data, and **claude/opencode agents as `node:22-slim` + an ACP CLI + `stdio-to-ws`**
(a WebSocket on `:3007`, mounting `/workspace`). Its Django Dockerfile uses the
same `UV_PROJECT_ENVIRONMENT=/opt/.venv`-outside-the-mount trick. M3's agent
images and the pgvector service are liftable from it almost verbatim; angee's base
is the cleaner, framework-owned equivalent of fyltr's `djangoflow/docker-django`.

### 3. Workspaces as chained, containerized dev stacks

`angee workspace create <name> --template dev` inside the instance:

- materializes the chosen **source** (e.g. angee-django) as a worktree on
  `workspace/<name>` — the framework is *just a source* (`global-source-registry`),
- chains a **`container`-runtime dev stack** (django + vite over the base image,
  `workdir: source://app`), leased its own ports,
- **optionally** adds a `postgres` service (`db: sqlite|postgres`, pgvector, bind
  `persist:` data, leased port) for RAG / real-data work — seeded out-of-band via
  `operator-backup-restore`, never from the template.

Each workspace is an **isolated compose project** (§Isolation), so many run at
once on the one daemon.

### 4. Agents (codex / opencode / claude) as per-workspace services

`angee service create` (or Angee agent provisioning) renders an agent-runtime
container per workspace:

- mounts the worktree at `/workspace`, carries **git push credentials**, and
  reaches the workspace's app + the operator over MCP/GraphQL (the edge routes the
  agent's endpoint),
- named `agent-<name>-<agentslug>` so **multiple agents per workspace** (codex and
  claude side by side) never collide,
- reuses `ephemeral-workspace-pool` for the throwaway "give me an isolated env
  now, reap it later" case.

The agent edits the worktree, runs *that workspace's* containerized stack, tests,
commits, and pushes `workspace/<name>`.

### 5. The circle — develop → test → push → reconcile into main

The develop-and-merge loop, entirely over existing primitives:

1. Agent (or human) works in workspace `X`; tests run green in X's own
   containerized stack (isolated db, ports, network).
2. `WorkspaceSourcePush` publishes `workspace/X`.
3. Reconcile into the main instance: merge `workspace/X` → the source's main
   (PR/`gh`, or `WorkspaceSourceMerge`), and `WorkspaceSyncBase` keeps other live
   workspaces current with the new main.
4. The main instance picks up the merged code (rebuild/restart, or — later — the
   git→apply reconciler in `local-platform-instance` M5).
5. Repeat.

**Data does not travel with the merge** (see `operator-backup-restore`):
migrations run forward against the target's data; snapshots are the rollback net.

## Isolation & naming (the load-bearing correctness)

Running N workspaces on one daemon relies on the `compose-project-isolation` fix
— **already implemented** (`internal/service/compose_project.go`, wired at
`platform.go:235`): the operator **derives the Compose project identity from the
absolute stack root** (`sanitize(name)-sha256(root)[:8]`) instead of `stack.Name`
verbatim, keeping `stack.Name` a friendly label. That one change already makes
the whole tree correct-by-construction:

| Identity | Uniqueness owner |
| --- | --- |
| workspace name | operator-enforced unique **within the instance** (it sees its own) |
| compose project | operator, root hash — globally unique per instance |
| service / container names | in-project (`django`, `agent-<name>-<agent>`) |
| ports | leased per workspace from `operator.port_pool.*` |
| data (incl. postgres) | **bind subpaths** under the root — isolated, rename-safe |

It retires angee-django's interim `name: ${example}-${name}` template hack.

## Ownership (mechanism below, policy above)

| Fact | Owner |
| --- | --- |
| Run services / mount code / lease ports / worktrees | **operator** (generic mechanism) |
| Snapshot/restore data | **operator** `backup.Backend` (declared hooks) |
| Which sources/agents/workspaces exist, who may do what | **Angee** (Django), REBAC-gated |
| Stack/workspace *shape* | templates |
| A service's backup protocol | the service template |

Same split as `global-source-registry`: the operator stays kind-free; Angee owns
meaning and access.

## Milestones (each independently usable)

- **M1 — a containerized, testable workspace stack.** The `django-angee-base` image
  (§2, **built**) + a `container`-runtime dev stack. *New:* the stack variant + a
  Vite node image. *Reuses:* mount model, compose backend, `WorkspaceCreate`, and
  `composeProjectName` (**already implemented**). Result: `angee workspace create X`
  → X's Django/Vite in docker-compose, code mounted, tests runnable inside.
- **M2 — the central instance.** `stacks/local` (§1) + run the operator as a
  persistent daemon (local first; container next) hosting many M1 workspaces.
  Result: Angee is the control plane you drive.
- **M3 — agents in containers.** Per-workspace codex/opencode/claude services (§4)
  with push creds + MCP. Result: agents develop workspaces autonomously.
- **M4 — the reconcile circle.** Wire `push`/`merge`/`sync-base` into the
  develop→test→push→reconcile loop (§5). *Workflow, not new code.*
- **M5 — automate M4 (optional).** The git→converge reconciler (desired-state from
  a stack-root repo, orphan-pruning `Backend`, upsert-not-conflict) so merges
  apply themselves. Deferred; not needed to start.

## Dependencies on angee-django

- **Done:** web scaffold de-sibled to `@angee/app/*` package refs, `config/`
  shipped in the wheel (config seam, hatch-angee ≥0.1.3), the full project
  template — together these make "run the framework from a wheel image, no sibling
  checkout" real; and **`ghcr.io/ang-ee/django-angee-base`** (`Dockerfile` +
  publish workflow), the base image M1 builds on (§2).
- **New (M1):** a `container`-runtime dev stack variant (django + vite over the
  base image, `workdir: source://app`) and a small **Vite node image**. (The
  `composeProjectName` root-hash isolation is already in the operator.)
- **New (M3, liftable from fyltr):** `agent-claude` / `agent-opencode` node images.

## Design options

- **Template refs — framework-as-source vs extend the resolver.** The template
  resolver today accepts only GitHub-HTTPS (`parseGitHubTemplateRef`), not `gh:`
  / SSH / go-getter. Rather than broaden it, prefer **registering the framework as
  a Source** (arbitrary git URLs already work for sources) and rendering the
  `templates/stacks/local` subtree — one mechanism, reuses `global-source-registry`.
  `stack init --template` becomes sugar over "add source + render subtree."
- **Operator: local daemon vs container first.** Start as a persistent local
  daemon (fastest path to M2); containerize it (Dockerfile.operator + docker.sock)
  once the workspace stacks are container-native. Both end at the same place.
- **Postgres: dedicated-per-workspace vs shared-per-database.** Dedicated (full
  isolation for destructive migrations / RAG re-index) is the default for this
  use case; shared-instance-per-database is the efficiency option. Seeding is
  `operator-backup-restore`, not templates.

## Out of scope

- The full **git→apply reconciler** (M5) — sketched here, specified when M1–M4
  land.
- A **kubernetes backend** — a future third `runtime.Backend` impl behind the
  existing seam; docker-compose first.
- Cross-instance/cluster federation.

## Acceptance (the demo that proves the circle)

1. `angee stack init --template <ref> ./instance` + `angee up` → a running central
   instance (operator + edge in containers).
2. `angee workspace create feat-x --template dev` → a containerized, isolated
   dev stack for a worktree of angee-django on `workspace/feat-x`.
3. An agent service (claude/codex/opencode) develops in `feat-x`, runs its stack's
   tests green, and pushes `workspace/feat-x`.
4. Merge `workspace/feat-x` → main; `sync-base` brings a second live workspace
   current — no container/port/data collision between the two.

## See also

- [`compose-project-isolation.md`](./compose-project-isolation.md) — the
  root-hash project identity M1 depends on.
- [`ephemeral-workspace-pool.md`](./ephemeral-workspace-pool.md) — leased/reaped
  agent environments.
- [`global-source-registry.md`](./global-source-registry.md) — framework-as-source;
  the kind-free mechanism/policy split.
- [`stack-update-template-sync.md`](./stack-update-template-sync.md) — re-render
  from template; the M5 reconciler precursor.
- [`operator-backup-restore.md`](./operator-backup-restore.md) — the data plane
  that seeds workspaces and nets experiment rollbacks.
