# Angee v2 Rebuild Plan

Status: proposed plan to execute the design in `refactor/OVERVIEW-v2.md`.
The two documents are aligned — this plan is the execution sequence; the
overview is the target shape.

## Scope recap

**Angee provisions workspaces, fetches sources, resolves secrets, runs git
operations on worktree-backed sources, supervises services. It does not
know what an "agent" is.**

Anything an agent needs (MCP wiring, model API keys, addressability,
agent-specific TTL semantics, "talk to me over HTTP", PR creation) is
the application layer's job. (Workspace TTLs *are* core v1 — Angee
sweeps expired workspaces; the carve-out is on agent-shaped state, not
on TTL semantics in general.) The
application layer asks Angee for two things:

1. A **workspace** — a directory rendered from a chain of Copier templates
   with sources mounted into it.
2. A **service** — a runnable workload (container or local process) that
   mounts that workspace, gets env vars, and is supervised.

Everything else above that lives in the application (Django, etc.).

### Litmus test: an agent-free stack must work end-to-end

A `angee.yaml` with only `services:` and `jobs:` (no `workspaces:`, no
`sources:`, no template chain, no inner ANGEE_ROOT) must be fully
supported. `angee dev` on such a manifest never touches a single line of
workspace, source, or template-chain runtime code. Copier is still used by
`stack init`; after init, workspaces and sources are **optional features
layered on top** of the plain service-supervision substrate, not
load-bearing for the dev loop.

Concretely: Phases 3–4 ship a complete, useful Angee — postgres + redis +
Django + Vite under one command, secrets resolved, supervised by docker
compose + process-compose. Phase 5 adds workspaces for callers that want
them. A user who only wants a tidy compose-plus-host-processes dev loop
never has to learn what a workspace is.

## Concepts (six, not seven)

| Concept | Meaning |
|---|---|
| Stack | The environment for one `ANGEE_ROOT`: sources, workspaces, services, jobs, secrets, ports, volumes. |
| Source | A typed pool entry: `git`, `template`, `archive`, `url`, `local`, `volume`. Declared once at the stack and referenced by name from workspaces. The compose-`volumes:` analogue. |
| Workspace | A materialized directory under `$ANGEE_ROOT/workspaces/<name>/`. Rendered from a chain of Copier templates with sources mounted at subpaths. Mountable into one or more services. |
| Service | A runnable workload. `runtime: container` → docker compose. `runtime: local` → process-compose. Declares env, secrets, port pins, and mounts (workspaces, sources, volumes). |
| Job | A one-shot or scheduled command. Same mounts/env semantics as a service. |
| Operator | The reconciler. Renders workspaces, resolves secrets, compiles backend files, applies runtime changes, exposes HTTP + MCP. |

Notably absent: **Agent**. There is no agent noun, no agent CLI command, no
agent HTTP endpoint, no per-agent MCP wiring, no agent addressability. The
old "Agent" composite is rebuilt at a higher layer by combining a workspace
with a service.

## What Angee gives an "agent" (without naming it)

When the application wants what OVERVIEW-v2 called an agent, it asks Angee
for two things:

1. `POST /workspaces` — provision a workspace from a template (or chain of
   templates) with declared sources (e.g. `kind: git, mode: worktree`),
   inputs, and any inner ANGEE_ROOT chained underneath.
2. `POST /services` — start a service that mounts that workspace and
   receives env vars (operator URL, secrets, ports, workspace path,
   addresses of stack peers).

Everything that distinguishes "an agent" from "a process with a directory
and some env vars" — MCP server lifecycle, model selection, message routing,
TTL semantics, name-derivation rules, addressability proxies — lives in the
caller. Angee doesn't render `.mcp.json`, doesn't know what Playwright is,
doesn't keep a service registry keyed by "agent type".

If an MCP server needs to run alongside an agent, the caller declares it as
a *separate service* (or a sidecar) in the same stack and lets Angee
supervise it like any other service.

## Reuse stance (unchanged from OVERVIEW-v2)

| Layer | Reused tool |
|---|---|
| Container runtime | `docker compose` (shell out) |
| Local-process supervisor | `process-compose` binary + REST API |
| Templating | `github.com/fyltr/copier-go` (in-tree at `../copier-go`) |
| Secrets backends | `.env` and OpenBao native (Backend interface for the rest) |
| Worktrees | `git worktree add` via `os/exec` |

Angee's only novel pieces are:
- The `angee.yaml` schema (sources + workspaces + services + jobs + secrets + ports).
- The `${ns.path | filter}` substitution resolver.
- The two-pass compile rule (resolved secrets never in committed files).
- The workspace materialization pipeline (template chain + source mounts +
  optional inner ANGEE_ROOT).

