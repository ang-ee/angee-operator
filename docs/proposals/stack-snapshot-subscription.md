# Proposal: `onStackSnapshotChange` aggregate snapshot subscription

**Status:** Draft · **Area:** operator, graphql, subscriptions · **Surfaces:** GraphQL

## Summary

Add one subscription, `onStackSnapshotChange`, that emits the aggregate stack
snapshot (stack, services, jobs, sources, workspaces, templates, secrets,
gitOps topology, health) whenever the polled aggregate's hash changes — the
same server-side hash-and-publish pattern the daemon already uses for
`onGitOpsTopologyChange` and `onWorkspaceStatusChange`. This lets the Angee web
console drop its 5 s client-side snapshot poll and instead receive live pushes,
while the *polling stays inside the daemon* (one poller for all subscribers)
rather than in every browser tab.

The console (`@angee/operator/runtime` → `useOperatorSnapshot`) today re-executes
a multi-root snapshot query on a 5 s `setInterval` because the daemon publishes
no aggregate change event. The Angee frontend rule is **never poll for data
freshness** — live updates ride GraphQL subscriptions, not a client refetch loop.
The per-resource subscriptions (`onWorkspaceStatusChange`, `onServiceLogs`)
already satisfy the per-agent views; this proposal closes the one remaining gap:
the overview/services/workspaces panes that read many root fields at once.

## Naming

The aggregate type is **`StackSnapshot`** and the subscription is
**`onStackSnapshotChange`** — not `ConsoleSnapshot`. The operator schema names
types after the *domain resource* they describe (`ServiceState`, `StackStatus`,
`WorkspaceStatus`, `GitOpsTopology`, `IngressStatus`) and subscriptions as
`on<Resource>Change` (`onGitOpsTopologyChange`, `onWorkspaceStatusChange`). The
daemon is the control-plane server for one root and serves the CLI, REST, and
any GraphQL client — not just the web console — so the public type must not bake
one consumer's name into the schema. The aggregate *is* the full state of one
stack/root as the operator sees it, which is exactly what `StackSnapshot` says,
and it sits naturally beside the existing `StackStatus`. The web console keeps
its own client-side `useOperatorSnapshot`/`OperatorSnapshot` names; those are a
frontend concern and need not match the daemon's surface.

## Problem

The web console's `useOperatorSnapshot` hook assembles one query over the root
fields each pane needs (`health`, `stackStatus`, `services`, `jobs`, `sources`,
`workspaces`, `templates`, `secrets`, `gitOpsTopology`) and re-runs it on a 5 s
`network-only` tick to stay current. That is a client polling loop:

- It runs once per open console tab, so N tabs = N independent pollers hitting
  the daemon, each re-walking git-backed resolvers (`sources`, `gitOpsTopology`)
  that can be expensive.
- It must defensively skip a tick while a prior fetch is in flight (a
  `network-only` re-execute aborts the pending request), so freshness latency is
  effectively `max(5 s, slowest-resolver)` and jittery.
- It is dead weight whenever nothing on the daemon changed — the common case for
  an idle stack.

The daemon already owns the right shape for the fix: a single background poller
that hashes a snapshot and fans the *changed* snapshot out to all subscribers.
It just doesn't expose an aggregate one. Per
[`graphql-websocket-transport.md`](graphql-websocket-transport.md), the 2 s
hash-polling event hub and the WebSocket transport are already in place; this
proposal adds one more poller and one more subscription field on top of them.

## Current behavior

- `type Subscription` (`internal/operator/schema.graphql`) exposes
  `onGitOpsTopologyChange`, `onServiceLogs`, `onWorkspaceLogs`,
  `onWorkspaceStatusChange` — all per-resource; **no aggregate snapshot event**.
- `EventHub` (`internal/operator/gql/events.go`) already implements the pattern:
  `pollTopology` ticks every `pollInterval` (default 2 s), calls
  `platform.GitOpsTopology`, `hashJSON`s the result, and `Publish`es to a
  generic `broker[T]` only when the hash changes. `pollWorkspaceStatus` does the
  same per named workspace, started lazily on first subscribe.
- Subscribers receive **no initial snapshot on connect** by design; they issue a
  one-shot query for current state and then consume change pushes. The console
  already issues the snapshot query, so this contract fits unchanged.
- The aggregate the console wants is a fan-out over existing `service.API`
  methods: `StackStatus`, `ServiceList`, `JobList`, `SourceList`,
  `WorkspaceList`, `Templates`, `SecretsList`, `GitOpsTopology` — all already
  resolved by the `Query` root.

## Proposal

Add an aggregate change subscription, mirroring `onGitOpsTopologyChange`. Three
focused changes, all already-trodden ground in the daemon:

1. **Schema.** Add to `type Subscription` in
   `internal/operator/schema.graphql`:

   ```graphql
   "Emitted whenever the aggregate stack snapshot's hash changes."
   onStackSnapshotChange: StackSnapshot!

   "The stack overview aggregate — the root fields the web console reads as one."
   type StackSnapshot {
     health: MutationResult
     stackStatus: StackStatus
     services: [ServiceState!]!
     jobs: [JobState!]!
     sources: [SourceState!]!
     workspaces: [WorkspaceRef!]!
     templates: [TemplateDescriptor!]!
     secrets: [SecretRef!]!
     gitOpsTopology: GitOpsTopology
   }
   ```

   The fields are exactly the `Query`-root types the console already selects, so
   the generated client types and the console's snapshot assembly need no new
   shapes — only a new root to read them from. `StackSnapshot` is left unbound in
   `gqlgen.yml`, so gqlgen generates `model.StackSnapshot` whose fields reuse the
   already-bound `api.*` models; no new `api` type is introduced.

