# Operator API

Run the standalone operator:

```sh
angee operator --root . --bind 127.0.0.1 --port 9000
```

Non-loopback binds require `--token`. Protected endpoints use:

```http
Authorization: Bearer <token>
```

Surface parity between `service.Platform`, CLI, REST, and GraphQL is tracked in
[Surface parity](/reference/surfaces).

## REST

Health:

```http
GET /healthz
```

Stack:

```http
GET  /stack/status
POST /stack/init
POST /stack/update
POST /stack/prepare
POST /stack/build
POST /stack/up
POST /stack/dev
POST /stack/down
POST /stack/destroy?purge=true
GET  /stack/logs?service=name
```

Services:

```http
GET   /services
POST  /services                 field-based init (image / command / env)
POST  /services/create          template-based create (Copier template, kind=service)
PATCH /services/{name}
POST  /services/{name}/up        idempotent create-and-start
POST  /services/{name}/start
POST  /services/{name}/stop
POST  /services/{name}/restart
POST  /services/{name}/destroy
GET   /services/{name}/logs
```

`POST /services/create` body:

```json
{
  "template": "agents/claude-code",
  "workspace": "my-pa",
  "inputs": {"auth_mode": "api_key"},
  "name": "agent-my-pa",
  "start": false
}
```

The template must declare `_angee.kind: service` and render a
`service.yaml` containing exactly one service entry. The operator
appends that entry to the outer stack's `services:` map, installs any
other rendered files (typically `docker/`) at
`<root>/.angee/services/<service_name>/`, and allocates ports from
declared pools under owner `service/<name>/<pool>`.

Jobs:

```http
GET  /jobs
POST /jobs/{name}/run
```

Job output is returned by `POST /jobs/{name}/run`.

Sources:

```http
GET  /sources
GET  /sources/{name}/status
POST /sources/{name}/fetch
POST /sources/{name}/pull
POST /sources/{name}/push
```

`POST /sources/{name}/pull` is the top-level "update from upstream"
operation: fetch + fast-forward the cached source's tracking ref. The
per-workspace-slot equivalent is `POST /workspaces/{name}/sync-base`
(across all slots) and the GraphQL `workspaceSourcePull` mutation
(one slot).

Workspaces:

```http
GET   /workspaces
POST  /workspaces
GET   /workspaces/{name}
PATCH /workspaces/{name}
GET   /workspaces/{name}/status
GET   /workspaces/{name}/logs
POST  /workspaces/{name}/destroy?purge=true
GET   /workspaces/{name}/git
POST  /workspaces/{name}/push
POST  /workspaces/{name}/sync-base
```

Workspaces are a pure file primitive — `POST /workspaces` renders Copier
output and materializes sources, but the operator never starts services
on a workspace's behalf. If a workspace renders an inner stack and you want
it running, drive it with `POST /stack/up` against the inner root (start a
second operator with `--root workspaces/<name>/.angee`, or run `angee stack
up --root workspaces/<name>/.angee` locally).

Workspace status is the authoritative branch-identity surface for managed git
worktrees. Each status source includes the manifest `branch`, actual
`current_ref`, and `state`; `state: "branch-mismatch"` means the worktree is not
on its manifest branch, and the workspace top-level state is `discrepancy`.
`sync-base` updates each workspace branch from its base ref without switching
branches; body: `{"method":"merge"}` or `{"method":"rebase"}`.

### Update scopes

The operator exposes three "update" operations, distinguished by scope:

| Scope | Endpoint | Behaviour |
| --- | --- | --- |
| **Whole source** | `POST /sources/{name}/pull` | Fetch and fast-forward the cached top-level source's tracking ref. |
| **One workspace slot** | GraphQL `workspaceSourcePull(workspace, slot)` | Fast-forward a single slot's worktree from its tracking ref. The slot lives on the **workspace branch**, not the source's main branch. |
| **All slots of a workspace** | `POST /workspaces/{name}/sync-base` | Merge or rebase each slot's workspace branch against its declared base ref. Stays on the workspace branch — this is "stay current with `main`". |

GitOps topology (derived view across sources × workspace slots):

```http
GET /gitops/topology[?with_commits=N]
```

