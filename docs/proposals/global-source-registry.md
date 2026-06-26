# Proposal: global source registry + per-render workspace materialization

**Status:** Draft · **Area:** workspaces, sources, copier · **Surfaces:** REST (operator) + Angee provisioning

## Summary

Give the daemon two generic, kind-agnostic capabilities so a control plane can
place arbitrary git content into a workspace at render time:

1. A **global source registry** — git repos the daemon clones/caches **once** and
   reuses across workspaces, upserted over REST (`POST /sources`) and pruned
   (`POST /sources/{id}/destroy`), keyed by an opaque `id`.
2. **Per-render materialization** — a new render input `inputs.sources`, a JSON
   list of `{source, subpath, dest}`, telling the daemon to copy the `subpath`
   subtree of a registered `source` into `<workspace>/<dest>`.

Crucially, **the daemon learns nothing about what a source *is***. There is no
"skill source" or "template source" type in the operator. A source is a git repo;
a render says "put this subtree there." **Source identity, kind, and access
control live entirely in Angee/Django, REBAC-gated there.** The daemon is a
generic, deterministic materializer.

## Principle: sources are generic and global

> The operator does not model "skills", "templates", or any other source *kind*.
> It registers git repos by id, caches them, and copies named subtrees into
> workspaces where told. Which repos exist, who may see them, and what a given
> subtree *means* (a skill, a template, a dataset) is the control plane's
> concern — owned and REBAC-gated in Angee/Django.

This keeps the operator a small, kind-free mechanism and lets Angee add new uses
(skills today; datasets, prompt packs, anything tomorrow) without touching the
daemon.

## Problem

Today the daemon can clone git content into a workspace **only** from what a
template **statically declares** in its `_angee.sources` block. A control plane
that wants to place *dynamic, per-render* content — chosen at provision time, not
known to the template — has no path:

- no way to hand the daemon a runtime list of "copy this subtree to that
  workspace dir", and
- no out-of-band registry to clone a repo **once** by a stable id and reference
  subtrees of it across many renders.

Copier can't bridge it either: the daemon runs copier-go with
`WithSkipTasks(true)` and passes inputs only as Jinja data
(`internal/copierx/copierx.go`), so a template can't loop a dynamic input list
into N sources. The dynamic part must live in daemon code.

The motivating consumer is Angee agent **skills** (an agent's chosen skill
directories must land at `/workspace/.claude/skills/<dir>/` before its service
starts), but the daemon needn't — and shouldn't — know that. Angee computes the
`dest` and the source set; the daemon just clones and copies.

## Current behavior

- **`workspaceCreate`** (`internal/service/workspaces.go:27`) resolves the
  template, merges `inputs` over `_angee.inputs` defaults, materializes the
  template's declared sources (`materializeWorkspaceSources`, `:62`), then renders
  copier (`:71`). `inputs` are `map[string]string` (Copier answers); unknown keys
  are inert.
- **Sources are already generic git clones.** `copierx.TemplateSource`
  (`internal/copierx/copierx.go:158`) and `manifest.Source`
  (`internal/manifest/manifest.go:149`) are `{Repo, Ref/DefaultRef, CachePath,
  …}` — no semantic kind. A repo is cloned/fetched **once** into a shared cache at
  `Platform.sourcePath()` (`internal/service/sources.go:246`) via
  `materializeSource` (`sources.go:150` — `git clone` / `git fetch --all
  --prune`).
- **Stack sources** already expose generic REST: `GET /sources`, `POST
  /sources/{name}/fetch|pull|push` (`internal/operator/operator.go:118`), with
  the standard `decode[T]` (`:816`) / `writeJSON` (`:871`) / `s.auth(...)` pattern
  and `api.ErrorResponse` (`api/types.go:22`) via `writeServiceError`
  (`internal/operator/errs.go:11`).
- **`WorkspaceSourceStatus`** (`internal/operator/schema.graphql:88`) already
  describes a materialized workspace source generically: `{slot, source, kind,
  mode, ref, subpath, path, …}`.

