# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is angee

Self-managed agent containerization and orchestration engine. An extension of docker-compose where AI agents are first-class citizens. Users define services, MCP servers, and AI agents in a single `angee.yaml`, then run one command to get a fully operational platform.

## Build & Development Commands

```sh
make build            # Build operator + CLI → dist/angee-operator, dist/angee
make build-cli        # Build CLI only
make build-operator   # Build operator only
make test             # go test -v -race ./...
make test-cover       # Tests with coverage report (opens HTML)
make lint             # golangci-lint run ./...
make fmt              # gofmt + goimports
make vet              # go vet ./...
make check            # fmt + vet + lint + test (full pre-commit check)
make run-operator     # Build and run operator against ~/.angee
make dev ARGS="init"  # Build CLI and run with args
```

Run a single test: `go test -v -race -run TestName ./internal/compiler/`

Requirements: Go 1.25+, Docker, git, golangci-lint (for linting)

## Architecture

**Operator embedding:**
- `cmd/angee/` — CLI tool. Calls `cli.Execute()` which sets up Cobra commands and includes a hidden `operator` subcommand for explicitly requested operator services.
- `cmd/operator/` — standalone daemon wrapper for the same operator package when a separate service binary is useful.

**CLI → Operator → Backend flow:** The CLI never touches containers directly. Local commands instantiate the operator runtime in-process and dispatch through shared API request/response types without opening ports. `--operator`/`ANGEE_OPERATOR_URL` are explicit remote-service opt-ins. The operator compiles `angee.yaml` into backend files and delegates to a `RuntimeBackend`.

**Key packages:**

| Package | Purpose |
|---------|---------|
| `cli/` | Cobra command implementations. Each file = one command. `root.go` has global flags (`--root`, `--operator`, `--json`). |
| `internal/config/` | YAML types. `angee.go` = `AngeeConfig` (the source of truth). `operator.go` = `OperatorConfig` (local runtime config). |
| `internal/compiler/` | Translates `AngeeConfig` → `ComposeFile` (docker-compose YAML). Handles Traefik labels, lifecycle policies, agent env injection. |
| `internal/runtime/` | `RuntimeBackend` interface. Only implementation: `compose/backend.go` (shells out to `docker compose`). Kubernetes backend planned for Phase 2. |
| `internal/operator/` | HTTP server. `server.go` = setup/routing. `handlers.go` = all endpoint logic (deploy, rollback, status, logs, agent control). |
| `internal/root/` | ANGEE_ROOT filesystem management (`~/.angee/`). Creates directory structure, reads/writes configs, manages git. |
| `internal/git/` | Git CLI wrapper. Every deploy = git commit. Rollback = git revert/reset. |
| `internal/tmpl/` | Target template resolver. Templates should be Copier templates with Angee `_angee` metadata, fetched from local paths, bundled refs, or remote refs. |

## Key Concepts

**Lifecycles** determine service behavior (restart policy, routing, scaling):
- `platform` — web-facing, gets Traefik routing labels and domains
- `sidecar` — internal service (DB, cache), always restarts
- `worker` — background processing, always restarts
- `system` — always-on agent, always restarts
- `agent` — AI agent
- `job` — one-shot or scheduled

**ANGEE_ROOT** is the control directory for one stack, usually `.angee` in a project worktree. Target contents include:
- `angee.yaml` — source of truth for sources, services, jobs, workflows, workspaces, agents, MCP servers, secrets, port leases, and backend settings
- `state/` — file-backed observed state, locks, leases, and workflow records when using the file state source
- `agents/<name>/` — rendered agent config, instructions, state, and workspace material
- `workspaces/<name>/` — rendered workspace files, sources, state, and local runtime material
- generated backend files such as `docker-compose.yaml` when the selected backend needs them

**Connectors are application-managed.** Angee does not manage connectors (OAuth, API keys, IMAP, etc.) — the application layer (e.g., Django's `fyltr.connect`) handles connection management. Angee's role is limited to storing secrets via `${secret:name}` resolution.

**RuntimeBackend interface** (`internal/runtime/backend.go`): All runtime interaction goes through this interface (Diff, Apply, Status, Logs, Scale, Stop, Down). Adding a new backend means implementing this interface.

## Dependencies

Core dependencies:

- `github.com/spf13/cobra` — CLI framework.
- `gopkg.in/yaml.v3` — config parsing for `angee.yaml` and rendered template metadata.

Target additions should serve the unified operator path, not a separate mode:

- Copier integration for rendering and updating stack, workspace, and agent templates.
- `github.com/charmbracelet/{bubbletea,lipgloss,bubbles}` only for optional terminal UIs.

Target refactor rules:

- One manifest: `$ANGEE_ROOT/angee.yaml`.
- No separate project/compose modes.
- No legacy template metadata file.
- No framework-specific CLI dispatch for Django, React, Vite, uv, pnpm, or `manage.py`.
- Unused commands, flags, adapters, and compatibility paths should be removed from active code. Reference-only material can live under `deferred/`, but not as buildable/imported Go code.
- `angee dev` runs the operator runtime for the lifetime of the dev command, reconciles declared services/jobs/workflows from `angee.yaml`, and stops local dev processes on exit.
- `angee stack init`, `angee workspace init`, `angee agent init`, HTTP, MCP, and backend control planes must reuse the same operator provisioning code.

## Patterns

- Config structs use `yaml:"field"` and `json:"field"` tags consistently
- The compiler outputs `map[string]any` for docker-compose compatibility (not typed structs)
- CLI commands follow the pattern: parse flags, dispatch to the in-process operator runtime or an explicitly configured remote operator, format response (or `--json` for raw)
- Operator handlers follow: parse request, load `angee.yaml`, resolve templates/sources/secrets/ports, reconcile through backend, respond
- Templates use Copier with `copier.yml`; Angee-specific template metadata lives under `_angee`
