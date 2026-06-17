# Proposal: compose project identity derived from the stack root, not the manifest `name:`

**Status:** Draft · **Area:** runtime backends (compose), stack compile · **Surfaces:** compile (`compose.File.Name`) + container/network/volume namespacing

## Summary

The Docker Compose **project name** the operator emits is the manifest `name:`
verbatim ([`internal/service/platform.go:232`](../../internal/service/platform.go)
sets `compose.File{Name: stack.Name}`). The Compose project is a **global
namespace in the shared Docker daemon** — it keys container, network, and volume
names — but `stack.Name` is a human-facing label that is **not** unique across
stacks. Two stacks that share a `name:` get their containers merged into one
project even though they live in different roots and are managed by different
operator daemons.

This proposal **decouples the compose project identity from the display name**:
keep `stack.Name` as the friendly label (API responses, gitops, console), and
derive the compose project name from the **absolute stack root**, which is the
one identifier a single-root operator daemon knows is unique. This restores the
per-directory isolation Docker Compose gives by default — isolation the operator
currently overrides by writing an explicit `name:`.

## Motivation

### The collision, observed in the wild

Running two `angee-django` dev **workspaces** at once (the supported parallel
flow — each leases its own ports) produced an agent chat WebSocket that closed
with `1006` ("no response from the edge"). Both workspace stacks default their
`name:` to the example name, `notes-angee`. Inspecting the live containers:

```
$ docker inspect notes-angee-edge-1 \
    --format '{{ index .Config.Labels "com.docker.compose.project" }} {{ index .Config.Labels "com.docker.compose.project.working_dir" }}'
notes-angee  /…/workspaces/integrations-improvements/.angee

$ docker inspect notes-angee-agent-demo-agent-1 --format '… same …'
notes-angee  /…/workspaces/platform-improvements/.angee
```

One Compose project (`notes-angee`) held the **edge from one workspace** and the
**agent from another**. Compose deduplicates by `project + service`, so the
second stack to come up adopted/clobbered the first stack's containers:
- only one `notes-angee-edge-1` can exist, bound to whichever stack came up last;
- the other workspace's agent ends up fronted by the wrong edge (or none), so the
  browser opens `ws://localhost:<that stack's edge_port>/…` against a port with no
  listener → `1006`.

The edge **port** was already distinct per stack (each workspace leases
`operator.port_pool.edge`); the failure is purely the shared **project name**.

### Why the operator must own this (not the manifest author)

A single operator daemon manages **one** stack root and has **no global view** of
other stacks on the host. It therefore *cannot validate* that another stack uses
the same `name:` — it never sees the other manifest. Uniqueness cannot be
checked; it must be **constructed locally** from something this operator already
knows is unique. The absolute root is exactly that.

Pushing the requirement onto the manifest author (e.g. templates encoding the
workspace name into `name:`) is fragile: it is an invariant enforced by
convention across every template and every hand-written stack, with no daemon
able to catch a violation. `angee-django` has already patched its dev workspace
template to scope `name:` per workspace as an interim fix, but that is a
belt-and-suspenders mitigation, not the owner of the invariant.

### Why only the compose backend is affected

Every compose subcommand is already root-scoped through
[`baseArgs`](../../internal/runtime/compose/backend.go) —
`docker compose -f <root>/docker-compose.yaml …` with `--env-file` under the same
root. The **file path** isolates per root; the **project name inside the file**
does not. The process-compose backend doesn't have this bug: it is isolated by
its per-stack control port (workspace-allocated), not by a daemon-global name.
So the gap is specific to Compose's global project namespace.

## Ownership split (the load-bearing decision)

| Fact | Owner today | Owner proposed |
| --- | --- | --- |
| Friendly stack label (API, gitops, console) | `stack.Name` | `stack.Name` (unchanged) |
| Compose project identity (container/network/volume namespace) | `stack.Name` | **derived from the absolute stack root** |

These are two different facts conflated into one field. The display name wants to
be readable and may legitimately repeat across stacks; the project identity must
be globally unique per running instance. Splitting them is the whole proposal.

## Proposed change

Compute the project name where the stack is compiled — `Compile` already receives
`root` ([`platform.go:222`](../../internal/service/platform.go)):

```go
// internal/service/platform.go
Compose: compose.File{
    Name:     composeProjectName(stack.Name, root), // was: stack.Name
    Services: map[string]compose.Service{},
    Volumes:  map[string]compose.Volume{},
},
```

