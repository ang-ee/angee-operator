# Changelog

All notable changes to this repository should be recorded here. Sections
correspond to released git tags; `Unreleased` collects work merged after the
latest tag.

## Unreleased

### Fixes

- Template rendering no longer HTML-escapes manifest and config values. The
  `copier-go` engine (pongo2) shipped with autoescape on by default, so a
  rendered value containing `"`, `<`, `>`, or `&` came out as `&quot;` / `&amp;`
  — corrupting non-HTML output such as a quoted `angee.yaml` value, which then
  failed to parse. Bumped `github.com/fyltr/copier-go` to the build that
  disables autoescape globally to match Jinja2 / Python Copier semantics
  (templates that genuinely emit HTML can still opt in with
  `{% autoescape on %}`).
- `scripts/install.sh` no longer calls the GitHub REST API to resolve the latest
  release. The unauthenticated API is rate limited to 60 requests/hr/IP, so on
  shared or CI networks (or after a few retries) the lookup returned an empty
  version and the installer silently fell back to building from source — which
  fails outright when Go isn't installed. It now downloads straight from
  `…/releases/latest/download/`, which needs no API call and isn't rate limited;
  building from source is now only a fallback when the binary download itself
  fails. Also fixed the release workflow's version pin, which stamped
  `ANGEE_VERSION=v0.5.14` and produced a doubled `vv0.5.14` download URL.

## v0.5.14 — 2026-06-15

### Improvements

- `ingress` gained two edge-routing controls. `ingress.routing` selects how the
  Caddy edge matches inbound requests: `host` (default, unchanged) gives one
  subdomain per service (`wss://<service>.<domain>/`), while `path` puts every
  routed service under one shared host with a stripped prefix
  (`wss://<domain>/<service>/`, override per service with `route.path`).
  `ingress.tls: off` drops the edge to plain HTTP so URLs become `ws://…` and
  the edge publishes only `:80`. Together (`routing: path`, `tls: off`,
  `domain: localhost`) they give a zero-setup local dev path —
  `ws://localhost/<service>/` with no wildcard DNS and no local-CA cert trust —
  while the defaults keep existing `host`/`auto` stacks byte-identical. The same
  per-service `forward_auth → /edge/verify` auth model applies in both modes.

## v0.5.13 — 2026-06-15

### Improvements

- `angee dev` now streams logs from **every** service regardless of runtime. It
  previously ran container services detached (`docker compose up -d`) and let
  process-compose own the foreground, so a mixed stack's container/agent logs
  never appeared. `dev` now brings both runtimes up attached and concurrently —
  `docker compose up` (no `-d`) interleaving its native per-service coloured
  output with the process-compose stream — and a shared context ties them
  together so Ctrl-C (or either backend exiting) shuts the other down cleanly.
  New `angee dev -d`/`--detach` starts the whole stack in the background and
  returns.

### Fixes

- `service create` now declares the secrets its service references. A service
  template is (correctly) forbidden from declaring `secrets:` itself, but a
  service legitimately references `${secret.NAME}` in its env/command — so the
  operator now declares those referenced secrets in the stack on the service's
  behalf, as plain external entries whose value comes from the secrets backend
  (e.g. a prior `secretSet`). Previously such a reference failed the post-create
  compose re-render with `secret "…" is not resolved` (only declared secrets are
  resolvable), which blocked agent provisioning whose per-agent token secret is
  set via the REST API rather than declared in a stack template. Referencing a
  secret grants no value: resolution still fails if none was ever set.

## v0.5.12 — 2026-06-14

### Fixes

