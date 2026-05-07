# Angee Refactor Overview v2

Status: target clean design. **Major simplification** from earlier drafts —
agents, MCP wiring, and addressability proxies are not core concepts; they
belong to the application layer. Angee's job is reduced to two narrow
primitives: an environment with secrets and supervised services, and
workspaces (rendered template + mounted sources) that those services can
consume.

This document is the target shape; [`PLAN.md`](./PLAN.md) is the execution
sequence. The two are aligned.

If you read an earlier version of this doc, the practical change is: there
is no `angee agent` command, no `/agents/*` endpoint, no `_angee.kind:
agent` template family, no per-agent MCP server lifecycle. What that
surface used to do is now expressed as `angee workspace create` followed
by `angee service init` — two boring steps a higher layer composes.

## The Two Goals

Angee exists to make these two flows boring and reproducible:

1. **Set up an environment with secrets and services.** One manifest, one
   command, get a running stack with secrets resolved and services
   healthy.
2. **Provision a workspace from a template chain with mountable sources.**
   One command renders a template (or chain of templates) into a
   directory, materializes declared sources (git worktrees, clones,
   archives, local paths) at subpaths, and registers the workspace as a
   mountable resource for any service in the stack.

Anything not in service of those two goals is out of scope for v1.

### Litmus test: an agent-free, workspace-free stack must work end-to-end

An `angee.yaml` with only `services:` and `jobs:` is fully supported.
**The runtime path** — `angee dev`, `angee up`, `angee build`, `angee
down`, `angee logs`, `angee start/stop/restart` — never touches workspace,
source, or git-worktree code on such a manifest. Compilation, secrets
resolution, compose + process-compose dispatch, and runtime supervision
are the entire code path.

`angee init` and `angee stack init` *do* go through Copier to render the
initial `angee.yaml` from a stack template. The carve-out is on the
runtime side: workspaces, sources, and the worktree machinery never get
touched if the rendered manifest doesn't declare them.

A user who only wants a tidy compose-plus-host-processes dev loop —
postgres + redis + Django + Vite under one command, secrets resolved,
supervised — never has to learn what a workspace is. That's the simple
case, and Phases 3–4 of the rebuild plan ship it before workspaces and
sources enter the codebase.

## Reuse Stance

Angee is not a new orchestrator. It is a thin Go binary that composes
existing tools and adds one provisioning primitive (the Workspace).

| Layer | Reused tool | Why |
|---|---|---|
| Container runtime | `docker compose` (shell out) | Already does up/down/start/stop/restart/logs/networks/volumes/build. Angee's commands are one-line shims. |
| Local-process supervisor | `process-compose` (shell out + REST API) | Already does depends_on, health checks, readiness probes, env files, TUI, cron, REST API. Angee generates `process-compose.yaml`, starts the daemon, drives it via HTTP. |
| Templating | `github.com/fyltr/copier-go` (in-process Go library, in-tree at `../copier-go`) | Native Go reimplementation of Copier including `copier update`. No Python runtime dependency. |
| Secrets | Pluggable `Backend` interface; v1 implements `.env` and `openbao` natively | Angee owns `${secret.name}` resolution at compile time. The interface is shaped to fit a Teller-compatible adapter if needed later. |
| Worktrees | `git` CLI via `os/exec` | No wrapper library is worth a dependency. |

What stays Angee's:
- The `angee.yaml` manifest schema and the `angee.yaml → docker-compose.yaml`
  + `angee.yaml → process-compose.yaml` compilers.
- The `${ns.path | filter}` substitution resolver.
- The two-pass compile rule (resolved secrets never in committed files).
- **The Workspace provisioning primitive** — one command that renders a
  template chain, materializes sources, and registers an addressable
  directory that services can mount.
- The Operator HTTP + MCP API that exposes stack/service/workspace
  operations to the application layer.

## What Angee deliberately does NOT do

The carve-out is more important than the inclusion list. Angee will not:

- Know what an "agent" is. There is no agent CLI, no agent endpoint, no
  agent in the manifest schema, no agent template family.
- Manage application or agent MCP servers. If an MCP server needs to run
  alongside another process, the application declares it as a service in
  the manifest and Angee supervises it like any other service. Angee
  does not render `.mcp.json`, does not start MCP servers as a side
  effect of provisioning, and does not proxy MCP traffic to
  bundled-with-an-agent MCP processes. (Angee does expose its **own**
  MCP server at `${operator.url}/mcp` — the operator API offered as MCP
  tools so model clients can drive `service.Platform`. That's the
  operator surface, not a managed app/agent MCP.)
- Proxy traffic to "agent" endpoints. Services have ports; ports are
  reachable. There is no `/agents/<name>/messages` proxy.
- Resolve per-user OAuth credentials at runtime. The application layer
  passes resolved values in as inputs/secrets if it needs them.
- Run a TTL sweep over agent-shaped objects. Workspaces *do* have TTLs
  in v1 (declared on creation, extendable via update, swept by the
  operator); the carve-out is that "agent" is not the unit being swept,
  the workspace is.
- Create pull requests. Angee makes sure `git push` works from worktrees
  and exposes status; the agent (or the application calling `gh`) opens
  the PR.

If a feature would only make sense in the context of "running an AI
agent", it lives in the application layer.

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
    ├─► git CLI                              (shell out — clones, worktrees, fetches, push/pull)
    └─► secrets backend                      (in-process — .env | openbao)
```

CLI default is in-process Platform; `--operator` / `ANGEE_OPERATOR_URL`
switches to HTTP. No command implements provisioning or runtime logic
directly.

## Core Concepts

Six concepts. No Agent.

| Concept | Meaning |
|---|---|
| Stack | The environment for one `ANGEE_ROOT`: sources, workspaces, services, jobs, secrets, ports, volumes. |
| Source | A typed pool entry: `git`, `template`, `archive`, `url`, `local`, `volume`. Declared once at the stack and referenced by name from workspaces (and from anywhere else that wants to mount it). The compose-`volumes:` analogue. |
| Workspace | A materialized directory under `$ANGEE_ROOT/workspaces/<name>/`. Rendered from a chain of Copier templates with sources mounted at subpaths. Mountable into one or more services. |
| Service | A runnable workload. `runtime: container` → docker compose. `runtime: local` → process-compose. Declares env, secrets, port pins, and mounts (workspaces, sources, volumes). |
| Job | A one-shot or scheduled command (migrations, fixtures, asset builds). Same mounts/env semantics as a service. |
| Operator | The reconciler. Renders workspaces, resolves secrets, compiles backend files, applies runtime changes, exposes HTTP + MCP. |

## Command Surface

```sh
# Stack
angee init --dev [path]                          # alias for: angee stack init dev [path]
angee stack init <template> [path] [--input k=v ...]
angee stack update
angee stack destroy

# Runtime power (literal compose / process-compose shims)
angee build [service...] [--no-cache] [--pull]   # docker compose build pass-through
angee up    [service...] [--build]               # compose-only; services only
angee down                                       # both backends
angee start   <service> [<service>...]           # service-only (workspaces use `workspace start`)
angee stop    <service> [<service>...]
angee restart <service> [<service>...]
angee logs    [service...]
angee status                                     # alias: ls, ps; shows services + workspaces + jobs
angee dev   [--build]                            # compose + process-compose + in-process operator

# Services — direct manifest edits with service-spec flags. Stack-template-defined
# services land via `angee stack init`/`stack update`; `service init` is for ad-hoc
# additions outside of a stack template.
#
# Runtime selection rules:
#   container service: --image <img> required; --command optional (overrides image CMD)
#   local service:     --command <cmd> required; --image rejected (no image to run)
#   --runtime is optional and inferred from the flags above. Pass it explicitly only
#   to disambiguate or to assert a defensive check.
angee service init    <name> [--runtime <container|local>]
                            --image <img>     [--command <cmd...>]   # container shape
                          | --command <cmd...>                       # local shape
                            [--mount URI ...] [--env K=V ...] [--port spec...]
                            [--workdir URI] [--start]