## Architecture

```
angee CLI (cobra)
    │
    ▼
service.Platform   ◄── one business-logic layer; CLI/HTTP/MCP are thin adapters
    │
    ├─► docker compose                       (shell out — containerized services)
    ├─► process-compose binary + REST API    (shell out + HTTP — local processes)
    ├─► copier-go (library)                  (in-process — template render + update)
    ├─► git CLI                              (shell out — clones, worktrees, fetches)
    └─► secrets backend                      (in-process — .env | openbao)
```

Same dispatch rule as OVERVIEW-v2: CLI default is in-process Platform;
`--operator` / `ANGEE_OPERATOR_URL` switches to HTTP.

## Command surface

```sh
# Stack
angee init --dev [path]                   # alias for: angee stack init dev [path]
angee stack init <template> [path] [--input k=v ...]
angee stack update
angee stack destroy

# Runtime power (literal compose / process-compose shims)
angee build [service...] [--no-cache] [--pull]   # docker compose build pass-through
angee up    [service...] [--build]               # compose-only; services only
angee down                                       # both backends
angee start   <service>...                       # service-only (workspaces use `workspace start`)
angee stop    <service>...
angee restart <service>...
angee logs    [service...]
angee status                                     # alias: ls, ps; shows services + workspaces + jobs
angee dev   [--build]                            # compose + process-compose + in-process operator

# Services — runtime inferred from flags. Container needs --image (and optional
# --command override); local needs --command (and rejects --image).
angee service init <name> [--runtime <container|local>]
                          (--image img [--command ...] | --command ...)
                          [--mount URI ...] [--env K=V ...] [--port spec ...]
                          [--workdir URI] [--start]
angee service update <name> [--image img] [--command ...] [--env K=V ...]
                            [--mount URI ...] [--port spec ...] [--workdir URI]
angee service destroy <name>
angee service start|stop|restart|logs <name>
angee service list

# Jobs
angee job run <name> [--input k=v ...]
angee job list
angee job logs <name>

# Workspaces (the only novel provisioning primitive)
angee workspace create <template> [--input k=v ...] [--name <name>] [--ttl <dur>] [--start]
angee workspace update <name> [--input k=v ...] [--ttl <dur>] [--sync-sources]
angee workspace destroy <name> [--purge|--force]
angee workspace start|stop|restart <name>   # only meaningful when chain produces an inner root
angee workspace logs <name>
angee workspace list
angee workspace get <name>                  # path, sources, mounted-by, status, ttl
angee workspace push <name> [--ref <r>]     # Phase 5b — push every worktree-mode source
angee workspace git  <name>                 # Phase 5b — status of every worktree-mode source

# Sources (declared in angee.yaml; CLI is read + git ops on the cached source)
angee source list                           # Phase 5a
angee source fetch  <name>                  # Phase 5a — git fetch on the cache
angee source status <name>                  # Phase 5a — branch / dirty state (read-only)
angee source pull   <name> [--ref <r>]      # Phase 5b — adds pull semantics on the 5a auth machinery
angee source push   <name> [--ref <r>]      # Phase 5b

# Operator
angee operator [--bind 0.0.0.0] [--port 9000]
```

There is no `angee agent`. Anything that command would have done is
expressible as `angee workspace create` followed by `angee service init`.

## Manifest sketch

```yaml
version: 1
kind: stack
name: angee-notes

operator:
  url: http://127.0.0.1:9000
  token_secret: operator-token
  port_pool:
    web:        { range: "8200-8299" }
    custom:     { range: "10000-10999" }

secrets_backend:
  type: env-file
  path: .env                           # relative to $ANGEE_ROOT

secrets:
  postgres-password: { generated: true, length: 32 }
  operator-token:    { generated: true, length: 32 }
  anthropic-api-key: { required: true, import: env:ANTHROPIC_API_KEY }

ports:
  django: { value: 8100, export_env: DJANGO_PORT }

volumes:
  app-data: { driver: local-fs, path: data }   # → $ANGEE_ROOT/data

sources:
  django-angee:
    kind: git
    repo: ../django-angee                      # parent of $ANGEE_ROOT
    default_ref: dev
    cache_path: sources/django-angee           # → $ANGEE_ROOT/sources/django-angee
  runner-skills:
    kind: git
    repo: https://github.com/fyltr/runner-skills
    default_ref: main

workspaces:
  feat-issue-123:
    template: workspaces/pr            # entry-point only; chain comes from its _angee metadata
    inputs:
      branch: feat/issue-123
      base_branch: dev
    sources:
      code:
        source: django-angee
        mode:   worktree
        branch: feat/issue-123
        subpath: code
      skills:
        source: runner-skills
        subpath: skills
    resolved:                          # written by operator at create time
      chain:
        - workspaces/pr                # _angee.kind: workspace
        - stacks/angee-notes-dev       # _angee.kind: stack
      chain_root: code/.angee          # → workspaces/feat-issue-123/code/.angee/

services:
  postgres:
    runtime: container
    image: postgres:16
    env: { POSTGRES_PASSWORD: "${secret.postgres-password}" }

  agent-claude-feat-issue-123:           # caller chose the name
    runtime: container
    image: ghcr.io/example/claude-runner:latest
    mounts:
      - workspace://feat-issue-123:/workspace
      - source://runner-skills:/skills:ro
    env:
      ANTHROPIC_API_KEY:   "${secret.anthropic-api-key}"
      ANGEE_OPERATOR_URL:  "${operator.url}"
      WORKSPACE_PATH:      "${workspace.feat-issue-123.path}"

jobs:
  migrate:
    runtime: local
    mounts: [workspace://feat-issue-123:/workspace]
    command: ["uv", "run", "python", "manage.py", "migrate", "--noinput"]
```

