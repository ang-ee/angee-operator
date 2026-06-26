# Proposal: ephemeral workspace pool — leased, reaped, recycled

**Status:** Draft · **Area:** workspaces, sources, lifecycle · **Surfaces:** REST + GraphQL (operator) + Angee/agent provisioning

## Summary

Add a second, **ephemeral** tier to the workspace primitive — warm, leased, and
auto-reclaimed — alongside today's **durable, named** workspace. The durable tier
(`workspace/<name>`, pushed as a PR branch) is unchanged. The new tier is for the
agent that needs an isolated, ready-to-run environment *now*, will throw it away,
and must not strand it on the host when it dies.

Three coordinated mechanisms, each **completing or reusing** machinery the
operator already has:

1. A **workspace lease** — `{owner, expiry}` on a workspace — so a pooled or
   agent-held workspace has an explicit holder and a deadline. Reuses the
   `PortLease{Owner, CreatedAt}` precedent ([`manifest.go:68`](../../internal/manifest/manifest.go)).
2. A **TTL reaper** — a background sweep that actually destroys expired
   workspaces. Today the operator *computes* `state: "expired"`
   ([`workspaces.go:192`](../../internal/service/workspaces.go)) but **nothing
   reaps it**; the workspace lingers with its worktree, ports, and inner-stack
   data. This closes a half-built feature.
3. A **warm pool** — keep *K* pre-provisioned workspaces per template, hand one
   out on request, and **recycle** it (reset git + re-seed) instead of rebuilding
   it. This amortizes the expensive bootstrap (`build → migrate → rebac sync →
   resources load → schema`) across many short-lived tenants.

The operator stays a generic mechanism: it leases, reaps, and recycles
workspaces. *Why* an agent wants one, and *who* may hold one, stay in Angee
(REBAC-gated), exactly as for [sources](global-source-registry.md).

## Background: the two "treehouse" models

Two unrelated open-source tools named *treehouse* bracket the design space, and
Angee's workspace already sits at one end:

