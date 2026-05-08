# Ideas / Gaps Surfaced From v1 → v2 Comparison

Status: working notes. Captured by comparing what `new/` currently delivers
against what `refactor/PLAN.md` + `refactor/OVERVIEW-v2.md` describe and
against what v1 (top-level `cli/`, `internal/`, `api/`, `templates/`)
actually shipped.

The goal is to make explicit every v1 capability that does not survive in
v2, so each can either be deliberately dropped (with a one-line rationale
in PLAN.md) or scheduled into a phase. Items split into three buckets:

1. **Intentionally dropped** — refactor already states this; listed here
   only for completeness.
2. **Design gaps** — v1 solved something v2 does not address at all.
   These need an explicit "yes, we're dropping this" or "yes, here's
   where it lands" decision before the rebuild is feature-equivalent.
3. **Implementation gaps vs. the plan** — PLAN.md describes the
   capability; `new/` does not yet implement it (in some cases despite
   `new/PLAN.md` marking the phase complete).

---

## 1. Intentionally dropped (no action — listed for the record)

These are explicitly resolved by `refactor/PLAN.md`,
`refactor/OVERVIEW-v2.md`, or `refactor/examples-v2/GAPS.md`. Listed so
nobody re-opens them by accident.

| Capability (v1) | Resolution |
|---|---|
| `Agent` registry, `angee agent …`, `/agents/*`, agent MCP wiring, `_angee.kind: agent` | PLAN.md §"Concepts" / §"What Angee gives an 'agent' (without naming it)". Agents are reconstructed at the runtime layer by combining a workspace + a service. |
| `mcp_servers:` block in `angee.yaml`, MCP server credential resolution | PLAN.md §"What Angee gives an 'agent'". MCP servers are declared as ordinary services or sidecars. |
| `${connector.x.y}` substitution namespace | GAPS.md item 5. Application layer resolves per-user OAuth/connector credentials and passes them as inputs/secrets. |
| Manifest-as-git-history (every deploy = commit, `/rollback`, `/history`, `/deploy`, `/pull`) | Refactor scopes git operations to *sources* (worktrees), not to the manifest. The new operator does not commit on apply. |
| `/scale/{service}`, `/down`, `/plan`, `/reconcile`, `/config`, `/openapi.json` shapes | Replaced by the more granular Phase-6 surface in PLAN.md. (See §3.10 below for `/openapi.json` re-evaluation.) |

---

## 2. Design gaps — v1 capabilities that v2 does not address

Each of the following needs an explicit decision: **drop with rationale**
or **schedule into a phase**. None are currently in PLAN.md or
OVERVIEW-v2.md.

### 2.1. Service domains, ingress, Traefik labels

**v1 had it.** `internal/config/angee.go` defined `DomainSpec{Host, Port,
TLS}` and `LifecyclePlatform` services. `internal/compiler/compose.go`
auto-emitted Traefik labels (`traefik.enable=true`,
`traefik.http.routers.<n>.rule=Host(\`example.com\`)`,
`traefik.http.routers.<n>.entrypoints=websecure`, …). Tested at
`internal/compiler/compose_test.go:59 TestCompilePlatformTraefikLabels`.

**What v2 has.** Nothing. `manifest.Service` has no `domains:` field;
PLAN.md never mentions ingress, reverse proxy, hostnames, TLS, or
Traefik. Services that bind ports get raw `ports:` mappings only.

**Why it matters.** angee.ai and the dev-template's `angee-notes-dev`
flow assume a Traefik-fronted stack with subdomains per service. Without
ingress wiring, every service has to expose a host port, and there is no
TLS story. This is the visible-to-end-users surface.

**Decision needed.** Either (a) add a `domains:` field on services and
ship a router-labels emitter (the PLAN-friendly version: not
Traefik-specific in the manifest, but Traefik happens to be how
`runtime: container` materializes it), or (b) state in PLAN.md that v2
is port-only and ingress is a higher-layer concern (which contradicts
the dev-template story).

### 2.2. Service lifecycle taxonomy and restart policy