The `agent-claude-feat-issue-123` service is just a service. The fact that
it runs an agent process is invisible to Angee. The application layer (or a
human) chose the name, declared the image, and wired the env vars.

## Substitution grammar (trimmed)

Same `${ns.path | filter}` shape, fewer namespaces. `connector` and any
agent-scoped flavors are gone.

| Form | Resolver |
|---|---|
| `${secret.name}` | Secrets backend (rewritten to `${ENV_VAR}` in committed files; resolved into gitignored `.env`). |
| `${service.name}` / `.host` / `.port` | Service registry; host portion rewritten per consumer location. |
| `${ports.name}` | Stack `ports:` block. |
| `${alloc.pool}` | Operator port pool, allocated at workspace/service init. |
| `${workspace.name.path}` | Absolute path of a named workspace on disk. |
| `${source.name.path}` | Absolute path of a fetched source's local cache. |
| `${persist.<key>}` | Absolute path to a template-declared persistent dir (see workspace `persist_paths:`). |
| `${operator.url}`, `${operator.domain}` | Operator URL; host portion rewritten per consumer. |
| `${inputs.key}` | Provisioning-time inputs. |
| `${name}` | Resolved instance name (workspace or service). |

Filters: `slug`, `lower`, `upper`, `local_part`, `truncate(n)`, `default('x')`,
`required('msg')`, `b64encode`, `replace(a,b)` (same set as
`refactor/examples-v2/SUBSTITUTIONS.md`).

The two-pass compile rule from OVERVIEW-v2 stands: resolved secrets never
land in a committed file. Committed files use `${ENV_VAR}` placeholders;
gitignored `.env` carries the resolved values at `0600`.

## Workspace lifecycle (replaces agent provisioning)

Authoritative sequence for `workspace create` lives in
[`OVERVIEW-v2.md` §Goal 2: Workspace Provisioning](./OVERVIEW-v2.md).
Brief, kept aligned with that source:

1. **Load the entry-point template** the caller named; resolve the full
   chain from its `_angee.chain` (entry-point is implicitly first).
   Validate: at most one `kind: stack`; if any, `_angee.chain_root`
   required.
2. **Compute instance name** from `--name` or entry-point's
   `_angee.instance_naming.pattern`.
3. **Allocate ports** for `${alloc.<pool>}` references.
4. **Resolve / generate secrets** in the backend.
5. **Materialize sources** into `$ANGEE_ROOT/workspaces/<name>/<subpath>/`
   (worktree / clone / copier / archive / local / volume per `kind:`).
6. **Render the chain** with `copier-go`, two passes per entry. Render
   destination per `_angee.kind`: `workspace` → workspace root;
   `stack` → `<workspace-root>/<chain_root>/`.
7. **Prepare the inner ANGEE_ROOT** with `service.Platform.StackPrepare(<inner-root>)`
   (not `StackInit` — chain step 6 already produced the manifest).
   Resolves secrets, allocates inner ports, compiles backend files. Does
   not start runtimes.
8. **Register in the outer `angee.yaml`** under `workspaces.<n>` with
   the resolved chain + chain_root, port leases, source materializations,
   `ttl_expires_at`. Write workspace `.env`.

`workspace create --start` runs `workspace start` immediately after
step 8. `workspace start` itself uses the `_angee.chain_lifecycle`
declared by the entry-point template (`auto`/`dev`/`up`); `auto`
auto-detects from inner-stack runtimes.