- **[mark-hingston/treehouse-worktree](https://github.com/mark-hingston/treehouse-worktree)** —
  a *named worktree per agent*: durable branch, setup-on-create, merge/complete
  flow, plus an **agent lock** (`--agent <id> --expiry`, auto-release) and an MCP
  server so agents self-serve. **Angee's durable workspace is already a superset
  of this** (multi-slot sources, port pools, inner stack, GitOps topology, the
  three update scopes) — *except* it has no lock/lease and no MCP facade.
- **[kunchenguid/treehouse](https://github.com/kunchenguid/treehouse)** — a
  *worktree pool*: "manage worktrees without managing worktrees." Request a clean
  tree, get a recycled one (caches preserved), return it to the pool on exit;
  conservative pruning removes only *idle + clean + already-merged* trees.
  **Angee has none of this** — every workspace is a cold, durable build.

The durable tier is the first model, already shipped and richer. This proposal
lifts the *ideas* of the second (pool, recycle, prune) and the *lock* of the
first — not the tools — onto the existing primitive. We do **not** adopt their
detached-HEAD model or their separate `.treehouse`/`.cursor/worktrees.json`
metadata stores: the manifest is the single source of truth, and named
`workspace/<name>` branches are load-bearing for the PR flow.

## Problem

A managed agent fleet wants disposable environments at interactive speed. Today
that is expensive and leaky:

- **Cold start.** Every `workspaceCreate` materializes worktrees
  ([`workspaces.go:944`](../../internal/service/workspaces.go)), renders copier,
  and the caller then runs the full inner-stack bootstrap from scratch — install
  deps, migrate an empty DB, sync REBAC, load resource data. For a throwaway task
  this is minutes of setup per use, repaid nothing.
- **No reclamation.** `TTL`/`TTLExpiresAt` exist on the workspace
  ([`manifest.go:177`](../../internal/manifest/manifest.go)) and `WorkspaceStatus`
  reports `expired` ([`workspaces.go:192`](../../internal/service/workspaces.go)),
  but **no code destroys an expired workspace.** An agent that dies leaves its
  worktree, its leased ports
  ([`releaseWorkspacePorts`, `workspaces.go:573`](../../internal/service/workspaces.go)
  runs only on explicit `WorkspaceDestroy`), and its inner-stack data on the host
  indefinitely. The compose-isolation proposal already documents stale
  per-workspace containers piling up
  ([`compose-project-isolation.md`](compose-project-isolation.md)).
- **No coordination.** A workspace has no notion of *who holds it*. The only
  guard against two actors driving the same worktree is after-the-fact
  `branch-mismatch`/`discrepancy` detection
  ([`workspaces.go:22`](../../internal/service/workspaces.go)). For a shared pool
  that is not enough — handing the same warm tree to two agents corrupts both.

These are three faces of one gap: the workspace primitive models *durable,
human-owned branches* well and *ephemeral, machine-held environments* not at all.

## Current behavior (the machinery to reuse)

The operator already owns every hard part; the gaps are lifecycle policy, not
mechanism.

- **Worktree materialization** is native: `materializeWorkspaceSource`
  ([`workspaces.go:1072`](../../internal/service/workspaces.go)) calls
  `WorktreeAdd`/`WorktreeAddBranch`
  ([`git.go:99`](../../internal/git/git.go)) over a shared source cache. A pool
  member is just a workspace; recycling it is `git reset` + re-checkout against
  the base ref — git operations the package already performs for
  `WorkspaceSyncBase` ([`workspaces.go:834`](../../internal/service/workspaces.go)).
- **Leases already exist for ports.** `PortLease{Port, Owner, CreatedAt}`
  ([`manifest.go:68`](../../internal/manifest/manifest.go)) is owned by
  `workspace/<name>/<pool>` strings and reclaimed by prefix on destroy
  ([`workspaces.go:573`](../../internal/service/workspaces.go)). A **workspace
  lease** is the same shape one level up.
- **TTL is computed, never enforced.** `WorkspaceCreate` parses `req.TTL` into
  `TTLExpiresAt` ([`workspaces.go:92`](../../internal/service/workspaces.go)) and
  `WorkspaceStatus` derives `Expired`
  ([`workspaces.go:192`](../../internal/service/workspaces.go)) — but no sweeper
  acts on it. The reaper is the missing consumer.
- **An actor identity already exists.** `mintConnectionToken(actor, scope, ttl)`
  ([`schema.graphql:336`](../../internal/operator/schema.graphql),
  [`tokens.go:81`](../../internal/operator/tokens.go)) issues per-actor scoped
  JWTs. A lease `owner` keys off the same actor string.
- **State is observable.** `GitOpsTopology`
  ([`gitops.go:20`](../../internal/service/gitops.go)) and the per-slot
  `GitOpsLink` ([`api/types.go:154`](../../api/types.go)) already expose
  clean/dirty/ahead/behind/diverged — exactly the signals a *safe* reaper and a
  *safe* recycle must check before reclaiming.

So: worktrees, leases, TTL fields, actor identity, and clean/dirty signals all
exist. What is missing is the policy that ties them into a pool.

## Ownership (the load-bearing decision)

The two tiers are two different facts conflated under one word. Splitting them is
the spine of the proposal.

| Fact | Durable tier (today) | Ephemeral tier (new) |
| --- | --- | --- |
| Identity | named `workspace/<name>`, human-chosen | pool slot, machine-allocated |
| Branch | pushed, becomes a PR | local-only, discarded on recycle |
| Holder | a developer, implicitly | a **lease** `{owner, expiry}`, explicit |
| Lifecycle end | explicit `workspaceDestroy` | **reaped** on TTL/lease expiry; **recycled** to pool |
| Provisioning | cold build per workspace | **warm**, bootstrap amortized |

The operator owns *lease, reap, recycle* (host-local mechanism — it is the only
component that sees the worktree, the ports, and the running inner stack).
Angee/Django owns *who may take a lease* and *what the workspace is for*
(REBAC-gated), and never needs to know whether a given workspace came from the
pool.

## Proposal

### 1. Workspace lease (`{owner, expiry}`)

Add an optional `Lease` to the `Workspace` manifest entry
([`manifest.go:172`](../../internal/manifest/manifest.go)), mirroring `PortLease`:

```go
type WorkspaceLease struct {
    Owner     string    `yaml:"owner" json:"owner"`           // actor string, as minted
    CreatedAt time.Time `yaml:"created_at" json:"created_at"`
    ExpiresAt time.Time `yaml:"expires_at" json:"expires_at"` // lease deadline; renewable
}
```

- **Acquire/renew/release** ride the existing admin-bearer gate and reuse the
  decode/writeJSON/auth conventions: `POST /workspaces/{name}/lease`
  (`{owner, ttl}`), `DELETE /workspaces/{name}/lease`. Acquiring an
  already-leased, unexpired workspace held by a *different* owner is a fail-fast
  `409` — this is the coordination guard a shared pool needs.
- **Surface** the lease in `WorkspaceStatus` and as a field on the workspace node
  in `GitOpsTopology`, so the console and `angee ws status` show who holds what.
- The lease is **advisory over the daemon's admin caller**, like every other
  operator gate: REBAC in Angee decides who may request a lease before the bridge
  ever calls. The operator only enforces *one live holder at a time* and *expiry*.

Renewal (`ttl` bump) is how a long-running agent keeps a workspace; a dead agent
simply stops renewing and the reaper collects it.

### 2. TTL + lease reaper

Add one background sweep to the operator (the operator already runs background
goroutines for log streaming and shutdown —
[`operator.go:235`](../../internal/operator/operator.go) is the lifecycle home).
On a fixed interval it scans the manifest's workspaces and, for each whose
`TTLExpiresAt` **or** lease `ExpiresAt` has passed:

1. **Safety check** (the conservative-prune rule, lifted from treehouse ②): reap
   only when every source slot is **idle and clean** — not dirty, and either
   already pushed or its HEAD is already merged into the base ref. The
   `GitOpsLink` state ([`api/types.go:154`](../../api/types.go)) already carries
   `Dirty`/`Ahead`/`Pushed`; the reaper reads it, never re-derives it.
2. If safe → `WorkspaceDestroy`
   ([`workspaces.go:364`](../../internal/service/workspaces.go)), which already
   tears down the worktree and releases ports by owner prefix.
3. If **unsafe** (dirty or unpushed work past its deadline) → do **not** delete.
   Flip the workspace to a sticky `state: "expired"` (already a value) and leave
   it for a human; emit a log line. Silent deletion of unpushed work is the one
   thing a reaper must never do.

This is purely the missing *consumer* of fields that already exist; no new state
model. Interval and the dirty-grace policy are operator config.

### 3. Warm pool (recycle, don't rebuild)

A template may declare itself poolable, and the operator maintains a small warm
set:

- **Pool config** lives where port pools already live —
  `operator.port_pool` has a sibling `operator.workspace_pool.<template>:
  { size: K, ttl: <dur> }`. The operator keeps *K* pre-rendered, fully
  bootstrapped workspaces of that template **ready** (leased to a reserved
  `pool` owner).
- **Acquire** (`POST /workspaces/pool/{template}/acquire`, `{owner, ttl}`):
  transfer one ready member's lease to the caller and return its `WorkspaceRef`
  in O(reset) instead of O(build). If the pool is empty, fall back to a cold
  `WorkspaceCreate` (correctness over latency) and log the miss — **no silent
  cap** ([per-service-log-streaming.md](per-service-log-streaming.md) sets the
  precedent for surfacing operator-internal events).
- **Recycle** (on release or reap-when-clean): reset each worktree to the base
  ref (`git reset --hard` + clean, the inverse of `WorkspaceSyncBase`), drop the
  local throwaway branch, then re-run the **idempotent** bootstrap to restore
  seed state. Crucially this **keeps the expensive caches** the durable build
  pays for once — installed deps under the worktree and the inner-stack DB volume
  (a `persist:` bind subpath, [`compose-project-isolation.md`](compose-project-isolation.md)) —
  so re-seeding is a `migrate`/`resources load` delta, not a from-zero build.
- **Topping up** is the reaper's mirror image: after handing a member out, render
  a replacement toward `size: K` in the background.

A pooled workspace is otherwise an ordinary workspace — same worktree, same
GitOps topology, same status surface. "Pooled" is a lease owner plus a recycle
policy, not a new object.

## Design options

### A. Two tiers on one primitive, lease-keyed (recommended)

Keep the durable named workspace exactly as is; add the ephemeral tier as
*lease + reaper + pool policy* over the same `Workspace`/worktree machinery. One
object, one status surface, one set of git mechanics; the only new state is the
lease. Matches every existing pattern (PortLease, TTL fields, actor tokens) and
adds no parallel system. **This is the proposal.**

### B. Detached-HEAD scratch trees (rejected)

Mirror treehouse ②'s pool literally: pooled members run in detached HEAD with no
branch, eliminating branch-name bookkeeping. Rejected — it forks the worktree
model (the durable tier *needs* `workspace/<name>` for push/PR), and Angee's
worktrees already avoid name contention by construction (one branch per
workspace). A throwaway tree can simply hold a discarded local branch; no second
checkout model is warranted.

### C. Separate `.pool` metadata store (rejected)

Track pool membership/locks in a sidecar file, as both treehouse tools do
(`.treehouse`, `.cursor/worktrees.json`). Rejected — the manifest is the durable
source of truth for workspaces, ports, and leases already; a second store would
duplicate it and drift. The lease and pool config belong *in* the manifest.

### D. External agent-runtime orchestrator (out of scope, see below)

Let each agent-runtime service manage its own throwaway clones outside the
operator. Rejected as the owner — only the operator sees the host's worktrees,
ports, and running stacks, so only it can reap safely and pool centrally. This is
the same "single-root operator must construct the invariant locally" argument as
[`compose-project-isolation.md`](compose-project-isolation.md).

## Security

- All new endpoints ride the existing `s.auth(...)` admin-bearer gate; no new
  auth surface. **Authorization over who may hold a lease or acquire from a pool
  is enforced in Angee (REBAC) before the bridge calls the daemon** — identical
  to sources ([global-source-registry.md](global-source-registry.md)).
- The lease `owner` is the already-minted actor string
  ([`tokens.go:81`](../../internal/operator/tokens.go)); the operator treats it as
  an opaque label and enforces only single-holder + expiry.
- The reaper's dirty/unpushed guard prevents the one dangerous outcome
  (destroying unpushed work). Recycling resets only *local throwaway* branches,
  never a pushed `workspace/<name>` branch — the durable tier is untouched.

## Backward compatibility

- Purely additive. A workspace with no `Lease` and no pool config behaves exactly
  as today; `WorkspaceCreate`/`Destroy`/`SyncBase`/`Push` are unchanged.
- The reaper only acts on workspaces that *already* carry a `TTL` (opt-in at
  create time, `WorkspaceCreateRequest.TTL`
  [`api/types.go:84`](../../api/types.go)) or a lease. Existing TTL-less
  workspaces are never touched — so this is strictly safer than today, where an
  `expired` TTL is reported and then ignored.
- Old daemon + new Angee: pool/lease endpoints 404; Angee degrades to cold
  `WorkspaceCreate` with no lease (status quo). New daemon + old Angee: no lease
  requests arrive, reaper only sees TTL workspaces — unaffected.

## Out of scope

- **An operator MCP facade.** treehouse ① ships an MCP server so agents drive
  worktrees natively; Angee already exposes the full REST+GraphQL surface plus
  the `angee ws` CLI skills (`/workspace`, `/pull`, `/push`). Wrapping
  `workspaceCreate`/`acquire`/`lease`/`syncBase`/`push` as MCP tools is a thin,
  valuable follow-up — but it is a transport over this contract, not part of it.
  Record it; build it after the lease/pool land.
- **Recycle-vs-rebuild heuristics** (when a member is too drifted to reset and
  should be rebuilt). Start with always-reset; tune later.
- **Cross-host / multi-operator pools.** One operator owns one root; a fleet-wide
  pool is an Angee-layer concern above several operators.
- **Pre-warming policy beyond a fixed `size: K`** (demand prediction, burst
  scaling). Fixed size first.

## Acceptance

- A workspace created with a `ttl` is **destroyed** by the reaper once expired
  *and* clean+pushed; an expired-but-dirty workspace is **kept** and marked
  `expired`, with a log line. Survives a daemon restart (state in the manifest).
- `POST /workspaces/{name}/lease` grants a single holder; a second owner gets
  `409` until expiry or release; the lease shows in `WorkspaceStatus` and
  `GitOpsTopology`.
- `POST /workspaces/pool/{template}/acquire` returns a ready `WorkspaceRef` in
  O(reset) when the pool is warm, transfers the lease to the caller, and triggers
  a background top-up; an empty pool falls back to a cold create and logs the
  miss.
- A recycled member resets to the base ref with no leftover throwaway branch and
  re-seeded data, **without** reinstalling deps or recreating the DB volume from
  scratch (caches preserved).
- The operator source contains **no** detached-HEAD checkout path and **no**
  sidecar pool/lease store — lease and pool config live in the manifest.
- `docs/reference/operator-api.md` documents the lease endpoints, the pool
  acquire endpoint, and the `operator.workspace_pool` config.

## See also

- [`compose-project-isolation.md`](compose-project-isolation.md) — parallel
  workspaces and the stale-container problem this reaper helps bound.
- [`global-source-registry.md`](global-source-registry.md) — the same
  "operator is a generic mechanism, Angee owns kind + REBAC" split.
- [`per-service-log-streaming.md`](per-service-log-streaming.md) — surfacing
  operator-internal events (pool misses, reaps) rather than capping silently.
- [`internal/service/workspaces.go`](../../internal/service/workspaces.go) —
  `WorkspaceCreate` (`:27`), TTL set (`:92`), `Expired` derivation (`:192`),
  `WorkspaceDestroy` (`:364`), `releaseWorkspacePorts` (`:573`), `WorkspaceSyncBase`
  (`:834`), `materializeWorkspaceSource` (`:1072`).
- [`internal/manifest/manifest.go`](../../internal/manifest/manifest.go) —
  `PortLease` (`:68`), `Workspace`/`TTL`/`TTLExpiresAt` (`:172`), `WorkspaceSource`
  (`:181`).
- [`internal/git/git.go`](../../internal/git/git.go) — `WorktreeAdd` (`:99`),
  `WorktreeAddBranch` (`:108`): the materialize/recycle mechanics.
- [`internal/operator/tokens.go`](../../internal/operator/tokens.go) —
  `MintConnection` (`:81`): the actor identity a lease owner reuses.
- Inspiration (ideas, not dependencies):
  [mark-hingston/treehouse-worktree](https://github.com/mark-hingston/treehouse-worktree)
  (lock + MCP), [kunchenguid/treehouse](https://github.com/kunchenguid/treehouse)
  (pool + recycle + conservative prune).