angee service update  <name> [--image <img>] [--command <cmd...>] [--env K=V ...]
                             [--mount URI ...] [--port spec ...] [--workdir URI]
angee service destroy <name>
angee service start|stop|restart|logs <name>
angee service list

# Jobs — same shape as services, but one-shot. Templates define jobs; `job run` invokes them.
angee job run <name> [--input k=v ...]
angee job list
angee job logs <name>

# Workspaces (the only novel provisioning primitive)
angee workspace create <template> [--input k=v ...] [--name <name>] [--ttl <dur>] [--start]
angee workspace update <name> [--input k=v ...] [--ttl <dur>] [--sync-sources]
                                                   # bare:           rerun copier update on pinned chain
                                                   # --input k=v:    above + update inputs (re-render)
                                                   # --ttl <dur>:    TTL only, no render
                                                   # --sync-sources: also re-fetch sources (Phase 5b)
angee workspace destroy <name> [--purge|--force]
angee workspace start|stop|restart <name>        # only meaningful when chain_root: is set
angee workspace logs <name>                      # tails inner-stack logs when chain_root: is set
angee workspace list
angee workspace get <name>                       # path, sources, mounted-by, status, ttl
angee workspace push <name> [--ref <r>]          # push every worktree-mode source
angee workspace git  <name>                      # status of every worktree-mode source

# Sources (declared in angee.yaml; CLI is read + git ops on the cached source)
angee source list
angee source fetch  <name>                       # git fetch on the source's cache
angee source pull   <name> [--ref <r>]           # git pull on the cache or a specific worktree branch
angee source push   <name> [--ref <r>]           # git push (only meaningful for worktrees writing back)
angee source status <name>                       # branch / dirty state of the cache + every workspace worktree

# Operator
angee operator [--bind 0.0.0.0] [--port 9000]
```

There is no `angee agent`. The combination of `workspace create` +
`service init` covers what that command would have done; the application
layer that already understands "agent" semantics composes the two steps.

### `up` vs. `dev` — hard split

| Command | Container services | Local processes | Use |
|---|---|---|---|
| `angee up`  | `docker compose up -d` | **not started** | Production / staging / sidecars-only. |
| `angee dev` | `docker compose up -d` | `process-compose up -d` (daemon mode; CLI tails) | Local development with the full stack alive. |

`angee up` is **compose-only**. It never starts host processes. If your
`angee.yaml` has only `runtime: container` services, `up` brings up the
whole stack. If it has `runtime: local` services, those are simply not
started by `up` — use `dev` for that.

`angee dev` is **compose + process-compose + in-process operator**. The
`angee dev` CLI command blocks (foreground), tailing logs and handling
Ctrl+C; the underlying `process-compose` runs in **daemon mode** so its
REST API is available for the operator to drive (start/stop/restart/logs
on individual processes). The operator HTTP/MCP server runs in the same
process as `angee dev` on loopback for the lifetime of the session.

**Operator listener vs `StackDev` separation.** The HTTP/MCP listener is
started **once** by `angee dev` (or by `cmd/operator`) before any
`Stack*` call — it serves the configured ANGEE_ROOT and only that one.
`StackDev(<root>)` is a **pure runtime** call: compose up + process-
compose up. It never starts an HTTP/MCP listener. This is what lets the
outer operator drive a chained inner stack via `StackDev(<inner-root>)`
without violating the "one operator process serves HTTP/MCP for exactly
one ANGEE_ROOT" invariant — the inner-root call is runtime-only; nobody
serves HTTP for the inner root.

`angee start/stop/restart/logs <name>` route by `runtime:` of the named
service. `angee down` stops both backends.

The custom logic between user input and the underlying tool is only
secret resolution, port substitution, and address rewriting — not
orchestration.

### `angee build`

Image-construction shim for `docker compose build`. Operates only on
`runtime: container` services that declare a `build:` directive. Local
services don't have images, so `build` skips them with a one-line note;
local services that need a build step (Vite production bundle, Go
binary, asset compilation) declare it as a job and run via `angee job
run <name>`.

Standalone form:

```sh
angee build                  # build all container services with a build: directive
angee build web ui           # build specific services
angee build --no-cache web   # passes through to compose
angee build --pull           # always re-pull bases
```

Or composed with the runtime commands as a `--build` flag, mirroring
compose:

```sh
angee up  --build            # build, then docker compose up -d
angee dev --build            # build container sidecars, then start the dev loop
```

`--build` only rebuilds container services. The `--build` flag on `dev`
does **not** rerun any local-process build steps — those run as jobs
(or as a service with `runtime: local` that exits cleanly).

Build is deliberately **not** part of `start/stop/restart`. Those route
to already-built containers. Once an image exists, the runtime commands
consume it; building is a separate concern.

## Manifest: `$ANGEE_ROOT/angee.yaml`

One file. Source of truth. Rendered by Copier from a stack template; users
may edit it; template updates are the preferred path for structural
changes.

**Path resolution rule.** Every relative path inside `angee.yaml` is
resolved **relative to `$ANGEE_ROOT`** (the directory containing
`angee.yaml`). Use absolute paths or `${...}` substitutions for paths
outside that frame. Paths that step outside `$ANGEE_ROOT` use `..` —
e.g. a `local` source that points at a sibling repo writes
`repo: ../django-angee`. There is no ambient project-root frame to
worry about.

```yaml
version: 1
kind: stack
name: angee-notes

template:
  active: stacks/dev
  answers_file: .copier-answers.yml

operator:
  url: http://127.0.0.1:9000
  token_secret: operator-token
  port_pool:
    web:    { range: "8200-8299" }
    custom: { range: "10000-10999" }

secrets_backend:
  type: env-file                       # env-file | openbao
  path: .env                           # relative to $ANGEE_ROOT → $ANGEE_ROOT/.env

secrets:
  django-secret-key:  { generated: true, length: 50 }
  postgres-password:  { generated: true, length: 32 }
  operator-token:     { generated: true, length: 32 }
  anthropic-api-key:  { required: true, import: env:ANTHROPIC_API_KEY }

ports:
  django: { value: 8100, export_env: DJANGO_PORT }
  react:  { value: 5173, export_env: REACT_PORT }

volumes:
  app-data: { driver: local-fs, path: data }   # → $ANGEE_ROOT/data

sources:
  django-angee:
    kind: git
    repo: git@github.com:example/django-angee.git
    default_ref: dev
    cache_path: sources/django-angee   # → $ANGEE_ROOT/sources/django-angee
    auth:
      mode: ssh                              # ssh | https-token | host
      ssh_key_secret: github-deploy-key      # → ${secret.github-deploy-key}
    git:
      user_name:  "Angee Bot"                # author for operator-authored commits
      user_email: "angee@example.com"
  runner-skills:
    kind: git
    repo: https://github.com/example/runner-skills
    default_ref: main

workspaces:
  feat-issue-123:
    # The entry-point template the user/API picked. The full chain and
    # chain_root come from this template's _angee metadata; the operator
    # persists the resolved values below for reproducibility (so
    # `workspace update` re-renders exactly the same chain even if the
    # entry-point template's _angee block changes upstream).
    template: workspaces/pr
    inputs:
      branch:      feat/issue-123
      base_branch: dev
    sources:
      code:
        source: django-angee
        mode:   worktree
        branch: feat/issue-123
        subpath: code                  # → workspaces/feat-issue-123/code/ (the worktree)
      skills:
        source: runner-skills
        subpath: skills
    # Resolved fields below — written by the operator at create time, not by the user.
    resolved:
      chain:                           # comes from workspaces/pr's _angee.chain
        - workspaces/pr                # _angee.kind: workspace — renders into workspaces/feat-issue-123/
        - stacks/dev                   # _angee.kind: stack     — renders into <workspace>/<chain_root>/
      chain_root: code/.angee          # comes from workspaces/pr's _angee.chain_root
    ttl: 24h                           # optional; expired workspaces get auto-destroyed

