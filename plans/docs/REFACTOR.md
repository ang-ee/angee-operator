# angee-go Refactor Plan

This document consolidates two audits performed against `angee-go`:

1. **DRY audit** of CLI / REST / GraphQL surfaces over `service.Platform`.
2. **Build-vs-reuse audit** of in-house subsystems against current OSS.

It enumerates concrete refactors, prioritized by payoff, with enough detail to
execute each one as an isolated change. Dependency claims carry a
**Verified-on** date (today: 2026-05-10) so future readers know whether to
re-check upstream activity.

## Audit summary

### DRY across CLI / REST / GraphQL

The architecture stated in `CLAUDE.md` — CLI, REST, and GraphQL all dispatch
through `service.Platform` — is genuinely realized. Business logic is
centralized in `internal/service/`. Real but small violations exist:

- **Surface drift.** Several methods on `Platform` are exposed on only one
  surface. `gitOpsTopology` is GraphQL-only; workspace source `fetch`/`pull`/`push`
  are GraphQL-only; `WorkspaceLogsLimited` and `StackLogsLimited` are GraphQL-only.
  Some Platform methods are also legitimately CLI-local (foreground exec, internal
  compile) — see the parity matrix in R5.
- **`resolveRoot` duplicated.** `internal/operator/operator.go:660-684` and
  `internal/cli/root.go:748-775` reimplement the same walk-up algorithm with
  subtly different fallbacks (CLI also accepts `.templates/workspaces` and
  `templates/workspaces` directories).
- **Inline request structs that should live in `api/`.**
  `JobRunRequest`-shaped struct typed three times; `WorkspaceUpdateRequest`-shaped
  struct typed three times. Wire-contract drift risk.
- **Three near-identical `sorted<Type>s` helpers** in
  `internal/operator/graphql.go:1199-1250` duplicate what generic `sortedKeys`
  (`platform.go:549`) already does.
- **Service-error → HTTP status mapping is ad-hoc.** Everything flattens to 500
  or 400. CLI's `operatorHTTPError` (`internal/cli/operator_client.go:432`)
  unmarshals an `error` field from the body and **discards the HTTP status code**,
  so even if the operator returned 404/409 the CLI couldn't act on it.

### Build-vs-reuse

Most of the codebase is correct about what to own vs. delegate. Two concrete
swaps and one consolidation are worth scheduling:

- **GraphQL library.** `graphql-go/graphql` v0.8.1 — last release **April 2018**,
  ~200 open issues, no maintainer activity. Single highest-urgency change.
  `99designs/gqlgen` v0.17.90 (MIT) is the right replacement; schema-first
  codegen maps directly onto angee's Platform-forwarding pattern. **Verified-on
  2026-05-10:** gqlgen v0.17.90 latest release; graphql-go/graphql v0.8.1 still
  latest.
- **Compose model coverage.** angee already uses local typed structs for compose
  output (`internal/runtime/compose/doc.go:5`, used at
  `internal/service/platform.go:183`); it is **not** a text-template generator.
  The question is whether to keep the local minimal model or adopt
  `compose-spec/compose-go v2`. Lower priority unless missing spec fields start
  to bite. **Verified-on 2026-05-10:** compose-go v2.10.2 latest.
- **Manifest validation consolidation.** `Validate()` at
  `internal/manifest/manifest.go:292` mixes defaulting and validation. Splitting
  the two and then introducing `go-playground/validator` for constraint checks
  consolidates rules and unlocks JSON-Schema export via `invopop/jsonschema` for
  free.

Things to **definitely keep building**: `internal/substitute/` (domain-specific
namespaced expression language), `fyltr/copier-go` (no Go-native template
scaffolder with `copier update` semantics exists), `internal/ports/`
(range-based named lease allocator; `phayes/freeport` is unmaintained and
solves a different problem), `internal/runtime/compose/` and
`internal/runtime/proccompose/` (correct to shell out to `docker compose` and
`process-compose` for execution).

### Workspace runtime / lifecycle isolation

The current workspace incident is **not** branch-identity confusion:
`feature-storage` is on `workspace/feature-storage`, and `feature-knowledge` is
on `workspace/feature-knowledge`. The failure mode is runtime/lifecycle
isolation.

What went wrong:

- The root stack owns process-compose control port `8080`.
- Workspace manifests have allocated control ports (`feature-storage` `10002`,
  `feature-knowledge` `10003`), but `angee-go` does not pass those ports to
  process-compose.