**v1 had it.** `LifecyclePlatform | Sidecar | Worker | System | Agent |
Job` (`internal/config/angee.go:99`). The compiler used lifecycle to
pick `restart:` (e.g. `unless-stopped` for `system`, `no` for `job`),
and to decide whether Traefik labels apply.

**What v2 has.** Two values on `Service.Runtime`: `container | local`.
No restart policy field at all. `compose.Service` (in the new compose
renderer) has no `restart:` emission unless the user writes raw compose.

**Why it matters.** Containers crash. Without an opinionated default,
postgres has to be configured by hand to come back after a host reboot.
The "platform / sidecar / worker" vocabulary was also load-bearing for
docs ("which kind of service am I declaring?").

**Decision needed.** Either reintroduce a small `restart:` field
(`no | on-failure | always | unless-stopped`, defaulting to
`unless-stopped` for containers), or state explicitly that callers must
supply raw `restart:` themselves.

### 2.3. Healthchecks

**v1 had it.** `HealthSpec{Path, Port, Interval, Timeout}` →
compose `healthcheck:`. Used by `depends_on` with condition
`service_healthy`.

**What v2 has.** Nothing on `Service`, nothing in compose emission. The
new manifest's `depends_on` is the weak compose v2 form (start-order
only, no health gating).

**Why it matters.** Without healthcheck-gated `depends_on`, a Django
service that depends on Postgres frequently crashes on first boot
because Postgres accepts TCP before it's ready to accept queries. v1's
healthcheck story was the workaround.

**Decision needed.** Reintroduce `Service.Health` (HTTP path + interval
is enough for 95% of cases; allow custom command for the rest), or
declare that callers handle readiness inside their service.

### 2.4. Replicas and scale

**v1 had it.** `ServiceSpec.Replicas`, `compose.deploy.replicas`,
`/scale/{service}` HTTP, `angee scale` CLI.

**What v2 has.** Nothing. PLAN.md command surface and HTTP surface have
no scale verb.

**Why it matters.** Workers (Celery, RQ, etc.) commonly need N replicas.
Without scale, callers have to declare N copies as N services with
different names — workable but ugly.

**Decision needed.** Probably reasonable to defer (compose-level
replicas are limited anyway and most production scaling is k8s territory
which is outside v1 scope), but should say so.

### 2.5. Resource limits (CPU / memory)

**v1 had it.** `ResourceSpec{CPU, Memory}` → `compose.deploy.resources`.

**What v2 has.** Nothing.

**Decision needed.** Defer or include. Single `cpus:` + `memory:` fields
on `Service` are cheap to add and prevent runaway containers from
freezing the dev host.

### 2.6. Workflows / multi-step orchestration

**v1 had it.** `WorkflowSpec` + `WorkflowActivity` for ordered
multi-step sequences (e.g. "build → migrate → seed → smoke-test").

**What v2 has.** Jobs only, with `Job.RunOn` (a list, not a DAG) and
`depends_on`. No named workflow concept.

**Why it matters.** Migration-then-seed-then-restart sequences are common
in dev-stack templates. v1 used workflows for this; v2 has no equivalent
besides "run jobs in the right order by hand."

**Decision needed.** Either accept that callers script this with a job
that runs other jobs (via the operator HTTP API), or reintroduce a small
workflow primitive.

### 2.7. Scheduled / cron jobs

**v1 had it.** `LifecycleJob` jobs with `Kind` (`once | scheduled`).

**What v2 has.** PLAN.md describes Job as "one-shot or scheduled" but
the new `manifest.Job` struct has no `schedule:` / `cron:` field, and
there is no scheduler running anywhere. `angee job run` only triggers
manually.

**Why it matters.** Backups, periodic sync, certificate refresh — all
common stack chores. With no scheduler, callers need an external cron.

**Decision needed.** Either add `Job.Schedule` (cron expression) and a
ticker in the operator, or state that scheduled execution is the
caller's problem and remove the "scheduled" word from PLAN.md.

### 2.8. Workspace destroy safety guard

