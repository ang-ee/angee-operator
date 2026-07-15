# Template Render Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build one safe, stateful Copier reconciliation engine used by stacks, workspaces, chains, and template-created services, including conservative conflicts and explicit overwrite.

**Architecture:** `internal/copierx` owns ordered scratch rendering, file fingerprints, baseline state, deterministic preflight, apply, rollback, and state persistence. `internal/service` builds domain plans and merges special rendered documents (`angee.yaml` and `service.yaml`) before committing manifests. CLI, REST, GraphQL, and the remote client only pass options and display results.

**Tech Stack:** Go 1.25, `github.com/fyltr/copier-go`, `gopkg.in/yaml.v3`, Cobra, gqlgen, standard-library SHA-256/JSON/filesystem APIs.

## Global Constraints

- Bare `angee stack update` remains derived-files-only; template reconciliation requires `--template`.
- Existing files are preserved on ambiguous or locally modified conflicts unless `--overwrite` is explicit.
- Tracked, locally unchanged template deletions apply automatically; modified deletions require overwrite.
- Workspace update re-renders the workspace and declared chains automatically.
- Service template update preserves service name, workspace binding, and existing allocations.
- Field-based `service update` remains unchanged and is mutually exclusive with template mode.
- All render and result iteration is deterministically path-sorted.
- Runtime-managed stack fields and persistent paths are never overwritten by generic file reconciliation.
- Production code is written only after the corresponding failing test has been observed.

---

### Task 1: Domain-neutral reconciliation state and diff

**Files:**
- Create: `internal/copierx/reconcile.go`
- Create: `internal/copierx/reconcile_test.go`
- Modify: `internal/copierx/copierx.go`

**Interfaces:**
- Produces: `RenderPlan`, `RenderLayer`, `ReconcileOptions`, `PreparedReconcile`, `ReconcileResult`, `Change`, `Conflict`.
- Produces: `PrepareReconcile(context.Context, RenderPlan, ReconcileOptions) (*PreparedReconcile, error)`.
- Produces: `ReadRenderState(path string) (RenderState, bool, error)` for service-origin recovery.

- [ ] **Step 1: Write failing ordinary-file state tests**

Add table tests covering legacy add/adopt/conflict/overwrite, tracked update,
tracked deletion, modified deletion, and user-only preservation. The wished-for
API is:

```go
prepared, err := PrepareReconcile(ctx, RenderPlan{
	Target: target,
	StatePath: filepath.Join(root, "run/template-state/test.json"),
	Layers: []RenderLayer{{Name: "test", Template: template, Inputs: Inputs{}}},
}, ReconcileOptions{Mode: ReconcileUpdate, Overwrite: overwrite})
defer prepared.Close()
result := prepared.Result()
```

- [ ] **Step 2: Run the focused tests and confirm RED**

Run: `go test ./internal/copierx -run 'TestPrepareReconcile'`

Expected: compile failure because reconciliation types do not exist.

- [ ] **Step 3: Implement versioned state and fingerprint comparison**

Implement these concrete shapes:

```go
type RenderState struct {
	Version   int                    `json:"version"`
	Layers    []RenderLayerState     `json:"layers,omitempty"`
	Files     map[string]Fingerprint `json:"files,omitempty"`
	Documents map[string][]byte      `json:"documents,omitempty"`
}

type Fingerprint struct {
	Kind   string      `json:"kind"`
	SHA256 string      `json:"sha256,omitempty"`
	Mode   fs.FileMode `json:"mode,omitempty"`
	Link   string      `json:"link,omitempty"`
}
```

Implement the exact conflict matrix from
`docs/proposals/template-render-reconciliation.md`, stable path sorting, corrupt
state failure, and safe relative-path validation.

- [ ] **Step 4: Run focused tests and confirm GREEN**

Run: `go test ./internal/copierx -run 'TestPrepareReconcile'`

Expected: PASS.

