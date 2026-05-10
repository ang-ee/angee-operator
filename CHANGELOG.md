# Changelog

All notable changes to this repository should be recorded here. Sections
correspond to released git tags; `Unreleased` collects work merged after the
latest tag.

## Unreleased

## v0.4.7 — 2026-05-10

### Documentation

- Replaced the docs logo with the angee-django isometric cube SVG and
  rendered it as the homepage hero image.
- Reframed the homepage and `Concepts` page around the engine / Host
  boundary: the `angee` CLI + operator is the stack manager; the Host
  runtime (today [`angee-django`](https://github.com/fyltr/angee-django))
  composes Blocks into a working app on top.
- Added `docs/guide/concepts.md` covering Stack, Source, Workspace,
  Service, Host, and how self-building works end-to-end.

### Documentation (carried from prior Unreleased)

- Stood up a VitePress site under `docs/` published at
  [docs.angee.ai](https://docs.angee.ai) via GitHub Pages
  (`.github/workflows/docs.yml`). Existing markdown moved to
  `docs/guide/` and `docs/reference/`; internal design notes moved to
  `.agents/notes/`.
- Added prebuild scripts that render `internal/operator/schema.graphql`
  into `docs/reference/graphql/` and `docs/public/angee.schema.json` into
  `docs/reference/manifest-schema.md` on every site build.
- Moved the canonical manifest schema to `docs/public/angee.schema.json`
  so it is served at <https://docs.angee.ai/angee.schema.json>;
  `cmd/schema` now stamps `$id` accordingly.
- Reworked repository documentation to describe current implemented behavior.
- Moved changelog material to this root `CHANGELOG.md`.
- Documented the current CLI surface, manifest schema, operator API, template
  resolver, and development commands.
- Added `docs/reference/surfaces.md` enumerating every `service.Platform`
  method and its CLI / REST / GraphQL exposure (R5).
- Generated `docs/public/angee.schema.json` from the manifest types and
  documented editor LSP integration via `# yaml-language-server: $schema=...`
  (R8).
- Added `make schema` to `docs/guide/development.md` (R8).

### Operator API

- Replaced the hand-built `graphql-go/graphql` schema (~1100 LoC) with a
  schema-first `99designs/gqlgen` resolver layer rooted at
  `internal/operator/schema.graphql`. Body-size, content-type, log-output
  limiting, and the route-level cross-origin protection are preserved (R2).
- Added a `make check-generated` step that runs gqlgen and fails on
  out-of-date `internal/operator/gql/` (R2).
- Added typed domain errors (`*service.NotFoundError`, `*service.ConflictError`,
  `*service.InvalidInputError`) with consistent REST (404 / 409 / 400),
  GraphQL `extensions`, and CLI status preservation. The remote CLI client
  now decodes `api.ErrorResponse` and returns `errors.As`-able typed errors
  instead of flattening every non-2xx to a single string (R6).
- Added GraphQL surface coverage for `gitOpsTopology`, workspace source
  `fetch`/`pull`/`push`, and `WorkspaceLogsLimited` / `StackLogsLimited`
  (gaps identified by R5).

### CLI

- `angee workspace start|stop|restart|logs` accept an optional `[name]` when
  run from inside `ANGEE_ROOT/workspaces/<name>`, matching `status` and
  `sync-base` (R10).
- CLI no longer string-matches error messages to classify failures; uses
  `errors.As` against typed domain errors (R6).

### Service / runtime

- Migrated read-only git operations (`RefExists`, `CurrentBranch`,
  `CurrentRef`, `Upstream`, `AheadBehind`/`AheadCount`, `Config`, `Remotes`,
  `Dirty`) to `go-git/v5`. Network, worktree, and merge/rebase ops continue
  to shell out to the `git` CLI; the boundary is documented in the package
  doc-comment (R1a).
- Native `git` CLI fallback for read-only status calls when go-git cannot
  parse `extensions.worktreeConfig`, so workspace `branch-mismatch` is based
  on actual `current_ref != branch` rather than a parse failure (R10).
- Process-compose runtime targets now carry a control port; the backend
  passes `--address 127.0.0.1 --port <port>` for `up`, `down`, `start`,
  `stop`, `restart`, and `logs`. Root stacks default to `8080`; rendered
  stacks declare `ports.process_compose.value`. Workspace lifecycle and log
  commands inherit the rendered inner-stack port (R10).
- Workspace JSON, REST responses, GraphQL, and CLI text status expose
  `process_compose_port`, `playwright_mcp_name`, and `playwright_mcp_url`
  (R10).
- `dev-pr` and `dev-pr-multi` workspace templates render
  `${workspace.path}/.angee/data/chrome` as the Playwright `--user-data-dir`,
  isolating browser state per workspace; Django `ANGEE_DATA` continues to
  live at `{{ ANGEE_ROOT }}/data` (R10).
- Application asset loader adopts an existing target row when exactly one
  matching unique field is present and creates the missing ledger row,
  instead of attempting a duplicate insert (R10).

### Internal / refactors

- Hoisted root discovery into `internal/stackroot.Resolve`; CLI, doctor, and
  operator now share one walk-up implementation (R4).
- Promoted `JobRunRequest` and `WorkspaceUpdateRequest` to the shared `api/`
  package; deleted three sets of duplicated inline structs across CLI and
  operator (R3).
- Split `Stack.Defaults()` (mutating) from `Stack.Validate()` (pure) and
  introduced `go-playground/validator` for struct-tag constraint checks (R8).
- Replaced `sortedServiceStates`, `sortedJobStates`, and `sortedWorkspaceRefs`
  in `internal/operator/graphql.go` with a single generic `sortedMapValues`
  helper (R9).

### Evaluations (no code change)

- Evaluated `compose-spec/compose-go v2` against the local Compose model and
  decided to keep the local minimal model. The container runtime renders
  exactly the fields needed by current manifests and templates; revisit when
  a template needs an unsupported field (R7). Tracked in
  `.agents/plans/LATEST.md` Migration 3.
- Deferred the full go-git migration (network / worktree / merge) until
  upstream parallel-checkout (go-git/go-git#1956) lands and credential-helper
  parity is designed (R1b). Tracked in `.agents/plans/LATEST.md`.

## v0.4.6 — 2026-05-10

- Operator: added GitOps topology API (`#10`).

## v0.4.5 — 2026-05-09

- CLI: improved `ANGEE_ROOT` detection.
- Workspaces: stopped creating absolute symlinks during materialization.

## v0.4.4 — 2026-05-09

- CLI: `angee workspace status` now infers the target workspace from the
  current working directory when invoked without a name.
- Documentation refresh.

## v0.4.3 — 2026-05-09

- CLI: `angee workspace status` reports full source state, including
  branch-mismatch detection (`#8`).

## v0.4.2 — 2026-05-08

- CLI: added `angee doctor` for environment diagnostics (`#7`).

## v0.4.1 — 2026-05-08

- CLI: added `angee workspace open` (`#6`).