services:
  postgres:
    runtime: container
    image: postgres:16
    env: { POSTGRES_PASSWORD: "${secret.postgres-password}" }

  web:
    runtime: local                     # → process-compose
    workdir: workspace://feat-issue-123/code
    command: ["uv", "run", "python", "manage.py", "runserver", "127.0.0.1:${ports.django}"]
    after: [migrate]

  agent-claude-feat-issue-123:         # caller chose the name; just a service
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
    workdir: workspace://feat-issue-123/code
    command: ["uv", "run", "python", "manage.py", "migrate", "--noinput"]
```

The `agent-claude-feat-issue-123` service is just a service. The fact
that it runs an agent process is invisible to Angee. The application
layer (or a human) chose the name, declared the image, and wired the env
vars.

## Goal 1: Environments with Secrets + Services

The flow, end to end, on a stack with no workspaces:

```sh
angee init --dev --yes        # copier render → angee.yaml + .copier-answers.yml
angee dev                     # local dev: compose sidecars + process-compose locals + operator
# ── or, for prod/staging where every service is runtime: container ──
angee up                      # docker compose up -d (containers only; no process-compose)
angee logs web --follow
angee down
```

Compilation pipeline on every `up` / `dev` / `build` / `start`:

1. Load `angee.yaml`.
2. Resolve secrets through the configured backend (`.env` or OpenBao).
   Generate any `generated: true` secrets that don't exist yet. Import
   `env:VAR` references from the host environment and persist into the
   backend.
3. **Materialize any referenced source caches** that aren't yet on
   disk. The compile pipeline scans every `mounts:` and `workdir:`
   field in services and jobs for `source://<name>...` references. For
   each referenced source whose cache is missing, fetch it now —
   `git clone` (kind: git), download + checksum (kind: url/archive),
   etc. — using the source's declared `auth:`. This applies whether
   the source is mounted directly or via a workspace; the rule is "no
   service starts against an absent source cache". Fail compilation
   with a clear error if auth resolution fails.
4. Resolve references. Non-secret refs become literals in the generated
   compose / process-compose files. `${secret.x}` refs are **rewritten as
   `${ENV_VAR}` env-var references** in the generated files; the resolved
   values land only in the gitignored `.env` (written `0600`) which
   compose / process-compose load at launch. Generated files never carry
   literal secret values.
5. Split services by `runtime`:
   - `runtime: container` → emit `docker-compose.yaml`.
   - `runtime: local` → emit `process-compose.yaml`.
6. For `angee up`: **compose-only.** Run `docker compose up -d` for
   `runtime: container` services. Local services are not started.
7. For `angee dev`: **compose + process-compose.** Run `docker compose up
   -d` for sidecars, then `process-compose up -d` for local services,
   with cross-boundary `depends_on` resolved by readiness probes.
8. For `angee down/stop/restart/logs <name>`: route by `runtime:`.

### Secrets backend interface

```go
type Backend interface {
    Get(ctx context.Context, key string) (string, bool, error)
    Set(ctx context.Context, key, value string) error
    Delete(ctx context.Context, key string) error
    List(ctx context.Context) ([]string, error)
}
```

v1 implementations: `EnvFileBackend` (reads/writes
`$ANGEE_ROOT/.env` — the path comes from `secrets_backend.path:` in
the manifest, resolved per the path rule above), `OpenBaoBackend`
(KV v2 over plain `net/http`). Adding a backend later (Doppler, AWS
SSM, Infisical) is one struct that satisfies the interface.

### Bootstrap: host env vs. backend

The host process environment is the **bootstrap input**, not a backend.

1. **At first `up`/`dev`/`init`**, any secret declaration with `import:
   env:VAR_NAME` reads `VAR_NAME` from the host environment and persists
   it into the configured backend.
2. **At every subsequent `up`/`dev`**, the backend is the source of
   truth. Host env is only consulted to pick up newly added imports.
3. **`generated: true`** secrets are created in the backend on first run
   and persist there forever (or until `--rotate`).

### Hard rule: no resolved secrets in committed files

The compiler MUST NEVER write a resolved secret value into any file that
the user might commit:

- **`angee.yaml`** — only references and declarations are committed.
- **Generated `docker-compose.yaml` / `process-compose.yaml`** — keep
  `${secret.x}` references as `${ENV_VAR}` placeholders. Compose
  supports `${VAR}` interpolation against a secrets-loaded env, so the
  resolved file references env vars, never literals.
- **Per-workspace `.env` files** — may contain resolved secrets and
  **must be gitignored**. The operator writes them with `0600` perms and
  refuses to write to a path inside a tracked directory unless that path
  is in `.gitignore`.
- **`.copier-answers.yml`** — committed. Must never contain a resolved
  secret. If a Copier answer is sourced from a secret, the answer file
  stores the reference, not the value.
- **Template-rendered files** — committed. Render `${secret.<name>}`
  references as `${<ENV_VAR>}` placeholders that the consuming runtime
  interpolates from the gitignored `.env` at launch.

Two-pass compile:

1. **Render pass** (Copier): resolves only static answers. Preserves
   `${ns.path}` references verbatim.
2. **Resolve-into-env pass** (operator): writes resolved values **only**
   into gitignored `.env` files and into the in-memory environment passed
   to compose/process-compose at launch. Never modifies committed files.

## Goal 2: Workspace Provisioning

The novel primitive. One command renders a directory from a Copier
template chain, materializes sources at subpaths, and registers the
result as something services can mount.

```sh
angee workspace create pr \
  --input branch=feat/issue-123 \
  --input base_branch=dev \
  --start --yes
# → resolves the template at .templates/workspaces/pr/
# → name auto-derived from instance_naming.pattern → "feat-issue-123"
```

What `workspace create` does, in this order:

1. **Load the entry-point template** the caller named (`pr` →
   `.templates/workspaces/pr/`). Read its `_angee` metadata.
   Validation rules:
   - The entry-point's `_angee.kind` must be `workspace`.
   - **Resolve the full chain** as `[<entry-point>] ++ _angee.chain`.
     Each subsequent entry is loaded and its `_angee.kind` checked
     (`workspace` or `stack`).
   - At most one `kind: stack` entry across the chain.
   - If any entry is `kind: stack`, the entry-point template's
     `_angee.chain_root` must be set to a path relative to the
     workspace root.
2. **Compute the instance name.** From `--name` or from the
   entry-point template's `_angee.instance_naming.pattern`.
3. **Allocate ports** for any `${alloc.<pool>}` references encountered
   in the chain.
4. **Resolve / generate secrets** in the configured backend.
5. **Materialize sources** declared by the workspace into
   `$ANGEE_ROOT/workspaces/<name>/<subpath>/`:
   - `kind: git, mode: worktree` → `git worktree add` off the source's
     shared `cache_path`. Multiple workspaces share one fetch.
   - `kind: git, mode: clone` → shallow clone with locked commit.
   - `kind: template` → `copier copy` of a workspace template into a
     subpath.
   - `kind: url|archive` → download + checksum verify + extract.
   - `kind: local` → bind or copy.
   - `kind: volume` → mount a declared volume at a subpath.
