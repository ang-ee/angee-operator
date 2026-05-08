# Angee

Angee is a self-managed stack orchestration engine for agent-native applications. It compiles one declarative `angee.yaml` into runtime files, resolves secrets, manages services and jobs, provisions workspaces, and exposes the same control plane through the CLI plus REST and GraphQL operator APIs.

This branch is the v1 prototype. The old v0 line is preserved on `main-v0`; the prototype history is preserved on `prototype-v1`.

## Install

```sh
curl -fsSL https://angee.ai/install.sh | sh
```

From a checkout:

```sh
make init
```

`make init` builds `dist/angee` and `dist/angee-operator`, then runs `scripts/install.sh` against those local binaries. Set `ANGEE_INSTALL_DIR` to install somewhere other than `/usr/local/bin`.

## Quick Start

```sh
angee doctor
angee init --dev --yes
angee dev
```

`angee doctor` checks local prerequisites, the selected `ANGEE_ROOT`, manifest validity, source paths, ports, git ignore hygiene, and nearby templates. `angee init --dev` renders the local dev stack template into `.angee/`. `angee dev` prepares declared runtime files, starts container sidecars and local processes, and stays in the foreground.

## Core Commands

```sh
# Stack
angee doctor
angee init --dev [path]
angee stack init <template> [path] [--input key=value ...]
angee stack update
angee status

# Runtime
angee build [service...]
angee up [service...] [--build]
angee dev [--build]
angee down
angee logs [service...]

# Services and jobs
angee service init <name> [--runtime container|local] [--image image] [--command arg ...]
angee service list
angee job list
angee job run <name>

# Sources and workspaces
angee source list
angee source fetch <name>
angee workspace create <template> [--name name] [--input key=value ...]
angee workspace list
angee workspace git <name>

# Operator
angee operator --root .angee --bind 127.0.0.1 --port 9000
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

The CLI runs in-process by default. Passing `--operator` or setting `ANGEE_OPERATOR_URL` dispatches supported operations to a running HTTP operator.

## Project Layout

| Path | Purpose |
|---|---|
| `api/` | Shared API request and response types. |
| `cmd/angee/` | CLI entrypoint. |
| `cmd/operator/` | Standalone operator entrypoint. |
| `internal/cli/` | Cobra command implementation and HTTP operator client. |
| `internal/operator/` | HTTP operator server and routes. |
| `internal/service/` | Shared business logic for stacks, services, sources, jobs, and workspaces. |
| `templates/` | Bundled prototype stack and workspace templates. |
| `docs/` | Refactor notes, deferred items, and example v2 template sketches. |
| `scripts/install.sh` | Website installer script. |

## Development

```sh
make test
make build
```

Useful references:

- `docs/OVERVIEW-v2.md`
- `docs/PLAN.md`
- `docs/DEFERRED.md`
- `docs/API-GAPS.md`
