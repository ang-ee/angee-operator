# Proposal: reusable template render reconciliation

**Status:** Approved for implementation (2026-07-15)

## Summary

Replace the four independent Copier execution paths for stack initialization,
stack template updates, workspace creation, and chain rendering with one ordered
render-plan reconciler. The reconciler renders a composite template tree into a
scratch directory, compares ordinary files against persisted render state and
the live destination, reports conflicts before writing, and applies the result
with consistent create, update, deletion, dry-run, and overwrite semantics.

Stack manifests remain structurally merged through a service-owned file handler
because `angee.yaml` contains both template-origin declarations and
operator-managed runtime state. Copier answers remain machine-owned metadata.

## Motivation

`angee stack update --template` currently renders a complete Copier tree into a
temporary directory but consumes only `angee.yaml`. Any other new template
output, such as `AGENTS.md`, is deleted with the scratch directory. Stack and
workspace chains also each implement their own template resolution, input
overlay, destination selection, and direct `LocalRenderer.Copy` loop, while
`workspace update` updates only manifest metadata despite the documentation
claiming that it re-renders the workspace.

The immediate symptom is a missing `AGENTS.md`, but copying that one file would
leave the ownership and conflict problem unsolved. Existing rendered files may
contain user edits, templates can delete files, chains overlay multiple
templates into one destination, and `angee.yaml` cannot be overwritten like an
ordinary file.

## Goals

- Give stacks, workspaces, and their chains one render and reconciliation
  mechanism.
- Preserve the existing layer order: chained hosts first and stack overlays
  last; workspace template first and declared workspace chains afterward.
- Create newly introduced template files in existing destinations.
- Preserve locally modified or ambiguous files by default and report structured
  conflicts.
- Add an explicit `--overwrite` option that lets template content and template
  deletions win conflicts.
- Track enough file provenance for safe automatic modifications and deletions on
  subsequent updates.
- Make `workspace update` actually re-render the workspace template and its
  declared chains.
- Keep bare `stack update` derived-files-only; template reconciliation remains
  behind `stack update --template`.
- Make dry-run report both manifest and ordinary-file changes without writing.

## Non-goals

- Service-template updates. `service create --template` remains a one-shot
  render, although it can adopt the reconciler later.
- Full three-way merge of individual `angee.yaml` keys. The existing structural
  ownership contract remains: emitted template keys refresh, user-only keys and
  runtime sections survive, and allocated ports are preserved. Manifest-key
  conflict detection remains a separate roadmap item.
- Arbitrary recursive template chains. This change preserves the currently
  supported declared stack and workspace chain depth and adds cycle-safe plan
  validation where plans compose existing layers.
- Running Copier migration or post-render tasks. Angee continues to render with
  tasks disabled.

## Ownership and package boundaries

### `internal/copierx`

Copier owns parsing and rendering Copier templates. Angee's `copierx` package
owns the filesystem reconciliation seam around Copier:

- ordered render plans;
- scratch rendering;
- ordinary-file fingerprints and persisted state;
- deterministic diffing;
- legacy adoption rules;
- conflict detection;
- overwrite behavior;
- apply and rollback;
- state serialization.

The reconciler is domain-neutral. It does not know about stacks, workspaces,
ports, manifests, or operator state.

### `internal/service`

The service layer owns Angee template composition:

- resolving stack, workspace, and chain template references;
- building the ordered layer plan;
- constructing stack- and workspace-specific substitution contexts;
- locating the original Copier render target;
- registering machine-managed answer files;
- registering `angee.yaml` handlers;
- persisting stack and workspace records only after reconciliation succeeds;
- regenerating runtime files after a stack template update.

CLI, REST, and GraphQL remain thin adapters over `service.Platform`.

## Core model

The reconciler receives a complete plan rather than one template at a time:

```go
type RenderPlan struct {
	Target    string
	StatePath string
	Layers    []RenderLayer
	Handlers  map[string]FileHandler
	Metadata  map[string]MetadataFile
}

type RenderLayer struct {
	Name     string
	Template string
	DestRoot string
	Inputs   Inputs
}

type ReconcileOptions struct {
	Mode      ReconcileMode // create or update
	DryRun    bool
	Overwrite bool
}
```

`Target` is the original Copier destination, not necessarily `ANGEE_ROOT`. For
a project whose stack lives at `<project>/.angee`, the target is `<project>` and
the stack manifest path inside the plan is `.angee/angee.yaml`. A workspace plan
targets `workspaces/<name>`.

Each layer renders beneath `Target/DestRoot` in the composite scratch tree. The
reconciler applies layers in declaration order, so later layers overlay earlier
ones exactly as direct Copier copies do today.

`Handlers` contains paths with domain-specific merge behavior. `Metadata`
contains machine-owned paths, principally Copier answer files, which are updated
without user-file conflict rules. Both are excluded from ordinary render-state
ownership.

## Render state

Render state is runtime-owned and stored outside the rendered file inventory:

