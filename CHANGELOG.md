# Changelog

All notable changes to this repository should be recorded here. Sections
correspond to released git tags; `Unreleased` collects work merged after the
latest tag.

## Unreleased

### Breaking

- **Workspaces are now a pure file primitive.** The operator no longer
  owns workspace service lifecycle; an `angee workspace` instance only
  renders Copier output (including any chained inner-stack templates as
  files) and materialises sources. Removed:
  - `Platform.WorkspaceStart` / `WorkspaceStop`
  - `angee workspace start|stop|restart` CLI subcommands
  - `--start` flag on `angee workspace create`
  - `POST /workspaces/{name}/start|stop|restart` REST endpoints
  - `workspaceStart` / `workspaceStop` / `workspaceRestart` GraphQL mutations
  - `start` field on the GraphQL `WorkspaceCreateInput` and on
    `api.WorkspaceCreateRequest`
  - `_angee.chain_lifecycle` template metadata field and the corresponding
    `WorkspaceResolved.Lifecycle` / `api.WorkspaceRef.Lifecycle` /
    `api.WorkspaceStatusResponse.Lifecycle` fields (and the
    `WorkspaceStatus.lifecycle` GraphQL field)

  If a workspace renders an inner stack and you want to bring it up, drive
  it explicitly with `angee stack up --root workspaces/<name>/.angee` (or
  point a second operator at that root with `--root` /
  `--port`). `_angee.chain` and `_angee.chain_root` continue to work
  exactly as before — they only describe what to render and where.

### Operator

- Added secret CRUD over REST + GraphQL + CLI. Five operations
  (`secrets`, `secret(name)`, `secretValue(name)`, `secretSet`,
  `secretDelete`) backed by the existing `secrets.Backend` interface so
  both env-file and OpenBao deployments work without changes. The value
  read (`secretValue` / `GET /secrets/{name}/value` / `angee secret
  reveal`) is a dedicated endpoint so the privileged path is obvious in
  audit logs and code review. `secrets` (list) only returns declared
  secrets (entries in `stack.secrets`); set/delete accept any name
  matching `^[A-Za-z0-9._-]{1,256}$`. Mutating calls log to operator
  stderr (env-file's audit trail; OpenBao continues to own its own
  audit log).
- Closed the REST/GraphQL parity gap. Every operation added in this
  release now has a REST endpoint alongside the GraphQL surface, secured
  by the same admin-bearer middleware as the rest of the operator API.
  New endpoints: `GET /gitops/topology[?with_commits=N]`,
  `GET /sources/{name}/diff[?ref=...]`,
  `GET /workspaces/{name}/sources/{slot}/diff[?ref=...]`,
  `POST /workspaces/{name}/sources/{slot}/{fetch,pull,push,merge,rebase,merge-abort,rebase-abort,rebase-continue,publish}`,
  `POST /workspaces/preflight`,
  `GET /templates`, `GET /templates/{ref...}`,
  `POST /tokens/mint`. Subscriptions remain GraphQL-only by design
  (REST has no native pubsub).
- Added a GraphQL `Subscription` root over Server-Sent Events. Four
  operations are live: `onGitOpsTopologyChange`, `onWorkspaceStatusChange`,
  `onServiceLogs`, `onWorkspaceLogs`. Snapshot subscriptions poll the
  underlying query on a 2 s tick and publish only when the result hash
  changes; log subscriptions ride the existing runtime-backend follow
  channel. The transport is `POST /graphql` with
  `Accept: text/event-stream` per gqlgen 0.17.
- Retired the unused `GET /events` SSE stub (only ever emitted a static
  `ready` event). Live event streaming is now exclusively via GraphQL
  subscriptions on `/graphql`.
- Added `workspaceCreatePreflight(input)` mutation that validates a
  `WorkspaceCreateInput` against the resolved template's input
  declarations without materialising a workspace. Returns the effective
  inputs (after defaults), a `missingRequired` list, and an
  `invalidInputs` list of `{field, reason}` for type-mismatch failures.