6. **Render the template chain** with `copier-go`, walking left to
   right. Two passes per entry (reference pass + resolve-into-env
   pass). Per-entry rules:
   - **Render destination** is determined by `_angee.kind`:
     - `kind: workspace` → renders into the workspace root
       (`$ANGEE_ROOT/workspaces/<name>/`).
     - `kind: stack` → renders into `<workspace-root>/<chain_root>/`
       (the inner ANGEE_ROOT). The destination dir is created on
       demand.
   - **Inputs** are taken from the workspace's `inputs:` block
     (filtered to the inputs that entry's `_angee.inputs` declares)
     plus the answers file that earlier entries persisted. Each
     entry's answers file lives at `<destination>/.copier-answers.yml`.
   - **Substitutions** that appear in template strings (`${ports.x}`,
     `${alloc.x}`, `${workspace.<name>.path}`, etc.) resolve against
     the operator's view of the same workspace — so a stack template
     rendered into `chain_root:` sees the workspace-allocated ports
     and the workspace's path natively.
7. **Prepare the inner ANGEE_ROOT** (when a stack entry rendered into
   `chain_root:`). Call `service.Platform.StackPrepare(<inner-root>)`,
   not `StackInit`. The distinction matters:
   - `StackInit(<root>, <template>, <inputs>)` — used by `angee
     stack init` / `angee init` against a *fresh* root; renders the
     template, then prepares.
   - `StackPrepare(<root>)` — operates on an *existing* `angee.yaml`
     under `<root>`; resolves secrets, allocates ports against the
     inner pool, compiles `docker-compose.yaml` /
     `process-compose.yaml`, but does **not** rerun any template.
   The chain render in step 6 already produced the inner manifest;
   `StackPrepare` is the no-double-render way to make it runnable.
   Same per-root advisory lock as any other reconciliation. Does not
   start runtimes.
8. **Register the workspace in the outer `angee.yaml`** under
   `workspaces.<name>` (including the resolved instance name, the
   resolved chain + chain_root for reproducibility, port leases,
   source materializations, and `ttl_expires_at` if a TTL was passed)
   and write the workspace's path/ports into its gitignored `.env` so
   any service that mounts it can read addresses.

Eight steps, zero of which mention agents, MCP, or addressability.

`workspace start` (only meaningful when the resolved chain produced an
inner ANGEE_ROOT) brings the inner stack up. The lifecycle trigger is
declared by the entry-point template's `_angee.chain_lifecycle` (`dev`,
`up`, or `auto`). `auto` (the default) inspects the rendered inner
manifest and picks `StackDev` if any inner service is `runtime: local`,
else `StackUp`. `workspace create --start` runs `workspace start`
immediately after step 8.

`workspace stop` brings the inner stack down. `workspace destroy`
reverses provisioning, optionally removing the worktree and persistent
dirs with `--purge`.

### `workspace update`: what changes, what doesn't

`workspace update <name>` is a deliberately narrow operation. It
respects the resolved chain pinned at create time (under
`workspaces.<n>.resolved.chain`) — `update` never re-resolves the
chain to pick up upstream `_angee.chain` edits, because that would
silently restructure a workspace mid-life. The four flag shapes and
their effects:

| Invocation | What changes |
|---|---|
| `workspace update <n>` (no flags) | Reruns `copier update` on each pinned chain entry to pick up upstream **template content** changes (templates evolve; answers stay). Two-pass compile re-runs. `StackPrepare` re-runs on the inner root. **Sources are not re-fetched**; their pinned refs stand. |
| `workspace update <n> --input k=v ...` | Same as above, plus updates `inputs:`, which feeds into the rerendered chain and inner manifest. |
| `workspace update <n> --ttl <dur>` | TTL only. Recomputes `ttl_expires_at` from now. No render, no `StackPrepare`. Pass `--ttl ""` (empty) or PATCH `{ "ttl": null }` to remove the TTL. |
| `workspace update <n> --sync-sources` | Re-fetches each declared source (`git fetch`, plus `git pull` on worktree branches when fast-forward — pull lands in **Phase 5b**, so until 5b ships this flag is fetch-only and the operator returns a `worktree pull deferred to 5b` notice rather than merging). The flag is opt-in because it can change what the workspace "is" between update calls. |

What `update` will **not** do:

- Re-resolve the chain. Use `workspace destroy` + `workspace create`
  if the entry-point template's `_angee.chain` has structurally
  changed.
- Change source `mode:` (worktree → clone). Same answer: destroy +
  create.
- Move the worktree branch. Use git directly inside the worktree, or
  `workspace destroy --purge` + create on a new branch.