```text
<ANGEE_ROOT>/run/template-state/stack.json
<ANGEE_ROOT>/run/template-state/workspaces/<name>.json
```

The versioned JSON document stores slash-normalized paths relative to the plan
target. Every ordinary rendered entry records:

- entry kind: regular file or symlink;
- SHA-256 content hash for regular files;
- permission bits relevant to executable-mode changes;
- symlink target for symlinks.

Directories are created as needed but are not independently owned or deleted.
Empty directory cleanup is limited to directories emptied by reconciler-owned
deletions. The state file and its parents cannot become managed template
outputs. Configured persistent root directories are reserved from directory
ownership and deletion, while individual template files beneath them may still
be tracked.

State is written through a temporary file and rename only after the destination
apply succeeds. A corrupt state document fails closed and identifies its path;
it is never silently treated as missing state.

## Ordinary-file reconciliation

The comparison uses three fingerprints:

- `old`: the last rendered fingerprint from state;
- `current`: the live destination fingerprint;
- `new`: the newly rendered scratch fingerprint.

The deterministic rules are:

| Old | Current | New | Default action | With overwrite |
| --- | --- | --- | --- | --- |
| absent | absent | present | add and track | same |
| absent | equals new | present | adopt and track | same |
| absent | differs from new | present | conflict, preserve | replace and track |
| present | equals old | present | update and track | same |
| present | equals new | present | adopt new baseline | same |
| present | differs from old and new | present | conflict, preserve | replace and track |
| present | equals old | absent | delete and untrack | same |
| present | differs from old | absent | conflict, preserve | delete and untrack |
| absent | present | absent | ignore user file | same |

A file-kind change, mode change, or different symlink target participates in the
same fingerprint rules. Untracked entries absent from the new render are never
deleted because template ownership cannot be inferred.

Create mode deliberately preserves existing initialization behavior: template
layers win over source materialization and over existing files when stack init
was invoked with `--force`. It records the resulting ordinary-file baseline.

## Special files

### Stack manifests

Every rendered stack manifest path is registered with a service-owned handler.
The handler loads the current manifest when one exists and structurally merges
the rendered manifest:

- template-origin fields refresh from the rendered manifest;
- runtime fields `operator`, `workspaces`, and `port_leases` survive verbatim;
- top-level allocated port values survive;
- workspace inner-stack ports come from the parent workspace allocations;
- user-only map keys survive;
- `_angee.ensure` invariants run against the merged result.

The handler is reused for a top-level stack and any inner stack rendered by a
workspace chain. It also refreshes every currently template-owned scalar field,
including `ingress`, so the allowlist cannot silently omit that existing schema
field.

Manifest changes are returned in the same operation result as ordinary-file
changes but are not tracked in the ordinary file-state document.

### Copier answers

Copier answer files are machine-owned render metadata. The plan builder records
the answer path for each effective layer. The reconciler writes their newly
rendered values without ordinary-file conflicts so changed workspace inputs and
template source metadata remain current.

When flat overlays produce the same answer path, the later layer retains the
same precedence it has today. Rooted chains with distinct answer files remain
independently addressable.

### Persistent paths

Paths declared through `_angee.persist` are never removed as a consequence of a
template deletion. A template may still place managed files beneath a persistent
directory, but directory cleanup cannot remove the persistent root or unrelated
contents.

## Operation flows

### Stack initialization

1. Resolve the stack template and inputs.
2. Expand stack-chain layers in their current order.
3. Append the stack template as the final layer.
4. Render and apply in create mode, including initial render state.
5. Load the initialized stack and materialize its referenced sources.

### Stack template update

1. Load the stack and locate its answer file and original render target.
2. Reconstruct the same stack-chain plus stack plan used for initialization.
3. Overlay authoritative workspace ports when updating a managed inner stack.
4. Render, merge manifests, and preflight ordinary files.
5. Return all changes without writing for `--dry-run`.
6. Fail before writing when conflicts exist and `--overwrite` is absent.
7. Apply the prepared result and render state.
8. Run `StackPrepare` to regenerate runtime files.

Bare `stack update` continues to call only `StackPrepare`.

### Workspace creation

1. Resolve metadata, inputs, allocations, and materialized sources as today.
2. Add the workspace template layer.
3. Add declared chain layers in order with resolved destinations and inputs.
4. Register every inner stack manifest handler.
5. Apply in create mode and record workspace render state.
6. Save the workspace record in the parent stack manifest.

Existing failed-create rollback continues to remove only worktrees and files
created by that attempt.

### Workspace update

1. Build a prospective workspace record from current state plus requested
   inputs and TTL.
2. Resolve the workspace template and chain using the prospective inputs and
   existing authoritative allocations.
3. Render and preflight the composite workspace tree.
4. Fail without changing files or the parent manifest on conflicts.
5. Apply the tree and state, then save the prospective workspace record.
6. Roll back the applied tree if persistence of the parent record fails.

This makes the existing documentation promise that workspace update re-renders
its templates true.

