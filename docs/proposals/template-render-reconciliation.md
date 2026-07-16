# Proposal: reusable template render reconciliation

**Status:** Implemented on `codex/template-render-reconciliation` (2026-07-15)

## Summary

Replace the independent Copier execution paths for stack initialization, stack
template updates, workspace creation, chain rendering, and template-created
services with one ordered render-plan reconciler. The reconciler renders a
composite template tree into a scratch directory, compares ordinary files
against persisted render state and the live destination, reports conflicts
before writing, and applies the result with consistent create, update, deletion,
dry-run, and overwrite semantics.

Stack manifests remain structurally merged through a service-owned file handler
because `angee.yaml` contains both template-origin declarations and
operator-managed runtime state. A service-owned `service.yaml` handler performs
a three-way structural merge of template-created services. Copier answers remain
machine-owned metadata.

## Motivation

`angee stack update --template` currently renders a complete Copier tree into a
temporary directory but consumes only `angee.yaml`. Any other new template
output, such as `AGENTS.md`, is deleted with the scratch directory. Stack and
workspace chains also each implement their own template resolution, input
overlay, destination selection, and direct `LocalRenderer.Copy` loop, while
`workspace update` updates only manifest metadata despite the documentation
claiming that it re-renders the workspace.

`service create --template` has the same split in another form: it renders into
a scratch directory, consumes `service.yaml`, destructively replaces the
service's build-context directory, and discards the rendered baseline. There is
no way to refresh that service from its recorded template without either losing
local asset and manifest edits or implementing reconciliation a second time.

The immediate symptom is a missing `AGENTS.md`, but copying that one file would
leave the ownership and conflict problem unsolved. Existing rendered files may
contain user edits, templates can delete files, chains overlay multiple
templates into one destination, and `angee.yaml` cannot be overwritten like an
ordinary file.

## Goals

- Give stacks, workspaces, and their chains one render and reconciliation
  mechanism.
- Render template-created services through the same mechanism and allow their
  manifest entry and build assets to be refreshed safely.
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
- Preserve the service name, bound workspace, and allocated ports across service
  template updates.
- Keep bare `stack update` derived-files-only; template reconciliation remains
  behind `stack update --template`.
- Make dry-run report both manifest and ordinary-file changes without writing.

## Non-goals

- Changing field-based service update semantics. `service update` without
  `--template` continues to apply explicit image, command, environment, mount,
  port, and workdir changes.
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
- reconstructing stable service-template contexts from their recorded answers;
- locating the original Copier render target;
- registering machine-managed answer files;
- registering `angee.yaml` handlers;
- registering `service.yaml` handlers and committing their merged service entry;
- persisting stack, workspace, and service records only after reconciliation
  succeeds;
- regenerating runtime files after a stack template update.

CLI, REST, and GraphQL remain thin adapters over `service.Platform`.

## Core model

The reconciler receives a complete plan rather than one template at a time:

```go
type RenderPlan struct {
	Target                string
	StateRoot             string
	StatePath             string
	Layers                []RenderLayer
	Documents             []string
	AllowedSymlinkParents map[string]*TrustedRoot
	ProtectedPaths        []string
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
without user-file conflict rules. Both are excluded from ordinary
file-fingerprint ownership. A handler may opt into a stored rendered-document
baseline when it needs a structured three-way merge.

## Render state

Render state is runtime-owned and stored outside the rendered file inventory:

```text
<ANGEE_ROOT>/run/template-state/stack.json
<ANGEE_ROOT>/run/template-state/workspaces/<name>.json
<ANGEE_ROOT>/run/template-state/services/<name>.json
```

The versioned JSON document stores slash-normalized paths relative to the plan
target. Every ordinary rendered entry records:

- entry kind: regular file or symlink;
- SHA-256 content hash for regular files;
- permission bits relevant to executable-mode changes;
- symlink target for symlinks.

Handlers that request a structured baseline store their previous canonical
rendered document in the same versioned state. This is content, not only a hash:
a three-way merge needs the old rendered value. Stack manifest handlers retain
their current provenance rules and do not require this baseline; service
handlers do.

Directories are created as needed but are not independently owned or deleted.
Empty directory cleanup is limited to directories emptied by reconciler-owned
deletions. The state file and its parents cannot become managed template
outputs. Configured persistent root directories are reserved from directory
ownership and deletion, while individual template files beneath them may still
be tracked.

State is written through a temporary file and rename only after the destination
apply succeeds. A corrupt state document fails closed and identifies its path;
it is never silently treated as missing state.

Stack persist paths and newly required source clones are staged before this
commit and carry rollback handles through state publication. New repositories
clone into a private temporary tree before capability-bound installation;
existing repositories are validated but not fetched during reconciliation.
Generated runtime artifacts participate in the same journal, including explicit
deletions when Compose, process-compose, or OpenBao output is no longer desired.

Reconciliation preserves Copier `_preserve_symlinks`: a rendered symlink is
fingerprinted by its target and applied through a rooted symlink write, so it
round-trips through render state like any other entry. Declared workspace local
Source links are the only permitted symlink parents. The plan retains an opened
root and filesystem identity for each approved Source, verifies that the link
still resolves to that identity, and derives snapshots and destination guards
from the retained root rather than reopening the pathname. Destination guards
are anchored at the deepest verified parent, staged tree installs are copied
under that parent before replacement, and recursive backups remain rooted in
opened subdirectories. State I/O is rooted separately and cannot overlap
managed output; its root is retained from prepare through commit. Live file,
special-document, and parent-manifest baselines are revalidated immediately
before mutation so concurrent edits fail closed.

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

### Service manifests

A service template plan targets `<ANGEE_ROOT>/services/<name>`. Its
`service.yaml` is consumed by a service-owned handler and is never installed in
the build context. Other outputs, such as `docker/Dockerfile` and startup
scripts, are ordinary reconciler-managed files. The plan's answer file remains
at `services/<name>/.copier-answers.yml`, matching current creation behavior.

The handler parses exactly one service entry and reuses the existing
blast-radius, service-name, build-context containment, and service validation
checks. It stores the canonical rendered service entry as its handler baseline
and merges maps recursively:

- if current equals the old rendered value, use the new rendered value;
- if new equals the old rendered value, preserve the local value;
- if current equals new, accept it and advance the baseline;
- recursively merge map keys, including `env`, structured `build`, and `route`;
- treat scalars and lists as atomic values;
- preserve current-only keys;
- remove template-owned keys when current still equals the old rendered value;
- report a conflict when current and new changed the same atomic value
  differently;
- with overwrite, use the new rendered value for every conflict.

Conflicts name the structural path, for example
`services.agent-x.env.AUTH_MODE`. After merging, the handler decodes the result
back into `manifest.Service` and validates it before any asset or manifest write.

For a legacy template-created service with answers but no handler baseline, an
identical current and newly rendered service is adopted. A difference is
ambiguous and therefore conflicts unless overwrite is requested.

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

### Service creation

1. Resolve and validate the service template, workspace, stable name, inputs,
   and port allocations under the existing root lock.
2. Build a one-layer plan targeting `services/<name>` and register
   `service.yaml` plus the template answer file.
3. Apply in create mode, install ordinary build assets, and record both their
   fingerprints and the canonical rendered service baseline.
4. Add the rendered service entry and newly referenced secret declarations to
   the stack manifest.
5. Release leases for a routed result, persist the stack, and regenerate runtime
   files as today.

Creation rollback covers the reconciled assets, render state, service entry,
new secret declarations, and leases. The current destructive build-context
`RemoveAll`/move path disappears.

### Service template update

1. Require the existing service, its build-context answer file, recorded
   template source, and recorded workspace binding.
2. Merge recorded non-reserved answers with requested input overrides. Recompute
   `service_name`, `workspace_name`, `workspace_path`, and `alloc_*` from current
   authoritative state; callers cannot override them.
3. Preserve existing service leases and provision missing pool allocations with
   the same pool policy as creation under the root lock; roll back allocations
   the merged result does not retain.
4. Render and preflight the service manifest handler and ordinary assets.
5. For dry-run, return manifest, asset, and conflict changes without writing.
6. Without overwrite, fail before mutation when either the structured service
   entry or any asset conflicts.
7. Apply assets, handler baseline, answer metadata, and the merged service entry
   as one rollback-capable operation.
8. Declare newly referenced secrets but never remove an existing stack secret
   declaration merely because this service stopped referencing it.
9. Release service leases if the merged result is routed, persist the stack, and
   regenerate runtime files.

Template update never renames the service, rebinds its workspace, or starts,
stops, rebuilds, or restarts the running workload. Runtime lifecycle remains an
explicit follow-up command. Destroy/create is the supported rename or rebind
workflow.

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

Service template update is an explicit mode on the existing CLI command:

```text
angee service update <name> --template
  [--input key=value ...] [--dry-run] [--overwrite]
```

Template-mode flags are mutually exclusive with field-based update flags.
`--dry-run` and `--overwrite` require `--template`. The CLI dispatches to a new
`Platform.ServiceUpdateFromTemplate` method; field mode continues to dispatch to
`Platform.ServiceUpdate`.

The remote surfaces expose the same operation without overloading the existing
field-update DTO:

- API DTO: `ServiceUpdateTemplateRequest{Inputs, DryRun, Overwrite}`;
- REST: `POST /services/{name}/template/update`;
- GraphQL: `serviceUpdateFromTemplate(name, input)`;
- remote client and surface matrix entries matching both.

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

Service identity errors identify the missing or mismatched answer value. A
rendered service key that differs from the stable existing name is invalid even
under overwrite; overwrite resolves content conflicts, not identity changes.

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

### Template-created service integration tests

- Service creation installs assets through the reconciler and records answers,
  ordinary-file state, and the canonical service baseline.
- An unchanged service template update is a no-op.
- A changed Docker asset updates when untouched locally, conflicts after a local
  edit, and is replaced with overwrite.
- A new and deleted asset follow the shared ownership matrix.
- Independent local and template `env` key changes merge recursively.
- A local and template edit to the same scalar conflicts at the exact service
  field path; overwrite uses the template value.
- Template deletion of an unchanged service field removes it; current-only
  fields survive.
- Legacy answers without state adopt an identical service and conflict on an
  ambiguous difference.
- Name, workspace, and allocations remain stable across input and template
  changes.
- A routed/non-routed change releases or retains leases consistently and rolls
  allocation changes back on conflict.
- Newly referenced secrets are declared; old declarations are not removed.
- Dry-run changes neither service assets, state, leases, secrets, nor the stack
  manifest.
- CLI field and template modes reject mixed flags.
- REST, GraphQL, and the remote client expose matching overwrite and dry-run
  semantics.

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
- Refactor `internal/service/service_create.go` to use the reconciler and add the
  service structured-document handler plus `ServiceUpdateFromTemplate`.
- Extend reconciler state with optional canonical handler baselines.
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
- Service creation and service template update render assets through that same
  reconciler.
- Template-created service entries merge recursively against their previous
  rendered baseline, preserving non-conflicting field edits and reporting
  precise conflicts.
- Service identity, workspace binding, and port allocations remain stable.
- Workspace update re-renders its workspace and chain templates.
- Bare stack update behavior remains unchanged.
- Runtime-managed manifest state and workspace port allocations survive.
- All documented verification commands pass.