- [ ] **Step 5: Add failing mode, symlink, dry-run, and deterministic-state tests**

Assert executable-bit changes, symlink-target conflicts, dry-run state
immutability, and byte-identical JSON across repeated writes.

- [ ] **Step 6: Implement minimal support and verify GREEN**

Run: `go test ./internal/copierx`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/copierx/copierx.go internal/copierx/reconcile.go internal/copierx/reconcile_test.go
git commit -m "feat(copier): add stateful render reconciliation"
```

### Task 2: Ordered rendering, special documents, apply, and rollback

**Files:**
- Modify: `internal/copierx/reconcile.go`
- Modify: `internal/copierx/reconcile_test.go`

**Interfaces:**
- `RenderPlan.Documents []string` identifies rendered documents consumed by service handlers.
- `PreparedReconcile.RenderedDocument(path)` and `PreviousDocument(path)` expose new and old canonical bytes.
- `PreparedReconcile.ApplyFiles()` returns an idempotent rollback closure.
- `PreparedReconcile.SaveState()` atomically records fingerprints and rendered document baselines.

- [ ] **Step 1: Write failing ordered-overlay and document tests**

Render two templates that both emit `shared.txt`; assert the second layer wins.
Mark `service.yaml` as a document and assert it is available through
`RenderedDocument` but absent from installed ordinary files.

- [ ] **Step 2: Run tests and confirm RED**

Run: `go test ./internal/copierx -run 'TestPrepareReconcileOrderedLayers|TestPrepareReconcileDocument'`

Expected: FAIL because layers and documents are not rendered/applied yet.

- [ ] **Step 3: Implement composite scratch rendering and metadata detection**

For each layer, render into `scratch/<DestRoot>` with `LocalRenderer.Copy`.
Export the template answer filename from `copierx.go`, and record the final
answer path as machine metadata rather than an ordinary managed file.

- [ ] **Step 4: Write failing rollback test**

Inject an apply failure after the first changed path and assert both destination
files and the prior state bytes are restored.

- [ ] **Step 5: Implement apply journal and atomic state write**

Use temporary sibling files plus rename for regular files. Snapshot replaced and
deleted entries into a rollback directory and restore them in reverse order.
Apply symlink replacement with `Lstat`/`Readlink`; never follow the destination
symlink while resolving a managed path.

- [ ] **Step 6: Run package tests and confirm GREEN**

Run: `go test ./internal/copierx`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/copierx/copierx.go internal/copierx/reconcile.go internal/copierx/reconcile_test.go
git commit -m "feat(copier): reconcile ordered render plans"
```

### Task 3: Stack plans, chain reuse, and non-manifest file updates

**Files:**
- Create: `internal/service/template_plan.go`
- Create: `internal/service/template_plan_test.go`
- Modify: `internal/service/stack.go`
- Modify: `internal/service/stack_update.go`
- Modify: `internal/service/stack_update_test.go`
- Modify: `internal/service/stack_chain_test.go`
- Modify: `internal/cli/root.go`

**Interfaces:**
- Produces: `buildStackRenderPlan(ctx, templatePath, target string, inputs copierx.Inputs, statePath string) (copierx.RenderPlan, []stackDocument, error)`.
- `StackUpdateTemplateOptions` gains `Overwrite bool`.
- `StackUpdateTemplateResult` reports file changes and conflicts in addition to manifest changes.

- [ ] **Step 1: Write the failing `AGENTS.md` regression**

Extend the real-template end-to-end fixture so the template gains
`{{ ANGEE_ROOT }}/AGENTS.md.jinja` without changing `angee.yaml`. Assert dry-run
reports `+ files/AGENTS.md`, apply creates it, and the no-op message is not used.

- [ ] **Step 2: Run the test and confirm RED**

Run: `go test ./internal/service -run TestStackUpdateFromTemplateAddsRenderedFile`