**v1 had no per-service "tear down with workspace" semantics.**
PLAN.md Phase 5 §9 specified this: refuse `WorkspaceDestroy` when any
service or job references the workspace via `mounts:`, `workdir:`, or
`${workspace.<n>...}`; `--force` overrides; `--purge` implies
`--force`.

**What v2 has.** `new/internal/service/workspaces.go` `WorkspaceDestroy`
removes the workspace entry without checking references. A running
Django container mounted at `workspace://feat-x:/code` will keep
running but reading from a deleted directory.

**Why it matters.** PLAN.md commits to this guard; users will rely on
it. Failing to enforce it leads to dangling-mount footguns.

**Decision needed.** Implement the guard as PLAN specifies. This is
both a design item (because PLAN already decided) and an implementation
gap.

### 2.9. Persistent paths (`${persist.<key>}`) lifecycle

**Refactor describes it.** Substitution grammar lists `${persist.<key>}`;
GAPS.md item 10 commits to template-declared `persist:` blocks that
survive workspace start/stop/update and are removed only on
`workspace destroy --purge`.

**What v2 has.** `manifest.PersistPath` struct exists in
`WorkspaceResolved`; `substitute.Context.Persist` exists; but
`new/internal/copierx/copierx.go` does not read a `_angee.persist:`
metadata block, nothing creates the persistent dirs at workspace
materialization, and nothing skips them on destroy.

**Decision needed.** Implement, or drop from the substitution namespace
list and from GAPS.md item 10.

### 2.10. OpenAPI / introspection endpoint

**v1 had it.** `GET /openapi.json` returned a full OpenAPI 3 spec for
the operator surface, used by Django code-gen and external clients.

**What v2 has.** Nothing. The "external runtime self-manages over HTTP"
story PLAN.md sells gets harder without a machine-readable contract.

**Decision needed.** Either generate OpenAPI from `api/` types (cheap,
high payoff), or commit to "MCP is the contract, OpenAPI is not
maintained." MCP alone is fine for LLM clients; less fine for a Django
RPC client.

### 2.11. Volume semantics (`Persistent`, `Size`, drivers)

**v1 had it.** `VolumeSpec{Driver, Path, Size, Persistent}`. `Driver:
local-fs` mapped to a host path; the `Persistent` flag survived
`destroy`.

**What v2 has.** `manifest.Volume{Driver, Path}`. No size, no
persistence flag. The PLAN talks about `volume://name:/target` mount
URIs but does not specify what the driver options are.