- `angee stack update --template` now honours a workspace's allocated ports when
  re-rendering a workspace's inner stack. Previously the re-render sourced its
  inputs solely from the inner stack's frozen `.copier-answers.yml`; if that file
  drifted to template defaults (e.g. a stray direct re-render reset it), the
  update silently baked the default ports into the inner stack, colliding with
  the host stack and other workspaces. The update now detects a managed workspace
  inner stack (`<root>/workspaces/<name>/<chain_root>`) and overlays the
  authoritative `${alloc.*}`-derived port inputs from the parent stack's
  `workspaces.<name>.resolved.allocations` record. Only allocation-bearing inputs
  are reconciled — project/source inputs still come from the answers file, so a
  stale workspace record cannot repoint them — and the answers' already-resolved
  path inputs are reused verbatim instead of being re-resolved.

## v0.5.10 — 2026-06-04

### Repository & module rename

- The repository is now `ang-ee/angee-operator` and the Go module path is
  `github.com/ang-ee/angee-operator` (was `github.com/fyltr/angee`). Install with
  `go install github.com/ang-ee/angee-operator/cmd/angee@latest` /
  `.../cmd/angee-operator@latest`. The `angee` / `angee-operator` binary names are
  unchanged. GitHub redirects the old repo URL, but update any pinned imports or
  clone URLs.

### Architecture (single control-plane contract)

- Extracted `service.API`, one exported interface that is now the single
  control-plane contract. Both the in-process `*service.Platform` and a remote
  HTTP client implement it, and the CLI, REST operator, and GraphQL operator all
  dispatch through it — replacing the private, hand-maintained `cli.platformClient`
  interface (deleted) that had drifted from the concrete type. The remote client
  moved out of `internal/cli` into a new `internal/platformclient` package
  (`platformclient.RemoteClient`), paired with the contract it implements.
  Compile-time `var _ service.API` assertions on both implementations keep them
  in lockstep.
- Closing remote gaps so the contract is uniform across transports:
  - Interactive `angee init` / `stack init` now works against `--operator`: the
    prompt set is derived from the `Template(ref)` descriptor (served over local,
    REST, and GraphQL alike) instead of a local-only call. Template input
    descriptors gain a `question` field distinguishing answerable Copier
    questions from generated/immutable metadata inputs.
  - `angee up` / `angee dev` against `--operator` now stream the combined
    foreground output via new `GET /stack/up/stream` and `GET /stack/dev/stream`
    routes, matching local behavior.
  - New REST routes `GET /services/{name}/endpoint` and `GET /ingress/status`
    give the previously GraphQL-only ingress lookups full REST parity.

## v0.5.9 — 2026-06-02

### Stacks

- `angee stack update --template` re-renders `angee.yaml` from the stack's Copier
  template before regenerating the derived runtime files, so template changes (a
  new service, job, port, or source) reach an already-initialized stack — parity
  with `workspace update`. The merge refreshes template-origin sections (template
  wins for keys it emits) while preserving user-added keys, allocated `ports`
  values, and operator-managed state (`operator`, `workspaces`, `port_leases`).
  `--dry-run` prints the additions/refreshes (`+`/`~` per section) without
  writing. Runs locally; requires the stack's `.copier-answers.yml`. Default
  `stack update` (derived-files-only) is unchanged. Full 3-way conflict detection
  for locally-edited template keys is a follow-up.

## v0.5.8 — 2026-06-02

### Ingress (Caddy edge)