2. **EventHub.** Add a `snapshot *broker[*model.StackSnapshot]` and a
   `pollSnapshot` goroutine that copies `pollTopology` verbatim: tick,
   `hasSubscribers()` guard, build the aggregate by fanning out the existing
   `platform.*` calls, `hashJSON` the result, `Publish` only on hash change.
   Start it in `Start()` alongside `pollTopology`. No new polling primitive — it
   reuses `broker[T]`, `hashJSON`, `reportPollError`, and `subscriberBuffer`.

3. **Resolver.** Add `OnStackSnapshotChange(ctx)` to `subscriptionResolver`
   (`internal/operator/gql/schema.resolvers.go`), returning
   `r.Events.SubscribeSnapshot(ctx)`, with the same `errSubscriptionsUnavailable`
   guard as the existing subscription resolvers.

On the Angee side, `useOperatorSnapshot` then opens `onStackSnapshotChange`
(scoped to the requested panes via the same `@include` toggles, or a single
all-fields snapshot) and drops the `setInterval` — issuing the existing snapshot
query once for the initial paint and applying each pushed `StackSnapshot`
thereafter.

## Design options

### One aggregate field vs. per-pane subscriptions

A single `onStackSnapshotChange` returning the whole aggregate keeps the daemon
to **one** extra poller regardless of how many panes a tab shows, and matches how
the console already assembles its snapshot from one query. The alternative — a
subscription per pane (`onServicesChange`, `onWorkspacesChange`, …) — multiplies
pollers and brokers for no client benefit, since the console reads the panes
together. Prefer the single aggregate; a client that needs only one slice selects
only that field in the subscription document (gqlgen still pushes on any aggregate
change, but the payload is field-pruned by the selection set).

### Hash granularity

`hashJSON` over the full aggregate means any field change wakes every subscriber.
That is the same coarseness `onGitOpsTopologyChange` already accepts and is fine
at console cardinality (a handful of services/workspaces). Per-field hashing is a
premature optimization; revisit only if a noisy field (e.g. a constantly-changing
timestamp) causes churn, in which case exclude it from the hashed projection.

### Poll interval

Reuse the hub's existing `pollInterval` (default 2 s, test-overridable via
`SetPollInterval`). The daemon poll replaces N client polls at 5 s with one
server poll at 2 s — fresher *and* less aggregate load once more than two tabs
are open, and the `hasSubscribers()` guard means zero cost when nothing is
connected.

## Security

- No new auth surface: `onStackSnapshotChange` rides the same `Subscription`
  root, the same SSE/WebSocket transports, and the same `InitFunc` /
  minted-`aud: operator`-token gate as the existing subscriptions
  ([`graphql-websocket-transport.md`](graphql-websocket-transport.md),
  [`edge-ingress-caddy.md`](edge-ingress-caddy.md)).
- The aggregate exposes only fields the actor can already read via the `Query`
  root; it adds no field that was not already queryable.
- `secrets` returns `SecretRef` metadata (names/declarations), never secret
  values — `secretValue` stays a deliberate, separately-gated query and is **not**
  part of the snapshot.

## Backward compatibility

Purely additive. The existing subscriptions, queries, and both transports are
untouched. Clients that keep issuing the one-shot snapshot query (or even keep
polling) continue to work; the subscription is opt-in. The "no initial snapshot
on connect" contract is preserved, so it composes with the documented
query-then-subscribe pattern.

## Out of scope

- **Retiring the snapshot query.** The one-shot aggregate query stays — it is the
  initial-state read the subscribe-then-stream pattern requires.
- **Client changes beyond `useOperatorSnapshot`.** How the Angee console wires the
  subscription (drop the `setInterval`, apply pushes) lives in the Angee repo;
  this proposal owns only the daemon surface it consumes.
- **Push-based platform events.** This keeps the daemon's server-side hash-poll
  model (the daemon polls the platform; clients subscribe to the daemon). A true
  event-sourced platform feed is a separate, larger change.

## Acceptance

- A client subscribing to `onStackSnapshotChange` receives a `StackSnapshot`
  push within one poll interval of any change to stack/services/jobs/sources/
  workspaces/templates/secrets/gitOps, and **no** push while the aggregate is
  unchanged.
- With nothing connected, `pollSnapshot` performs no platform reads (the
  `hasSubscribers()` guard holds), matching `pollTopology`.
- The Angee `useOperatorSnapshot` hook, switched to subscribe + one-shot initial
  query, shows live updates with the 5 s `setInterval` removed (verified against
  the daemon).
- `docs/reference/operator-api.md § Subscriptions` documents
  `onStackSnapshotChange` and its `StackSnapshot` payload alongside the
  existing per-resource subscriptions.

## See also

- [`docs/proposals/graphql-websocket-transport.md`](graphql-websocket-transport.md)
  — the WebSocket transport browser clients use for these subscriptions.
- [`internal/operator/gql/events.go`](../../internal/operator/gql/events.go) —
  `EventHub`, `pollTopology`, `broker[T]`, `hashJSON`: the pattern `pollSnapshot`
  copies.
- [`internal/operator/gql/schema.resolvers.go`](../../internal/operator/gql/schema.resolvers.go)
  — the `subscriptionResolver` the new field is added to.
- [`internal/operator/schema.graphql`](../../internal/operator/schema.graphql) —
  the `Subscription` and `Query` roots.
- `docs/reference/operator-api.md § Subscriptions` — the subscription contract
  this extends.