Expected: FAIL because only `angee.yaml` is consumed.

- [ ] **Step 3: Build stack and chain layers through one plan**

Move stack-chain resolution and input overlay into `template_plan.go`; append
chain layers first and the stack layer last. Compute manifest document paths
relative to the original Copier target for both `ANGEE_ROOT=.` and
`ANGEE_ROOT=.angee`.

- [ ] **Step 4: Replace direct stack Copy calls with reconciliation**

Use create mode in `StackInit`. In `StackUpdateFromTemplate`, use update mode,
run the existing structural merge on the rendered manifest document, add
ordinary file results, reject conflicts, then apply files, write the merged
manifest, save state, and call `StackPrepare`.

- [ ] **Step 5: Add overwrite, conflict, chain, ingress, and target-mapping tests**

Cover local `AGENTS.md` edits, overwrite replacement, chain host changes,
stack-last overlay, `.angee` parent targeting, runtime-state preservation, and
refresh of `merged.Ingress`.

- [ ] **Step 6: Wire CLI flags and verify GREEN**

Add `--overwrite` to `stack update`, requiring `--template`. Run:

`go test ./internal/service ./internal/cli -run 'StackUpdate|RenderStackChain'`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/service/template_plan.go internal/service/template_plan_test.go internal/service/stack.go internal/service/stack_update.go internal/service/stack_update_test.go internal/service/stack_chain_test.go internal/cli/root.go
git commit -m "feat(stack): reconcile complete template output"
```

### Task 4: Workspace create and update through render plans

**Files:**
- Modify: `api/types.go`
- Modify: `internal/service/api.go`
- Modify: `internal/service/template_plan.go`
- Modify: `internal/service/template_plan_test.go`
- Modify: `internal/service/workspaces.go`
- Modify: `internal/service/workspaces_test.go`
- Modify: `internal/platformclient/client.go`
- Modify: `internal/operator/operator.go`
- Modify: `internal/operator/schema.graphql`
- Modify/generated: `internal/operator/gql/`
- Modify: `internal/cli/root.go`

**Interfaces:**
- `WorkspaceUpdate(ctx, name string, req api.WorkspaceUpdateRequest) (api.WorkspaceRef, error)`.
- `api.WorkspaceUpdateRequest` gains `Overwrite bool`.
- Produces: `buildWorkspaceRenderPlan(...)` using the same `copierx.RenderPlan`.

- [ ] **Step 1: Write failing workspace update tests**

Create a workspace whose outer template and chained inner stack each emit a
file. Change both templates, call update, and assert both files refresh while
the allocated inner-stack port remains authoritative.

- [ ] **Step 2: Run and confirm RED**

Run: `go test ./internal/service -run 'TestWorkspaceUpdateRerenders'`

Expected: FAIL because `WorkspaceUpdate` only saves metadata.

- [ ] **Step 3: Build workspace and chain layers once**

Replace `renderWorkspaceChain` with plan construction that returns resolved
chain refs, chain root, persistent roots, and inner-manifest document specs.
Use create mode from `WorkspaceCreate` and update mode from `WorkspaceUpdate`.

- [ ] **Step 4: Preserve inner stack state and make update transactional**

Merge each current inner stack with its rendered document using
`authoritativePorts=true`; preflight ordinary conflicts before saving the
prospective parent workspace record. Roll back applied files and inner manifests
if parent `manifest.SaveFile` fails.

- [ ] **Step 5: Add conflict and overwrite tests**

Assert a locally edited workspace template file blocks every update write,
overwrite replaces it, and untracked source/worktree files remain untouched.

- [ ] **Step 6: Propagate overwrite across surfaces**

Update the API DTO, service interface, remote client, REST decoder, GraphQL
workspace update input, CLI flag, and surface tests. Regenerate gqlgen using the
repository's existing generation command.

- [ ] **Step 7: Verify GREEN**

Run: `go test ./api ./internal/service ./internal/cli ./internal/platformclient ./internal/operator/...`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add api internal/service internal/cli internal/platformclient internal/operator
git commit -m "feat(workspace): reconcile templates on update"
```

