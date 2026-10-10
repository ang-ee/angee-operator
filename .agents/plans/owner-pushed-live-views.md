# Live views from owner events: implementation plan

**Status:** Draft, O1 answered, revised after an Opus review (2026-10-10) ·
**Created:** 2026-10-10 · **Decided by:** the owner, 2026-10-10, while
answering Q6 of [`jj-native-operator.md`](jj-native-operator.md). File:line
references are valid at `cd7e8fd` (v0.21.2) here and at `292b6a7` in
angee-django.

## The rule

The console updates live only for what an owner pushes: Docker's events,
process-compose's state stream, and the operator's own actions. Everything
else is read on demand, slot and source state included, with a refresh
button in the console. That removes the 2 s polling loops.

The polling code stays, switched off by default, for a future state cache
or poll mode. It is not deleted.

## Why

Two pollers run in the operator, and one in the console:

- `operator/gql/events.go` runs three loops on a 2 s ticker
  (`defaultEventPollInterval`, `:25`) while anything is subscribed:
  `pollTopology` (`:126`), `pollSnapshot` (`:162`) and one
  `pollWorkspaceStatus` per subscribed workspace (`:246`), which never stops
  once started (`:315-317`). Each tick re-reads, hashes and publishes only
  when the hash changed.
- The Hasura-style list subscriptions (`services`, `jobs`, `sources`,
  `workspaces`, `templates`, `secrets`; `schema.resolvers.go:868-928`) each
  poll every 2 s, one goroutine per subscriber (`liveList`,
  `operator/gql/collections.go:15`, `:74`).
- The console polls `latestJobRun` every 2 s, idle or not
  (angee-django `job-run.ts:13`, `:25`).

One snapshot tick (`buildSnapshot`, `:197`) calls `StackStatus` three times
(directly, through `ServiceList` and through `JobList`), each querying Docker
Compose and process-compose; runs `git status` on every source cache
(`SourceList`); runs git on every cache and slot, and reads every chained
workspace's inner stack (`GitOpsTopology`, `gitops.go:86`); queries the
secrets backend once per secret (`secrets.go:66`); and re-lists workspaces
and templates.