## Commands and API surfaces

Stack template reconciliation remains local-only:

```text
angee stack update --template [--dry-run] [--overwrite]
```

`StackUpdateTemplateOptions` gains `Overwrite bool`. Its result reports ordered
manifest and file changes. Dry-run reports conflicts without treating them as an
apply failure because it performs no write.

Workspace update gains overwrite across every existing surface:

```text
angee workspace update <name> [--input key=value ...] [--ttl duration] [--overwrite]
```

`api.WorkspaceUpdateRequest` gains `overwrite`. The service API consumes that
request shape, and REST, GraphQL, the remote client, and CLI pass the field
through without implementing reconciliation policy themselves.

## Result and error model

The reconciler returns stable, path-sorted results:

```go
type Change struct {
	Path string
	Kind ChangeKind // add, modify, delete, adopt
}

type Conflict struct {
	Path   string
	Reason ConflictReason // locally-modified, untracked-different, type-changed
}

type ReconcileResult struct {
	Changes   []Change
	Conflicts []Conflict
}
```

Non-dry-run reconciliation with conflicts and no overwrite returns one
structured template-conflict error containing all conflicts. CLI output lists
every path. REST and GraphQL expose the operation as a conflict using the
existing service error translation, with the structured list retained in the
error payload where the surface supports extensions.

Render errors include the layer name and template reference. Destination safety
errors include the invalid relative root. Context cancellation is checked
between layers, before diffing, and before apply.

Apply first completes render, handler execution, state loading, destination
inspection, and conflict preflight without mutation. It then uses temporary
siblings plus rename for regular-file writes and keeps a rollback journal of
replaced and deleted entries. Any apply or caller-persistence error restores
entries changed earlier in the operation.

## Test strategy

### Reconciler unit tests

- Legacy missing file is created and tracked.
- Legacy identical file is adopted.
- Legacy differing file conflicts and is preserved.
- Overwrite replaces a legacy differing file.
- A tracked unchanged file updates automatically.
- A tracked locally modified file conflicts.
- A tracked template deletion removes an unchanged file.
- A modified deletion conflicts; overwrite deletes it.
- User-only files remain untouched.
- Dry-run reports changes and conflicts without writing files or state.
- Ordered layers preserve overlay order.
- Executable modes and symlinks update and conflict through fingerprints.
- Path escapes and corrupt state fail closed.
- An injected apply failure rolls back earlier changes.
- Results and serialized state are deterministic.

### Service integration tests

- A stack template that gains `AGENTS.md` adds it on
  `stack update --template`.
- A local edit to that file conflicts; `--overwrite` replaces it.
- `ANGEE_ROOT=.` and `ANGEE_ROOT=.angee` map scratch paths to the correct
  original target.
- Stack chain host changes and stack overlays use one plan and retain order.
- Top-level stack runtime state survives structural manifest merge.
- `ingress` refreshes from the rendered manifest.
- Workspace update changes outer template files and inner-stack files.
- Workspace inner-stack allocation values remain authoritative.
- Workspace conflicts leave the parent manifest and destination unchanged.
- CLI, REST, GraphQL, and the remote client propagate workspace overwrite.

### Verification

- Focused red/green tests for every behavior slice.
- `gofmt` on changed Go files.
- `go vet ./...`.
- `go test -race ./...`.
- Proactive project `go-code-reviewer` review after the non-trivial Go change.

## Planned code shape

- Create `internal/copierx/reconcile.go` for plans, state, diff, and apply.
- Create `internal/copierx/reconcile_test.go` for domain-neutral behavior.
- Create `internal/service/template_plan.go` for stack/workspace/chain plan
  construction and manifest handlers.
- Create `internal/service/template_plan_test.go` for composition and target
  mapping.
- Refactor `internal/service/stack.go`, `stack_update.go`, and `workspaces.go`
  into thin callers of the shared plan builder and reconciler.
- Update `api/`, CLI, platform client, REST, GraphQL inputs, and surface tests
  for workspace overwrite.
- Remove the now-unused direct chain rendering loops and the unused
  `copierx.Renderer.Update`/`UpdateRequest` API if no caller remains.
- Update command, template, API, surface, roadmap, and changelog documentation.

The abstraction earns its cost by deleting duplicate direct render loops,
centralizing ownership policy, and making later template-aware operations
mechanical plan construction rather than new filesystem code.

## Acceptance criteria

- Updating the current dev stack after the template gains `AGENTS.md` creates
  `<ANGEE_ROOT>/AGENTS.md`.
- A locally modified managed file is preserved with a structured conflict unless
  overwrite is explicitly requested.
- Safe tracked template deletions apply automatically; modified deletions require
  overwrite.
- Stack initialization, workspace creation, stack chains, and workspace chains
  render through one reconciler.
- Workspace update re-renders its workspace and chain templates.
- Bare stack update behavior remains unchanged.
- Runtime-managed manifest state and workspace port allocations survive.
- All documented verification commands pass.
