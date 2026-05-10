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

---

## Refactor backlog

Items are ranked by payoff. Each is independently shippable. LOC deltas
separate handwritten code from generated code.

| #  | Item                                                              | Status     | Priority    | Risk    | LOC (handwritten / generated) |
| -- | ----------------------------------------------------------------- | ---------- | ----------- | ------- | ----------------------------- |
| R1a | Hybrid go-git for read-only ops in `internal/git/`               | **DONE**   | High        | Low     | ±0 / 0                        |
| R1b | Full go-git migration (network/worktree/merge) — deferred         | Deferred   | Low         | High    | TBD                           |
| R2 | Replace `graphql-go/graphql` with `gqlgen`                         | Pending    | High        | Medium  | -1100 / +1500                 |
| R3 | Promote inline request structs to `api/`                           | Pending    | High        | Low     | -20 / 0                       |
| R4 | Hoist root discovery into `internal/root` (or similar)             | Pending    | Medium-High | Low     | -25 / 0                       |
| R5 | Surface parity matrix (Platform method × CLI/REST/GraphQL)         | Pending    | Medium-High | Low     | +200 / 0                      |
| R6 | Typed domain errors + status preservation across CLI/REST/GraphQL  | Pending    | Medium      | Low     | +60 / 0                       |
| R7 | Evaluate `compose-spec/compose-go v2` against local Compose model  | Pending    | Low-Medium  | Medium  | varies                        |
| R8 | Split defaulting from validation, then adopt `validator` + schema  | Pending    | Medium      | Low     | +50 / +1 schema file          |
| R9 | Collapse `sorted*` helpers in `graphql.go` (mooted by R2)          | Pending    | Low         | Low     | -40 / 0                       |

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

**Goal:** delete `internal/operator/graphql.go` (1271 LOC of handwritten Go)
and replace with a schema-first gqlgen-generated resolver layer that forwards
to `service.Platform`.

**Why:** current lib's last release was April 2018 (security exposure), and
the hand-built schema is the largest single file in the repo. **Verified-on
2026-05-10:** gqlgen v0.17.90 (MIT) latest; graphql-go/graphql v0.8.1 still
latest.

### Guardrails — must preserve current handler behavior

The existing handler (around `internal/operator/graphql.go:758`) enforces:

- **POST-only.**
- **Content-type allowlist** with `errUnsupportedGraphQLMediaType` returning
  HTTP 415.
- **Body-size limit** via `http.MaxBytesReader(w, r.Body, maxGraphQLBodyBytes)`
  returning HTTP 413 on overflow.
- **Cross-origin protections** (verify the existing Origin/Referer or CSRF
  checks before deleting the handler).
- **JSON scalar handling** for variables.
- **Log-output limiting** for log-related queries (`WorkspaceLogsLimited`,
  `StackLogsLimited`).

R2 must port each of these into the gqlgen handler before flipping the route.

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
5. Run `go run github.com/99designs/gqlgen generate`. Generated files go under
   `internal/operator/gql/` and should be marked with a `// Code generated`
   header for review tooling.
6. Implement `Resolver` methods. Each resolver should be a 3-line forward to
   `Platform`. No business logic.
7. Build a transport wrapper that re-enforces the guardrails above
   (`MaxBytesReader`, content-type, CORS, etc.).
8. Wire the wrapper into `operator.go`'s mux at `POST /graphql`.
9. **Capture a schema snapshot** (`gqlgen generate` produces an SDL; commit it
   and add a CI check that diffs the generated SDL against the snapshot).
10. **Query parity tests:** for every existing query and mutation, run the
    same operation against both the old and new handler in tests; assert
    response-shape equality (and error-shape equality after R6).
11. Delete `internal/operator/graphql.go`.
12. Update `docs/OPERATOR-API.md` GraphQL section.

### Acceptance

- [ ] `internal/operator/graphql.go` deleted.
- [ ] All existing GraphQL queries return identical shapes (verified by parity
  tests).
- [ ] Body-size limit (HTTP 413), content-type rejection (HTTP 415),
  POST-only enforcement, and log-output limiting are reproduced and tested.
- [ ] `go.mod` no longer requires `graphql-go/graphql`.
- [ ] `internal/operator/schema.snapshot.graphql` committed and CI-diffed.
- [ ] Subscription support is **out of scope**; track separately.

---

## R3. Promote inline request structs to `api/`

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

- [ ] `rg --pcre2 'struct\s*\{' internal/operator/operator.go internal/cli/operator_client.go`
  shows no remaining inline request structs that mirror existing API types.
- [ ] Existing wire-format tests pass; new round-trip test covers
  marshal/unmarshal symmetry for both new types.

---

## R4. Hoist root discovery into a shared package

