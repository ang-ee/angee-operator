# Operator API gaps

This tracks the control-plane surface needed by humans, `angee-django`, and agents that need to inspect a stack, update sources, and redeploy without shelling into the host.

## Current transports

- REST is implemented directly in `internal/operator`.
- GraphQL is available at `POST /graphql`. It mirrors the request/response REST operations for stacks, services, jobs, sources, and workspaces, with GraphQL introspection for Django clients.
- MCP is still a static descriptor at `GET /mcp`; it is not a JSON-RPC MCP server yet.

GraphQL intentionally does not replace streaming surfaces. `stackLogs`, `serviceLogs`, and `workspaceLogs` return snapshot strings. `/events` remains the REST/SSE place for future streaming, and GraphQL subscriptions should wait until there is a real operation/event bus.

Dependency note: the GraphQL transport uses `github.com/graphql-go/graphql` because the Go standard library has no GraphQL parser, executor, validation, or introspection implementation. `angee-django` needs a real GraphQL contract rather than a hand-parsed JSON query shim, and this package is the smallest code-first dependency that lets the operator keep its current `service.Platform` boundary.

## Keep transports DRY

`service.Platform` is currently the shared business-logic boundary. The next step should be a transport-neutral operation registry in `api/`:

- operation name, description, input DTO, output DTO, sync/async behavior, and permission metadata
- adapters that render REST routes, GraphQL fields, MCP `tools/list` metadata, and MCP `tools/call` dispatch from the same registry
- generated OpenAPI and GraphQL SDL/introspection tests from the same operation list

Framework options:

- `goa.design/goa` is strong for design-first REST, generated clients, and OpenAPI, but it does not give MCP or GraphQL for free.
- `gqlgen` is stronger than `graphql-go/graphql` for schema-first GraphQL and typed resolvers, but REST and MCP would still need a separate source of truth.
- A small Angee registry over Go DTOs or CUE is probably the best fit if MCP parity is a hard requirement. It can emit OpenAPI, GraphQL schema/resolvers, and MCP tool schemas while keeping `service.Platform` as the executor.

## Missing stack/service APIs

- Real runtime status: `StackStatus` currently reports declared services. It should merge backend state, health, container/process IDs, exposed URLs, and last error.
- Service inspect/get: REST and GraphQL list services, but there is no single service detail endpoint with full manifest config plus runtime state.
- Service update coverage: `build`, `env_file`, `after`, `depends_on`, domains/routes, resources, replicas, secrets, and health checks are not manageable through the API.
- Async operations: long-running build/up/create/fetch calls block. They should return operation IDs with progress over SSE and, later, GraphQL subscriptions.

## Missing source APIs

- Source CRUD: create/update/delete source declarations, not only list/status/fetch/pull/push declared sources.
- Source kinds: only `git` and `local` materialize; `template`, `archive`, `url`, and `volume` are still design-only.
- Git auth: `auth.mode` fields exist but are not wired into `git` calls for SSH keys, HTTPS tokens, or loopback-only host auth.
- Rich git status: return HEAD SHA, branch, upstream, ahead/behind counts, remote URLs, dirty file list, and worktree list.
- Branch operations: list, create, checkout, rename, delete, set upstream, and prune.
- Diff operations: summary diff, per-file diff, staged vs unstaged, base ref selection, and binary/file metadata.
- Commit operations: stage/unstage, commit with identity, amend, reset, stash, and clean with safety guards.
- Sync operations: fetch, pull/merge/rebase strategy, conflict reporting, push with `--set-upstream` support and preflight checks.
- PR operations: decide whether Angee owns this or delegates to app/agent credentials. If owned, model provider, remote, base/head, title/body, draft flag, labels, reviewers, and returned URL.

## Missing workspace APIs

- Workspace detail should include rendered inputs, source slots, chain root, mounted paths, inner stack status, TTL, runtime state, and mounted-by services.
- Workspace source sync should fetch/update sources and optionally rerender template chain.
- Workspace git operations should accept a source slot, not only push every worktree-mode source.
- Workspace destroy needs safety guards for dirty worktrees, running services, and persisted paths.
- TTL sweep loop is not implemented even though TTL fields are persisted.

## Missing MCP/event APIs

- Implement real JSON-RPC 2.0 MCP with `initialize`, `tools/list`, and `tools/call`.
- Replace `/events` one-shot response with an event bus for operation progress and state changes.
- Add `/operations/{id}` for polling, cancellation where safe, and durable enough history for UI refreshes.