Returns the full topology snapshot. `with_commits` opts in to populating
each git source's recent commit history (`sources[].commits`); omit or
set to 0 to keep the response cheap.

Source diffs:

```http
GET /sources/{name}/diff[?ref=...]
```

`ref` empty → working tree vs HEAD (uncommitted changes); set → HEAD vs
ref. Returns `[]DiffFile`.

Per-workspace-source slot operations (slot lives at
`workspace.sources.<slot>` in the manifest):

```http
POST /workspaces/{name}/sources/{slot}/fetch
POST /workspaces/{name}/sources/{slot}/pull
POST /workspaces/{name}/sources/{slot}/push        body: {"ref":"..."}    (optional)
GET  /workspaces/{name}/sources/{slot}/diff[?ref=...]
POST /workspaces/{name}/sources/{slot}/merge       body: {"ref":"..."}
POST /workspaces/{name}/sources/{slot}/rebase      body: {"ref":"..."}
POST /workspaces/{name}/sources/{slot}/merge-abort
POST /workspaces/{name}/sources/{slot}/rebase-abort
POST /workspaces/{name}/sources/{slot}/rebase-continue
POST /workspaces/{name}/sources/{slot}/publish     body: {"remote":"...","branch":"..."}
```

The convergence endpoints (`merge`, `rebase`, `merge-abort`,
`rebase-abort`, `rebase-continue`, `publish`) return a `GitOpResult`
with `{ok, conflicted, conflictFiles, message}`. On conflict the
worktree is left in the conflicted state; `conflictFiles` lists the
affected paths.

Workspace preflight:

```http
POST /workspaces/preflight                          body: WorkspaceCreateRequest
```

Validates the request against the resolved template's input
declarations without rendering anything. Returns
`WorkspaceCreatePreflightResponse` with `ok`, `missingRequired`,
`invalidInputs`, and the effective input map.

Template introspection:

```http
GET /templates
GET /templates/{ref...}
```

`GET /templates` enumerates every template under `<root>/.templates/<kind>/<name>`
and `<root>/templates/<kind>/<name>`. `GET /templates/{ref...}` resolves a
specific ref (relative path, absolute path, or supported remote URL) and
returns a single descriptor with the input schema.

Connection tokens:

```http
POST /tokens/mint                                  body: {"actor":"...","ttl":"30m"}
```

Secrets (CRUD against the configured secrets backend):

```http
GET    /secrets                                   list declared secrets (metadata only)
GET    /secrets/{name}                            one secret's metadata
GET    /secrets/{name}/value                      privileged read: returns the value
POST   /secrets/{name}                            body: {"value":"..."}
DELETE /secrets/{name}                            remove the backend entry
```

`GET /secrets` returns only the **declared** secrets (entries in
`stack.secrets`). Set/delete/get accept any name matching
`^[A-Za-z0-9._-]{1,256}$` — declared or not — so callers can provision
values before adding the manifest declaration. The list will only show
the secret once it's declared.

Every mutating call (`POST`, `DELETE`) is logged to operator stderr with
the secret name and the request's remote address. OpenBao keeps its own
audit log on top of that; env-file deployments rely on the operator log
as the only paper trail.

Mints an HS256-signed JWT scoped to the supplied actor. TTL defaults to
1h and is capped at 24h. The signing key resolves via
`--jwt-secret` / `ANGEE_OPERATOR_JWT_SECRET` / HKDF-from-admin-bearer
/ per-process random (loopback dev only). The endpoint itself is gated
by the admin bearer.

MCP descriptor:

```http
GET /mcp
```

`/mcp` currently returns a static descriptor; it is not a JSON-RPC MCP server.
Live event streaming is GraphQL subscriptions on `/graphql` (see below) —
SSE has no REST equivalent today.

## GraphQL

GraphQL is available at:

```http
POST /graphql
Content-Type: application/json
```

Example:

```sh
curl -s http://127.0.0.1:9000/graphql \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ stackStatus { name root services { name runtime status } } }"}'
```

The GraphQL schema exposes stack, service, job, source, workspace, log snapshot,
and mutation fields corresponding to the REST operations. Workspace source types
use the same branch-identity fields as REST (`branch`, `currentRef`, `state`),
and `workspaceSyncBase(name:, method:)` mirrors the REST `sync-base` endpoint.