`workspace start` (only meaningful when `chain_root:` is set) brings the
inner stack up via the matching lifecycle trigger (`StackUp` for
all-container inner stacks, `StackDev` if the inner stack has any
`runtime: local` services). `workspace stop` brings it down. `workspace
destroy` reverses, optionally removing the worktree and persistent dirs
with `--purge`.

That's it. Eight steps, zero of which mention agents, MCP, or
addressability. Each step is testable in isolation.

## Service lifecycle (unchanged from compose semantics)

`service init` is a manifest edit + first-time render:

1. Validate that all referenced workspaces / sources / volumes / secrets /
   ports exist.
2. **Materialize any referenced source caches that aren't yet on disk.**
   A service that mounts `source://django-angee:/code:ro` against a
   declared but un-fetched source triggers a `git clone` (kind: git) or
   download (kind: archive/url) at this step. This is the same
   fetch-on-use rule `StackPrepare` follows — services and workspaces
   share one source-cache materialization path. If the source's `auth`
   block can't be resolved (missing secret, wrong mode for the bind),
   fail validation here with a clear error.
3. Resolve substitutions for the service entry.
4. Append to `docker-compose.yaml` (container) or `process-compose.yaml`
   (local).
5. If `--start`, call the runtime backend's `Up(<service>)`.

`service start/stop/restart/logs` route by `runtime:` to compose or
process-compose. `service destroy` removes the entry from `angee.yaml` and
calls the runtime backend's `Stop` + `Remove`.

There is no special handling for "agent services" because there is no such
category.

## Mount syntax

