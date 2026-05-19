# AGENTS.md - angee-go

This is the canonical agent-instructions file for `angee-go`, the Go CLI,
operator, and runtime backend implementation for Angee.

> ## For new development work, start in `angee-django`
>
> If you are starting a fresh session and want to make changes to angee-go, use
> the multi-repo workspace flow from `~/Work/fyltr/angee-django` rather than
> cloning or editing this repository directly:
>
> ```sh
> cd ~/Work/fyltr/angee-django
> angee workspace create fix-issue-123 --template dev-pr-multi
> ```
>
> Shared slash commands and sub-agents live in `angee-django/.agents/` and
> operate on the worktrees materialized by that workspace.

> ## Workspace branch identity
>
> In a managed Angee workspace, each materialized git source has a manifest
> branch, normally `workspace/<name>`. Stay on that branch. To update from
> `main`, merge/rebase `main` into the workspace branch or run
> `angee workspace sync-base [<name>]` from the host root or current workspace.
> Do not switch a
> workspace worktree to `main`, create a `codex/...` branch inside it, or keep
> working when `angee workspace status [<name>] --json` reports a source state of
> `branch-mismatch` or a top-level state of `discrepancy`.

## What Angee Is

Angee is a self-managed stack manager for agent-native applications. It
compiles one `angee.yaml` manifest into Docker Compose and process-compose
runtime files, resolves secrets, manages services and jobs, provisions
workspaces, and exposes the control plane through the CLI plus REST and GraphQL
operator APIs.

Agents, application MCP servers, OAuth connectors, and PR creation are
application-layer concerns. In this repository, the core primitives are stacks,
services, jobs, sources, workspaces, secrets, ports, and runtime backends.

## Build And Development Commands

```sh
make build            # Build dist/angee and dist/angee-operator
make build-cli        # Build the CLI only
make build-operator   # Build the standalone operator only
make test             # go test -v -race ./...
make fmt              # gofmt -w .
make vet              # go vet ./...
make check            # fmt + vet + test
make install          # Build and install local binaries via scripts/install.sh
make clean            # Remove dist/ and coverage.out
```

Run a focused test with:

```sh
go test -v -race -run TestName ./internal/service
```

Requirements: Go 1.25+, Docker, git, and process-compose for local-process
runtime services.

## Architecture

Local CLI commands instantiate `service.Platform` directly. Passing
`--operator` or setting `ANGEE_OPERATOR_URL` routes supported operations to a
running HTTP operator.

```text
CLI / HTTP operator
        |
        v
service.Platform
        |
        +-- docker compose for container services
        +-- process-compose for local services
        +-- copier-go for stack and workspace templates
        +-- git for source caches and worktrees
        +-- env-file or OpenBao for secrets
```

## Key Packages

| Package | Purpose |
|---|---|
| `api/` | Shared request and response DTOs. |
| `cmd/angee/` | CLI binary entrypoint. |
| `cmd/operator/` | Standalone operator binary entrypoint. |
| `internal/cli/` | Cobra commands and remote operator client. |
| `internal/copierx/` | Copier integration and `_angee` metadata parsing. |
| `internal/git/` | Thin git CLI wrapper. |
| `internal/manifest/` | `angee.yaml` schema, strict loading, validation, and saving. |
| `internal/mount/` | Mount URI parsing and workdir resolution. |
| `internal/operator/` | HTTP routes, GraphQL schema, auth, and server lifecycle. |
| `internal/ports/` | Port pool and lease helpers. |
| `internal/runtime/` | Runtime backend interface. |
| `internal/runtime/compose/` | Docker Compose backend. |
| `internal/runtime/proccompose/` | process-compose backend. |
| `internal/secrets/` | env-file and OpenBao secret backends. |
| `internal/service/` | Business logic shared by CLI and operator. |
| `internal/substitute/` | `${...}` substitution resolver and filters. |

## Current Concepts

- **Stack**: one `ANGEE_ROOT` containing `angee.yaml` plus generated runtime
  files and materialized workspace/source directories.
- **Service**: a long-running workload. `runtime: container` is rendered to
  Docker Compose; `runtime: local` is rendered to process-compose.
- **Job**: an explicitly invoked command with service-like env, mounts, and
  workdir handling.
- **Source**: reusable source material. Implemented materialization kinds are
  `git` and `local`.
- **Workspace**: a rendered Copier template under
  `$ANGEE_ROOT/workspaces/<name>`, optionally with materialized sources and an
  inner stack.
- **Operator**: the REST and GraphQL control-plane server for one root.

## Patterns

- Config structs use matching `yaml:"field"` and `json:"field"` tags.
- Runtime interaction goes through `internal/runtime.Backend`.
- CLI, REST, and GraphQL should dispatch through `service.Platform`; avoid
  implementing business logic in adapters.
- Generated runtime files are `docker-compose.yaml` and
  `process-compose.yaml`.
- Resolved OpenBao secrets are written to `run/secrets.env`; env-file secrets
  use the configured `secrets_backend.path`.
- Templates use Copier with `copier.yml`; Angee metadata lives under `_angee`.

## Documentation

Published at [docs.angee.ai](https://docs.angee.ai). Source lives in `docs/`
as a VitePress site (`cd docs && npm run dev` for a local preview):

- `README.md`
- `docs/guide/getting-started.md`
- `docs/guide/commands.md`
- `docs/guide/manifest.md`
- `docs/guide/templates.md`
- `docs/guide/development.md`
- `docs/reference/operator-api.md`
- `docs/reference/surfaces.md`
- `docs/reference/manifest-schema.md` (generated by `npm run gen:schema`)
- `docs/reference/graphql/` (generated by `npm run gen:graphql`)
- `CHANGELOG.md`

Internal design notes that are not part of the published site live in
`.agents/notes/`.

## Agent Workspace Files

The `.agents/` directory holds material that supports agentic development of
this repo but is intentionally not part of the published documentation.
Everything here is checked in.

```text
.agents/
├── agents/   # Sub-agent prompts (markdown with YAML frontmatter)
├── plans/    # Long-form implementation plans for in-flight refactors
└── notes/    # Internal design notes and TODOs not yet promoted to issues
```

- `agents/` contains sub-agent definitions. They're surfaced to Claude Code
  via the `.claude/agents` symlink (`.claude/agents` → `../.agents/agents`),
  which is how Claude Code auto-discovers project-local agents. Other tools
  with their own conventions (Codex, Gemini, etc.) can read the same files
  directly. Today this contains `go-code-reviewer.md`, which should be
  invoked proactively after any non-trivial Go change.
- `plans/LATEST.md` is the active multi-step implementation plan. When a
  plan is finished, summarise the outcome in `CHANGELOG.md` and either
  archive the plan or replace it with the next one.
- `notes/todo.md` tracks short follow-ups that aren't worth a full plan but
  shouldn't be lost. `notes/ideas.md` and other note files are scratch
  design material — they may be partially stale; prefer the manifest, the
  code, and the published docs as sources of truth.

Shared sub-agents and slash commands that operate across the whole Angee
multi-repo workspace (Django- and React-flavoured ones, the workspace
orchestrator skill, etc.) live in `angee-django/.agents/` rather than here.
