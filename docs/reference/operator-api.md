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
POST  /services
PATCH /services/{name}
POST  /services/{name}/start
POST  /services/{name}/stop
POST  /services/{name}/restart
POST  /services/{name}/destroy
GET   /services/{name}/logs
```

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

Workspaces:

```http
GET   /workspaces
POST  /workspaces
GET   /workspaces/{name}
PATCH /workspaces/{name}
GET   /workspaces/{name}/status
GET   /workspaces/{name}/logs
POST  /workspaces/{name}/start
POST  /workspaces/{name}/stop
POST  /workspaces/{name}/restart
POST  /workspaces/{name}/destroy?purge=true
GET   /workspaces/{name}/git
POST  /workspaces/{name}/push
POST  /workspaces/{name}/sync-base
```

Workspace status is the authoritative branch-identity surface for managed git
worktrees. Each status source includes the manifest `branch`, actual
`current_ref`, and `state`; `state: "branch-mismatch"` means the worktree is not
on its manifest branch, and the workspace top-level state is `discrepancy`.
`sync-base` updates each workspace branch from its base ref without switching
branches; body: `{"method":"merge"}` or `{"method":"rebase"}`.

MCP descriptor:

```http
GET /mcp
```

`/mcp` currently returns a static descriptor; it is not a JSON-RPC MCP server.
Live event streaming has moved to GraphQL subscriptions on `/graphql` (see
below).

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

### Workspace preflight

`workspaceCreatePreflight(input: WorkspaceCreateInput!)` validates the
caller's inputs against the resolved template's input declarations
without touching the filesystem. The response carries the effective
inputs (defaults plus caller-provided), a `missingRequired` list, and
an `invalidInputs` list of `{field, reason}` for type-mismatch errors.
Use this from any client that builds workspace-create forms before
committing the irreversible-but-recoverable materialisation.

### Connection tokens

`mintConnectionToken(actor: String!, ttl: String): ConnectionToken!`
returns an HS256-signed JWT carrying `sub=<actor>`, `iss=angee-operator`,
plus `iat`/`exp`. TTL defaults to 1 h and is capped at 24 h. The signing
key resolves in this precedence order:

1. `--jwt-secret` flag on the operator command line.
2. `ANGEE_OPERATOR_JWT_SECRET` env var.
3. HKDF-derived from the admin `--token` (one-way; leaking JWT secret
   does not reveal the admin bearer).
4. Per-process random fallback when neither secret nor admin bearer is
   set (loopback dev only — tokens won't survive an operator restart).

The mutation itself is gated by the admin bearer (`Authorization: Bearer
<admin-token>` on the request that mints the new token). Callers should
treat the returned token as opaque.

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

### Template introspection

`templates: [TemplateDescriptor!]!` enumerates every template under
`<root>/.templates/<kind>/<name>` and `<root>/templates/<kind>/<name>`.
`template(ref: String!): TemplateDescriptor` resolves an explicit ref
(`workspaces/dev-pr`, an absolute path, or a supported remote URL) and
returns the same shape. Each descriptor carries `ref`, `kind`, `name`,
`path`, and a sorted list of `TemplateInputDescriptor`
(`name`, `type`, `required`, `immutable`, `generated`, `default`).
