# Development

Requirements:

- Go 1.25+
- Docker, for container services and compose-backed tests
- git 2.24+
- process-compose, for local-process services

## Make Targets

```sh
make build
make build-cli
make build-operator
make generate
make check-generated
make schema
make check-schema
make test
make fmt
make vet
make check
make install
make clean
```

`make test` runs:

```sh
go test -v -race ./...
```

`make generate` refreshes gqlgen output for the operator GraphQL schema.
`make check-generated` runs generation and fails if `internal/operator/gql/`
is not committed fresh.

`make schema` refreshes `docs/public/angee.schema.json` from the manifest
structs for editor integration and YAML language-server completion.
`make check-schema` regenerates the schema and fails if it is not committed
fresh.

`make install` builds both binaries and runs `scripts/install.sh` with
`ANGEE_DIST_DIR` pointing at this checkout's `dist/`.

## Package Map

| Path | Purpose |
|---|---|
| `api/` | Shared DTOs for CLI, REST, and GraphQL. |
| `cmd/angee/` | CLI binary entrypoint. |
| `cmd/operator/` | Standalone operator binary entrypoint. |
| `internal/cli/` | Cobra command tree and remote operator client. |
| `internal/copierx/` | Copier integration and `_angee` metadata parsing. |
| `internal/git/` | Thin git CLI wrapper. |
| `internal/manifest/` | Manifest schema, strict YAML loading, validation, and save helpers. |
| `internal/mount/` | Mount URI parsing and workdir resolution. |
| `internal/operator/` | REST routes, GraphQL schema, auth, and server lifecycle. |
| `internal/ports/` | Port pool and lease helpers. |
| `internal/runtime/compose/` | Docker Compose backend. |
| `internal/runtime/proccompose/` | process-compose backend. |
| `internal/secrets/` | env-file and OpenBao secret backends. |
| `internal/service/` | Business logic shared by CLI and operator. |
| `internal/substitute/` | `${...}` substitution resolver and filters. |

## Useful Checks

```sh
go test ./...
go test -race ./...
make check-generated
make build
```

Use `angee doctor --root <path>` to inspect a local stack root and prerequisite
tooling.