The schema source lives at `internal/operator/schema.graphql`; generated gqlgen
runtime files live under `internal/operator/gql/`.

### Subscriptions

The operator exposes a `Subscription` root over Server-Sent Events. The
gqlgen SSE transport dispatches on `POST /graphql` with
`Accept: text/event-stream`; the response is a `text/event-stream` body
that emits one `data:` frame per change.

Available subscription operations:

| Operation | Argument | Payload |
| --- | --- | --- |
| `onGitOpsTopologyChange` | — | `GitOpsTopology` snapshot, emitted when the polled topology hash changes. |
| `onWorkspaceStatusChange` | `name: String!` | `WorkspaceStatus` snapshot for that workspace, emitted on change. |
| `onServiceLogs` | `name: String!` | Service log lines, follow-tailed from the runtime backend. |
| `onWorkspaceLogs` | `name: String!` | Workspace log lines, follow-tailed from the runtime backend. |

Snapshot subscriptions (`onGitOpsTopologyChange`,
`onWorkspaceStatusChange`) poll their underlying query on a 2 s tick and
publish only when the result hash changes. **No initial snapshot is
emitted on connect** — issue a one-shot `gitOpsTopology` /
`workspaceStatus` query alongside the subscription if you need the
current state at startup. Log subscriptions stream directly from the
runtime backend's follow channel; cancelling the subscription tears down
the underlying `logs --follow` process.

Slow subscribers have their per-subscription buffer dropped rather than
slowing the producer — clients should treat snapshot subscriptions as
"latest known" rather than guaranteed-delivery.

Example (curl, line-buffered):

```sh
curl -N http://127.0.0.1:9000/graphql \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{"query":"subscription { onGitOpsTopologyChange { summary { sources workspaces dirty diverged } } }"}'
```

#### WebSocket transport

The same subscription root is also served over the standard
`graphql-transport-ws` protocol as a WebSocket upgrade on `GET /graphql`,
alongside SSE. Browser GraphQL clients (urql, Apollo, `graphql-ws`) ship this
transport by default; point them at the same `/graphql` URL. SSE is unchanged
and remains the right choice for curl and server-side consumers.

