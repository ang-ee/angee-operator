# Angee

Angee is a self-managed stack manager for agent-native applications. It
compiles one `angee.yaml` into runtime files, resolves secrets, manages
container and local-process services, runs jobs, provisions workspaces, and
exposes the same control plane through the CLI plus REST and GraphQL operator
APIs.

This repository contains the current Go prototype.

## What the operator owns

One operator runs against exactly one Stack root. Everything below is what
a client (CLI, Django host, agent, custom UI) can read and drive over
REST + GraphQL:

| Primitive | Operations |
| --- | --- |
| **Stack** | `stackInit`, `stackPrepare` (compiled view, no run), `stackUp`/`Dev`/`Down`/`Build`/`Update`/`Destroy`, `stackStatus`, `stackLogs`. |
| **Service** | `services`, `serviceInit`/`Update`/`Start`/`Stop`/`Restart`/`Destroy`, `serviceLogs`. |
| **Job** | `jobs`, `jobRun`. |
| **Source** | `sources`, `sourceFetch`, **`sourcePull`** (= update from upstream), `sourcePush`, `sourceDiff`. |
| **Workspace** (file primitive — never starts services) | `workspaces`, `workspaceCreate`, `workspaceCreatePreflight` (validate without rendering), `workspaceUpdate`, `workspaceStatus`, `workspaceLogs`, `workspaceDestroy`, `workspaceGit`, `workspacePush`, **`workspaceSyncBase`** (= multi-slot update against base). |
| **Workspace source slot** (one git materialization inside a workspace) | `workspaceSourceFetch`, **`workspaceSourcePull`** (= slot-level update), `workspaceSourcePush`, `workspaceSourceMerge`/`Rebase`/`MergeAbort`/`RebaseAbort`/`RebaseContinue`/`Publish`, `workspaceSourceDiff`. |
| **GitOps topology** (derived view: sources × slots) | `gitOpsTopology(withCommits: Int)` snapshot; `onGitOpsTopologyChange` live. |
| **Templates** (discoverable Copier templates) | `templates`, `template(ref)`. |
| **Connection token** (per-actor scoped JWT) | `mintConnectionToken(actor, ttl)` (gated by admin bearer). |
| **Subscriptions** (SSE, GraphQL-only) | `onGitOpsTopologyChange`, `onWorkspaceStatusChange`, `onServiceLogs`, `onWorkspaceLogs`. |

Every operation above is reachable over both REST (`POST /...` /
`GET /...`) and GraphQL — subscriptions are the one deliberate split
(REST has no native pubsub). The operator's admin bearer token guards
the entire surface; `mintConnectionToken` issues short-lived per-actor
JWTs for finer-grained scoping.

"Update" has three scopes: `sourcePull` (whole source), `workspaceSourcePull`
(one slot), `workspaceSyncBase` (every slot's workspace branch against its
base ref — typically `origin/main`).

Workspaces materialize files — including any chained inner-stack template
as files. They never start services. If a workspace renders an inner stack
and you want it running, drive it explicitly:

```sh
angee stack up --root workspaces/<name>/.angee
# or run a second operator pointed at that root:
angee operator --root workspaces/<name>/.angee --port 9100
```

See [docs/guide/concepts.md](docs/guide/concepts.md) for the full
mental model and [docs/reference/operator-api.md](docs/reference/operator-api.md)
for the REST + GraphQL contract.

## Install

From a release:

```sh
curl -fsSL https://angee.ai/install.sh | sh
```

From this checkout:

```sh
make install
```

`make install` builds `dist/angee` and `dist/angee-operator`, then runs
`scripts/install.sh` against those local binaries. Set `ANGEE_INSTALL_DIR` to
install somewhere other than `/usr/local/bin`.

## Quick Start

Angee needs an `angee.yaml` in the selected `ANGEE_ROOT`. By default the CLI
walks upward from the current directory, or uses the checkout's `.angee` for
dev checkouts that contain `templates/workspaces`.

```sh
angee doctor
angee status
angee up
angee dev
```

`angee init --dev --yes` is supported when a `dev` stack template is available
through the template search paths.

## Core Commands

```sh
# Stack
angee doctor
angee stack init <template> [path] [--input key=value ...]
angee stack update
angee stack destroy [--purge]
angee status

# Runtime
angee build [service...]
angee up [service...] [--build]
angee dev [--build]
angee down
angee start <service>...
angee stop <service>...
angee restart <service>...
angee logs [service...] [--follow]

# Services and jobs
angee service init <name> [--runtime container|local] [--image image] [--command arg ...]
angee service update <name>
angee service destroy <name> [--stop=false]
angee service list  # or: angee service ls
angee job list      # or: angee job ls
angee job run <name> [--input key=value ...]

# Sources and workspaces
angee source list   # or: angee source ls
angee source fetch <name>
angee source status <name>
angee source pull <name>
angee source push <name> [--ref ref]
angee workspace create <name> --template <template> [--ttl duration] [--input key=value ...]
angee workspace list  # or: angee ws ls
angee workspace get <name>
angee workspace status [name]
angee workspace git <name>
angee workspace push <name> [--ref ref]
angee workspace sync-base [name] [--merge|--rebase]
angee workspace destroy <name> [--purge]

# Operator
angee operator --root . --bind 127.0.0.1 --port 9000
angee --operator http://127.0.0.1:9000 status
curl -s http://127.0.0.1:9000/graphql \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ stackStatus { name services { name runtime status } } }"}'
```

## Architecture

```text
CLI / HTTP operator
        |
        v
service.Platform
        |
        +-- docker compose for container services
        +-- process-compose for local processes
        +-- copier-go for stack and workspace templates
        +-- git for source caches and worktrees
        +-- env-file or OpenBao for secrets
```

Local CLI commands instantiate `service.Platform` directly. Passing
`--operator` or setting `ANGEE_OPERATOR_URL` dispatches supported operations to
a running HTTP operator.

## Project Layout

| Path | Purpose |
|---|---|
| `api/` | Shared API request and response types. |
| `cmd/angee/` | CLI entrypoint. |
| `cmd/operator/` | Standalone operator entrypoint. |
| `internal/cli/` | Cobra command implementation and HTTP operator client. |
| `internal/manifest/` | `angee.yaml` schema, validation, and load/save helpers. |
| `internal/operator/` | HTTP operator server, REST routes, and GraphQL schema. |
| `internal/runtime/` | Runtime backend interface plus compose and process-compose backends. |
| `internal/service/` | Shared business logic for stacks, services, sources, jobs, and workspaces. |
| `scripts/install.sh` | Release/local binary installer. |
| `docs/` | VitePress source for [docs.angee.ai](https://docs.angee.ai). |

## Documentation

The published documentation lives at [docs.angee.ai](https://docs.angee.ai).
Source markdown for the site is in `docs/`:

- [Getting started](docs/guide/getting-started.md)
- [Commands](docs/guide/commands.md)
- [Manifest](docs/guide/manifest.md)
- [Templates](docs/guide/templates.md)
- [Development](docs/guide/development.md)
- [Operator API](docs/reference/operator-api.md)
- [Surface parity](docs/reference/surfaces.md)
- [Changelog](CHANGELOG.md)

To run the docs site locally:

```sh
cd docs
npm install
npm run dev
```

## Development

```sh
make test
make build
```