See OVERVIEW-v2 §Mount syntax for the full table. Summary: container
services bind URIs as Linux mounts; **local services receive env vars
in the form `WORKSPACE_<N>_PATH`, `SOURCE_<N>_PATH`, etc.** (one per
mount, with the URI's `:/path` portion as documentation only).
`workdir:` is a separate explicit field that resolves against the same
URI scheme.

## Architecture invariants (kept from OVERVIEW-v2)

- One operator process serves HTTP/MCP for exactly one `ANGEE_ROOT`.
- Reconciliation against any single root takes a per-root advisory lock at
  `<root>/run/operator.lock`. Inner-root reconciliation (when a workspace
  has `chain_root:`) is library calls into `service.Platform.Stack*` against
  the inner root, each call taking that inner root's lock.
- `service.Platform` is the only business-logic layer. CLI, HTTP, MCP are
  thin adapters.
- `angee up` is compose-only. `angee dev` is compose + process-compose +
  in-process operator.
- Resolved secrets never enter committed files.
- No backward-compatibility branches in active code. Old paths live in
  `_deprecated/`.

## Phase plan

The phases compress because Goal 2 is now an order of magnitude smaller.

### Phase 1 — Quarantine the old code

Same as before. `git mv` everything from `cli/`, `cmd/`, `internal/`,
`api/`, `templates/`, `examples/` into `_deprecated/`. Leading underscore
so Go's tooling skips the tree for `./...`.

`cmd/angee/main.go` and `cmd/operator/main.go` are minimal stubs (`angee
version`; operator prints "not implemented"). `Makefile` strips down. `make
build && make test && make lint` is green against an essentially empty
tree.

`_deprecated/README.md` documents the rule: read-only reference; copy
forward into the new tree; never `import` from `_deprecated/`.

### Phase 2 — Skeleton

```
api/                              # request/response types shared by CLI/HTTP/MCP
internal/
  manifest/                       # v2 angee.yaml types: stack, source, workspace, service, job
  substitute/                     # ${ns.path | filter} resolver
  secrets/                        # Backend interface + .env impl
  ports/                          # in-memory pool, leases persist in angee.yaml
  copierx/                        # thin wrapper around github.com/fyltr/copier-go
  git/                            # `git worktree add` + `git clone --depth 1` shims
  fslock/                         # per-root advisory lock
  service/                        # service.Platform — the only business-logic layer
  runtime/
    backend.go                    # Runtime interface
    compose/                      # docker compose adapter (Phase 3)
    proccompose/                  # process-compose adapter (Phase 4)
  operator/                       # HTTP + SSE + MCP (Phase 6)
  cli/                            # cobra commands; thin adapters on service.Platform
cmd/
  angee/
  operator/
templates/                        # seeded from refactor/examples-v2 once Phase 5 is real
```

`service.Platform` exposes one method per operator action (no agent
methods):

```
Stack:      StackInit, StackUpdate, StackPrepare, StackBuild, StackUp,
            StackDev, StackDown, StackDestroy, StackStatus, StackLogs
Service:    ServiceInit, ServiceUpdate, ServiceBuild, ServiceStart,
            ServiceStop, ServiceRestart, ServiceLogs, ServiceList,
            ServiceGet, ServiceDestroy
Job:        JobRun, JobLogs, JobList
Workspace:  WorkspaceCreate, WorkspaceUpdate, WorkspaceStart,
            WorkspaceStop, WorkspaceRestart, WorkspaceLogs,
            WorkspaceDestroy, WorkspaceList, WorkspaceGet,
            WorkspacePush, WorkspaceGitStatus
Source:     SourceList, SourceFetch, SourceStatus, SourcePull, SourcePush
Operations: OperationGet, EventStream  (in-memory; SSE)
```

`StackInit` vs `StackPrepare`: `StackInit(<root>, <template>, <inputs>)`
renders a stack template into a *fresh* root and then prepares it
(compiles backend files, resolves secrets, allocates ports).
`StackPrepare(<root>)` operates on an *existing* `angee.yaml` under
`<root>` — no template rendering. The workspace creation flow uses
`StackPrepare` for the inner ANGEE_ROOT (the chain render in step 6
already produced the manifest). The CLI's `angee init` / `angee stack
init` use `StackInit`. This split avoids double-render when a workspace
template chain emits an inner stack.

`StackBuild` and `ServiceBuild` are compose-shims (`docker compose
build`) and operate only on `runtime: container` services with a
`build:` directive. `StackUp` and `StackDev` accept a `Build bool` field
on their request struct so `angee up --build` and `angee dev --build`
route through one code path.

Tests cover: manifest round-trip, substitution grammar (every namespace
listed above), secrets backend (env-file), port pool, fslock contention.
No subprocess is spawned yet. `angee internal stack compile` prints the
resolved compose + process-compose YAML to stdout — enough to drive
Phase 3 acceptance tests.

### Phase 3 — Goal 1 container slice (`angee up` / `build` / `down` / `logs`)

`internal/runtime/compose/`: emit `docker-compose.yaml` from a manifest
with `runtime: container` services only, two-pass secret split, shell out
to `docker compose`. CLI adds `init`, `stack init`, `build`, `up`,
`down`, `logs`, `status`, `start`, `stop`, `restart`, `service list`.
`build` is a literal `docker compose build` shim; `up --build` and
`dev --build` pass through the same flag.

**Dynamic service ops (container path).** `service.Platform.ServiceInit`,
`ServiceUpdate`, `ServiceDestroy`, `ServiceBuild` for `runtime: container`
land here. `ServiceInit` is a manifest edit + reconcile: validate the
spec, resolve substitutions, append to `docker-compose.yaml`, and (if
`--start`) call the runtime backend's `Up(<service>)`. CLI surface:
`angee service init|update|destroy|start|stop|restart|logs`.

After this phase, dynamic **container** service init/update/destroy
works against a manifest that has no workspaces (services declared
inline, ad-hoc additions over HTTP). The full `workspace create +
service init` composition the success-shape demonstrates is **not**
runnable yet — workspaces don't land until Phase 5. Phase 3 only
delivers the service half of that composition.

Acceptance test: postgres + redis stack comes up cleanly; a service with
a `build:` directive (e.g., a tiny Dockerfile referencing the local
context) builds via `angee build` and then runs via `angee up`; secrets
land in gitignored `.env`, absent from any committed file; `down` cleans
up. Plus: `angee service init my-extra --image alpine:3 --command ['sleep','3600']
--start` adds a service to the running stack and `angee service destroy my-extra`
removes it, with no `dev`/`up` restart in between.

### Phase 4 — Goal 1 local slice (`angee dev`)

`internal/runtime/proccompose/`: emit `process-compose.yaml`, start the
process-compose binary in daemon mode, drive lifecycle through its REST
API. Address rewriting in `internal/substitute/` (the consumer's runtime
location now matters). In-process operator HTTP+MCP listener, loopback
only, bound for the lifetime of `angee dev`. Env injection
(`ANGEE_OPERATOR_URL`, `ANGEE_OPERATOR_TOKEN`, port env vars) into every
supervised process.

**Dynamic service ops (local path).** `ServiceInit` etc. extended for
`runtime: local`: append to `process-compose.yaml`, hot-add via
process-compose's REST API. After this phase, dynamic service init
works for both runtimes. Acceptance: a stack with both `runtime: container` and
`runtime: local` services brings everything up under `angee dev`; `curl
$ANGEE_OPERATOR_URL/healthz` works from the local process; Ctrl+C tears
down cleanly; container sidecars persist across `dev` runs.

### Phase 5 — Goal 2a: workspaces + sources (read path)

The novel primitive, much smaller than the previous agent provisioner.
This phase ships everything needed to materialize a workspace and mount
it into a service. Git **write** operations land in Phase 5b.

1. `internal/manifest/` extends with `sources:`, `workspaces:`, mount URI
   scheme, and `_angee.inputs` / `instance_naming` template metadata.
2. `internal/git/` — read-side git operations: `clone`, `fetch`,
   `worktree add`, `worktree list`, `status`. Per-source advisory lock
   so concurrent worktree-adds against the same cache serialize.
   **Auth wiring lands here, not in 5b** — clone/fetch/worktree-add
   on a private repo need credentials just as much as push does.
   `auth.mode: ssh` (key materialized to a 0600 tempfile under
   `<root>/run/`), `auth.mode: https-token` (credential helper),
   `auth.mode: host` (loopback-bound operators only) all ship in 5a.
   Phase 5b adds *write*-path semantics (push pre-flight checks,
   conflict detection on pull) on top of the same auth machinery. A
   stack with only `kind: local` or public-`kind: git` sources
   technically doesn't need auth at all in 5a, but the auth path is
   a hard requirement for any private-repo workspace and ships with
   the read primitive, not after.
3. `service.Platform.WorkspaceCreate / WorkspaceUpdate / WorkspaceStart /
   WorkspaceStop / WorkspaceRestart / WorkspaceLogs / WorkspaceDestroy /
   WorkspaceList / WorkspaceGet` — the eight-step create sequence above
   plus the lifecycle methods. `WorkspaceRestart` is `Stop` + `Start` on
   the inner stack (no-op if there is no inner stack); `WorkspaceLogs`
   tails inner-stack logs (`StackLogs(<inner-root>)`; returns "no
   inner stack" if the workspace has none).
4. `service.Platform.SourceList / SourceFetch / SourceStatus` — read &
   refresh source caches; report branch + dirty state of every workspace
   worktree off a source.
5. Service-side mount resolution: `mounts:` URIs translate into compose
   `volumes:` entries (container) or `WORKSPACE_<N>_PATH=...` env vars
   (local). `workdir:` is an explicit field on services that resolves
   against the same URI scheme.
6. Template-chain rendering rules: the **entry-point template** the
   user names declares the rest of the chain in its `_angee.chain`
   metadata; the user/API never passes the chain explicitly. Each
   entry's `_angee.kind` decides the destination (`workspace` →
   workspace root; `stack` → `<workspace-root>/<chain_root>/`, where
   `chain_root:` is also declared on the entry-point template). At
   most one `kind: stack` entry per chain. Per-entry answers files at
   `<destination>/.copier-answers.yml`. The resolved chain is
   persisted under `workspaces.<n>.resolved.chain` for reproducibility.
7. Inner-ANGEE_ROOT support: when the chain produced an inner manifest,
   the workspace flow calls `service.Platform.StackPrepare(<inner-root>)`
   (no template re-render). `StackPrepare` resolves secrets, allocates
   inner ports, and compiles backend files. Lifecycle triggers route
   to `Stack*(<inner-root>)` calls under the inner root's advisory
   lock. Auto-detection picks `StackDev` if any inner service has
   `runtime: local`, else `StackUp`. Override via `_angee.chain_lifecycle:
   dev | up` on the entry-point template.
8. TTL fields on workspace records (`ttl`, `ttl_expires_at`) accepted
   on create/update. The sweep loop lands in Phase 6.
9. Destroy safety guard: refuse `WorkspaceDestroy` when the resolved
   manifest has any running service or job referencing the workspace
   via `mounts:`, `workdir:`, or `${workspace.<n>...}` env values.
   `--force` overrides; `--purge` implies `--force`.
10. Acceptance test: a workspace template (seeded from
    `refactor/examples-v2/templates/workspaces/pr/`) renders into
    `$ANGEE_ROOT/workspaces/feat-x/` with a git worktree at `code/`, an
    inner `angee-notes-dev` stack at `code/.angee/`, and the workspace
    shows up in `angee workspace list`. Then a separate
    `agent-claude-feat-x` service mounting that workspace starts cleanly
    via `angee service init --image ... --mount workspace://feat-x:/workspace
    --start`. The `--runtime container` is implicit because `--image` is
    set.

### Phase 5b — Goal 2b: git write path on sources

Once worktrees exist, agents and humans commit into them. The operator
needs to ship those commits without the application shelling onto the
host. Deferrable past Phase 5 only if the deployment model is read-only
sources or local-dev only — for any workflow that involves an agent
opening a PR, this phase is mandatory.

1. `internal/git/` adds: `pull`, `push`, per-worktree branch tracking,
   conflict detection (refuse fast-forward-only ops on conflict).
   Reuses the auth wiring shipped in Phase 5a (ssh / https-token /
   host); no new credential machinery here.
2. Pull/push semantics:
   - `pull` is fast-forward-only by default. Detect divergence and
     fail with a clear "agent should rebase or merge inside the
     worktree" message rather than auto-merging.
   - `push` pre-flight: refuse if the worktree has unstaged or
     uncommitted changes, or if the branch has no upstream and the
     caller didn't pass `--set-upstream`.
3. `service.Platform.SourcePull / SourcePush / WorkspacePush /
   WorkspaceGitStatus`. (`SourceStatus` shipped in Phase 5a; it reads
   only local repository state — `git rev-parse`, `git status` —
   so it didn't need the auth machinery even though Phase 5a wired
   it for clone/fetch.)
4. CLI: `angee source pull|push`, `angee workspace push|git`.
5. HTTP: `POST /sources/{n}/{pull|push}`, `GET /workspaces/{n}/git`,
   `POST /workspaces/{n}/push`. (`GET /sources/{n}/status` shipped in
   Phase 5a.)
6. Operator commit identity: `sources.<name>.git.user_name` /
   `git.user_email` apply only to commits the operator itself makes (e.g.
   automated `copier update` rebases). Agent-driven commits use whatever
   identity the agent configures inside the worktree.
7. Pre-flight checks on `push`: refuse if the worktree has unstaged
   changes (the agent should commit first); refuse if the branch has no
   upstream and no `--set-upstream` argument was provided.
8. Acceptance test: a workspace with a worktree-backed source on a private
   GitHub repo authenticated via `auth.mode: ssh` and an SSH key in the
   secrets backend → a commit made inside the worktree by `git -C
   workspace/<n>/code commit -am 'x'` is visible to `angee workspace
   git <n>` and successfully ships via `angee workspace push <n>`.

PR creation stays out of scope. An agent that wants to open a PR runs
`gh pr create` inside the worktree; the operator just makes sure the
push succeeded and the branch is reachable.

### Phase 6 — Operator standalone (HTTP + MCP)

`cmd/operator/` becomes a real daemon. Same server type as Phase 4's
in-process listener, bound on a non-loopback address; bearer auth required.
HTTP surface (no agent endpoints):

```
POST   /stack/init                      POST /stack/update
POST   /stack/{build|up|dev|down|destroy}     GET  /stack/status
GET    /stack/logs                                  # tails all services
POST   /services                        PATCH /services/{n}
POST   /services/{n}/{start|stop|restart|destroy}
GET    /services/{n}                    GET  /services/{n}/logs       GET /services
POST   /jobs/{n}/run                    GET  /jobs                    GET /jobs/{n}/logs
POST   /workspaces                      PATCH /workspaces/{n}
POST   /workspaces/{n}/{update|start|stop|restart|destroy}
GET    /workspaces/{n}                  GET  /workspaces
GET    /workspaces/{n}/logs
GET    /sources                         POST /sources/{n}/{fetch|pull|push}
GET    /sources/{n}/status              GET  /workspaces/{n}/git
POST   /workspaces/{n}/push
GET    /operations/{id}                 GET  /events                  (SSE)
GET    /mcp                             (MCP JSON-RPC; same operations as tools)
```

No `/agents/*`. Async provisioning works the same way (202 +
`operation_id` + SSE) for `POST /workspaces`.

**Workspace TTL sweep — implementation lives here.** The sweep is
implemented as `service.Platform.startSweepLoop()` and ships **in this
phase**. Once it lands, every operator process — the in-process operator
under `angee dev` (Phase 4) and the standalone daemon (this phase) —
calls `startSweepLoop()` at startup and runs the sweep. The behavior
isn't gated on which mode you're in; the implementation phase is just
the one phase. Pre-Phase-6 versions of the in-process operator have no
sweep loop at all; expired workspaces accumulate until the operator
process is restarted on a Phase-6+ binary and catches them on the next
tick. The sweep iterates every minute over workspaces with
`ttl_expires_at < now()` and calls `WorkspaceDestroy` on each (skipping
any that hit the destroy safety guard, with a warning log +
`workspace.destroy_blocked` SSE event).

Acceptance: a tiny external app (could be Django, could be `curl`)
provisions a workspace, then a service that mounts it, watches
`/events` for readiness, and reaches the running service over its
declared port — all without Angee knowing what an "agent" is.

### Phase 7 — Secrets backend pluralism

`internal/secrets/openbao/` lands. KV v2 over `net/http`, no new deps.
Cherry-pick from `_deprecated/internal/credentials/openbao/` if intact.
Switch via `secrets_backend.type` in `angee.yaml`. Acceptance: same Phase 3
stack with `type: openbao` produces identical end-state behavior; gitignored
`.env` is empty; resolved values land in OpenBao.

### Phase 8 — Polish

Workspace TTLs are *not* deferred to this phase. TTL declarations land
in Phase 5 (`ttl_expires_at` on workspace records, accepted on
`WorkspaceCreate` / `WorkspaceUpdate`). The periodic sweep loop lives
in `service.Platform.startSweepLoop()` and runs inside **any operator
process alive for the ANGEE_ROOT** — the in-process operator under
`angee dev` (Phase 4) and the standalone daemon (Phase 6) both pick
it up. Phase 8 is just polish on top.

1. Port lease persistence and release on workspace/service destroy
   (the data structure landed earlier; this phase audits leak paths and
   adds GC for stale leases).
2. `--purge` semantics (worktrees, persistent dirs, inner `.angee/`).
3. Optional `lifecycle.destroy_with: workspace://<n>` on services for
   the "tear this down with the workspace" convenience link discussed
   in Open Question 1.
4. Documentation pass: rewrite `docs/USAGE.md`, `docs/OPERATOR.md`,
   `docs/ARCHITECTURE.md`. Old `docs/REFACTOR.md` becomes obsolete and
   is archived under `_deprecated/docs/`.

## Out of Phase 1–8 (deferred)

Same as before, plus:
- Anything agent-shaped (MCP wiring, agent addressability, `/messages`
  proxy, agent TTL sweep, `_angee.kind: agent` template family) — handled
  by the application layer.
- Connector resolvers (`${connector.x.y}`) — handled by the application
  layer if it wants per-user dynamic credentials.
- Per-template "pretty" CLI flags.

## Cherry-picking guide (per package)

Most of the v1 code that targeted agents is **deleted**, not ported.
What's worth carrying forward:

| Old path | Likely fate |
|---|---|
| `_deprecated/internal/config/angee.go` | Reference for field names; rewritten in `internal/manifest/` (new schema: sources, workspaces, mount URIs, `runtime:` discriminator). |
| `_deprecated/internal/compiler/compose.go` | Reference for the docker-compose mapping; rewritten in `internal/runtime/compose/` for the two-pass secret split + new substitution grammar. |
| `_deprecated/internal/operator/server.go` + `handlers.go` | Reference for routing layout; rewritten in `internal/operator/` against the agent-free API surface. |
| `_deprecated/internal/runtime/compose/backend.go` | Likely reusable for the `docker compose` shell-out; surrounding interface is new. |
| `_deprecated/internal/copier/template.go` | Replaced. New `internal/copierx/` is a thin wrapper over `github.com/fyltr/copier-go`. |
| `_deprecated/internal/git/` | Likely usable for the `git worktree add` shim. |
| `_deprecated/internal/service/platform.go` + `provision.go` | Reference for orchestration order. The agent-specific 12-step flow is **dropped**; what survives lands in `WorkspaceCreate`'s 8 steps. |
| `_deprecated/internal/service/local_runtime.go`, `_deprecated/internal/dev/` | Replaced. Local-process supervision goes through `process-compose`'s REST API. |
| `_deprecated/templates/components/`, `_deprecated/templates/default/` | Replaced. Stack templates seeded from `refactor/examples-v2/templates/stacks/`; workspace templates seeded from `refactor/examples-v2/templates/workspaces/`. |

## Open questions

### 1. Should a workspace template declare services in the *outer* `angee.yaml`?

Two options:

- **A.** A workspace template is purely about the directory contents. The
  caller separately runs `angee service init` to add a service that mounts
  the workspace. Cleaner separation; more steps for the caller.
- **B.** A workspace template can emit an `angee.yaml.fragment.yaml` that
  the operator merges into the outer manifest at workspace create (services,
  jobs, port pins). Fewer caller steps; the workspace template now has
  reach into the outer stack.

Recommend **A** for the v1 cut. It keeps Angee strictly substrate-shaped.
A `POST /workspaces?with_service=...` convenience endpoint can land in
Phase 8 without changing the model.

### 2. ~~Should `auth.mode: host` be allowed in the standalone operator?~~ — **decided**

Resolved: `auth.mode: host` is allowed only when the operator binds on
loopback. The standalone daemon binding to a non-loopback address
refuses the mode at startup with a clear error pointing at the source's
`auth.mode`. The in-process operator under `angee dev` (loopback by
construction) accepts it. This gives developers the "uses my SSH key"
ergonomics locally and refuses to surface the same hole on a hosted
operator. Implementation lives in Phase 5a's auth wiring — same
machinery, gated by the listener's bind address.
