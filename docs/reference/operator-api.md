# Operator API

Run the standalone operator:

```sh
angee operator --root . --bind 127.0.0.1 --port 9000
```

Non-loopback binds require `--token`. Protected endpoints use:

```http
Authorization: Bearer <token>
```

Keep-alive connections are closed after 120 seconds of idle time; streaming
responses remain unbounded while active. Git commands started by the operator
are non-interactive, so missing credentials or SSH host trust fail instead of
opening a prompt.

Surface parity between `service.Platform`, CLI, REST, and GraphQL is tracked in
[Surface parity](/operator/surfaces).

## Logging and verbosity

The operator writes structured `slog` text records to stderr. Its default
verbosity (count `0`) is `INFO`, so completed API requests and server lifecycle
events are visible without flags. Pass `--verbose` / `-v` one or more times to
enable `DEBUG`; additional repetitions such as `-vv` stay at `DEBUG`.
`ANGEE_VERBOSE=0|1|2` provides the count when the flag is absent, and an
explicit flag value takes precedence over the environment.

Every request receives an eight-character hexadecimal request ID, or keeps
the `X-Request-ID` it sent (see [Errors](#errors)); the response returns it in
`X-Request-ID`. A completed request is one line in this form (timestamps and
durations vary):

```text
time=2026-09-04T12:34:56.789Z level=INFO msg=request req=7f3a2c19 method=POST path=/stack/prepare status=200 duration=812.4ms remote=127.0.0.1:54321
```

The request-scoped logger is stored in the request context, so logs emitted by
`service.Platform` during the call carry the same `req` value. At `DEBUG`, an
additional `request started` record makes long-running streams and WebSocket
connections visible before their completion line, and successful bearer
authentication records the resolved actor or admin scope. `/healthz`
completion records are `DEBUG` only so polling does not fill normal logs.

Request records include only the HTTP method and URL path. The operator never
logs URL query strings (which can contain connection tokens), `Authorization`
headers, or request bodies. Secret audit records contain secret names but never
secret values.

## Errors

Every failed operation is reported in one shape. GraphQL puts it in
`errors[].extensions`; REST error bodies carry the same keys beside `error`
(the message):

| Key | When | Meaning |
|---|---|---|
| `code` | always | The category, from the table below. |
| `cause` | when known | What went wrong, such as `uncommitted_changes`. |
| `operation` | when known | The verb that failed, such as `workspace.sync-base`. |
| `workspace`, `slot`, `source`, `job`, `service` | when they apply | The objects it concerns, including the one a `NOT_FOUND` names. |
| `remote` | git failures | The remote URL, with credentials masked. |
| `paths` | when they apply | Files or directories, such as a slot's uncommitted files (up to 100). |
| `hint` | when angee knows the remedy | One sentence saying what to do. |
| `detail` | git failures | git's output, redacted, at most the last 4 KiB. |
| `request_id` | always | The request's ID (see [Logging and verbosity](#logging-and-verbosity)). |

| `code` | Meaning | REST status |
|---|---|---|
| `NOT_FOUND` | The named object is not declared. | `404` |
| `INVALID_INPUT` | The request is malformed or names an invalid value, including a template answer copier refuses (a validator, a choice, a missing required input). | `400` |
| `CONFLICT` | Another mutation is running, or the object already exists. | `409` |
| `PRECONDITION_FAILED` | The target's state must change first. | `409` |
| `GIT_FAILED` | A git or jj command failed. | `500` |
| `TIMEOUT` | The operation ran out of time (`ANGEE_GIT_TIMEOUT`, `ANGEE_JOB_TIMEOUT`). | `500` |
| `JOB_FAILED` | A job run failed. | `500` |
| `INTERNAL` | Anything else. | `500` |

Failures outside the client's control answer `500`, rather than a gateway
status (`502`, `504`) that a proxy in front of the operator might retry or
replace. A REST request rejected for its credentials answers `401` with code
`UNAUTHORIZED`; it never reaches an operation. A GraphQL query that gqlgen
rejects itself (a parse or validation error) keeps gqlgen's code, such as
`GRAPHQL_VALIDATION_FAILED`.

Causes come from angee's own state and from what a step was doing. angee
never derives one by reading a tool's output: git, ssh and the remote own
their diagnostics, and their words are passed through.

| `cause` | `code` | Meaning |
|---|---|---|
| `uncommitted_changes` | `PRECONDITION_FAILED` | A checkout has uncommitted changes; `paths` lists them. |
| `slot_missing` | `PRECONDITION_FAILED` | A declared slot is not on disk; the hint names `angee workspace repair`. |
| `branch_mismatch` | `PRECONDITION_FAILED` | A slot is not on its workspace branch. |
| `detached_head` | `PRECONDITION_FAILED` | A slot with commits of its own has no branch to push. |
| `jj_unavailable` | `PRECONDITION_FAILED` | A workspace with a jj slot needs jj with `workspace add --colocate`, git 2.48 or later, and not Windows, to create one, sync it or rebase it; the hint says what to install. |
| `jj_slot_unsupported` | `INVALID_INPUT` | A slot verb angee does not run on a jj slot yet (fetch, pull, push, publish, merge, diff), or `workspace push` over a workspace with a slot whose manifest names mode `jj`; `slot` or `paths` name the jj slots. Use jj in the slot. |
| `shared_commits` | `PRECONDITION_FAILED` | A rebase of a jj slot (sync-base, or the slot's rebase) would rewrite commits another checkout holds: another jj workspace's working-copy commit, or a git worktree's checked-out branch; the message names them, and `paths` the worktrees. Nothing is rebased. |
| `immutable_commits` | `PRECONDITION_FAILED` | A rebase of a jj slot would rewrite commits jj holds immutable (a tag's, an untracked remote bookmark's); the message names them. Nothing is rebased. |
| `source_cache_missing` | `PRECONDITION_FAILED` | A source has no cache to push from. |
| `stack_root_exists` | `CONFLICT` | `stack init` would overwrite a non-empty root; `paths` names it. |
| `template_too_new` | `PRECONDITION_FAILED` | The template's `_min_copier_version` is newer than the Copier angee implements. |
| `merge_conflict` | `GIT_FAILED` | A merge or rebase stopped on conflicts (from git's index); `paths` lists them. |
| `fetch_failed`, `push_failed`, `clone_failed` | `GIT_FAILED` or `TIMEOUT` | That git action failed. |
| `git_unavailable` | `GIT_FAILED` | git is not installed. |
| `dependency_failed` | `JOB_FAILED` | A job run's dependent failed or was blocked. |

Messages read `<operation> for <target>: <what failed>. <remedy>`. A git
failure ends with git's own words: its first `fatal:` or `error:` line, with
the line before it, where git relays what ssh or the remote said. A jj
failure ends with ` jj: ` and jj's first `Error:` line, with jj's output in
`detail`. A not-found, conflict or invalid-input error keeps its own message. For
example, sync-base on a workspace whose slot has uncommitted changes:

```json
{
  "message": "Sync base for workspace \"src\": source slot \"django\" has uncommitted changes in addons/angee/operator/web/schema/operator.graphql. Commit or restore the changes, then retry.",
  "path": ["workspaceSyncBase"],
  "extensions": {
    "code": "PRECONDITION_FAILED",
    "cause": "uncommitted_changes",
    "operation": "workspace.sync-base",
    "workspace": "src",
    "slot": "django",
    "source": "angee-django",
    "paths": ["addons/angee/operator/web/schema/operator.graphql"],
    "hint": "Commit or restore the changes, then retry.",
    "request_id": "7f3a2c19"
  }
}
```

and a fetch whose SSH remote the operator cannot reach (`code` `GIT_FAILED`,
`cause` `fetch_failed`, git's whole output in `detail`):

```text
Sync base for workspace "src": fetching source "angee-arp" (git@github-arpee:ang-ee/angee-arp.git) failed. git: git@github.com: Permission denied (publickey). fatal: Could not read from remote repository.
```

Workspace and source verbs check angee's own state before running git, so a
refusal leaves nothing half done. `workspace sync-base` and `workspace push`
check every slot before touching any: each must be on disk and, as its
version control needs, on its branch with no uncommitted changes (a git
worktree), and (for push) have a branch to push. A jj slot's working-copy
commit rides along with its rebase. `workspace sync-base` also fetches every
slot, resolves its base and checks what each rebase would rewrite before
syncing any.

A failed job run's receipt (`GET /job-runs/{id}`, GraphQL `jobRun`) carries
`error_code` (`errorCode`) and `error_cause` (`errorCause`) beside `error`.
When a dependent fails, `error` names it and why, such as
`service "frontend" failed: apply service frontend: ...`.

Every response carries an `X-Request-ID` header. A request that sends a short
`X-Request-ID` (letters, digits, `.`, `_`, `-`; up to 64 characters) keeps it,
so a client can correlate its own logs. Each failed operation is logged, with
that ID, at `WARN` for `NOT_FOUND`, `INVALID_INPUT`, `CONFLICT` and
`PRECONDITION_FAILED`, and at `ERROR` otherwise. A failed GraphQL operation
still answers HTTP `200`, so its record is the only trace in the log:

```text
level=WARN msg="graphql operation failed" req=7f3a2c19 path=workspaceSyncBase code=PRECONDITION_FAILED cause=uncommitted_changes operation=workspace.sync-base error="Sync base for workspace \"src\": source slot \"django\" has uncommitted changes in addons/angee/operator/web/schema/operator.graphql. Commit or restore the changes, then retry."
```

## Running in a container

The `ghcr.io/ang-ee/angee-operator` image runs the operator as the owner of
the mounted stack root, so git checkouts and compose files on the bind mount
stay owned by the host user. Its entrypoint gives that uid a user and adds
it to the docker socket's group, then drops privileges to it. When the root
is owned by root, the operator stays root. The image includes git, the
OpenSSH client, and the Docker CLI with the compose plugin.

The user's home is the OS's: the `HOME` the container is started with, or
`adduser`'s default `/home/<name>` when none is given (Docker's `/root` for
the entrypoint counts as none).

The platform can do the same itself, and then the entrypoint has nothing to
do: run the service as the deploy user, give it the socket's group, and
mount the host's user database, from which Docker takes `HOME`:

```yaml
services:
  operator:
    user: "1000:1000"
    group_add: ["<docker socket gid>"]
    volumes:
      - /etc/passwd:/etc/passwd:ro
      - /etc/group:/etc/group:ro
```

### Git over SSH

SSH belongs to the platform: keys, `known_hosts`, `Host` aliases and agents
are set up and maintained by whoever runs the operator, as they are for the
deploy user's own git. The image ships the OpenSSH client, and git runs it
non-interactively (`BatchMode=yes`) from the operator's home. Give the
container the deploy user's home and mount its `.ssh` there, read-only:

```yaml
services:
  operator:
    image: ghcr.io/ang-ee/angee-operator:0.19.0
    environment:
      HOME: /home/deploy
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /srv/angee:/srv/angee
      - /home/deploy/.ssh:/home/deploy/.ssh:ro
```

When the deploy user owns the stack root, the operator runs with the same
uid and home path, so git over SSH behaves in the container as it does for
that user on the host. Write `HOME` as a literal path: compose run by the
operator interpolates `${HOME}` from the operator's own environment.

## REST

Health:

```http
GET /healthz
```

Stack:

```http
GET  /stack/status
GET  /stack/template-inputs
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
GET   /services/{name}/template-inputs
POST  /services/{name}/template/update  re-render and reconcile a template-created service
POST  /services/{name}/up        idempotent create-and-start
POST  /services/{name}/start
POST  /services/{name}/stop
POST  /services/{name}/restart
POST  /services/{name}/destroy
GET   /services/{name}/logs
GET   /services/{name}/logs/stream   live per-service log WebSocket
GET   /services/{name}/endpoint      service endpoint + log-stream descriptor
```

### Manual backups

All `/backups` routes require the configured admin bearer; minted connection
tokens are denied. Django owns plans, authorization, workflow attempts, and
completed Backup/File records. The operator captures declared stack resources.

```text
POST /backups/preview
POST /backups
GET  /backups/{id}/archive
```

Preview/start bodies are decoded strictly, unlike other REST bodies: an unknown
field (such as a misspelled group) or anything after the one JSON object answers
`400` with code `INVALID_INPUT` before discovery or capture starts, so a typo
never silently captures less.

Preview/start requests have `version: 1`, a `selection` object, and start also
requires `request_id` (1–128 letters/digits/underscores/hyphens). Selection groups
are `databases`, `service_state`, `workspaces`, `sources`, `storage`; every group
uses `mode: all|selected|none`, `names`, `exclude`. Sources add
`policy: bundle|fetch_published`. Source names are `stack:<source>` or
`workspace:<workspace>/<slot>`; workspaces and their sources are independent.
Preview reports included/excluded/unsupported resources, estimated source bytes,
and unknown sizes explicitly. A selected unsupported resource prevents start.

Backup preview uses bounded quick discovery: incomplete resource sizes remain
`null`, included resources remain visible, and `has_unknown_sizes` is true.
The total sums only known sizes; capture performs complete validation.

Start returns HTTP 202 with the existing `JobRunOperation` receipt; poll
`GET /job-runs/{id}` or GraphQL `jobRunOperation(id:)`. Its `root_job` is
`backup` and its nodes, `capture` and `verify`, have kind `backup` (GraphQL
`BACKUP`). Like the `/backups` routes, a backup receipt answers only the admin
bearer: its artifact names every archived path and the digest of the captured
secrets, so to a minted token both routes answer as for an unknown ID (404,
`NOT_FOUND`). A backup never becomes `GET /job-runs/latest` (`latestJobRun`),
which stays the latest job run. Equal IDs and selections are idempotent across restarts;
changed selections conflict. Interrupted receipts fail rather than restarting
an unknown partial operation. Only success includes `artifact` with `path`,
`size_bytes`, `sha256`, `manifest_version: 1`, and immutable `manifest` evidence.
Publication is under `backups/archives/<id>.zip`; staging, runtime directories,
and unsafe physical database persists are excluded through containing trees.

An interrupted backup's receipt fails when it is next read once the operator
has restarted. That read also removes the backup's staging directory
(`state/backups/<id>`) and any `backups/archives/<id>.zip` it published
without a completed receipt; files it cannot remove, such as a directory a
root container capture created, are named in the receipt's `error` for an
operator to remove. Nothing else deletes archives or receipts yet:
`backups/archives/*.zip` and `state/job-runs/*.json` stay until the caller
removes them, so retention is the caller's.

A running backup holds the operator's mutation lease and `run/operator.lock`
until its receipt is final, so every other stack mutation (job runs, restarts,
applies, file, secret, source and workspace changes, another backup) answers
`409` (`CONFLICT`) until it ends. A backup cannot be cancelled through the API.
`ANGEE_JOB_TIMEOUT` (a Go duration, `30m` by default, `0` for none) bounds the
whole backup, capture, archive and verification together; a backup that runs
out of it fails.

A backup receipt's `output` keeps every capture's output, each line labelled
`<group>/<name> | ` after its capture, as the last whole lines within 1 MiB.
Capture output and errors lose their terminal control sequences (CSI and OSC,
such as colours and the window title process-compose sets to the host name).
Like every job run's receipt output and errors, they are redacted: registered
secret values are masked. A multi-line secret is masked whole, and each of its
lines on its own when that line can carry secret material: a line that is not
PEM armour (`-----BEGIN …-----`, `-----END …-----`) and either holds at least
16 consecutive base64, base64url or hex characters, as key material does, or
has a value (the line less a leading `"key":` or `key:`) of at least 8 bytes
with a character other than a letter, a space, `_`, `-` or `.`, as a password
does. Its other lines, such as `"enabled": true` or `kind: Config`, are masked
only as part of the whole value.

A file deleted between its directory's listing and its copy is not part of a
live capture and is left out, except in repository metadata, where a deletion
(such as a repack removing the pack it replaced) fails the backup.

Container capture jobs write their output into the backup's staging
directory, which the operator creates with mode `0700`. On Linux a container
capture must therefore run as root or as the operator's uid.

A job that declares `capture:` runs only inside a backup, which chooses its
output directory. `POST /jobs/{name}/run`, `GET /jobs/{name}/run-preview` and
GraphQL `jobRun` and `jobRunPreview` refuse it, with or without inputs, with code
`INVALID_INPUT` (`400`) naming the `job`, and a chained restart never runs one
as a dependent.

The [capture/format contract](../proposals/operator-backup-restore.md) documents
trusted native job declarations, per-entry verification, structured Git/JJ
recovery identity, original encryption-key preservation, and the per-database
consistency boundary. ZIP commands are never executable restore input.

### Per-service live log socket

`GET /services/{name}/logs/stream` is a WebSocket that streams a single
service's logs live, one JSON `LogLine` (`service`, `runtime`, `message`,
optional `level`) per frame, as the runtime produces them. Because the socket
is scoped to one service the attribution is exact and no log prefix is parsed.

Pass `?tail=<n>` (alias `?n=<n>`) to replay the last `n` lines that are still
available before the live follow begins (maps to the backends' `--tail`); `0`
or omitted replays the last 200 lines. A value that is not a number is a `400`.

`GET /stack/logs` and `GET /services/{name}/logs` take `?tail=<n>`, and the
GraphQL `stackLogs`, `serviceLogs` and `onServiceLogs` take `tail: Int`: the
lines from the end of each service's or job's log, `0` or omitted for the last
200, at most 10000 (a larger value is capped).

`stackLogs` and `serviceLogs` also take `limit: Int`, a byte limit on the whole
read (`0`, omitted or above 1 MiB: 1 MiB). It applies once the lines are
labelled, and keeps the last whole lines in read order. That order is not time:
container lines come first, as docker compose orders them, then local lines
grouped by service in the order the services were requested (by name when none
is named). So a read of `[worker, api]` with a small limit keeps `api`'s last
lines and can leave `worker` out. A read the limit shortens starts with a
`[truncated]` line, which counts toward the limit. A limit of 12 bytes or less
has room only for that line, so a read it shortens is the marker alone, longer
than a limit below 12. The REST log endpoints take no byte limit.

A local service or job is read from the process-compose supervisor, which holds
its last 1000 lines; its full output is in `run/logs/<name>.log` under the stack
root. A read while process-compose replaces a service with its updated
definition is `CONFLICT`; retry it once the service has restarted. A follow of one (`onServiceLogs`, the log socket) is refused a tail above
256 with `INVALID_INPUT`, naming that file: process-compose drops the newest
lines of a longer replay. The socket refuses it before the upgrade. A local
service the running supervisor has not loaded, such as one declared after it
started, is `NOT_FOUND` at once.

Like the `GET /graphql` upgrade it carries no `Authorization` header, so auth
runs in the handshake: present a per-service route token (`aud=svc:<name>`, see
[Connection and route tokens](#connection-and-route-tokens)) or the admin
bearer / a minted `aud=operator` token, via `?token=`, the
`Sec-WebSocket-Protocol` header, or `Authorization`. The upgrader enforces the
same `Origin` allowlist as the GraphQL WebSocket. The stream is opened before
the upgrade, so an unknown service or an unconfigured production backend
surfaces as a plain HTTP error rather than a socket close; closing the socket
(or any read error) tears down the upstream follow.

The operator routes each service's socket to a backend it owns — in
development an **ephemeral** live proxy (no persistence); in production a
configured durable backend (stubbed today). `GET /services/{name}/endpoint`
advertises the resolved socket in a `log_stream` descriptor so a consumer can
connect without guessing the URL or minting its own token:

```json
{
  "routed": false,
  "internal_host": "web",
  "log_stream": {
    "url": "ws://127.0.0.1:9000/services/web/logs/stream",
    "target": "operator",
    "protocol": "ws",
    "token": "<short-lived route token>",
    "expires_at": "2026-06-17T12:00:00Z"
  }
}
```

`target` is informational (`operator` | `edge` | `production`); connecting is
identical regardless. In open dev (no configured operator token) the descriptor
omits the credential.

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
`<root>/services/<service_name>/`, and allocates ports from
declared pools under owner `service/<name>/<pool>`.

`POST /services/{name}/template/update` accepts:

```json
{
  "inputs": {"auth_mode": "oauth"},
  "dry_run": false,
  "overwrite": false
}
```

It returns the resulting declared service state plus ordered `changes` and
`conflicts`. Dry runs report conflicts in the result without mutating files or
returning an apply error. The same operation is available in GraphQL as
`serviceUpdateFromTemplate(name:, input:)`. Without `overwrite`, any locally
modified rendered asset or conflicting service field fails before mutation.

Jobs:

```http
GET  /jobs
GET  /jobs/{name}/run-preview?chained_restart=true
POST /jobs/{name}/run
GET  /job-runs/{id}
GET  /job-runs/latest
```

`GET /jobs` returns each job's latest run as the runtime holds it: `status` is
`never-run`, `pending`, `running`, `completed`, `failed`, `skipped` (a dependency
failed) or `unknown`, with `exit_code` for a finished run and `reason` for a
skipped job, a command that could not start, or a pending job waiting on a
service the runtime keeps restarting. `GET /services` adds a `reason` to a
skipped or such a pending service, `outcome` `restarting` and `restarts` (local
services only) to one its restart policy is restarting, and `GET /stack/status`
counts both under `summary` (`total`, `running`, `completed`, `failed`,
`restarting`, `skipped` for services and for jobs). GraphQL exposes the same fields on `JobState`, `ServiceState` and
`StackStatus.summary`, and `jobs(where:, order_by:)` accept `status`.
`GET /stack/logs?service=` also takes a job name.

`POST /jobs/{name}/run` accepts an optional JSON body containing `inputs` and
`chained_restart`. It returns `202 Accepted` with a job-run receipt immediately;
the operation continues under the operator daemon after the initiating HTTP
request disconnects. Poll `GET /job-runs/{id}` for its operation status, current
step, per-node outcomes, terminal root-job output, and error. The `latest` route
returns the most recently started job run's receipt, never a backup's, or JSON
`null` when this daemon has not started one. A backup capture job is refused
with `INVALID_INPUT` (see [Manual backups](#manual-backups)).

The preview route returns the jobs and services affected by the requested run
without starting it. With `chained_restart`, a successful root job is followed by
its downstream jobs and services in dependency order; failed prerequisites block
their descendants.

Receipts are retained in bounded daemon memory so clients can reconnect and keep
polling. Job-run receipts are not written to disk: restarting or stopping the
operator loses the retained receipts, and stopping the daemon cancels an active
operation. Backup receipts are also written under `state/job-runs/`, so
`GET /job-runs/{id}` keeps answering for them, to the admin bearer only (see
Manual backups).

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
GET   /workspaces/{name}/template-inputs
PATCH /workspaces/{name}
GET   /workspaces/{name}/status
GET   /workspaces/{name}/logs
POST  /workspaces/{name}/destroy?purge=true
GET   /workspaces/{name}/git
POST  /workspaces/{name}/push
POST  /workspaces/{name}/sync-base
POST  /workspaces/{name}/repair
```

Workspaces are a pure file primitive — `POST /workspaces` renders Copier
output and materializes sources, but the operator never starts services
on a workspace's behalf, and a workspace renders no stack: a workspace
template that declares `chain` or `chain_root` is refused with
`INVALID_INPUT` ([#110](https://github.com/ang-ee/angee-operator/issues/110)),
and so is `PATCH /workspaces/{name}` of a workspace created from such
a template before then. A stack's services and jobs reach a workspace
through `workspace://` mounts, `workdir` and env.

Deprecated, removed in a later release: `GET /workspaces/{name}/logs` answers
an empty body, since a workspace has no logs of its own. Workspace status
always sends `"chain": []` and omits `chain_root`, `inner_stack` and
`inner_error`. The operator no longer reads a stack that sits inside a
workspace's directory; that stack's own operator reports it.

Workspace status is the authoritative branch-identity surface for managed git
worktrees. Each status source includes the manifest `branch`, actual
`current_ref`, and `state`; `state: "branch-mismatch"` means the worktree is not
on its manifest branch, and the workspace top-level state is `discrepancy`.
`sync-base` updates each workspace branch from its base ref without switching
branches; body: `{"method":"merge"}`, `{"method":"rebase"}`, or no method
(`{}`), which syncs each slot its own way: a git worktree by merging, a jj
slot by rebasing. It acts on the slots whose manifest names a mode that syncs
(`worktree`, `jj`) and skips a slot that names none, even a jj slot under
`vcs: jj`. `merge` is refused with `INVALID_INPUT`, naming the slot, for a
workspace with a slot whose manifest names mode `jj`, before any slot is
read. A jj slot's conflict is a
result, not an error: the slot's entry in the answer carries `conflicted` and
`conflict_paths` (`conflictPaths` in GraphQL), read from its status, and the
slots after it are synced too. A git slot that stops on conflicts still fails the sync with
`merge_conflict`. A rebase rewrites the slot's commits, so a slot published
before it is force-updated by the next publish.

A jj slot (`mode: jj`, or no mode under `vcs: jj`) is never
`branch-mismatch`, and its status carries what jj reads beyond git: in
workspace status `sources[]`, `repair`'s slot status, the topology's
`links[]` and `workspace git`'s source states. The per-slot verbs, `push`
and `sync-base` return the same fields, but refuse a jj slot until phase 8 of
the jj plan runs them there. The state vocabulary is unchanged; `conflicted`
is a flag beside it, since jj records a conflict in a commit and goes on, so
a client that does not know the fields reads the slot as before. For any
other slot `conflicted` is `false` and the rest are absent (GraphQL: empty,
`0` or `null`).

| JSON | GraphQL | Meaning |
| --- | --- | --- |
| `conflicted` | `conflicted: Boolean!` | A conflicted commit among the slot's own (its base to its working copy). Always present; `false` too for a slot whose base does not resolve (state `error`), whose conflicts are not read. |
| `conflict_paths` | `conflictPaths` | Those commits' conflicted files, sorted. |
| `change_id` | `changeId` | The change id of the slot's working-copy commit. |
| `base_behind` | `baseBehind` | The commits of the slot's base it lacks; `behind` counts against its upstream when it has one, so a moved base shows here. |
| `untracked_paths` | `untrackedPaths` | New files jj leaves out of the working-copy commit (over its `snapshot.max-new-file-size`, or outside its `snapshot.auto-track`), as jj lists them, a directory ending in `/`. Only the slot holds them, so the slot is `dirty`, and `unpushed_reason` names up to ten. |

```json
{"slot": "work", "mode": "jj", "state": "ahead", "ahead": 1, "pushed": false,
 "change_id": "kmvwzlqo...", "conflicted": true, "conflict_paths": ["f.txt"], "...": "..."}
```

A source slot declared in the manifest but not materialized (one added to an
existing workspace, a dangling link, or for a git slot a directory that is not
its own checkout) makes `sync-base`, `push` and the per-slot routes under
`/workspaces/{name}/sources/{slot}/` fail with `409` before any slot is
touched, with code `PRECONDITION_FAILED` and cause `slot_missing`. The error
names the slot and points at `repair`; status reports a missing slot with
`state: "missing"`.

`repair` (no body) materializes each declared slot whose path is missing (for
a git slot, also an empty directory), the way workspace create cuts it, and
leaves anything else at a slot's path untouched. A repair that ran answers
`200` with one entry per slot, even when some slots failed; `ok` is `false`
then. A repair that cannot run answers with the usual error mapping: `404` for
an undeclared workspace, `409` when the workspace directory does not exist or
is not a directory, `409` while another stack mutation is active.

```json
{
  "workspace": "feature-a",
  "path": "/srv/stack/workspaces/feature-a",
  "ok": true,
  "slots": [
    {"slot": "app", "source": "app", "path": "/srv/stack/workspaces/feature-a/app",
     "action": "ok", "reason": "clean", "status": {"slot": "app", "state": "clean", "...": "..."}},
    {"slot": "lib", "source": "lib", "path": "/srv/stack/workspaces/feature-a/lib",
     "action": "created", "reason": "worktree on branch \"feature-a\", base main", "status": {"...": "..."}}
  ]
}
```

`action` is `created`, `ok`, `needs-attention` (dirty, diverged, on the wrong
branch or unreadable, or the path holds something other than the slot; left
untouched) or `failed` (nothing the attempt created is left at the path).
`reason` is a single line. `status` is the slot's workspace source status
after the repair, present when the slot is on disk or repair tried to create
it.

### Update scopes

The operator exposes three "update" operations, distinguished by scope:

| Scope | Endpoint | Behaviour |
| --- | --- | --- |
| **Whole source** | `POST /sources/{name}/pull` | Fetch and fast-forward the cached top-level source's tracking ref. |
| **One workspace slot** | GraphQL `workspaceSourcePull(workspace, slot)` | Fast-forward a single slot's worktree from its tracking ref. The slot lives on the **workspace branch**, not the source's main branch. |
| **All slots of a workspace** | `POST /workspaces/{name}/sync-base` | Merge or rebase each slot that names a mode (worktree or jj) against its declared base ref; by default a worktree is merged and a jj slot rebased. Stays on the workspace branch — this is "stay current with `main`". |

GitOps topology (derived view across sources × workspace slots):

```http
GET /gitops/topology[?with_commits=N]
```

Returns the full topology snapshot. `with_commits` opts in to populating
each git source's recent commit history (`sources[].commits`); omit or
set to 0 to keep the response cheap.

`summary.conflicted` counts the links of conflicted jj slots, beside their
states, as `unpushed` counts beside them. `summary.worktrees` counts git
worktree slots only: a colocated jj slot is registered as a git worktree of
its cache, but it is not counted.

A source whose cache or local store is a jj repository (it has a `.jj`, as a
cache has once a jj slot was made from it) lists its jj workspaces in
`sources[].workspaces`, here and in `GET /sources` and `/sources/{name}/status`.
Each is `{name, owned, path}`. `owned` is true when the checkout at the path
this stack's manifest gives one of its jj slots of that repository is that
workspace (named `<stack>--<workspace>`, or with `--<slot>`), and `path` is
then that path. Any other workspace, the repository's own (`default`), other
stacks' slots, and a slot of this stack whose checkout is gone or cannot be
read alike, has `owned: false` and no `path` (GraphQL: an empty one): jj's
record of a workspace's path goes stale once it moves, and a stack of the
same name sharing the repository computes the same names. When one checkout
holds two such names (one workspace `jj edit`ed the other's working-copy
commit), neither is owned. A source that is not a jj repository, or an
operator that found no jj on PATH the first time it looked (it looks once per
process), omits the field and runs no jj. Listing costs one `jj workspace
list` per jj source and one `jj log` per slot of this stack that names one of
its workspaces. A listing jj cannot read is logged as a warning and omitted,
and the source's state is read from git as before.

```json
{"name": "app", "kind": "git", "state": "clean", "workspaces": [
  {"name": "default", "owned": false},
  {"name": "dev--feature-a", "owned": true, "path": "/srv/stack/workspaces/feature-a/app"},
  {"name": "staging--feature-a", "owned": false}
], "...": "..."}
```

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
affected paths. A jj slot never stops mid-operation: `rebase` records a
conflict in its commits and answers `ok` and `conflicted` together, and
`merge-abort`, `rebase-abort` and `rebase-continue` answer `400`
`INVALID_INPUT`, since there is nothing in progress to abort or continue.

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
specific ref (`stacks/`, `workspaces/`, or `services/`, or a supported remote URL) and
returns a single descriptor with the input schema.

Each descriptor contains `ref`, `kind`, `name`, `path`, and `inputs`. Inputs
follow top-level `copier.yml` question order, then metadata-only `_angee.inputs`
entries sorted by name. Each input carries `name`, `type`, `required`,
`immutable`, `generated`, `default`, and these form fields:

| REST field | Meaning |
| --- | --- |
| `order` | Zero-based question position; `-1` for metadata-only inputs. |
| `help` | Question help text. |
| `placeholder` | Hint for an empty input. |
| `secret` | Hide the value in prompts and text displays. |
| `multiselect` | The question allows multiple choices. |
| `choices` | Ordered `{ "value": "...", "label": "..." }` entries. |
| `choices_expr` | Raw Jinja expression for dynamic choices. |
| `when` | Raw condition; booleans are strings `"true"` or `"false"`. |
| `validator` | Raw Jinja validation expression. |
| `question` | True for top-level questions; false for metadata-only inputs. |

Optional empty form fields are omitted in REST JSON. Dynamic choices, `when`,
and `validator` are carried unchanged; the CLI does not evaluate their Jinja.
Absolute paths and refs containing `..` are rejected by introspection.

### Recorded template inputs

Fetch a target's template descriptor and previous render's answers before
building an update form:

```http
GET /stack/template-inputs
GET /workspaces/{name}/template-inputs
GET /services/{name}/template-inputs
```

Each returns `TemplateInputsResponse`:

```json
{
  "target": "workspace/fix-123",
  "template": {
    "ref": "workspaces/dev-pr",
    "kind": "workspace",
    "name": "dev-pr",
    "path": "/stack/templates/workspaces/dev-pr",
    "inputs": []
  },
  "recorded": {"project_name": "notes"},
  "unrecorded": ["api_token"]
}
```

`target` is `stack`, `workspace/<name>`, or `service/<name>`; `template` has
the full descriptor shape described above. `recorded` excludes `_` metadata
keys. Stack and service answers use the same origin and answers-file
resolution as their template update operations. Workspace answers combine
stack `workspace_defaults` for the template with the workspace's persisted
`inputs`, which take precedence. `unrecorded` lists secret questions with no
recorded value and is omitted from REST JSON when empty.

Template updates reject changes to immutable inputs with an invalid-input
error naming the input: `template input <key> is immutable; it was rendered
as "<old>"`. Secret inputs instead report `template input <key> is immutable;
it cannot change after the first render` without including the value.

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

Files (scoped read/write inside a stack source):

```http
GET /files?source=<name>&path=<rel>               read one file
PUT /files?source=<name>&path=<rel>               body: {"content":"...","etag":"..."}
```

`source` and `path` are query parameters (a file path holds slashes).
`GET /files` returns `{ path, source, content, etag }`. `PUT /files` writes
the body's `content` and returns metadata only (`{ path, source, etag }`);
an optional `etag` is a compare-and-set precondition — a stale value is a
`409 Conflict`. Paths are confined to the source root (traversal and
symlink-escape are rejected) and content is UTF-8 text within a 1 MiB cap.
Both routes sit behind the admin-bearer/`operator`-token gate; writes are
logged to operator stderr with the source, path, and remote address.

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
`workspaceRepair(name:)` mirrors REST `repair` and returns a
`WorkspaceRepairResult { workspace path ok slots { slot source path action
reason status { ... } } }`. When some slots failed, the response carries the
result as `data` and also an entry in `errors` naming the failed slots.

The schema source lives at `internal/operator/schema.graphql`; generated gqlgen
runtime files live under `internal/operator/gql/`.

### Files

Read and write files inside a stack source, mirroring the REST `/files`
routes:

```graphql
query   { file(source: "app", path: "settings.yaml") { path source content etag } }
mutation { fileWrite(source: "app", path: "settings.yaml", content: "…", etag: "…") { path source etag } }
```

`file` returns the content and current `etag`; `fileWrite` writes and returns
metadata only. The optional `etag` argument is a compare-and-set precondition —
a stale value fails with a `409`-equivalent conflict error. Paths are confined
to the source root and content is UTF-8 text within a 1 MiB cap. `serviceEndpoint`
likewise exposes a `logStream { url target protocol token expiresAt }` descriptor
mirroring the REST endpoint's `log_stream` field.

### Subscriptions

The operator exposes a `Subscription` root over Server-Sent Events. The
gqlgen SSE transport dispatches on `POST /graphql` with
`Accept: text/event-stream`; the response is a `text/event-stream` body
that emits one `data:` frame per change.

Available subscription operations:

| Operation | Argument | Payload |
| --- | --- | --- |
| `onStackSnapshotChange` | — | `StackSnapshot` aggregate (health, stackStatus, services, jobs, sources, workspaces, templates, secrets, gitOpsTopology), emitted when the aggregate hash changes. |
| `onGitOpsTopologyChange` | — | `GitOpsTopology` snapshot, emitted when the polled topology hash changes. |
| `onWorkspaceStatusChange` | `name: String!` | `WorkspaceStatus` snapshot for that workspace, emitted on change. |
| `onServiceLogs` | `name: String!` | Service log lines, follow-tailed from the runtime backend. |
| `onWorkspaceLogs` | `name: String!` | Deprecated: a workspace has no logs, so the stream ends at once. |

`onStackSnapshotChange` is the aggregate the web console reads as one — it
fans out the same Query-root reads (`stackStatus`, `services`, `jobs`,
`sources`, `workspaces`, `templates`, `secrets`, `gitOpsTopology`) on a single
daemon-side poller, so one server poll replaces a per-browser-tab refetch loop.
`secrets` carries `SecretRef` metadata only — secret values are never part of
the snapshot and remain behind the separately-gated `secretValue` query.

Snapshot subscriptions (`onStackSnapshotChange`, `onGitOpsTopologyChange`,
`onWorkspaceStatusChange`) poll their underlying query on a 2 s tick and
publish only when the result hash changes. **No initial snapshot is
emitted on connect** — issue a one-shot snapshot query (the aggregate fields,
`gitOpsTopology`, or `workspaceStatus`) alongside the subscription if you need
the current state at startup. Log subscriptions stream directly from the
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

### Ingress

When a stack sets `ingress.type: caddy` (see the
[Edge Ingress guide](../guide/ingress.md) for the full picture, or the
[manifest reference](../guide/manifest.md#ingress) for the fields), routed
services are reached through one Caddy edge instead of host-published ports. Two
queries expose the routing, replacing host-side compose-port-scraping:

- `serviceEndpoint(name: String!): ServiceEndpoint` — returns
  `{routed, url, internalHost, internalPort}`. `routed` is `false` when
  `ingress.type` is `none`; when routed, `url` is the public `wss://…/` address
  and `internalHost`/`internalPort` are the in-network Docker DNS name and port.
- `ingressStatus: IngressStatus` — returns `{type, domain, routes}` where
  `routes` is `[{service, url}]` for every routed service.

The edge authenticates each inbound connection against a non-public forward_auth
target on the operator:

- `GET /edge/verify?service=<name>` — reads the token from `?token=`,
  `Authorization: Bearer`, or `Sec-WebSocket-Protocol`, and verifies it carries
  `aud=svc:<name>` (a route token from `mintRouteToken` / `POST /tokens/route`).
  Returns **200** on success and **401** otherwise — never `101`; the edge
  performs the actual WebSocket upgrade. It is **not** behind the admin-bearer
  gate (a route token is not an operator token) and is intended to be reachable
  only from the edge network.

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
diffable — local sources fail with code `INVALID_INPUT`.

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
| `workspaceSourceRebase(workspace, slot, ref)` | `git rebase ref`. Conflict semantics match merge; resolve and call `rebaseContinue`, or call `rebaseAbort`. On a jj slot, `jj rebase -b @ --onto ref` with `ref` in jj's spelling (`main@origin`), after the same check as sync-base that it rewrites no other checkout's commits (`shared_commits`); a conflict is recorded in the commits and answered as `ok` and `conflicted`. |
| `workspaceSourceMergeAbort(workspace, slot)` | `git merge --abort`. |
| `workspaceSourceRebaseAbort(workspace, slot)` | `git rebase --abort`. |
| `workspaceSourceRebaseContinue(workspace, slot)` | `git rebase --continue` with `core.editor=true` so it never opens an editor. |
| `workspaceSourcePublish(workspace, slot, remote, branch)` | `git push --set-upstream <remote> <branch>`. `remote` defaults to the source's `push_default`, else the remote git pushes the branch to, else `origin`, and a remote the slot doesn't have is refused, as is a branch that matches none of that remote's `branches`; `branch` defaults to the workspace source's manifest branch, then the current branch. Useful for publishing a workspace branch to the remote for the first time so a PR can be opened. For a git source: without `branch`, a source with no upstream and no commits beyond its base returns `ok` with a `nothing to publish` message and pushes nothing; on a detached HEAD, `branch` is required and is created at the current commit before the push (an existing local branch of that name is refused). |

Conflict files come from `git ls-files -u`, so the list is exact and
reflects only paths the index reports as conflicted. The
`message` field carries the combined stdout + stderr from git for
diagnostic display. On a jj slot, the files are those of the conflicted
commits the slot holds beyond `ref`, as jj records them, and `message` is
jj's own account of the rebase.

### Template introspection

`templates: [TemplateDescriptor!]!` enumerates every template under
`<root>/.templates/<kind>/<name>` and `<root>/templates/<kind>/<name>`.
`templates_by_pk(id: String!): TemplateDescriptor` resolves an explicit ref
(`workspaces/dev-pr`, `stacks/dev`, `services/worker`, or a supported remote URL) and
returns the same shape. Each descriptor carries `ref`, `kind`, `name`,
`path`, and an ordered list of `TemplateInputDescriptor`. Inputs carry `name`,
`type`, `required`, `immutable`, `generated`, `default`, `order`, `help`,
`placeholder`, `secret`, `multiselect`, `choices`, `choicesExpr`, `when`,
`validator`, and `question`, with the same meanings as the REST fields above.
`choices: [TemplateInputChoice!]!` contains `value: String!` and `label: String!`
in declaration order. GraphQL uses `choicesExpr` for REST's `choices_expr`.
Question inputs come first in file order; metadata-only entries have `order: -1`
and follow in name order. Conditions and validators remain informational.

### Recorded template inputs

The equivalent GraphQL queries are:

```graphql
type Query {
  stackTemplateInputs: TemplateInputsResponse!
  workspaceTemplateInputs(name: String!): TemplateInputsResponse!
  serviceTemplateInputs(name: String!): TemplateInputsResponse!
}

type TemplateInputsResponse {
  target: String!
  template: TemplateDescriptor!
  recorded: [KeyValue!]!
  unrecorded: [String!]!
}
```

`recorded` uses the same `{key, value}` list as workspace `inputs` and
preflight `effectiveInputs`, sorted by key. `unrecorded` is an empty list when
every secret question has a recorded value. For example:

```graphql
query {
  workspaceTemplateInputs(name: "fix-123") {
    target
    template { ref inputs { name help immutable secret } }
    recorded { key value }
    unrecorded
  }
}
```