The loops exist because the operator is not the only writer and had no
change signal: agents edit and commit in slots, the local CLI runs
`service.Platform` in its own process, containers crash and restart, and
people edit `angee.yaml`. The console already has the right rule ("If a
foreign system publishes no change subscription, add one there rather than
polling it from the client", angee-django `docs/frontend/guidelines.md:656`)
and moved its snapshot loop into the daemon (`transport.tsx:213`). The
daemon then polled instead. This plan removes the poll rather than moving it
again.

Slot state is the part that cannot be watched cheaply. Watching a checkout
means recursive file watching: Go has no built-in watcher, fsnotify uses
kqueue on macOS (one open descriptor per file, which `node_modules` defeats),
and git's own file-monitor daemon does not run on Linux, where the operator
image runs (its git 2.52 answers "fsmonitor--daemon not supported on this
platform"). Watchman would be a new dependency, and jj slots would add
snapshot writes on a timer. On demand avoids all of that. Safety never
depended on the display: the destroy guard and every slot verb read the slot
when they act.

## What updates what

| State | Changes when | Updated by |
|---|---|---|
| Container services and jobs | a container starts, stops, dies or changes health | `docker compose events --json` for the stack |
| Local services and jobs | a process changes status or health | `process-compose process monitor -o json` (v1.120.0: a snapshot event per process on connect, then one event per transition) |
| Job runs (status, per-node progress) | the runner moves a node (`setNode`, `jobs.go:567`) | the operator, on each transition |
| Anything else the operator changes | an operator mutation finishes | the operator, when the mutation lease is released |
| Inner stacks of chained workspaces | their containers or processes change | the same two watches, per inner stack (O3) |
| Workspace list, templates, secrets, stack name | an operator mutation, the local CLI, a hand edit | the operator's own mutations; otherwise on demand |
| Source caches, topology, workspace slot status | agents, the local CLI, git or jj in the slot | on demand (refresh); also after an operator mutation that named that workspace or source |
| A workspace's `expired` state | the clock passes `ttl_expires_at` (`workspaces.go:341`) | the console, from `ttlExpiresAt`; no timer anywhere |
| Logs | a line is written | already pushed (`SubscribeServiceLogs`, `events.go:342`) |

Changes made outside the operator (an agent's commit, a local CLI command,
a hand edit) show up on the next refresh. The console shows when each
on-demand section was read.

## Steps

Each numbered item is one pull request. The polling loops stay on until
step 5, so live views never lose an update source in between, and nothing
the console reads is removed before the console has stopped reading it.

### 1. The operator's own actions push

1. `Platform` gains a hook the operator registers (an optional interface,
   so test fakes of `service.API` need not implement it), called when
   `beginMutation`'s release runs (`service/platform.go:105`). A nested
   lease already returns a no-op release (`:106-107`), so each top-level
   mutation fires once. The hook fires after failures too: a failed
   mutation can still have changed state.
2. The hook carries what the mutation touched: the stack, or a workspace or
   source by name. Slot and source reads in the refresh happen only for
   what the mutation named, so a `SecretSet` never reads, or snapshots, a
   slot.
3. Lease coverage was audited in review: every GraphQL mutation reaches a
   method that takes the lease, except preflight and the token mints, which
   change no state. A new mutating method must take the lease.
4. `EventHub` gains `Refresh(ctx, scope)`: re-read the snapshot (services
   and jobs from one `StackStatus` call, not three), plus the topology and
   workspace status for the named scope, and publish what changed. Requests
   that arrive while a refresh runs coalesce into one more. No ticker.
5. Job runs: `JobRunStart` hands its lease release to `executeJobRun`
   (`jobs.go:108`, `:370`), so the hook fires only when the run ends. Each
   `setNode` transition publishes through a new `onJobRunChange`
   subscription.

Tests: the hook fires once per top-level mutation, with its scope; a
`SecretSet` triggers no slot read; the hub coalesces a burst into at most
two reads; a job run publishes each node transition.

### 2. Runtime watches

1. `runtime.Backend` (`runtime/backend.go:77`) gains
   `Watch(ctx, target) (<-chan struct{}, error)`: a signal that the backend's
   state changed, not a copy of it. The hub re-reads `StackStatus` on a
   signal; the backend stays the only reader of its state.
2. Compose: a long-running `docker compose events --json` built from the
   backend's own base arguments (`-f <root>/docker-compose.yaml` and the env
   file, `compose/backend.go:357`); the project name lives in the generated
   file.
3. process-compose: `process-compose process monitor -o json` with the
   backend's client arguments (`clientArgs`, `proccompose/backend.go:1471`).
   process-compose owns the stream and its client; the operator does not
   hand-write a websocket client. When the subcommand is missing (an older
   process-compose), `Watch` returns a typed unsupported error and local
   service state is read on demand.
4. No reconnect loop. When a stream ends or errors (the stack was stopped,
   Docker restarted), the hub does one full re-read and the watch stops. It
   is re-armed by an operator action that starts or restarts the runtime
   (the step 1 hook), by a new subscriber, and by a refresh. Docker resumes
   with `--since` the last event's time, so nothing between the end and the
   re-arm is lost.
5. Broker changes (`operator/gql/broker.go`):
   - first-subscribe and last-unsubscribe callbacks start and stop watches,
     instead of the loops checking `hasSubscribers` on each tick;
   - a full subscriber buffer keeps the latest value instead of dropping it
     (`:80-90`): without a next tick, a dropped value would never be
     repaired;
   - a value sent on subscribe goes to that subscriber only, outside the
     hub-wide change hash.
6. Inner stacks get the same watches while their workspace is subscribed
   (O3).

Tests: recorded `docker compose events --json` and `process monitor -o json`
lines decode; a stream end causes exactly one full re-read and no further
reads until a re-arm; `--since` is passed on resume; a full buffer delivers
the latest value; watches start on the first subscriber and stop after the
last.

### 3. On-demand reads (additive)

1. `WorkspaceStatus`, `SourceState` and `GitOpsTopology` gain `read_at`, so
   the console can show "as of". Regenerate gqlgen and the SDL.
2. `onWorkspaceStatusChange` and `onGitOpsTopologyChange` send one current
   value to each new subscriber: subscribing is a request. Today the first
   value only arrives with the first tick, and `useWorkspaceStatus`
   (angee-django `provision.ts:89`) has no query of its own.
3. Nothing is removed or nulled in this step.

Tests: subscribing reads once and delivers to that subscriber only;
`read_at` is set; `make check-generated`.

### 4. angee-django: refresh, "as of", and no client polling

1. The workspace, source and topology panes get a refresh button that runs
   their query, and show `read_at`. That includes the agents addon's
   provisioning view (`AgentProvisioning.tsx:250`, which uses
   `useWorkspaceStatus`).
2. The snapshot subscription document (`documents.daemon.ts:236`) stops
   selecting `sources` and `gitOpsTopology`. Pushes overwrite cached fields
   with whatever they carry (`snapshot-cache.ts:19-21`, null becomes empty
   at `transport.tsx:315`, `:319`), so the console must stop selecting them
   before the operator stops filling them.
3. The console subscribes to `onGitOpsTopologyChange` (nothing does at
   `292b6a7`). Its `sources` feed the Sources pane after an operator
   action; the Query-root reads (`SNAPSHOT_QUERY` selects both) are the
   refresh.
4. `useJobRunOperation` replaces its 2 s `latestJobRun` poll (`POLL_MS`,
   `job-run.ts:13`) with `onJobRunChange`.
5. `expired` is derived in the console from `ttlExpiresAt`.

### 5. Poll off by default

Lands only once the step 4 console release is what stacks run: the operator
image defaults to `:latest` (angee-django `templates/stacks/dev/copier.yml`),
so an older console meets a newer operator.

1. `EventHub` starts no ticker loop when the interval is zero, and zero
   becomes the default. `liveList` takes its ticks from the hub's signals
   (runtime watches, the mutation hook) instead of `liveSubInterval`;
   `sources` gets only the mutation signal (O1). An operator flag
   `--poll-interval` (and `ANGEE_OPERATOR_POLL_INTERVAL`) turns both back on,
   unchanged. `SetPollInterval` (`events.go:75`) stays for tests. Turning
   the poll on brings back per-tick slot reads, which on a jj slot means a
   snapshot per tick.
2. Snapshot pushes send null for `sources` and `gitOpsTopology`, which
   become nullable and `@deprecated` in `StackSnapshot`
   (`schema.graphql:991`). They are removed only after every console
   release in use has stopped selecting them; a client that still selects a
   removed field fails validation of the whole subscription, and the
   console drops subscription errors silently (`transport.tsx:163`).
3. Docs: `docs/reference/operator-api.md` describes subscriptions as polled
   (`:650`, `:716-725`); rewrite it with the table above. Changelog.

Tests: with the default, a subscribed hub and a live list make no platform
read without a trigger (a fake `service.API` counts calls); with
`--poll-interval 50ms`, the existing event and live-list tests pass
unchanged.

## Open questions for the owner

- **O1. Snapshot shape. Answered 2026-10-10: the git-backed sections leave
  the pushed snapshot** (steps 4 and 5). The snapshot carries what owners
  push; git state is on demand. The question as it was put:
  `onStackSnapshotChange` carries every section, `sources` and
  `gitOpsTopology` included, so a Docker push would either re-read git or
  re-send the last git read, and an old topology would overwrite a fresher
  one the console had queried, since socket pushes are cache writes
  (`transport.tsx:151`).
- **O2. The local CLI.** Its writes bypass a running operator, so they show
  only on the next refresh. Recommendation: accept that here; routing the
  CLI through a running operator is a separate decision.
- **O3. Inner stacks.** A chained workspace's status mixes its inner stack's
  runtime (Docker and process-compose, which push) with its slots (on
  demand) (`workspaces.go:376-387`, `:1029`). Recommendation: the same split
  as O1. Inner runtime is pushed by watches on that inner stack while the
  workspace is subscribed; slot state stays on demand with `read_at`. The
  alternative, inner runtime on demand too, is simpler but breaks the rule
  for state an owner does push.

## Relation to the jj plan

The jj plan's D7 now says every slot read snapshots, Q6 is answered, and its
phase 10 (op-heads watch) is dropped. A jj slot needs nothing from this plan:
its state is slot state, read on demand like a git slot's. Step 1's scoped
refresh keeps operator actions from snapshotting slots they did not touch.