- Agents manually started pieces of the stack, creating orphaned services
  outside the workspace lifecycle.
- `angee workspace status --json` is failing source status because go-git cannot
  read worktrees with `extensions.worktreeConfig`, masking the real branch
  state.
- Demo asset loading is not idempotent when `auth.User(admin)` already exists
  but the asset ledger row does not, so `web`/`ui` dependencies get stuck.

Immediate recovery:

- First preserve changes without altering worktrees: capture
  `git status --short --branch`, `git diff --stat`, `git diff --binary`, cached
  diff, and untracked file lists for both workspaces into
  `/private/tmp/angee-workspace-recovery/<timestamp>/`.
- Do not run `git reset`, `git clean`, branch switching, or stash unless
  explicitly requested later.
- Stop only workspace-local orphaned services. Target ports for
  `feature-storage`: `10002`, `9003`, `9225`, `5276`, `8103`, `5176`. Target
  ports for `feature-knowledge`: `10003`, `9004`, `9226`, `5277`, `8104`,
  `5177`.
- Also terminate processes whose cwd/path is under those workspace directories,
  including the stale `.orphaned-*` ruff process.
- Leave the root stack alone: `8080`, `8100`, `5173`, `9000`, `9223`.
- After code fixes land, restart using only `angee workspace start
  feature-storage` and `angee workspace start feature-knowledge`.
- Register isolated MCP entries:
  `playwright-feature-storage -> http://127.0.0.1:9225/mcp` and
  `playwright-feature-knowledge -> http://127.0.0.1:9226/mcp`. Do not use the
  global Playwright MCP as a fallback.

Implementation changes:

- Make process-compose control ports first-class: render a `process_compose`
  port for the dev stack, defaulting to `8080` for the root stack; pass
  `${alloc.custom}` from workspace templates so each workspace renders its own
  control port; update the process-compose backend so `up`, `down`, `start`,
  `stop`, `restart`, and `logs` all pass `--address 127.0.0.1 --port
  <process_compose_port>`; `angee workspace start|stop|restart|logs` must use
  the rendered inner stack port, never implicit `8080`.
- Fix workspace data paths: add a stack input for `data_root`; for workspaces,
  render it as absolute `<workspace>/.angee/data`; use that for `ANGEE_DATA` and
  Playwright `--user-data-dir`.
- Fix status authority: replace or fallback the go-git read-only calls used by
  workspace status/guards with native git CLI calls for worktrees;
  `angee workspace status --json` must report `branch-mismatch` only when
  `current_ref != branch`, not when go-git cannot parse worktree config. Do not
  add `workspace-check.sh`; `angee workspace status --json` remains the source
  of truth.
- Expose the same interface everywhere: add runtime/MCP facts to workspace JSON
  (`process-compose` control port, Playwright MCP name, Playwright MCP URL),
  expose the same fields through operator REST and GraphQL, and print the
  control port and MCP URL clearly in CLI text output.
- Make single-workspace CLI commands accept optional `[name]` when run inside a
  workspace: `status`, `sync-base`, `start`, `stop`, `restart`, and `logs`.
- Fix asset idempotency: when loading assets and no Assets ledger row exists,
  adopt an existing target row if exactly one matching unique field is present
  and exactly one row matches, for example `auth.User.username`; create the
  missing ledger row instead of attempting a duplicate insert, then continue.

Progress in this session:

- Process-compose runtime targets now carry a control port, and the
  process-compose backend passes `--address 127.0.0.1 --port <port>` for
  `up`, `down`, `start`, `stop`, `restart`, and `logs`. Root stacks default to
  `8080`; rendered stacks can declare `ports.process_compose.value`.
- Workspace lifecycle/log commands now inherit the rendered inner stack
  process-compose port because `workspace start|stop|restart|logs` dispatch
  through the inner stack platform.
- Workspace JSON, REST responses, GraphQL, and CLI text status now expose
  `process_compose_port`, `playwright_mcp_name`, and `playwright_mcp_url`
  where the workspace allocation/input data is available.
- `angee workspace start|stop|restart|logs` now accept optional `[name]` when
  run from inside `ANGEE_ROOT/workspaces/<name>`, matching `status` and
  `sync-base`.
- Git read-only status calls now fall back to native git CLI when go-git cannot
  read a worktree config extension, so branch-mismatch is based on actual
  `current_ref != branch` state.

Remaining:

- Render `ports.process_compose.value` from the dev stack template and pass
  `${alloc.custom}` from workspace templates.
