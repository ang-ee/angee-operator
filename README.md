# Angee

Angee is a self-managed stack manager for agent-native applications. It
compiles one `angee.yaml` into runtime files, resolves secrets, manages
container and local-process services, runs jobs, provisions workspaces, and
exposes the same control plane through the CLI plus REST and GraphQL operator
APIs.

This repository contains the current Go prototype.

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
uses the current directory, or `./.angee` when that directory contains a
manifest.

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
angee service list
angee job list
angee job run <name> [--input key=value ...]

# Sources and workspaces
angee source list
angee source fetch <name>
angee source status <name>
angee source pull <name>
angee source push <name> [--ref ref]
angee workspace create <template> [--name name] [--ttl duration] [--input key=value ...] [--start]
angee workspace list
angee workspace get <name>
angee workspace status [name]
angee workspace git <name>
angee workspace push <name> [--ref ref]
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
| `docs/` | Current user and developer reference docs. |

## Documentation

- [Commands](docs/COMMANDS.md)
- [Manifest](docs/MANIFEST.md)
- [Operator API](docs/OPERATOR-API.md)
- [Templates](docs/TEMPLATES.md)
- [Development](docs/DEVELOPMENT.md)
- [Changelog](CHANGELOG.md)

## Development

```sh
make test
make build
```