### Task 5: Structured three-way service merge and reconciled creation

**Files:**
- Create: `internal/service/service_template_update.go`
- Create: `internal/service/service_template_update_test.go`
- Modify: `internal/service/service_create.go`
- Modify: `internal/service/service_create_test.go`
- Modify: `internal/service/services.go`

**Interfaces:**
- Produces: `mergeRenderedService(base []byte, current manifest.Service, rendered []byte, name string, overwrite bool) (manifest.Service, []copierx.Change, []copierx.Conflict, error)`.
- Produces: `ServiceUpdateFromTemplate(ctx, name string, req api.ServiceUpdateTemplateRequest) (api.ServiceTemplateUpdateResult, error)`.

- [ ] **Step 1: Write failing recursive merge tests**

Use a base service with two env keys, a current service changing one key, and a
new render changing the other. Assert both survive. Add same-scalar conflict,
overwrite, template deletion, current-only field, and atomic-list cases.

- [ ] **Step 2: Run and confirm RED**

Run: `go test ./internal/service -run TestMergeRenderedService`

Expected: compile failure because the merge function does not exist.

- [ ] **Step 3: Implement recursive map merge**

Marshal services to generic YAML maps, recursively apply the base/current/new
rules, treat sequences and scalars atomically, unmarshal to `manifest.Service`,
then call existing build-context and service validators. Emit conflicts rooted
at `services.<name>`.

- [ ] **Step 4: Write failing reconciled create and update tests**

Assert create records `.copier-answers.yml`, asset fingerprints, and the
rendered `service.yaml` baseline. Change both Dockerfile and service env in the
template and assert update refreshes both. Add legacy-no-state, dry-run,
identity, workspace, allocation, routed lease, and secret tests.

- [ ] **Step 5: Refactor create and implement update**

Replace scratch `RemoveAll`/`moveRenderedAssets` with a one-layer render plan.
Recover service origin from render state, falling back to a single top-level
Copier answers file for legacy instances. Force reserved inputs from current
state and reject identity changes. Reuse existing leases and allocate only
missing owners before render.

- [ ] **Step 6: Remove destroyed service state**

Extend `ServiceDestroy` to remove
`run/template-state/services/<name>.json` with the build context and leases.

- [ ] **Step 7: Verify GREEN**

Run: `go test ./internal/service -run 'Service(Create|UpdateFromTemplate|Destroy)|MergeRenderedService'`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/service/service_create.go internal/service/service_create_test.go internal/service/service_template_update.go internal/service/service_template_update_test.go internal/service/services.go
git commit -m "feat(service): update template-created services"
```

### Task 6: Service template update API and CLI surfaces

**Files:**
- Modify: `api/types.go`
- Modify: `api/types_test.go`
- Modify: `internal/service/api.go`
- Modify: `internal/cli/root.go`
- Modify: `internal/cli/root_test.go`
- Modify: `internal/platformclient/client.go`
- Modify: `internal/operator/operator.go`
- Modify: `internal/operator/operator_test.go`
- Modify: `internal/operator/schema.graphql`
- Modify: `internal/operator/gql/schema.resolvers.go`
- Modify/generated: `internal/operator/gql/generated.go`, `internal/operator/gql/models_gen.go`
- Modify: `internal/service/surface_matrix_test.go`

**Interfaces:**
- `api.ServiceUpdateTemplateRequest{Inputs map[string]string, DryRun bool, Overwrite bool}`.
- `api.ServiceTemplateUpdateResult{Service ServiceState, Changes []TemplateChange, Conflicts []TemplateConflict}`.
- REST `POST /services/{name}/template/update`.
- GraphQL `serviceUpdateFromTemplate(name: String!, input: ServiceUpdateTemplateInput!): ServiceTemplateUpdateResult!`.

- [ ] **Step 1: Write failing CLI mutual-exclusion tests**

Assert `--template` dispatches template mode, `--template --image` fails,
`--dry-run` without template fails, and repeated `--input` values reach the
service request.

- [ ] **Step 2: Write failing REST and GraphQL parity tests**

Exercise dry-run and overwrite through both transports and compare change and
conflict payloads.

- [ ] **Step 3: Add DTOs and dispatch surfaces**

Keep field-based `ServiceUpdate` intact. Add a distinct service API method,
remote-client call, REST handler, GraphQL mutation/input/result, and CLI branch.

- [ ] **Step 4: Regenerate GraphQL and verify GREEN**

Run the existing gqlgen generation command, then:

`go test ./api ./internal/cli ./internal/platformclient ./internal/operator/... ./internal/service`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add api internal/cli internal/platformclient internal/operator internal/service/api.go internal/service/surface_matrix_test.go
git commit -m "feat(api): expose service template updates"
```