- New optional `ingress` backend, selected by `type` and defaulting to `none`
  (today's host-published-ports behavior). With `ingress.type: caddy`, `Compile`
  injects a single `caddy-docker-proxy` edge into the compose file: routed
  services (those with a `route:` block) drop their host ports, join a private
  `<name>_edge` network, and get stamped with Caddy router labels; only the edge
  publishes a host port. A `route:` on a `runtime: local` service is rejected,
  and routed services take no `operator.port_pool` lease.
- `GET /edge/verify` — the operator's forward_auth target for the edge. Reads a
  route token from `?token=` / `Authorization` / `Sec-WebSocket-Protocol` and
  verifies `aud=svc:<service>`; returns 200/401 (never 101). Not behind the
  admin-bearer gate.
- New GraphQL queries `serviceEndpoint(name)` (`{routed, url, internalHost,
  internalPort}`) and `ingressStatus` (`{type, domain, routes}`), replacing
  host-side compose-port-scraping.
- Manifest validation hardens the edge against Caddyfile injection: under
  `ingress.type: caddy`, routed service names and `route.host` are charset-checked
  and the ingress fields are rejected if they contain Caddy metacharacters, and
  `edge` is a reserved service name. The forward_auth wiring (`caddy.forward_auth`
  + `/edge/verify?service=<name>`) and the `X-Forwarded-Uri` token read were
  validated end-to-end against a live `caddy-docker-proxy` run-spike.

### Internal

- Bumped the CI/release/CodeQL Go toolchain from 1.25.10 to **1.25.11**,
  clearing the `net/textproto` (GO-2026-5039) and `crypto/x509` (GO-2026-5037)
  standard-library advisories govulncheck flagged against the pinned version.

## v0.5.7 — 2026-06-02

### Internal

- Fixed the release and CI workflows to use the `ghcr.io/ang-ee/*` container
  image namespace (and `scripts/install.sh` to use the `ang-ee/angee-operator` repo)
  after the GitHub org rename from `fyltr` to `ang-ee`. The v0.5.6 release
  published its binaries but the docker image push was denied against the old
  `fyltr` namespace; v0.5.7 re-publishes with images under the correct one.
  No source changes — the operator/CLI binaries are identical to v0.5.6.

## v0.5.6 — 2026-06-02

### Operator auth & tokens

- The operator now mints **scoped, audience-bound JWTs** and verifies them
  centrally. `mintConnectionToken` gained a `scope: [String!]` argument and
  stamps `aud=operator`; a new `mintRouteToken(actor, service, ttl)` issues
  `aud=svc:<service>` route tokens. Both are exposed over GraphQL and REST
  (`POST /tokens/mint`, `POST /tokens/route`).
- The operator-API auth middleware is now **two-tier**: it accepts the admin
  bearer (full, server-to-server access) **or** a minted `aud=operator` token,
  binding the token's actor and capability scope to the request context.
  Existing admin-bearer callers are unchanged. A single shared `Verify`
  enforces HS256, the `angee-operator` issuer, a present expiry, and audience
  membership, and is reused by the WebSocket transport below.
- Together these let a host backend hold the admin bearer **server-side** and
  hand the browser a short-lived, capability-scoped token instead of the admin
  bearer. (Scopes are carried but not yet enforced per-mutation — that follows.)

### GraphQL

- Added a **`graphql-transport-ws` WebSocket transport** on `GET /graphql`,
  alongside the existing SSE transport, so browser GraphQL clients can run
  subscriptions over their built-in transport. Authentication happens in the
  `connection_init` handshake (token in `connectionParams`) via the same
  two-tier check as the HTTP API. A cross-site-WebSocket-hijacking origin
  allowlist guards the upgrade — loopback and no-`Origin` requests are allowed;
  extend it with the repeatable `--allowed-origin` flag. SSE, `POST` queries,
  and the admin-bearer header gate are unchanged.

### Internal

- Bumped `golang.org/x/crypto` to v0.52.0, clearing the reachable
  `golang.org/x/crypto/ssh` advisories (GO-2026-5013/5015 and related) that
  govulncheck flagged on the copier git-over-SSH path.
- Removed the VitePress docs build/deploy CI workflow — the docs site no
  longer lives in this repo. The `docs/` markdown remains as source.

## v0.5.5 — 2026-05-21

### Services

- `StackStatus` now reports real runtime state for **process-compose**
  services too (previously they always came back as `"declared"`).
  `proccompose.Backend.Status` queries the supervisor via
  `process-compose list -o json` and returns the literal status string
  lowercased (`running`, `completed`, `pending`, …). If the supervisor
  is offline or the control port is wrong, services fall back to
  `"declared"` exactly as before. Readiness probes (`is_ready: Ready`)
  also surface as a `healthy` health verdict on the response.
- `runtime.Backend.Status` now takes a `runtime.StatusRequest`
  (`Root` + `ControlPort`) instead of a bare root. This is an internal
  interface change; CLI, REST, GraphQL surfaces are unchanged.

## v0.5.4 — 2026-05-21

### Internal

- Simplified the IPv6 `EAFNOSUPPORT` check in `hostPortUnavailable` to
  satisfy staticcheck S1008. No behavioural change; the port allocator
  still treats hosts without IPv6 as having `[::]` ports available.

## v0.5.3 — 2026-05-21

### Services

- `StackStatus` (REST `GET /stack/status`, GraphQL `stackStatus`, CLI
  `angee stack status`) now reports the real runtime state of each
  service by querying the runtime backends, instead of always returning
  `"declared"`. `ServiceList` / `angee service list` inherit the fix.
  Services that haven't been brought up still report `"declared"`;
  brought-up services report docker's literal state (`running`,
  `exited`, `created`, …). Backend query errors are swallowed and fall
  back to `"declared"` so partial-stack scenarios stay useful.