- Add the `data_root` stack input and render workspace-local
  `<workspace>/.angee/data` for `ANGEE_DATA` and Playwright user data.
- Implement demo asset ledger adoption/idempotency in the application repo.

---

## Refactor backlog

Items are ranked by payoff. Each is independently shippable. LOC deltas
separate handwritten code from generated code.

| #  | Item                                                              | Status     | Priority    | Risk    | LOC (handwritten / generated) |
| -- | ----------------------------------------------------------------- | ---------- | ----------- | ------- | ----------------------------- |
| R1a | Hybrid go-git for read-only ops in `internal/git/`               | **DONE**   | High        | Low     | ±0 / 0                        |
| R1b | Full go-git migration (network/worktree/merge) — deferred         | Deferred   | Low         | High    | TBD                           |
| R2 | Replace `graphql-go/graphql` with `gqlgen`                         | **DONE**   | High        | Medium  | -1100 / +1500                 |
| R3 | Promote inline request structs to `api/`                           | **DONE**   | High        | Low     | -20 / 0                       |
| R4 | Hoist root discovery into `internal/stackroot`                     | **DONE**   | Medium-High | Low     | -25 / 0                       |
| R5 | Surface parity matrix (Platform method × CLI/REST/GraphQL)         | **DONE**   | Medium-High | Low     | +200 / 0                      |
| R6 | Typed domain errors + status preservation across CLI/REST/GraphQL  | **DONE**   | Medium      | Low     | +60 / 0                       |
| R7 | Evaluate `compose-spec/compose-go v2` against local Compose model  | Pending    | Low-Medium  | Medium  | varies                        |
| R8 | Split defaulting from validation, then adopt `validator` + schema  | Partial    | Medium      | Low     | +50 / +1 schema file          |
| R9 | Collapse `sorted*` helpers in `graphql.go` (mooted by R2)          | **DONE**   | Low         | Low     | -40 / 0                       |
| R10 | Workspace runtime/lifecycle isolation fixes                       | Partial    | High        | Medium  | TBD                           |

---

## R1a. Hybrid go-git for read-only ops — DONE

**Status:** shipped in this session. Read-only queries migrated to go-git;
write/network/worktree ops deliberately remain on the git CLI.

### What was done

- `go.mod` now has a direct dep on `github.com/go-git/go-git/v5 v5.19.0`.
  **Verified-on 2026-05-10:** v5.19.0 is the latest released tag.
- `internal/git/git.go` rewritten as a hybrid: `openRepo` helper plus go-git
  implementations for `RefExists`, `CurrentBranch`, `CurrentRef`, `Upstream`,
  `AheadBehind`/`AheadCount`, `Config`, `Remotes`, `Dirty`. `SyncBaseRef` and
  `PushRemote` reuse the new query methods.
- Existing tests in `internal/git/git_test.go` pass unchanged against the
  go-git-backed implementation.
- `make check` passes across the full project.

### What was deliberately NOT migrated

The following methods still shell out to `git`:

- `Clone`, `CloneRef`, `Fetch`, `Pull`, `Push`, `PushSetUpstream` — network ops
  need credential helpers, SSH config, and submodule handling.