**Decision needed.** State the driver inventory ("local-fs only in
v1; tmpfs / named docker volume / nfs deferred"), and either restore
`Persistent` or document that all volumes survive `stack destroy`
unless `--purge` is passed.

### 2.12. Source kinds beyond `git` and `local`

**PLAN.md lists six.** `git`, `template`, `archive`, `url`, `local`,
`volume`.

**What v2 has.** `new/internal/service/sources.go:152` switches on
`git` and `local`; everything else returns
`source kind %q is not implemented`. Workspaces cannot mount a
downloaded tarball or a templated subtree.

**Decision needed.** Either schedule the missing four into a phase, or
narrow the manifest to the two kinds that work (git + local) and update
PLAN.md.

### 2.13. Git auth wiring (`ssh` / `https-token` / `host`)

**PLAN.md Phase 5a is explicit:** auth modes ship in 5a (not 5b),
because clone/fetch/worktree-add against private repos need them.
`auth.mode: host` is allowed only when the operator binds on loopback;
the standalone daemon refuses it on a non-loopback bind with a clear
error.

**What v2 has.** `manifest.SourceAuth{Mode, SSHKeySecret, TokenSecret}`
fields exist but `new/internal/git/git.go` shells out plain `git` with
no `GIT_SSH_COMMAND`, no credential helper config, no key
materialization to a 0600 tempfile under `<root>/run/`. No
loopback-binding check on `auth.mode: host` either.

**Decision needed.** Implement per Phase 5a. Without it, no private
repo can be a source — which is most real workspaces.

### 2.14. Push pre-flight checks

**PLAN.md Phase 5b §2.** Refuse push on dirty/untracked worktrees;
require `--set-upstream` if the branch has no upstream.

**What v2 has.** `git.Client.Push` (`new/internal/git/git.go:101`) is a
plain `git push`. No dirty check, no upstream check.

**Decision needed.** Implement.

---

## 3. Implementation gaps vs. PLAN.md

These are capabilities PLAN.md / OVERVIEW-v2 already commit to but that
`new/` does not yet deliver. Several are silently marked "complete" in
`new/PLAN.md`.

### 3.1. Real MCP JSON-RPC implementation

**Plan says.** PLAN.md Phase 6 lists `GET /mcp` returning the same
operations as tools.

**v1 had.** `internal/operator/mcp.go` — full JSON-RPC 2.0 with
`initialize`, `tools/list`, `tools/call`, ~14 tools including
`platform_status`, `service_logs`, `agent_*`, `history`.

**What v2 has.** `new/internal/operator/operator.go:561` — a single
`GET /mcp` handler that returns:

```go
writeJSON(w, http.StatusOK, map[string]any{
    "name":  "angee-operator",
    "tools": []string{"stack.status", "stack.up", "stack.down",
                     "services.create", "workspaces.create", "sources.fetch"},
})
```

That's a static six-string list, no JSON-RPC, no tool dispatch.
`new/PLAN.md` Phase 6 reads "Status: complete" — it is not.

**Action.** Implement JSON-RPC 2.0 with `tools/list` + `tools/call`
that dispatch to `service.Platform` methods. The 1:1 mapping is
straightforward because every CLI command already routes through the
same Platform method.

### 3.2. Operations API + SSE event stream

**Plan says.** PLAN.md Phase 2 includes `OperationGet, EventStream` on
`service.Platform`. PLAN.md Phase 6 lists `GET /operations/{id}` and
`GET /events (SSE)`. Async provisioning works as `202 + operation_id +
SSE`.

**What v2 has.** `api/types.go` defines `Operation` and
`OperationStatus` constants. There is **no operations store**, **no
`/operations/{id}` route**, and `/events` is a stub:

```go
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    _, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
}
```

It writes one `ready` event and closes — not a real SSE stream.

**Action.** Implement an in-memory operations store with an event bus.
Long-running operations (workspace create, source clone, stack up)
return `202 + {operation_id}` and stream progress events. Polled by
runtime callers; trivially extended to push.

### 3.3. TTL sweep loop

**Plan says.** PLAN.md Phase 6 / Phase 8 explicitly: implement
`service.Platform.startSweepLoop()`; every operator process (in-process
under `dev`, standalone daemon) calls it at startup. Sweep iterates
every minute, finds workspaces with `ttl_expires_at < now()`, calls
`WorkspaceDestroy` (skipping any blocked by the destroy safety guard,
emitting a `workspace.destroy_blocked` SSE event).

**What v2 has.** Workspaces have `TTL` and `TTLExpiresAt` recorded.
`grep -rn 'startSweep\|sweepLoop' new/internal/` returns nothing.
Nothing reaps expired workspaces.

`new/README.md:408` claims "Standalone operator: HTTP, SSE, MCP, bearer
auth, operation tracking, and TTL sweep loop" — none of those four
exist.

**Action.** Implement the loop in `service.Platform`. Single ticker,
single per-root lock acquisition per tick, idempotent.

### 3.4. In-process operator under `angee dev`

**Plan says.** PLAN.md Phase 4 — required. `angee dev` runs a loopback
HTTP+MCP listener for the lifetime of the dev command, injects
`ANGEE_OPERATOR_URL` and `ANGEE_OPERATOR_TOKEN` into every supervised
process, address-rewriting kicks in for cross-boundary networking.

**What v2 has.** `DEFERRED.md` acknowledges: *"angee dev does not start
the in-process HTTP/MCP operator for the lifetime of the dev command."*
`new/PLAN.md` Phase 3 still reads "Status: complete." Phases 3 and 4
in PLAN.md cannot honestly be complete without this listener — the
whole address-rewriting story (cross-boundary
`${operator.url}` / `${service.<name>}`) presumes services can reach
the operator over HTTP.

**Action.** Spin up `internal/operator.Server` on a loopback port at
the start of `StackDevForeground`, inject the URL+token into supervised
env, tear it down on Ctrl+C. Not large; it's the same code path as the
standalone daemon, just bound on `127.0.0.1:0` and short-lived.

### 3.5. Starter / example templates

**Plan says.** Phase 5 seeds templates from `refactor/examples-v2/`.

**What v2 has.** `new/templates/stacks/dev/` and
`new/templates/workspaces/basic/`. The fleshed-out
`angee-notes-dev` stack template and `pr` workspace template from
`refactor/examples-v2/templates/` are not ported. `DEFERRED.md`
mentions "embedded fallback templates not implemented."

**Action.** Either port the example templates verbatim into
`new/templates/`, or update `DEFERRED.md` to enumerate which examples
are blocking and which are reference-only.

### 3.6. Source materialization paths beyond git/local

(Same point as §2.12 — listed there as a design item because the
manifest schema already advertises six kinds; listed here because PLAN
explicitly enumerates them in Phase 5.)

### 3.7. Git auth + push pre-flight

(Same points as §2.13 and §2.14.)

### 3.8. Workspace destroy safety guard

(Same point as §2.8 — design and implementation gap.)

### 3.9. Persistent paths

(Same point as §2.9.)

### 3.10. `/openapi.json` (if kept)

(Same point as §2.10.)

---

## 4. Plan/code self-inconsistency

`new/PLAN.md` marks Phases 3, 4, 6, 7, 8 as **"Status: complete."**
At minimum the following items from those phases are not implemented
in `new/`:

| `new/PLAN.md` phase | Item claimed delivered | Actual state |
|---|---|---|
| Phase 3 | "`angee dev` with container sidecars and local processes" — implies in-process operator wiring | `DEFERRED.md` says the in-process operator is not running under `dev` |
| Phase 4 | "Workspace TTL fields and port leases" | Fields stored; no sweep loop, leases not released on destroy in all paths |
| Phase 5 | "Source pull, source push, workspace git status, and workspace push. Dirty-worktree checks before push." | Plain `git push`; no dirty check, no upstream check |
| Phase 6 | "Stack, service, job, workspace, source, event, and MCP endpoints." | MCP endpoint is a static stub (§3.1); SSE is a one-shot ready event (§3.2); no `/operations/{id}` |
| Phase 8 | "Port lease release on workspace destroy. Purge semantics." | Implemented for workspaces but partial; safety guard from PLAN.md §9 not enforced |

`refactor/DEFERRED.md` only lists 2 of these. The two plans should be
reconciled: either tighten `new/PLAN.md` so "complete" matches reality,
or extend `refactor/DEFERRED.md` to enumerate the open items honestly.

---

## 5. Suggested triage

If we treat PLAN.md as the contract and `new/` as the work-in-progress
implementation, the items roughly split:

**Must-fix to call v2 feature-equivalent (block any "v2 GA" claim):**

- §3.1 real MCP JSON-RPC
- §3.2 operations + real SSE
- §3.3 TTL sweep loop
- §3.4 in-process operator under `dev`
- §2.13 / §3.7 git auth (private repos do not work today)
- §2.8 / §3.8 workspace destroy safety guard

**Should-decide before further building (open design questions):**

- §2.1 domains / ingress
- §2.2 lifecycle / restart policy
- §2.3 healthchecks
- §2.6 workflows
- §2.7 scheduled jobs
- §2.10 OpenAPI

**Nice-to-have / can be deferred with one-line PLAN note:**

- §2.4 replicas, §2.5 resources, §2.11 volume options, §2.14 push
  pre-flight (small fix), §3.5 templates port

**Schema honesty:**

- §2.12 / §3.6 trim manifest source kinds to what works, or implement
  the missing four.