```go
// composeProjectName derives the Docker Compose project name for a stack.
// It MUST be unique per stack *instance* (root): the Compose project is a
// global namespace in the shared Docker daemon, so two stacks sharing a name
// have their containers/networks/volumes merged. stack.Name is a human label
// and is not unique (every dev workspace defaults to the same example name),
// and a single-root operator cannot see other stacks to detect a clash — so
// uniqueness is constructed from the one identifier this operator knows is
// unique: its absolute root.
func composeProjectName(name, root string) string {
    base := sanitizeProjectName(name) // lower-case, [a-z0-9_-], leading alnum
    if base == "" {
        base = "angee"
    }
    abs, err := filepath.Abs(root)
    if err != nil {
        abs = root
    }
    sum := sha256.Sum256([]byte(abs))
    return fmt.Sprintf("%s-%s", base, hex.EncodeToString(sum[:4])) // 8 hex chars
}
```

Writing the derived name into `compose.File.Name` is sufficient: every
subcommand reads the project from the rendered file's `name:` (no `-p` is
passed today), so `up`/`down`/`ps`/`logs` stay consistent through one source of
truth. No change to `baseArgs` is required.

`stack.Name` keeps flowing unchanged to the display surfaces
([`platform.go:167`](../../internal/service/platform.go) `StackStatus`,
[`gitops.go:72`](../../internal/service/gitops.go)).

Container names then read like `notes-angee-1a2b3c4d-edge-1` — the friendly base
plus a stable per-root suffix, never colliding across roots.

### Compose name constraints

Project names must match `^[a-z0-9][a-z0-9_-]*$`. `sanitizeProjectName` must
lower-case, replace illegal runes with `-`, collapse repeats, and guarantee a
leading alphanumeric (the hash suffix is already `[0-9a-f]`).

## Migration

Changing the derived name re-namespaces a stack's containers, networks, and
volumes. On first `up` after upgrade the operator brings the stack up under the
**new** project; the **old** project's containers linger as orphans.

- **App data is safe.** Angee persists app state via `persist:` **bind subpaths**
  under the stack root (e.g. `subpath: .angee/data`), not Docker named volumes, so
  a project rename does not touch it. Any genuinely named Docker volume *would* be
  recreated empty — call this out in release notes and prefer bind `persist:`.
- **Orphan cleanup.** Two options (pick in review):
  1. *Automatic* — at bring-up, if a legacy project named exactly `stack.Name`
     has containers for this root, `down` it first, then `up` under the new name.
  2. *Manual* — document a one-time `angee down` (old binary) before upgrading.

## Alternatives considered

- **Pass `--project-name` to every subcommand instead of the file `name:`.**
  Equivalent isolation, but must be threaded through *all* of
  `baseArgs`'s callers uniformly or `down`/`ps` silently target the wrong
  project. Writing the file `name:` keeps one source of truth and is the smaller
  change.
- **Readable path token instead of a hash** (e.g. `name-<stackdir>` →
  `notes-angee-platform-improvements`). Prettier and what the `angee-django`
  template now produces, but two equally-named stacks under different parents can
  still collide, and the operator would have to choose "the meaningful segment."
  A root hash is unconditionally unique. The two compose: the template may keep
  emitting a readable `name:`, and the operator still appends the root hash —
  correctness from the operator, readability from the template.
- **Validate uniqueness and reject a clash.** Impossible for a single-root
  daemon — it never sees the other stack (see Motivation).
- **Leave it to manifest authors.** The status quo; unenforceable, already bit us.

## Testing

- Unit: `composeProjectName` — same `name` + different `root` ⇒ different project;
  same `name`+`root` ⇒ stable; illegal-rune / empty `name` ⇒ valid sanitized
  output with a leading alphanumeric.
- Compile: two stacks with identical `name:` and distinct roots compile to
  distinct `compose.File.Name`.
- Backend (with a fake `Runner`): `Up`/`Down`/`Status` for two same-named,
  different-root stacks operate on disjoint projects.

## Open questions

1. Hash width — 4 bytes (8 hex) is ample for a host; confirm.
2. Auto-`down` the legacy project on upgrade, or document a manual step?
3. Should the rendered `name:` stay the friendly base for readability (this
   proposal) or become the full derived value? (Containers show the latter
   regardless.)

## Related

- [`edge-ingress-caddy.md`](edge-ingress-caddy.md) — the central edge whose
  per-stack container this collision was breaking.
- [`ingress-routing-modes.md`](ingress-routing-modes.md) — `routing: path` +
  per-stack `edge_port`, the layer that made parallel dev stacks a first-class
  flow (and surfaced this bug).