- `Merge`, `Rebase` — go-git does not natively implement these.
- `WorktreeAdd`, `WorktreeAddBranch` — go-git lacks parallel checkout
  (go-git/go-git#1956); workspace creation is a hot path.
- `Run` — escape hatch (used by `templates.go` for `checkout`).

The split is documented in the package doc-comment of `internal/git/git.go`
and is a defensible long-term boundary, not a stopgap.

### Acceptance (this PR)

- [x] All callers of `internal/git/Client` compile unchanged.
- [x] `make check` passes.
- [ ] Smoke test creates a workspace, fetches, pulls, pushes a branch — to be
  run from a real workspace before tagging a release.

---

## R1b. Full go-git migration — deferred

**Status:** not pursued. Tracked for future evaluation.

Migrating the network/worktree/merge ops would require:

- Implementing credential discovery: SSH key auto-detection from `~/.ssh/`,
  HTTPS via `git credential` helper exec, GPG signing parity.
- A custom rebase/merge implementation (go-git plumbing only).
- Awaiting parallel-checkout support in go-git (#1956) before migrating
  worktree creation, since workspace creation is on a critical path.

The cost outweighs the benefit until either upstream lands the missing pieces
or angee needs to drop the `git` binary as a runtime requirement. **Verified-on
2026-05-10:** go-git/go-git#1956 still open; no parallel-checkout PR merged.

### Acceptance (when revisited)

- [ ] Workspace creation latency on a 100k-file repo is within 2× of CLI
  baseline.
- [ ] SSH and HTTPS auth work without a `git` binary on PATH.
- [ ] Merge/rebase semantics match `git --ff-only` and standard rebase for the
  test fixtures used in `internal/service/workspaces_test.go`.

---

## R2. Replace `graphql-go/graphql` with `99designs/gqlgen`

**Status:** shipped. The handwritten schema builder was replaced by
`internal/operator/schema.graphql`, gqlgen output under `internal/operator/gql/`,
and a thin handler wrapper in `internal/operator/graphql.go`.

**Goal:** delete `internal/operator/graphql.go` (1271 LOC of handwritten Go)
and replace with a schema-first gqlgen-generated resolver layer that forwards
to `service.Platform`.

**Why:** current lib's last release was April 2018 (security exposure), and
the hand-built schema is the largest single file in the repo. **Verified-on
2026-05-10:** gqlgen v0.17.90 (MIT) latest; graphql-go/graphql v0.8.1 still
latest.

### Guardrails — must preserve current behavior

The existing handler (around `internal/operator/graphql.go:758`) enforces:

- **POST-only.**
- **Content-type allowlist** with `errUnsupportedGraphQLMediaType` returning
  HTTP 415.
- **Body-size limit** via `http.MaxBytesReader(w, r.Body, maxGraphQLBodyBytes)`
  returning HTTP 413 on overflow.
- **JSON scalar handling** for variables.
- **Log-output limiting** for log-related queries (`WorkspaceLogsLimited`,
  `StackLogsLimited`).

These belong **inside** the gqlgen-served handler. Cross-origin protection is
**outside** the handler — it lives at `internal/operator/operator.go:90` as a
route wrapper:

```go
cop := http.NewCrossOriginProtection()
mux.Handle("POST /graphql", s.auth(cop.Handler(s.graphqlHandler)))
```

R2 must **preserve the existing `cop.Handler(...)` route wrapper** as-is and
port the in-handler guardrails (body size, content-type, log limiting, JSON
scalar) into the gqlgen-backed handler before flipping the route.

### Steps

1. Add deps: `go get github.com/99designs/gqlgen@latest` and
   `go get github.com/vektah/gqlparser/v2@latest`.
2. Add `gqlgen` tool dep via the `tools.go` pattern.
3. Define `internal/operator/schema.graphql` covering all currently-exposed
   queries, mutations, scalars, and types. Reference the existing `graphql.go`
   to enumerate.
4. Add `internal/operator/gqlgen.yml`:
   - `model.bindings` → bind generated models to types in `api/` and
     `internal/service/` where they exist.
   - `resolver.layout: follow-schema` → one file per top-level type.
5. Add a `//go:generate go run github.com/99designs/gqlgen generate` directive
   to `internal/operator/gen.go` (or wire `make generate`). Run it. Generated
   files land under `internal/operator/gql/` with the standard
   `// Code generated by gqlgen. DO NOT EDIT.` header.
6. Implement `Resolver` methods. Each resolver should be a 3-line forward to
   `Platform`. No business logic.
7. Build a transport wrapper that re-enforces the **in-handler** guardrails
   (`MaxBytesReader`, content-type, log-output limiting, JSON scalar). Leave
   the **route-level** `cop.Handler(...)` wrapper at
   `internal/operator/operator.go:90` untouched.
8. Wire the new handler into the existing `mux.Handle("POST /graphql",
   s.auth(cop.Handler(s.graphqlHandler)))` line (only the inner
   `s.graphqlHandler` changes).
9. **Generated-files freshness check.** The schema source of truth is
   `internal/operator/schema.graphql` itself; `gqlgen` does not produce a
   separate SDL to snapshot. Instead, add a CI step that runs
   `make generate` (or `go generate ./internal/operator/...`) and fails if
   `git diff --exit-code` shows any change in the generated tree — this
   catches stale resolvers / generated models when the schema is edited.
10. **Query parity tests:** for every existing query and mutation, run the
    same operation against both the old and new handler in tests; assert
    response-shape equality (and error-shape equality after R6).
11. Delete `internal/operator/graphql.go`.
12. Update `docs/OPERATOR-API.md` GraphQL section.

### Acceptance

- [x] Hand-built `graphql-go/graphql` schema code deleted from
  `internal/operator/graphql.go`.
- [x] Existing GraphQL query/mutation contract tests pass against the gqlgen
  handler.
- [x] Body-size limit (HTTP 413), content-type rejection (HTTP 415),
  POST-only enforcement, and log-output limiting are reproduced and tested.
- [x] Route-level `cop.Handler(...)` wrapper at `operator.go:90` retained
  unchanged.
- [x] `go.mod` no longer requires `graphql-go/graphql`.
- [x] Generated freshness target added as `make check-generated`; it runs
  gqlgen and fails if `internal/operator/gql/` changes.
- [x] Subscription support is **out of scope**; track separately.

---

## R3. Promote inline request structs to `api/`

**Status:** shipped. `api.JobRunRequest` and `api.WorkspaceUpdateRequest`
are used by both REST and the remote CLI client.

**Goal:** eliminate three duplicated request shapes.

### Steps

1. In `api/types.go`, add:
   - `JobRunRequest{ Inputs map[string]string }`.
   - `WorkspaceUpdateRequest{ Inputs map[string]string; TTL string }`.
2. Replace inline declarations:
   - `internal/operator/operator.go:295-303` → use `api.JobRunRequest`.
   - `internal/operator/operator.go:486-498` → use `api.WorkspaceUpdateRequest`.
   - `internal/cli/operator_client.go:192-196` → use `api.JobRunRequest`.
   - `internal/cli/operator_client.go:270-280` → use `api.WorkspaceUpdateRequest`.
3. Verify JSON tags match the wire format already used (no breaking change).

### Acceptance

- [x] `rg --pcre2 'struct\s*\{' internal/operator/operator.go internal/cli/operator_client.go`
  shows no remaining inline request structs that mirror existing API types.
- [x] Existing wire-format tests pass; new round-trip test covers
  marshal/unmarshal symmetry for both new types.

---

## R4. Hoist root discovery into `internal/stackroot`

**Status:** shipped. CLI, doctor, and operator root discovery now call
`stackroot.Resolve`.

**Goal:** one root-discovery implementation used by both operator and CLI.

**Naming:** prefer `internal/stackroot` over `internal/root`. `root` is a
heavily-used local variable name across `internal/cli/root.go` (line 733 and
elsewhere) and `internal/service/`; importing a package named `root` would
shadow constantly. The function also discovers `.angee` control roots and
template directories — not just manifests — so `manifest` is the wrong home.

### Steps

1. Create `internal/stackroot/stackroot.go` with
   `Resolve(start string) (string, error)`. (Package name is `stackroot`, so
   the call site reads `stackroot.Resolve(dir)`.)
2. Behavior — preserve current CLI semantics from `internal/cli/root.go:765`:
   walk up from `start`, accept any directory containing `angee.yaml` OR a
   directory that contains `templates/workspaces` or `.templates/workspaces`
   (the broader CLI fallback). Document the **template-only** behavior in the
   function's doc comment.
3. Replace `internal/operator/operator.go:660-684` with a call to
   `stackroot.Resolve`.
4. Replace `internal/cli/root.go:748-775` with a call to `stackroot.Resolve`.
5. Add unit tests for: stack-rooted detection, template-only directory
   detection, walk-up termination at filesystem root.

### Acceptance

- [x] Both call sites delegate to the shared function.
- [x] A directory containing only `templates/workspaces/` is accepted as a
  root by both surfaces.
- [x] Tests cover the three cases above plus failure when no root is found.

---

## R5. Surface parity matrix (Platform × CLI/REST/GraphQL)

**Status:** shipped in `docs/SURFACES.md`, linked from
`docs/OPERATOR-API.md`.

**Goal:** enumerate every method on `service.Platform` and decide — explicitly
— whether it is exposed on each surface. Some methods are legitimately
CLI-local (foreground execution, internal compile flows) and should not be on
REST or GraphQL.

### Deliverable

A table in `docs/OPERATOR-API.md` (or a new `docs/SURFACES.md`) shaped like:

| Platform method                | CLI | REST | GraphQL | Omit reason (if any) |
| ------------------------------ | --- | ---- | ------- | -------------------- |
| `StackStatus`                  | ✓   | ✓    | ✓       | —                    |
| `GitOpsTopology`               | ✗   | ✗    | ✓       | (gap — to add)       |
| `WorkspaceSourceFetch/Pull/Push` | ✗ | ✗    | ✓       | (gap — to add)       |
| `WorkspaceLogsLimited`         | partial (`--limit`) | ✗ | ✓ | (gap — to add) |
| `Compile` (internal)           | ✓   | ✗    | ✗       | Internal flow; no remote use case |
| _foreground exec helpers_      | ✓   | ✗    | ✗       | Local-only; cannot stream over operator |

### Steps

1. Inventory: list every method on `Platform` (use `rg '^func \(p \*Platform\)'
   internal/service/`).
2. Fill out the table; for each gap, decide **expose** or **omit (with
   reason)**.
3. For each "expose" gap, add the REST route, the CLI command, and the
   `platformClient` method.
4. **Annotate selectively, not exhaustively.** Add a code comment near a
   Platform method **only when the omission would surprise a future
   contributor** (e.g., a method whose name suggests it should be reachable
   remotely but isn't). For obviously local-only methods, the matrix is
   sufficient — don't paper every signature with `// REST: omit because ...`.
5. Update `docs/COMMANDS.md` and `docs/OPERATOR-API.md`.

### Acceptance

- [x] Matrix committed to docs.
- [x] Lightweight checker (`internal/service/surface_matrix_test.go`) asserts
  every exported `Platform` method is classified in the matrix. It does not yet
  parse the HTTP route registry.

---

## R6. Typed domain errors + status preservation across CLI/REST/GraphQL

**Status:** shipped for REST, remote CLI, and gqlgen GraphQL errors. Broader
coverage of incidental free-form errors can still improve incrementally.

**Goal:** stop string-matching error categories. Use typed domain errors that
each surface maps consistently.

### Domain error shape

```go
package service

type NotFoundError struct{ Kind, Name string }
func (e *NotFoundError) Error() string { ... }

type ConflictError struct{ Kind, Name, Reason string }
func (e *ConflictError) Error() string { ... }

type InvalidInputError struct{ Field, Reason string }
func (e *InvalidInputError) Error() string { ... }
```

These give surfaces structured fields (`Kind`, `Name`) instead of forcing them
to parse free-form messages.

### Wire DTO

The REST/CLI/test/docs surfaces all need to agree on the wire body. Add to
`api/types.go`:

```go
package api

// ErrorResponse is the JSON body returned for non-2xx responses.
type ErrorResponse struct {
    Kind   string `json:"kind,omitempty"`   // e.g. "service", "workspace"
    Name   string `json:"name,omitempty"`
    Field  string `json:"field,omitempty"`  // for validation errors
    Reason string `json:"reason,omitempty"`
    Error  string `json:"error"`            // human-readable
}
```

REST server, remote CLI client, doc examples, and integration tests must use
`api.ErrorResponse` — no inline anonymous structs.

### Steps

1. Define the error types in `internal/service/errors.go`.
2. **`StackRootExistsError` compatibility.** The CLI does
   `errors.As(err, &exists)` against the concrete `*service.StackRootExistsError`
   at `internal/cli/root.go:624` (creation site at `internal/service/stack.go:54`,
   type at `stack.go:18`). Two acceptable approaches — pick one and execute in
   the same PR as R6:
   - **(a) Direct replacement:** delete `StackRootExistsError`, return
     `&ConflictError{Kind: "stack-root", Name: path}`, update `cli/root.go:624`
     to `errors.As(err, &conflict)` and check `conflict.Kind == "stack-root"`.
     Cleaner end-state.
   - **(b) Alias for one release:** keep `StackRootExistsError` as a thin type
     embedding `*ConflictError` so the existing CLI `errors.As` keeps working;
     delete in a follow-up PR. Only worth the churn if there are external
     consumers of the type — there aren't, so prefer (a).
3. Sweep `internal/service/` for `fmt.Errorf("... not declared ...")` and
   similar; convert to `&NotFoundError{Kind, Name}`.
4. Add `internal/operator/errs.go` with `writeServiceError(w, err)`. Body type
   is `api.ErrorResponse`:
   - `*NotFoundError` → 404, `{kind, name, error}`.
   - `*ConflictError` → 409, `{kind, name, reason, error}`.
   - `*InvalidInputError` → 400, `{field, reason, error}`.
   - default → 500, `{error}`.
5. Add a parallel `formatGraphQLError` helper for the gqlgen handler (after
   R2) that puts the same structured fields into `extensions`.
6. **Fix `operatorHTTPError` (`internal/cli/operator_client.go:432`)** to:
   - Decode the body into `api.ErrorResponse`.
   - **Preserve the HTTP status** by returning a typed CLI error
     (`&RemoteNotFound{Kind, Name}` etc.) so callers can `errors.As` against
     it. Today the function flattens both 404 and 409 to a single
     `fmt.Errorf` string; that's the bug to fix.
   - Keep the human-readable message but include the status code.
7. Update CLI command handlers to surface specific errors via `errors.As` /
   `errors.Is` (replaces `internal/cli/root.go:623-629` ad-hoc string match
   and the concrete-type check at line 624 from step 2).

### Acceptance

- [x] REST integration tests assert typed 404, 400, and stack-root 409
  responses.
- [x] CLI tests assert: `errors.As(err, &cli.RemoteNotFound{})` works against
  a 404 from a remote operator.
- [x] GraphQL tests assert domain fields are included in `errors[0].extensions`.
- [x] No remaining `strings.Contains(err.Error(), "...")` in CLI command code
  (`rg "strings\.Contains\(err\.Error" internal/cli/`).

---

## R7. Evaluate `compose-spec/compose-go v2` against local Compose model

**Goal:** decide whether the local typed Compose model
(`internal/runtime/compose/doc.go`) should be replaced by upstream
`compose-spec/compose-go v2`. **Lower priority than originally written** —
angee already uses typed structs, not text templates.

### Decision factors

- **What's missing today?** Audit whether any Compose-spec field that
  templates need is absent from `internal/runtime/compose/doc.go`. If yes,
  list them.
- **Dep weight.** compose-go v2 brings ~15 direct + ~20 indirect deps
  (logrus, mapstructure, jsonschema). **Verified-on 2026-05-10:**
  compose-go v2.10.2 (Apache-2.0).
- **Validation benefit.** compose-go's validators run the Compose schema; the
  local model's marshal-as-yaml does not.

### Steps (only if decision is "swap")

1. Add dep: `go get github.com/compose-spec/compose-go/v2@latest`.
2. Replace `compose.File`/`compose.Service`/etc. constructions in
   `internal/service/platform.go:183` and surrounding code with `types.Project`
   / `types.ServiceConfig`.
3. Marshal via `project.MarshalYAML()`.
4. Add a golden-file test that diffs generated `docker-compose.yaml` against
   committed fixtures for representative manifests.
5. Re-run `internal/service/...` tests; expect output drift on whitespace and
   key ordering — normalize via the golden test.

### Acceptance (only if pursued)

- [ ] All compose-related tests pass against golden fixtures.
- [ ] Dep policy review (this is the largest dep increase across all
  refactors): confirmed acceptable by maintainer.
- [ ] Process-compose generation **not** changed (process-compose has no
  equivalent typed library).

### Default recommendation

**Defer R7** unless an audit of missing Compose fields finds a real gap. The
local minimal model is fit-for-purpose; replacing it is a dep weight increase
without proportional benefit today.

---

## R8. Split defaulting from validation, then adopt `validator` + JSON Schema

**Status:** partially shipped. Step 0 is complete: `Defaults()` and pure
`Validate()` are split. Validator and JSON Schema generation remain pending.

**Goal:** consolidate manifest validation rules and emit a JSON Schema for
editor LSP integration.

### Step 0 — split defaulting from validation (precondition)

`internal/manifest/manifest.go:292` defines `(s *Stack) Validate() error` that
**mixes defaulting and validation**: it sets `s.Version = VersionCurrent`,
`s.Kind = KindStack`, `s.SecretsBackend.Type = "env-file"`, etc. as side
effects.

Refactor to:

```go
func (s *Stack) Defaults()           // mutates: applies all defaults, no error
func (s *Stack) Validate() error     // pure: returns error, never mutates
```

Call sites adopt the order `Defaults()` then `Validate()`. After this, R8
proper can introduce struct-tag validation without entangling it with
defaulting logic.

### Steps (after Step 0)

1. Add deps:
   - `go get github.com/go-playground/validator/v10@latest`
     **Verified-on 2026-05-10:** v10 line, MIT, actively maintained.
   - `go get github.com/invopop/jsonschema@latest`
     **Verified-on 2026-05-10:** MIT, actively maintained.
2. Annotate manifest types in `internal/manifest/` with `validate:"..."` tags
   (e.g., `validate:"required,oneof=container local"` on a service `runtime`
   field).
3. Replace remaining `if`-block validation in `internal/manifest/manifest.go`
   (post Step 0) with `validator.Validate(m)`. Fold in any non-tag-expressible
   rules behind a single `(s *Stack) ValidateExtended() error` for things
   `validator` can't express (cross-field, set-uniqueness checks).
4. Add `make schema` target that emits `docs/angee.schema.json` from the
   manifest types via `invopop/jsonschema`.
5. Document the schema in `docs/MANIFEST.md`; reference it for editor
   LSP integration via `# yaml-language-server: $schema=...`.

### Caveat: validator tags do not auto-translate to JSON Schema

`invopop/jsonschema` reflects Go types and reads its own `jsonschema:"..."`
tags. It does **not** automatically translate `go-playground/validator` tags
(`required`, `oneof=...`, `min=...`) into JSON Schema constraints. The two
systems overlap on intent but not on syntax. Plan for one of:

- **Duplicate the constraint** with a `jsonschema:"..."` tag alongside the
  `validate:"..."` tag (acceptable for ~10s of fields; ugly past that).
- **Write a small bridge** that walks the struct via reflection and injects
  validator-derived constraints into the emitted schema (more work, but
  single source of truth).
- **Accept partial coverage:** the JSON Schema gives type/required-field
  hints to the editor; runtime validation still authoritatively comes from
  `validator`. The schema doesn't need to express every rule.

Pick one and document the choice in `docs/MANIFEST.md` so future contributors
know whether to update one place or two.

### Acceptance

- [x] `Stack.Defaults()` and `Stack.Validate()` exist as separate functions;
  `Validate()` does not mutate (verified by a test that compares pre/post
  byte-equal serializations).
- [x] Existing valid manifests still validate; existing invalid manifests
  still fail with equivalent or better messages.
- [ ] `docs/angee.schema.json` is generated and matches the structs.
- [ ] `make schema` target documented in `docs/DEVELOPMENT.md`.
- [ ] Decision recorded in `docs/MANIFEST.md` about how validator constraints
  surface in the JSON Schema (duplicate tags, bridge, or accept partial).
- [ ] Editors with `yaml-language-server` get autocompletion against the
  schema (manual smoke test documented in `docs/MANIFEST.md`).

---

## R9. Collapse `sorted*` helpers in `graphql.go`

**Status:** shipped. The duplicated typed helpers were replaced by one generic
`sortedMapValues` helper.

If R2 ships first, this is moot. Otherwise:

- Replace `sortedServiceStates`, `sortedJobStates`, `sortedWorkspaceRefs`
  (`internal/operator/graphql.go:1199-1250`) with a single generic helper
  using `sortedKeys` from `platform.go:549`.

### Acceptance

- [x] Three helpers replaced by one generic.
- [x] `rg 'func sorted[A-Z]' internal/operator/graphql.go` returns one match.

---

## Sequencing

Recommended order:

1. **R1a** (go-git read-only) — DONE.
2. **R3** (api types) — trivial, removes drift risk.
3. **R4** (root discovery) — trivial, removes duplication.
4. **R5** (parity matrix) — unblocks single-surface migrations and informs R6.
5. **R6** (typed errors + status preservation) — improves UX, prereq for R2's
   GraphQL error parity.
6. **R2** (gqlgen) — large but well-bounded; consumes R6's error helpers and
   R5's matrix.
7. **R8** (split defaulting + validator + JSON Schema) — independent.
8. **R7** (compose-go) — only if an audit finds missing Compose fields.
9. **R9** — only if R2 doesn't happen first.

Ship each as its own PR. None require coordinated lockstep changes.

## Out of scope

- **R1b**: full go-git migration (network/worktree/merge) — deferred until
  upstream parallel-checkout (#1956) lands and credential-helper parity is
  designed.
- Migrating to `openbao/api/v2` — defer until functionality exceeds the four
  KV ops the hand-rolled client supports.
- Replacing `phayes/freeport` (unmaintained) — angee's port pool solves a
  different problem (range-based named leases).
- Replacing `internal/substitute/` — namespaced filter pipeline is core
  domain; no library models it.
- Replacing `fyltr/copier-go` — no Go-native alternative exists.

## References

All upstream-version claims **verified on 2026-05-10**.

- DRY audit: this plan, "Audit summary" section above.
- https://github.com/compose-spec/compose-go (v2.10.2)
- https://github.com/99designs/gqlgen (v0.17.90, MIT)
- https://github.com/graphql-go/graphql (v0.8.1, last release Apr 2018)
- https://github.com/go-git/go-git (v5.19.0)
- https://github.com/go-git/go-git/issues/1956 (worktree perf — open)
- https://github.com/go-playground/validator (v10 line, MIT)
- https://github.com/invopop/jsonschema (MIT)