- Added `mintConnectionToken(actor, ttl)` mutation that issues an
  HS256-signed JWT scoped to the supplied actor. Signing key resolves in
  this order: explicit `--jwt-secret` / `ANGEE_OPERATOR_JWT_SECRET`,
  then HKDF-derived from the admin `--token`, then a per-process random
  fallback for loopback dev. TTL defaults to 1 h, capped at 24 h.
- Added template-descriptor introspection: `templates: [TemplateDescriptor]!`
  walks `<root>/.templates/<kind>/*` and `<root>/templates/<kind>/*` and
  returns descriptors with `kind`, `name`, `path`, and per-input
  metadata. `template(ref)` returns the single descriptor for an explicit
  ref (relative path, absolute path, or supported remote URL).
- Added commit-DAG fields on `GitOpsTopology`: `gitOpsTopology(withCommits: Int)`
  populates `sources[].commits` with `{sha, parents, refs, time, summary,
  author}` for each git source, capped at `withCommits`. Default value
  is 0 so the polling subscription stays cheap; clients opt in for the
  DAG renderer view.
- Added `sourceDiff(name, ref)` and `workspaceSourceDiff(workspace, slot, ref)`
  queries that return unified-diff `[DiffFile{hunks: [DiffHunk]}]`
  payloads parsed via `bluekeyes/go-gitdiff`. `ref` empty means
  uncommitted (working-tree-vs-HEAD); otherwise it diffs against the
  named revision.
- Added higher-level convergence mutations on workspace source slots:
  `workspaceSourceMerge(ref)`, `workspaceSourceRebase(ref)`,
  `workspaceSourceMergeAbort`, `workspaceSourceRebaseAbort`,
  `workspaceSourceRebaseContinue`, and `workspaceSourcePublish(remote,
  branch)`. Each returns a `GitOpResult` carrying `{ok, conflicted,
  conflictFiles, message}`. On conflict the worktree is left in the
  conflicted state for the caller to resolve; conflict files come from
  `git ls-files -u` rather than parsing free-form stderr.
- Added integration tests covering behind/diverged worktrees, dirty
  worktrees blocking pull/push, missing workspace-source paths, and
  undeclared source references in `gitOpsTopology`.

### Templates

- Added the bundled `templates/agent-runtime/` workspace template with
  a documented env-var contract: `AGENT_ID` (required), `MCP_URL` /
  `MCP_TOKEN` (optional, caller-supplied), `ACP_PORT` (allocated from
  the host stack's `acp` port pool), `ACP_TOKEN` (resolved via
  `${secret:acp_token}`). v1 ships a placeholder service that prints
  the contract and sleeps; downstream consumers (e.g. the angee-django
  `agents` addon) replace the command block with a real runtime
  invocation. Contract documented in `docs/guide/templates.md`.

### Dependencies

- `github.com/golang-jwt/jwt/v5@v5.3.1` (MIT) for `mintConnectionToken`.
- `github.com/bluekeyes/go-gitdiff@v0.8.1` (MIT) for the diff queries.

## v0.4.12 — 2026-05-15

### Operator

- Moved the stack-teardown shutdown trigger from SIGHUP to SIGINT (Ctrl-C
  in a foreground operator, or what process-compose forwards when a dev
  stack is interrupted). SIGTERM still performs a graceful HTTP shutdown
  only, and SIGHUP is back to its default disposition. This supersedes the
  SIGHUP-based behavior shipped in v0.4.11.

## v0.4.11 — 2026-05-15

### Operator

- The operator now treats SIGHUP as "shut down and bring the local stack
  down with you": after the HTTP server has shut down it runs
  `platform.StackDown`, terminating docker-compose services and
  process-compose-managed local processes. SIGINT and SIGTERM keep their
  prior behavior of stopping only the HTTP server. (Superseded by v0.4.12.)

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