**Goal:** one root-discovery implementation used by both operator and CLI.

**Naming:** the function discovers `.angee` control roots and template
directories — not just manifests. Place it in `internal/root` (or
`internal/stackroot`) rather than `internal/manifest`, where it would imply
that a root is defined solely by `angee.yaml`.

### Steps

1. Create `internal/root/root.go` with `ResolveRoot(start string) (string, error)`.
2. Behavior — preserve current CLI semantics from `internal/cli/root.go:765`:
   walk up from `start`, accept any directory containing `angee.yaml` OR a
   directory that contains `templates/workspaces` or `.templates/workspaces`
   (the broader CLI fallback). Document the **template-only** behavior in the
   function's doc comment.
3. Replace `internal/operator/operator.go:660-684` with a call to
   `root.ResolveRoot`.
4. Replace `internal/cli/root.go:748-775` with a call to `root.ResolveRoot`.
5. Add unit tests for: stack-rooted detection, template-only directory
   detection, walk-up termination at filesystem root.

### Acceptance

- [ ] Both call sites delegate to the shared function.
- [ ] A directory containing only `templates/workspaces/` is accepted as a
  root by both surfaces.
- [ ] Tests cover the three cases above plus failure when no root is found.

---

## R5. Surface parity matrix (Platform × CLI/REST/GraphQL)

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
4. For each "omit" entry, add a one-line code comment near the method body
   linking back to the matrix.
5. Update `docs/COMMANDS.md` and `docs/OPERATOR-API.md`.

### Acceptance

- [ ] Matrix committed to docs.
- [ ] No method appears on a surface without an explicit row in the matrix.
- [ ] Doc tests (or a small static analyzer / one-shot script) verify the
  matrix matches the actual route registry.

---

## R6. Typed domain errors + status preservation across CLI/REST/GraphQL

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

### Steps

1. Define the error types in `internal/service/errors.go`. Replace existing
   `StackRootExistsError` with `*ConflictError{Kind: "stack-root", Name: path}`.
2. Sweep `internal/service/` for `fmt.Errorf("... not declared ...")` and
   similar; convert to `&NotFoundError{Kind, Name}`.
3. Add `internal/operator/errs.go` with `writeServiceError(w, err)`:
   - `*NotFoundError` → 404 + body `{kind, name, error}`.
   - `*ConflictError` → 409 + body `{kind, name, reason, error}`.
   - `*InvalidInputError` → 400 + body `{field, reason, error}`.
   - default → 500.
4. Add a parallel `formatGraphQLError` helper for the gqlgen handler (after
   R2) that puts the same structured fields into `extensions`.
5. **Fix `operatorHTTPError` (`internal/cli/operator_client.go:432`)** to:
   - Decode the structured body (`kind`, `name`, etc.).
   - **Preserve the HTTP status** by returning a typed CLI error
     (`&remoteNotFound{Kind, Name}` etc.) instead of flattening to a single
     `fmt.Errorf` string.
   - Keep the human-readable message but include the status code.
6. Update CLI command handlers to surface specific errors via `errors.As` /
   `errors.Is` (replaces `internal/cli/root.go:623-629` ad-hoc string match).

### Acceptance

- [ ] REST integration tests assert: 404 for missing service/workspace, 409
  for stack-root-exists, 400 for invalid manifest input, 500 only for true
  internals.
- [ ] CLI tests assert: `errors.As(err, &cli.RemoteNotFound{})` works against
  a 404 from a remote operator.
- [ ] GraphQL tests assert: `errors[0].extensions.kind == "service"` etc.
- [ ] No remaining `strings.Contains(err.Error(), "...")` in CLI command code
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

### Acceptance

- [ ] `Stack.Defaults()` and `Stack.Validate()` exist as separate functions;
  `Validate()` does not mutate (verified by a test that compares pre/post
  byte-equal serializations).
- [ ] Existing valid manifests still validate; existing invalid manifests
  still fail with equivalent or better messages.
- [ ] `docs/angee.schema.json` is generated and matches the structs.
- [ ] `make schema` target documented in `docs/DEVELOPMENT.md`.
- [ ] Editors with `yaml-language-server` get autocompletion against the
  schema (manual smoke test documented in `docs/MANIFEST.md`).

---

## R9. Collapse `sorted*` helpers in `graphql.go`

If R2 ships first, this is moot. Otherwise:

- Replace `sortedServiceStates`, `sortedJobStates`, `sortedWorkspaceRefs`
  (`internal/operator/graphql.go:1199-1250`) with a single generic helper
  using `sortedKeys` from `platform.go:549`.

### Acceptance

- [ ] Three helpers replaced by one generic.
- [ ] `rg 'func sorted[A-Z]' internal/operator/graphql.go` returns one match.

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