- `api.ServiceState` and the GraphQL `ServiceState` type gained a
  `health` field that mirrors docker's healthcheck verdict
  (`healthy`, `unhealthy`, `starting`). Empty when the container has
  no healthcheck declared or the service has not been brought up.
  Process-compose services don't surface a health verdict yet — the
  field stays empty for them until the proc-compose `Status`
  implementation lands.

## v0.5.2 — 2026-05-21

### Services

- New `service up` verb across all surfaces (CLI `angee service up
  <name>...`, REST `POST /services/{name}/up`, GraphQL `serviceUp(name:
  String!)`). Maps to `docker compose up -d` / `process-compose up`, so
  it is idempotent across never-created services. `angee service create
  --start` now goes through `ServiceUp` instead of `ServiceStart`, so
  fresh services boot correctly on first try while `start`/`stop`/`restart`
  keep their literal docker compose semantics.
- Port allocations skip ports the host is already listening on. The
  allocator probes `0.0.0.0` and `[::]` for each candidate; ports in
  use are skipped so `docker compose up` does not later fail with
  `address already in use`. The IPv6 probe treats `EAFNOSUPPORT` as
  "available" so hosts without IPv6 are unaffected.

### Operator

- `angee-operator --version` now prints the build version, stamped at
  release time via the same ldflags pipeline that already drove
  `angee --version`.

## v0.5.1 — 2026-05-19

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

- Closed the CLI parity gap for the operator surface. Every operation
  that was REST + GraphQL-only now has a CLI subcommand: `angee
  gitops topology [--with-commits N]`, `angee source diff <name>`,
  `angee workspace preflight`, `angee workspace source
  {fetch,pull,push,diff,merge,rebase,merge-abort,rebase-abort,rebase-continue,publish}`,
  `angee template {list,get}`, `angee token mint`. The surface
  matrix in `docs/reference/surfaces.md` now reads `Yes` in the CLI
  column for every Platform method. Subscriptions remain GraphQL-only
  by design (REST/CLI have no native pubsub).
- Added template-based service creation. `angee service create
  --template <ref> --workspace <name>` (REST `POST /services/create`,
  GraphQL `serviceCreate(input)`) renders a Copier template with
  `_angee.kind: service` into the outer stack as one
  `manifest.Service` entry. Templates declare `name_pattern` and
  `ensure` port pools; the operator resolves the service name from
  the workspace name + caller inputs, allocates ports under owner
  `service/<name>/<pool>`, installs other rendered files (typically
  `docker/`) at `<root>/.angee/services/<service_name>/`, and strictly
  rejects anything outside `services:` in the rendered output. On
  render failure the port leases are released and the build context
  is removed. `service destroy` is updated to release the
  service-prefixed leases and delete the build-context dir.
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