Authentication happens in the `connection_init` handshake, not via an
`Authorization` header (a browser cannot set headers on a WS upgrade). Put the
credential in the client's `connectionParams`, which the daemon reads as the
`Authorization` payload key and runs through the same two-tier check as the
HTTP API — the admin bearer, or a minted `aud=operator` token (see
[Connection tokens](#connection-tokens)):

```js
import { createClient } from 'graphql-ws'

const client = createClient({
  url: 'ws://127.0.0.1:9000/graphql',
  connectionParams: { Authorization: `Bearer ${operatorToken}` },
})
```

An invalid or missing token closes the socket after a `connection_error`; a
valid token receives `connection_ack` and then `next` frames. Because the
upgrade is a `GET` (which `CrossOriginProtection` treats as safe), the upgrader
enforces an `Origin` allowlist instead: loopback origins and requests with no
`Origin` header are always allowed, and additional browser origins are
permitted with the repeatable `--allowed-origin` flag. A disallowed `Origin`
is rejected at the handshake with `403`.

### Workspace preflight

`workspaceCreatePreflight(input: WorkspaceCreateInput!)` validates the
caller's inputs against the resolved template's input declarations
without touching the filesystem. The response carries the effective
inputs (defaults plus caller-provided), a `missingRequired` list, and
an `invalidInputs` list of `{field, reason}` for type-mismatch errors.
Use this from any client that builds workspace-create forms before
committing the irreversible-but-recoverable materialisation.

### Connection and route tokens

The operator mints two kinds of short-lived HS256 JWT, both returned as a
`ConnectionToken` (`{token, actor, expiresAt}`) carrying `sub=<actor>`,
`iss=angee-operator`, plus `iat`/`exp`. TTL defaults to 1 h and is capped at
24 h. They differ only in audience and scope:

| Mutation (REST) | Audience | Purpose |
| --- | --- | --- |
| `mintConnectionToken(actor: String!, scope: [String!], ttl: String)` — `POST /tokens/mint` | `operator` | An operator-API token the host backend mints (server-side, over the admin bearer) and hands to a browser instead of the admin bearer. Carries the approved capability `scope`. |
| `mintRouteToken(actor: String!, service: String!, ttl: String)` — `POST /tokens/route` | `svc:<service>` | A route token authorizing one service's socket through the edge. Carries no scope. |

The operator accepts an `aud=operator` token on its API (and on the WebSocket
transport) as an alternative to the admin bearer; a route token verifies only
against its own `svc:<service>` audience and is rejected on the operator API.
The signing key resolves in this precedence order:

1. `--jwt-secret` flag on the operator command line.
2. `ANGEE_OPERATOR_JWT_SECRET` env var.
3. HKDF-derived from the admin `--token` (one-way; leaking JWT secret
   does not reveal the admin bearer).
4. Per-process random fallback when neither secret nor admin bearer is
   set (loopback dev only — tokens won't survive an operator restart).

Minting is gated by the admin bearer — the caller (the host backend) sends
`Authorization: Bearer <admin-token>` on the mint request after its own
authorization check, then returns the minted token to the browser. The admin
bearer never leaves the server. Callers should treat the returned token as
opaque.

### Commit DAG

`gitOpsTopology(withCommits: Int)` accepts an opt-in window for
commit-DAG population. When `withCommits` is omitted or 0, `sources[].commits`
stays empty and the query path matches the cheap snapshot used by the
topology subscription. Pass a positive integer to receive that many
commits per git source, newest first by committer time, with each
`CommitRef` carrying `{sha, parents, refs, time, summary, author}`.

### Source and workspace-source diffs

`sourceDiff(name, ref)` and `workspaceSourceDiff(workspace, slot, ref)`
return `[DiffFile]` where each `DiffFile` carries `{oldPath, newPath, mode,
isBinary, isNew, isDeleted, isRename, hunks: [DiffHunk]}`. The `hunks`
list mirrors unified-diff output: `{oldStart, oldLines, newStart, newLines,
header, body}` with `body` carrying the raw `+`/`-`/` ` prefixed lines.
When `ref` is empty the diff is "working tree vs HEAD" (uncommitted
changes); when set, it is "HEAD vs ref". Only git sources are
diffable — local sources surface a typed `InvalidInputError`.

### Convergence operations

The operator exposes per-workspace-source convergence mutations beyond
fetch/pull/push. Each returns a `GitOpResult`:

```graphql
type GitOpResult {
  ok: Boolean!
  conflicted: Boolean!
  conflictFiles: [String!]!
  message: String!
}
```

Operations:

| Mutation | Behaviour |
| --- | --- |
| `workspaceSourceMerge(workspace, slot, ref)` | `git merge --no-ff --no-edit ref`. On conflict the worktree is left conflicted and `conflictFiles` lists the affected paths. |
| `workspaceSourceRebase(workspace, slot, ref)` | `git rebase ref`. Conflict semantics match merge; resolve and call `rebaseContinue`, or call `rebaseAbort`. |
| `workspaceSourceMergeAbort(workspace, slot)` | `git merge --abort`. |
| `workspaceSourceRebaseAbort(workspace, slot)` | `git rebase --abort`. |
| `workspaceSourceRebaseContinue(workspace, slot)` | `git rebase --continue` with `core.editor=true` so it never opens an editor. |
| `workspaceSourcePublish(workspace, slot, remote, branch)` | `git push --set-upstream <remote> <branch>`. `remote` defaults to `origin`; `branch` defaults to the workspace source's manifest branch. Useful for publishing a workspace branch to the remote for the first time so a PR can be opened. |

Conflict files come from `git ls-files -u`, so the list is exact and
reflects only paths the index reports as conflicted. The
`message` field carries the combined stdout + stderr from git for
diagnostic display.

### Template introspection

`templates: [TemplateDescriptor!]!` enumerates every template under
`<root>/.templates/<kind>/<name>` and `<root>/templates/<kind>/<name>`.
`template(ref: String!): TemplateDescriptor` resolves an explicit ref
(`workspaces/dev-pr`, an absolute path, or a supported remote URL) and
returns the same shape. Each descriptor carries `ref`, `kind`, `name`,
`path`, and a sorted list of `TemplateInputDescriptor`
(`name`, `type`, `required`, `immutable`, `generated`, `default`).