So the cache, the per-render input plumbing, the REST conventions, and the
generic source vocabulary all exist. The two gaps are (a) registering a source
**out of band by id** (not only via a template's static `_angee.sources`), and
(b) a **per-render dynamic list** of subtree→dest copies. Neither needs a kind.

## The Angee contract (consumed by this proposal)

Angee/Django owns the source inventory (`integrate.Source`, REBAC-gated) and the
agent→content mapping. Its operator bridge (`addons/angee/operator/daemon.py`)
already speaks a **generic** contract:

- **On source change** → register/upsert one repo by id:
  ```http
  POST /sources
  { "id": "src_…", "repo": "https://github.com/anthropics/skills.git",
    "ssh_repo": "", "ref": "main" }
  ```
  Prune: `POST /sources/{id}/destroy`.
- **On provision** → `workspaceCreate` with one extra input:
  ```jsonc
  inputs.sources = "[{\"source\":\"src_…\",\"subpath\":\"document-skills/pdf\",\"dest\":\".claude/skills/pdf\"}]"
  ```
  `source` is a registered id; `subpath` is the repo-root-relative subtree;
  **`dest` is the workspace-relative target — Angee computes it** (here under
  `.claude/skills/`, but the daemon neither knows nor cares why). Refs are
  lightweight by design (no `repo`/`ref`), so the daemon resolves `source` against
  the registry.

Angee always sends `sources` (`"[]"` when none) and registration is best-effort,
so an un-upgraded daemon that **ignores** the unknown input + endpoints keeps
provisioning working (content simply absent).

## Proposal

### 1. Global source registry (kind-free)

Persist registered sources in the manifest (durable across restarts, like stack
sources) and clone each into the existing cache.

- **Register** (`POST /sources`, body `{ id, repo, ssh_repo, ref }`): upsert the
  manifest entry, then clone/fetch into a cache dir keyed by `id` (e.g.
  `Platform.sourcePath(id)`), reusing `materializeSource`. Idempotent; returns the
  source state. Prefer `ssh_repo` only when SSH auth is configured, else `repo`.
- **Deregister** (`POST /sources/{id}/destroy`): drop the manifest entry and its
  cache dir. `NotFound` → 404 (benign for the best-effort bridge).
- **List** (`GET /sources`): already exists; registered sources appear alongside
  any template/stack-declared ones — they are the same generic concept.

This reuses the `/sources` namespace deliberately: the registry **is** the
operator's set of sources. New service methods on `Platform`
(`internal/service/sources.go`); new handlers in
`internal/operator/operator.go`; request/response types in `api/types.go`.

### 2. Per-render materialization (`inputs.sources`)

In `WorkspaceCreate`, after the copier render (so the workspace tree exists), add
`materializeRenderSources(ctx, workspacePath, inputs["sources"])`:

1. JSON-decode `inputs["sources"]` into `[]RenderSource{ Source, Subpath, Dest }`
   (empty/absent → no-op).
2. Per entry: look up `Source` in the registry; ensure its cache is present and
   fetched at its `ref` (lazily clone via `materializeSource` if a render races a
   register); **export the `Subpath` subtree** from the cache into
   `<workspacePath>/<Dest>` — a plain file copy of the tree at that subpath (or
   `git archive <ref>:<subpath> | extract`), **no worktree, no `.git`** (the
   content is read-only in the workspace).
3. A `Source` not in the registry is a **fail-fast** `InvalidInputError` naming
   the id (→ 400) — the control plane surfaces "register the source first" rather
   than silently rendering missing content.

This completes before `WorkspaceCreate` returns — hence before any service mounts
the workspace. `Dest`/`Subpath` are sanitized against traversal (reject `..`/
absolute, like the subpath normalization at `workspaces.go:1131`) so a render can
only write under the workspace.

### Relationship to template `_angee.sources`

Template-declared sources and `inputs.sources` are the **same operation** —
materialize a source subtree into a workspace — differing only in where the list
comes from (template metadata vs render input). The recommended end state is one
internal materializer the static template path and the dynamic input path both
feed, so a workspace's sources are `template _angee.sources ∪ inputs.sources`. The
minimal change adds the registry + `inputs.sources` path; converging the two is a
clean follow-up, not a prerequisite.

## Design options

### A. Registry + lightweight refs (recommended — matches the shipped bridge)

Repos registered once by id; renders carry only `{source, subpath, dest}`. The
"which repos exist" lifecycle is decoupled from "what this render wants", clones
de-dupe by id, and provision inputs stay small. This is what the Angee bridge
already implements.

### B. Self-contained refs, no registry (rejected)

Have `inputs.sources` carry the full `{repo, ref, subpath, dest}` and skip the
registry (clone-cache by repo URL still de-dupes). Simpler daemon, but pushes repo
coordinates into every render and couples each render to repo availability;
diverges from the shipped bridge. Recorded as the considered alternative.

### C. Worktree per entry (rejected)

Reuse the worktree path. Worktrees imply a branch and push/dirty tracking the
content should never have here, and leave `.git` plumbing in the dest. A plain
subtree export is simpler and correct for read-only content.

## Security

- Both endpoints ride the existing `s.auth(...)` admin-bearer gate; no new auth
  surface. **Authorization over *which* sources exist and who may attach them is
  enforced in Angee (REBAC) before the bridge ever calls the daemon** — the daemon
  trusts its admin caller, exactly as for stack sources.
- Registration carries a repo URL + ref, no secrets; private repos use the
  daemon's existing git auth.
- `inputs.sources` carries no secrets. `Dest`/`Subpath` MUST be traversal-
  sanitized so a render can only write under the workspace; content is copied
  read-only (no worktree/branch/push back to the source).

## Backward compatibility

- Purely additive and kind-free. Renders without `inputs.sources` and templates
  without registered sources behave exactly as today; the registry endpoints are
  new (or extend the existing `/sources` namespace).
- New daemon + old Angee: unaffected. Old daemon + new Angee: ignores the unknown
  input, 404s the endpoints — Angee already degrades to "content absent,
  provisioning fine".

## Out of scope

- **Any notion of source *kind* (skill/template/…).** That lives in Angee. This
  proposal deliberately keeps the daemon kind-free.
- **Surfacing rendered sources in `WorkspaceStatus`.** Showing `inputs.sources`
  results in `WorkspaceSourceStatus` (so the console's Workspace pane lists them)
  is a nice follow-up, not required.
- **Re-materialization on reprovision.** Reprovision recreates only the service
  over the existing workspace; content changes need a fresh workspace render.
- **Unifying the template and dynamic source paths** into one materializer
  (recommended direction above) can land after the registry.

## Acceptance

- `POST /sources` for a repo clones it once into the cache and records it in the
  manifest; a repeat is a no-op fetch; `POST /sources/{id}/destroy` removes both.
  Survives a daemon restart.
- `workspaceCreate` with `inputs.sources` referencing a registered source writes
  the named subtree under `<workspace>/<dest>` before returning, with no `.git`;
  an empty/absent `sources` is a no-op.
- A `sources` entry referencing an unregistered id fails the create with a 400
  naming the id; the workspace is not left half-rendered.
- The daemon source code contains **no** reference to "skill"/"template" kinds in
  the registry or materialization paths.
- End-to-end against Angee: register a repo (e.g. `anthropics/skills`), provision
  an agent that references one of its subtrees → the file tree appears at the
  Angee-computed `dest` in the workspace.
- `docs/reference/operator-api.md` documents `POST /sources`, `POST
  /sources/{id}/destroy`, and the `inputs.sources` render input.

## See also

- [`internal/service/workspaces.go`](../../internal/service/workspaces.go) —
  `WorkspaceCreate` (`:27`), `materializeWorkspaceSources` (`:944`), subpath
  normalization (`:1131`).
- [`internal/service/sources.go`](../../internal/service/sources.go) —
  `materializeSource` (`:150`), `sourcePath` (`:246`): the clone/cache machinery
  the registry reuses.
- [`internal/copierx/copierx.go`](../../internal/copierx/copierx.go) —
  `TemplateSource` (`:158`), `WithSkipTasks(true)` (`:228`): why dynamic sources
  can't be template-driven.
- [`internal/operator/operator.go`](../../internal/operator/operator.go) — the mux
  (`:118`), `workspaceCreate` (`:635`), `decode`/`writeJSON` (`:816`/`:871`).
- [`api/types.go`](../../api/types.go) — `WorkspaceCreateRequest` (`:203`),
  `ErrorResponse` (`:22`).
- [`internal/manifest/manifest.go`](../../internal/manifest/manifest.go) —
  `Source` (`:149`), `WorkspaceSource` (`:181`).
- Angee consumer side (in `angee-django`): the generic bridge
  `addons/angee/operator/daemon.py` (`register_source`/`deregister_source`), the
  skill→source-ref mapping that computes `dest`
  (`addons/angee/agents/models.py`: `Skill.workspace_ref`,
  `Agent.provision_workspace_sources`), and the handover
  `.agents/handovers/skills-materialization.md`. Skill/REBAC semantics stay there.