- Change inputs that feed into **identity or source materialization**.
  These are *immutable after create* and `update --input k=v` rejects
  them with a clear "use destroy + create" error. The operator
  detects immutability automatically: an input is treated as immutable
  if it appears in any of the following, evaluated against the
  resolved workspace at create time —
    - the entry-point template's `instance_naming.pattern`,
    - any source's `branch:`, `ref:`, `mode:`, or `subpath:`,
    - any `chain_root:` value.
  Templates can also mark inputs immutable explicitly with
  `_angee.inputs.<name>.immutable: true`. Mutable inputs (e.g., model
  selection, log level, anything that only feeds into rendered
  templates' content) update freely.

The PATCH alias accepts the same body keys: `ttl`, `inputs`,
`sync_sources` (boolean).

The result on disk for the example manifest:

```
$ANGEE_ROOT/
  angee.yaml
  .env                                    # gitignored
  docker-compose.yaml
  process-compose.yaml
  workspaces/feat-issue-123/
    .env                                  # gitignored — workspace-scoped resolved secrets
    code/                                 # git worktree off ../django-angee
      .angee/                             # inner ANGEE_ROOT (chain_root)
        angee.yaml
        .env
        docker-compose.yaml
    skills/                               # cloned source
    README.md                             # rendered from the workspace template
```

### Mount syntax

Service `mounts:` accept a tiny URI scheme. Container and local services
interpret it differently — the syntax is shared so a manifest doesn't
fork on `runtime:`, but the execution model below is explicit.

| URI | Container (`runtime: container`) | Local (`runtime: local`) |
|---|---|---|
| `workspace://<n>:/path[:ro]` | bind workspace root at `/path` (read-only optional) | sets `WORKSPACE_<N>_PATH=<host-abs-path>` env var; no bind |
| `workspace://<n>/<sub>:/path[:ro]` | bind workspace subpath at `/path` | sets `WORKSPACE_<N>_<SUB>_PATH=<host-abs-path>` |
| `source://<n>:/path[:ro]` | bind source's local cache at `/path` | sets `SOURCE_<N>_PATH=<host-abs-path>` |
| `volume://<n>:/path` | named volume mount (compose proxy) | sets `VOLUME_<N>_PATH=<host-abs-path>` |
| `bind:///abs/host:/path` | host bind (compose-equivalent) | sets `BIND_<sanitized>=<host-abs-path>` |

For `runtime: local` services, the `:/path` portion is metadata: it's
the path the *equivalent containerized* service would see. Local
processes read from / write to the underlying host paths via the env
vars; there is no Linux mount.

To set a local process's working directory, declare it explicitly:

```yaml
services:
  web:
    runtime: local
    workdir: workspace://feat-issue-123/code   # accepts the same URI scheme
    command: ["uv", "run", ...]
```

Container services don't need `workdir:` for this purpose — they use
the image's `WORKDIR` or compose's `working_dir:` (which Angee can pass
through). `workdir:` on a container service is allowed as a
substitution-aware proxy for compose's `working_dir:`, but the common
case for containers is to bake the cwd into the image.

### Sources are a global pool

The top-level `sources:` block is the source pool — exactly like
`volumes:` in docker compose. Sources are declared once at the stack
level and referenced by name from workspaces (and from any service that
wants to mount one directly).

Two workspaces pointing at the same global git source share the local
clone (`cache_path`) and `git worktree add` distinct worktrees off it.
One fetch, many isolated checkouts — fast to provision and cheap on
disk.

### Git operations on sources

The operator is a first-class git client for sources of `kind: git`. It
needs to be one because `mode: worktree` workspaces are read-write — an
agent (or any process) running in a worktree commits there, and the
operator has to observe and ship those commits without the application
having to shell into the host.

The operator owns these operations:

- `source fetch  <name>` — `git fetch --all` on the cache. Fast, safe.
- `source pull   <name> [--ref r]` — fast-forward pull on the cache or a
  specific worktree branch. Fails on conflict; conflict resolution stays
  in the worktree.
- `source push   <name> [--ref r]` — push the cache or a worktree branch
  to its upstream. Pre-flight: refuse if there are unstaged changes in
  the named worktree (the agent should commit first).
- `source status <name>` — branch + dirty state of the cache and of
  every workspace worktree off it. Used by the application to poll for
  "agent has new commits" without shelling into the host.
- `workspace push   <name>` — convenience wrapper that pushes every
  worktree-mode source in the workspace.
- `workspace git    <name>` — same status data filtered to one
  workspace's worktrees.

Auth wiring (declared on the source):

- `auth.mode: ssh` — the operator runs `git` with
  `GIT_SSH_COMMAND="ssh -i <key-from-secret> -o IdentitiesOnly=yes"`.
  The key material lives in the secrets backend; the operator
  materializes it to a 0600 tempfile under `<root>/run/` for the
  duration of the call.
- `auth.mode: https-token` — custom credential helper that supplies the
  token from the secrets backend.
- `auth.mode: host` — inherit `~/.ssh/` and `~/.gitconfig`. Local-dev
  only; the standalone operator refuses this mode unless bound on
  loopback.

Commit identity (`git.user_name` / `git.user_email`) applies only to
commits the operator itself makes — for example, an automated `copier
update` rebase. Agent-driven commits use whatever identity the agent
configures inside the worktree.

PR creation is **not** an operator operation. An agent that wants to
open a PR runs `gh pr create` inside the worktree using credentials the
template wired through env vars. Angee's responsibility ends at "the
worktree is on a named branch with an upstream, and `git push` works."

Implementation note: every git operation on a source is funneled through
a single `internal/git` package that takes a per-source advisory lock
(distinct from the per-root operator lock). Concurrent worktree-add and
push operations against the same cache serialize on this lock;
operations against different sources run in parallel.

### Persistent paths declared by templates

Anything a workspace template needs to keep across restarts (browser
caches, model-weights downloads, SQLite databases, etc.) is declared
in the **template's `_angee` metadata**, under `_angee.persist_paths:`.
The map shape is the only schema for persistence; there is no boolean
`persist:` flag. The workspace directory itself is preserved by default
until `workspace destroy` (see workspace lifecycle).

Source of truth and resolved location:

```yaml
# .templates/workspaces/pr/copier.yml — the template declares paths it needs
_angee:
  kind: workspace
  name: pr
  persist_paths:
    browser-data: { subpath: .browser-data, scope: workspace }
    model-cache:  { subpath: .models,       scope: stack }
```

```yaml
# $ANGEE_ROOT/angee.yaml — the operator copies the resolved declarations
# into the workspace's resolved fields at create time, alongside chain
# and chain_root. Users don't write this; the operator does.
workspaces:
  feat-issue-123:
    template: workspaces/pr
    resolved:
      chain: [...]
      chain_root: code/.angee
      persist_paths:
        browser-data: { subpath: .browser-data, scope: workspace }
        model-cache:  { subpath: .models,       scope: stack }
```

The chain-walk in workspace create reads `_angee.persist_paths` from
each chain entry (workspace + stack templates can both declare them),
unions them by key (later entries override earlier ones; warn on
collision), and writes the union into `workspaces.<n>.resolved.persist_paths`.

The operator:
- **Creates the directory during `workspace create`** (after step 5,
  before step 6's render pass). The exact location depends on `scope:`:
  - `scope: workspace` → `${workspace.<name>.path}/<subpath>` (under
    the workspace directory; removed on `workspace destroy --purge`).
  - `scope: stack` → `$ANGEE_ROOT/persist/<key>` (under the **outer**
    ANGEE_ROOT — the one that hosts the workspace, not any chained
    inner root; removed only on `stack destroy --purge`). The `<key>`
    in the path is the persist_paths map key, namespaced across all
    workspaces sharing this stack: two workspaces declaring
    `model-cache` with `scope: stack` resolve to the *same* directory
    (intentional — that's the whole point of stack scope). The
    operator warns at create time if two workspaces declare the same
    `scope: stack` key with different `subpath:` values, since the
    `subpath:` is ignored for stack scope (the path is `persist/<key>`
    by convention, not derived from `subpath:`).
  The directory exists by the time any service mounts the workspace,
  even if `workspace start` is never called.
- Exposes the absolute path as `${persist.<key>}` for use anywhere in
  the same template.
- Preserves it across `workspace stop`/`workspace start` and `workspace
  update`.
- Removes it on `workspace destroy --purge` (or stack `--purge`).

Creating-on-create matters because services can mount a workspace
directly via `workspace://<n>:/path` without ever calling `workspace
start` (a workspace whose chain doesn't produce an inner stack has no
"start" semantics — the workspace is just a directory). If
`persist_paths` were first-start-only, those services would mount a
workspace whose `${persist.<key>}` paths don't exist yet.

## Substitution Grammar

Same `${ns.path | filter}` shape, fewer namespaces.

| Form | Resolver |
|---|---|
| `${secret.name}` | Secrets backend (rewritten to `${ENV_VAR}` in committed files; resolved into gitignored `.env`). |
| `${service.name}` / `.host` / `.port` | Service registry; host portion rewritten per consumer location. |
| `${workspace.name.path}` | Absolute path of a named workspace on disk. |
| `${source.name.path}` | Absolute path of a fetched source's local cache. |
| `${ports.name}` | Stack `ports:` block. |
| `${alloc.pool}` | Operator port pool, allocated at workspace/service init. |
| `${persist.<key>}` | Absolute path to a template-declared persistent dir. |
| `${operator.url}`, `${operator.domain}` | Operator URL; host portion rewritten per consumer. |
| `${inputs.key}` | Provisioning-time inputs. |
| `${name}` | Resolved instance name. |

Filters: `slug`, `lower`, `upper`, `local_part`, `truncate(n)`,
`default('x')`, `required('msg')`, `b64encode`, `replace(a,b)`. Full
grammar in `examples-v2/SUBSTITUTIONS.md`.

`connector` is dropped — per-user dynamic credentials are the
application's job. If the application wants to inject a per-user OAuth
token, it resolves the token itself and passes it as a normal `--input`
or `secret` value.

### Address resolution

Addresses (`${operator.url}`, `${service.<name>}`) resolve to **different
host strings depending on where they're consumed**. The operator does
the rewrite at render time.

| Source runs as | Destination consumes from | Host portion |
|---|---|---|
| host process | host process | `127.0.0.1` |
| host process | container in operator network | `host.docker.internal` |
| container | host process | `127.0.0.1` (compose publishes port) |
| container | container, same docker network | compose service DNS |
| container | container, different network | `host.docker.internal` |

Templates write the reference once; the operator writes the right host
into each rendered destination.

### CLI = operator-as-library

There is no separate CLI flag-marshaling code path. The CLI binary
always goes through `service.Platform`, either by starting an in-process
operator or by calling a daemon over HTTP.

The CLI shape for v1 is fixed and template-blind:

```sh
angee workspace create <template> [--input key=value ...] [--name name]
angee stack init        <template> [--input key=value ...] [path]
```

`--input key=value` is repeatable. There are no per-template "pretty"
flags in v1 — the CLI does not introspect templates to register typed
flags.

### `_angee.inputs` and `instance_naming`

Templates declare their input surface once in `copier.yml`:

```yaml
_angee:
  kind: workspace                      # workspace | stack
  name: pr

  instance_naming:
    pattern: "${inputs.branch | slug}"
    fallback: "${inputs.name}"
    max_length: 40

  inputs:
    branch:
      type: str
      required: true
      maps_to: { copier: branch }
    base_branch:
      type: str
      default: dev
      maps_to: { copier: base_branch }

  # Optional. If present, declares additional templates to render after this one.
  # Each entry is a kind-relative template ref. The entry-point template (the one
  # being declared right here) is always implicitly the first chain step; you
  # don't list it again. Inputs flow through to chained entries (filtered to each
  # entry's _angee.inputs).
  chain:
    - stacks/dev

  # Required iff `chain:` includes any kind: stack template. Path relative to the
  # workspace root where the inner ANGEE_ROOT renders.
  chain_root: code/.angee

  # Optional. Overrides the auto-detection rule for `workspace start` (which
  # otherwise picks `dev` if any inner service is runtime: local, else `up`).
  chain_lifecycle: dev                 # dev | up | auto (default)
```

CLI and HTTP both read this metadata to validate `--input` / request
bodies, compute the instance name, and resolve the chain. The user
passes only the entry-point template name; everything else (chain,
chain_root, chain_lifecycle) is template-owned.

## Templates

Copier templates with `_angee` metadata in `copier.yml`. Two families:

- **Stacks** — render an `angee.yaml` + supporting files. Used by
  `angee init` / `angee stack init` once per `ANGEE_ROOT`.
- **Workspaces** — render a workspace directory. Used by
  `angee workspace create` per workspace instance.

There is no agent template family. If a workspace template happens to
render an `.mcp.json` or other agent-specific file, that's the
template's business — Angee doesn't care. The operator never parses or
validates those files.

### Where templates live

Angee resolves template names from these paths, in order:

1. `.templates/<kind>/<name>/` in the project repo (always wins if
   present). `<kind>` is `stacks` or `workspaces`. So
   `angee init --dev` looks for `.templates/stacks/dev/`, and
   `angee workspace create pr` looks for `.templates/workspaces/pr/`.
2. Embedded in the angee binary — a small set of starter stacks and a
   reference workspace template. These are the fallback that lets a
   fresh repo run `angee init --dev` before it has its own
   `.templates/`.
3. (Phase 8) remote refs like `github.com/owner/repo//path@ref`.

The leading dot is intentional: `.templates/` sits with `.gitignore`
and `.copier-answers.yml` as project-config infrastructure rather than
as application code. A repo that already organizes templates somewhere
else can override the lookup root via `operator.template_paths:` in
`angee.yaml` (a list of search roots; first hit wins).

References inside `angee.yaml` (`workspaces.<n>.template:`,
`template.active:`, etc.) use the **kind-relative** form — `stacks/dev`,
`workspaces/pr`. The resolver prepends the lookup root.

### Template argument forms (CLI vs API vs manifest)

The CLI accepts a **bare name** as shorthand because the command
already declares the kind:

| Invocation | Resolves to |
|---|---|
| `angee init --dev`             | `stacks/dev`     (the `init --dev` alias hard-codes `stacks/`) |
| `angee stack init dev`         | `stacks/dev`     (kind = stack, from the `stack` subcommand) |
| `angee stack init stacks/dev`  | `stacks/dev`     (full form; kind must match `stack`) |
| `angee workspace create pr`    | `workspaces/pr`  (kind = workspace, from the `workspace` subcommand) |
| `angee workspace create workspaces/pr` | `workspaces/pr` (full form) |

Both forms accepted everywhere; the bare form is shorthand. If a user
passes `stacks/x` to `workspace create`, the resolver rejects with a
"kind mismatch" error.

The HTTP API and the manifest **always** use the full kind-relative
form (`workspaces/pr`, `stacks/dev`). HTTP/manifest contexts don't have
an implicit "kind"-from-command, so the bare form would be ambiguous.
This is why `POST /workspaces { "template": "workspaces/pr" }` and
`workspaces.<n>.template: workspaces/pr` both spell it out.

Rendering is done in-process by `github.com/fyltr/copier-go`. It
implements `copier copy`, `copier update`, and the answers-file
lifecycle natively in Go, so there is no Python runtime requirement.
`copier update` is what makes `angee stack update` and `angee
workspace update` viable.

## Operator HTTP + MCP API

Thin adapter over `service.Platform`. Same surface for HTTP and MCP. No
agent endpoints.

```
# Stack — all routes operate on the operator's served ANGEE_ROOT (one operator,
# one root). `path` is never accepted in request bodies on the HTTP surface;
# it's a CLI-only concept that decides which root the in-process operator runs
# against before binding.
POST   /stack/init                            # body: { template, inputs }; refuses if root is already initialized
POST   /stack/update                          # body: { inputs }; reruns copier update
POST   /stack/{build|up|dev|down|destroy}     GET  /stack/status
GET    /stack/logs                            # tails all services in the stack

# Note: POST /stack/dev runs detached — docker compose up -d + process-compose up -d.
# Returns 202 + operation_id like other async ops; clients tail /stack/logs or
# /events for output. The "foreground TUI + Ctrl+C" flow is a CLI-only
# affordance that exists because `angee dev` blocks the user's terminal;
# over HTTP, `dev` is just "compose + process-compose up", same as `up`
# but including local services.

# Services
POST   /services                              # body: full service spec; manifest edit + reconcile
PATCH  /services/{n}                          # body: partial service spec; manifest edit + reconcile
POST   /services/{n}/{start|stop|restart|destroy}
GET    /services/{n}                          GET  /services/{n}/logs       GET /services

# Jobs
POST   /jobs/{n}/run                          GET  /jobs                    GET /jobs/{n}/logs

# Workspaces
POST   /workspaces                            POST /workspaces/{n}/{update|start|stop|restart|destroy}
PATCH  /workspaces/{n}                        # alias for /update; body may include ttl, inputs, sync_sources
GET    /workspaces/{n}                        GET  /workspaces
GET    /workspaces/{n}/logs                   # tails inner-stack logs when chain produced an inner root
POST   /workspaces/{n}/push                   GET  /workspaces/{n}/git

# Sources
GET    /sources                               POST /sources/{n}/{fetch|pull|push}
GET    /sources/{n}/status

# Async + events
GET    /operations/{id}                       GET  /events                  (SSE)

# MCP
GET    /mcp                                   (MCP JSON-RPC; same operations as tools)
```

Bearer auth required for non-loopback binds. One operator serves
exactly one `ANGEE_ROOT`.

## Operator-as-Service: external-runtime composition

When the application layer (Django, etc.) wants what an "agent" would
have meant, it composes two Angee operations.

```
Django                              angee-operator                       host
  │                                       │                                │
  │ POST /workspaces { template, inputs } │                                │
  ├──────────────────────────────────────►│                                │
  │ 202 { name: "feat-issue-123",         │ render copier-go               │
  │       operation_id, status_url }      │ git worktree add               │
  │◄──────────────────────────────────────┤ resolve secrets / allocate ports│
  │                                       │ register workspace             │
  │                                       │                                │
  │ POST /services { name, image, mounts, │                                │
  │                  env, runtime }       │                                │
  ├──────────────────────────────────────►│ docker compose up -d <svc>     │
  │ 202 { name, operation_id }            ├───────────────────────────────►│
  │◄──────────────────────────────────────┤                                │
  │                                       │                                │
  │ GET /events  (SSE)                    │                                │
  ├──────────────────────────────────────►│                                │
  │ ◄── workspace.ready ──                │                                │
  │ ◄── service.healthy { url } ──        │                                │
  │                                       │                                │
  │ # Django talks to the service over     │                                │
  │ # its declared port. There is no      │                                │
  │ # /agents proxy; the service URL is   │                                │
  │ # whatever its compose port publishes.│                                │
```

The two requests in detail:

```jsonc
// POST /workspaces
{
  "template": "workspaces/pr",
  "inputs":   { "branch": "feat/issue-123", "base_branch": "main" },
  "start":    true,                     // run inner-stack `up`/`dev` if chain_root is set
  "ttl":      "24h"                     // optional; operator GC sweeps expired workspaces
}
```

```jsonc
// POST /services
{
  "name":    "agent-claude-feat-issue-123",
  "runtime": "container",
  "image":   "ghcr.io/example/claude-runner:latest",
  "mounts":  [
    "workspace://feat-issue-123:/workspace",
    "source://runner-skills:/skills:ro"
  ],
  "env": {
    "ANTHROPIC_API_KEY":  "${secret.anthropic-api-key}",
    "ANGEE_OPERATOR_URL": "${operator.url}",
    "WORKSPACE_PATH":     "${workspace.feat-issue-123.path}"
  },
  "ports":  [{ "publish": "auto", "container": 3007, "pool": "custom" }],
  "start":  true
}
```

Both responses are 202 Accepted with an `operation_id` and a
`status_url`. The caller polls `GET /operations/{id}` or subscribes to
`GET /events` for SSE.

`POST /services` is a manifest edit — the operator persists the new
entry into `angee.yaml` and reconciles. Subsequent `GET /services/<name>`
returns the deterministic published port (from the `custom` pool) so the
caller can talk to the service directly.

### Port leasing

The operator owns a port pool declared in `angee.yaml.operator.port_pool`.
When a request asks for `publish: auto`, the operator picks an unused
port from the matching pool, persists the assignment in `angee.yaml`,
and propagates the concrete value into the rendered compose /
process-compose files. The lease is released on workspace/service
destroy.

### Multi-tenancy and isolation

One operator per trust boundary. Per-environment (staging vs prod) or
per-tenant (one operator per customer when isolation matters). The
operator binary is small; spin up multiple. The operator is not designed
to multiplex tenants inside one ANGEE_ROOT.

### TTLs and cleanup

Workspaces created by an external runtime should have a TTL. Pass `ttl:
"24h"` (or any Go duration string) on `POST /workspaces` or `--ttl 24h`
on the CLI; the operator records `ttl_expires_at` on the workspace
entry. A background sweep loop in the operator calls `WorkspaceDestroy`
on expired entries.

Extend a TTL with `POST /workspaces/{n}/update` (`PATCH /workspaces/{n}`
is accepted as an alias) and a body like `{ "ttl": "24h" }`, which
recomputes `ttl_expires_at` from now. Pass `{ "ttl": null }` to remove
the TTL entirely.

The TTL sweep runs **whenever an operator process is alive** for the
ANGEE_ROOT — that means the in-process operator under `angee dev` runs
the sweep on its loopback port, and the standalone `angee operator`
daemon runs the sweep on its bound port. Default tick is 60 seconds.
If no operator is running for an ANGEE_ROOT (no `dev` session, no
daemon), nothing sweeps; expired workspaces sit at `ttl_expires_at <
now()` until the next operator process comes up and catches them on
its first tick.

**GC behavior when destroy is blocked.** A workspace whose
`ttl_expires_at` has passed but whose destroy is refused by the safety
guard (running services / jobs still reference it) is **skipped, not
force-destroyed**. The sweep:

1. Logs a warning naming the offending services.
2. Emits a `workspace.destroy_blocked` event on `/events` (payload
   includes the workspace name and the offending service/job names).
3. Leaves `ttl_expires_at` untouched and tries again on the next
   sweep tick (default 60s).

The application reacts to the event by either destroying the offending
services first, extending the TTL, or — as the explicit "I really mean
it" — calling `POST /workspaces/{n}/destroy?force=true`. The operator
never force-destroys on its own. This is by design: an in-flight agent
mid-commit deserves an explicit human/app-level decision over a
silent yank.

Services don't get TTLs — they're either declared in `angee.yaml`
(long-lived) or dynamically added with the expectation that the caller
manages their lifecycle.

**`workspace destroy` does not stop or remove services.** The two
lifecycles are independent in v1: a workspace going away is a directory
event, not a runtime event. If a caller wants "tear this service down
with the workspace", they issue `service destroy <name>` first (or in
parallel via SSE-driven orchestration). A future Phase 8 may add a
declarative `lifecycle.destroy_with: workspace://<n>` link on a service
entry — for v1, the caller composes the two operations.

**Inner-stack teardown is automatic.** `workspace destroy <n>` always
runs `workspace stop <n>` first — that is, `StackDown(<inner-root>)` if
the workspace has an inner stack. The workspace owns its inner stack,
so destroy doesn't need a separate guard for it. The TTL sweep follows
the same path (stop, then destroy); an expired workspace's inner stack
is brought down before the directory is removed.

The destroy safety guard below is *only* about **external** references
— other services or jobs in the outer stack that mount the workspace.
Those are the lifecycles the workspace doesn't own; they need explicit
caller orchestration to tear down.

`workspace destroy` refuses if any running service or job references
the workspace by any of these mechanisms:

- `mounts:` URI of the form `workspace://<name>...` (would yank a live
  bind-mount out from under a container).
- `workdir:` URI of the form `workspace://<name>...` (the local process
  is cwd'd into a directory that's about to disappear).
- Any `env:` value containing a `${workspace.<name>...}` reference (the
  process resolved a path that's about to disappear).

The check matches by **string scan over the resolved manifest** — any
mention of the workspace's name in those three positions counts. The
error message names the offending services and jobs. `--force`
overrides the check; `--purge` implies `--force` and additionally
removes the worktree, persistent dirs, and the inner ANGEE_ROOT
directory.

## `angee dev`

`angee dev` is the integrated dev loop. One command brings up three
things and keeps them running together for the lifetime of the session:

1. **The operator HTTP + MCP server** on loopback (port from
   `angee.yaml`'s `operator.url`, bearer from the `operator-token`
   secret).
2. **Container sidecars** via `docker compose up -d` (postgres, redis,
   openbao, anything `runtime: container`).
3. **Local processes** via `process-compose up -d` (daemon mode; the
   `angee dev` CLI tails logs over its REST API) — anything `runtime:
   local`: Django, Vite, build watchers, etc.).

Startup order:

1. Resolve secrets, allocate/confirm ports, compile
   `docker-compose.yaml` and `process-compose.yaml`. If `--build`, run
   `docker compose build` first.
2. Start the operator HTTP+MCP listener in-process. It binds to the
   configured loopback port and serves the same API surface as the
   standalone operator.
3. Start container sidecars via `docker compose up -d`.
4. Start `process-compose up` in daemon mode. Every local process gets
   `ANGEE_OPERATOR_URL`, `ANGEE_OPERATOR_TOKEN`, and resolved port env
   vars injected automatically.
5. Tail logs from all three sources into a unified TUI.

Shutdown on Ctrl+C is the reverse: stop process-compose (graceful
per-process shutdown), stop the operator, leave container sidecars up
unless template policy says otherwise.

### Provisioning workspaces and services during dev

Because the operator is live throughout the session, the developer (or
any local process) can spin up workspaces and services into the running
`.angee/` without leaving the dev loop:

```sh
# from another terminal, while `angee dev` is running:
angee workspace create pr --input branch=feat/issue-123 --start --yes

angee service init agent-claude-feat-issue-123 \
  --image ghcr.io/example/claude-runner:latest \
  --mount workspace://feat-issue-123:/workspace \
  --start --yes
```

Or equivalently, a Django process running under `angee dev` calls the
operator over HTTP. Either path goes through the same
`service.Platform`. The operator hot-applies the change: regenerates
the backend files, brings up the new resource, streams events on
`/events`. No `angee dev` restart required.

### Why the operator runs during dev (not just on staging)

- **Develop against the real API.** Django code that calls `POST
  /workspaces` and `POST /services` in prod also calls them in dev. No
  mock, no env-specific branch.
- **Use the provisioning surface in your own dev loop.** Spin up a
  workspace + service inside `angee dev`, talk to it on its declared
  port, destroy when done. No special "agent mode" — it's the same
  surface whether the service is a Django instance or a containerized
  agent runner.

## ANGEE_ROOT Layout

```
$ANGEE_ROOT/
  angee.yaml                  # source of truth, committed.
                              # Contains ${secret.*} references only — never resolved values.
  .copier-answers.yml         # committed. Stores references, not resolved secrets.
  docker-compose.yaml         # generated. Committed or gitignored — template choice.
                              # Contains ${ENV_VAR} refs resolved by docker compose at launch.
  process-compose.yaml        # generated. Same rules as docker-compose.yaml.
  .env                        # ALWAYS gitignored. Contains resolved secrets at 0600.
  workspaces/<name>/          # rendered template + materialized sources.
    .env                      # gitignored. Workspace-scoped resolved secrets at 0600.
    <subpath>/                # source mounts (worktrees, clones, archives, etc.)
    <chain_root>/             # optional inner ANGEE_ROOT, if the template emitted one
  volumes/                    # local persistent volumes when local-fs driver used
  run/                        # volatile operator working files, gitignored
    operator.lock             # per-root advisory lock
```

**Hard rule:** the compiler MUST refuse to write a resolved secret into
any path inside a tracked directory unless that path appears in
`.gitignore`. This is enforced at write-time, not by convention.

No `state/` directory. No `operator.yaml`. No `agents/` directory. The
manifest is the durable state. Ephemeral state — async operation
records (`/operations/{id}`) and the event stream backing `/events`
SSE — lives **in-memory** in the operator process and is lost on
restart.

## Out Of Scope For v1

Explicitly deferred so the v1 ships:

- Anything agent-shaped: agent CLI noun, agent endpoints, MCP server
  lifecycle, agent addressability proxy, agent-specific TTL semantics,
  `_angee.kind: agent` template family.
- PR creation. Angee makes sure `git push` works from worktrees and
  exposes status; the agent (or the application calling `gh`) opens
  the PR.
- Connector resolvers (`${connector.x.y}`).
- Kubernetes runtime backend.
- Durable workflow engine.
- ReBAC / SpiceDB authorization (use bearer auth until a real need
  surfaces).
- Generic mount type system beyond what Compose already supports
  (`bind`, `volume`, `tmpfs`).
- Custom secrets backends beyond `.env` + OpenBao.
- A web UI.
- Per-template "pretty" CLI flags.
- Persistence of `/operations/{id}` across operator restarts.
- Framework-specific dispatch (Django, Vite, pnpm, uv).

## Refactor Decisions

1. `angee.yaml` is the durable source of truth. There is no
   `operator.yaml`, no separate persistent state directory.
2. **Angee does not know what an agent is.** No agent CLI, no agent
   endpoint, no agent in the manifest schema, no agent template family.
   The combination of `workspace create` + `service init` covers what an
   agent command would have done.
3. An agent-free, workspace-free stack is a first-class use case. The
   *runtime* path (`up`, `dev`, `build`, `down`, `start`, `stop`,
   `restart`, `logs`) on such a manifest never touches workspace,
   source, or git-worktree code.
4. `service.Platform` is the only business-logic layer. CLI/HTTP/MCP
   are thin adapters.
5. `angee up` is **compose-only** (containerized services). `angee
   dev` is **compose + process-compose** (containerized + host
   processes, plus the operator HTTP/MCP server in-process). `angee
   start/stop/restart <name>` is service-only; workspaces use
   `workspace start/stop/restart`. `angee logs <name>` routes by
   `runtime:`.
6. `angee build` is a `docker compose build` shim; `angee up --build`
   and `angee dev --build` route through the same code path.
7. `angee dev` compiles to `process-compose.yaml` and starts the
   process-compose binary; runtime control goes through its REST API.
   No custom local-process supervisor.
8. Secrets go through a `Backend` interface; v1 implements `.env` and
   `openbao` natively in Go.
9. Templates are Copier templates with `_angee` metadata, rendered
   in-process by the `copier-go` library (no Python runtime). Resolved
   from `.templates/<kind>/<name>/` first, then from the binary's
   embedded fallbacks.
10. The Workspace provisioning primitive is the one feature Angee
    builds from scratch.
11. **One operator process serves HTTP/MCP for exactly one
    `ANGEE_ROOT`. Reconciliation against any single root runs under a
    per-root advisory lock (`<root>/run/operator.lock`).** A single
    operator process may make scoped `service.Platform.Stack*(<other-root>)`
    library calls against multiple roots (template chaining works this
    way); each call takes the lock for the target root.
12. The operator is a first-class git client for `kind: git` sources.
    It owns `fetch/pull/push/status` on source caches and worktree
    branches. PR creation stays in the worktree (the agent runs `gh pr
    create`).
13. `workspace destroy` does not auto-stop services. Lifecycles are
    independent; the runtime-safety guard refuses destroy when a
    running service mounts the workspace.
14. No backward-compatibility branches in active code. Old paths land
    in `_deprecated/`.

## Success Shape

```sh
# Goal 1 — dev loop with everything running together (no workspaces yet)
angee init --dev --yes
angee dev --build
# → operator HTTP+MCP on 127.0.0.1:9000
# → postgres, redis container sidecars
# → Django, Vite, build-watch as supervised local processes
# → ANGEE_OPERATOR_URL/TOKEN injected into every local process

# Goal 1, staging
angee stack init staging-docker --yes --input domain=staging.example.com
angee build && angee up
angee logs web --follow

# Goal 2 — provision a workspace + service into the live dev session
# (in a second terminal, while `angee dev` is running)
angee workspace create pr \
  --input branch=feat/issue-123 \
  --start --yes
# → resolves the template at .templates/workspaces/pr/
# → name auto-derived → "feat-issue-123"
# → worktree added off the global "django-angee" source
# → optional inner stack provisioned in the worktree

angee service init agent-claude-feat-issue-123 \
  --image ghcr.io/example/claude-runner:latest \
  --mount workspace://feat-issue-123:/workspace \
  --env ANTHROPIC_API_KEY='${secret.anthropic-api-key}' \
  --start --yes

# Or from a Django process running inside `angee dev`:
curl -H "Authorization: Bearer $ANGEE_OPERATOR_TOKEN" \
  -X POST $ANGEE_OPERATOR_URL/workspaces \
  -H "Content-Type: application/json" \
  -d '{
        "template": "workspaces/pr",
        "inputs":   { "branch": "feat/issue-123" },
        "start":    true
      }'

curl -H "Authorization: Bearer $ANGEE_OPERATOR_TOKEN" \
  -X POST $ANGEE_OPERATOR_URL/services \
  -H "Content-Type: application/json" \
  -d '{
        "name":    "agent-claude-feat-issue-123",
        "runtime": "container",
        "image":   "ghcr.io/example/claude-runner:latest",
        "mounts":  ["workspace://feat-issue-123:/workspace"],
        "start":   true
      }'

# Status + ship branch:
angee workspace get  feat-issue-123
angee source  status django-angee
angee workspace push feat-issue-123          # operator pushes worktree's branch upstream

# Tear down (caller composes service + workspace explicitly):
angee service   destroy agent-claude-feat-issue-123
angee workspace destroy feat-issue-123 --purge
```

If those flows are boring — and the dev loop, the staging deploy, and
the on-the-fly workspace + service composition all use the same
operator surface, with no agent-shaped code anywhere in Angee — v1 is
done.