### Task 7: Remove duplicate render paths and update documentation

**Files:**
- Modify: `internal/copierx/copierx.go`
- Modify: `docs/guide/commands.md`
- Modify: `docs/guide/templates.md`
- Modify: `docs/reference/operator-api.md`
- Modify: `docs/reference/surfaces.md`
- Modify: `docs/proposals/ROADMAP.md`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Removes unused `UpdateRequest`, `Renderer.Update`, direct chain render loops, and `moveRenderedAssets`.

- [ ] **Step 1: Search for remaining direct rendering duplication**

Run: `rg -n 'LocalRenderer\{\}\.(Copy|Update)|render(Stack|Workspace)Chain|moveRenderedAssets|UpdateRequest' internal`

Expected: only reconciler internals and intentionally retained one-shot service
or test uses; remove every obsolete production call.

- [ ] **Step 2: Update public documentation**

Document stack overwrite/dry-run file output, automatic workspace reconciliation,
service template mode, conflict policy, state locations, and REST/GraphQL
surfaces. Remove the old workspace parity claim once the behavior itself is
documented directly.

- [ ] **Step 3: Update roadmap and changelog**

Mark complete file reconciliation and service updates; retain manifest-key
three-way conflict detection as the explicit remaining item.

- [ ] **Step 4: Run formatting and focused tests**

Run: `make fmt`

Run: `go test ./internal/copierx ./internal/service ./internal/cli ./internal/operator/...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add CHANGELOG.md docs internal
git commit -m "docs: document template reconciliation"
```

### Task 8: Project review and full verification

**Files:**
- Review all changes since commit `593f5c4`.

**Interfaces:**
- No new interfaces; this task validates the complete contract.

- [ ] **Step 1: Run repository formatting and vet**

Run: `make fmt`

Run: `make vet`

Expected: both exit 0 with no diagnostics.

- [ ] **Step 2: Run the full race suite**

Run: `make test`

Expected: `go test -v -race ./...` exits 0.

- [ ] **Step 3: Run the project Go reviewer**

Invoke `.agents/agents/go-code-reviewer.md` proactively against the complete Go
diff. Address every valid P0-P2 finding with a failing regression test before
the fix.

- [ ] **Step 4: Re-run full verification after review changes**

Run: `make check`

Expected: fmt, vet, and race tests all pass.

- [ ] **Step 5: Inspect final scope**

Run: `git status --short`, `git diff --check`, and
`git diff --stat 593f5c4..HEAD`.

Expected: clean worktree, no whitespace errors, and only reconciliation-related
code/docs/tests.

- [ ] **Step 6: Commit reviewer fixes if needed**

```bash
git add api internal/copierx internal/service internal/cli internal/platformclient internal/operator docs CHANGELOG.md
git commit -m "fix: address template reconciliation review"
```
