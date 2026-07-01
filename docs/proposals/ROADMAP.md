# Proposal roadmap

This is the index of **open** design work in `docs/proposals/`. It has two
kinds of entry: proposals not yet started, and the remaining pieces of features
that have already partly shipped (the proposal is kept, trimmed to that
remainder, with an `## Implementation status` block at its head).

Proposals whose work has fully landed are **not** listed here — they are deleted
once their design is captured in the code, the published docs, and
`CHANGELOG.md`; git history preserves the original text.

## Open proposals (not started)

| Proposal | Gist |
|---|---|
| [global-source-registry](./global-source-registry.md) | A global source registry (`POST /sources` register-by-id, `POST /sources/{id}/destroy`) plus per-render workspace materialization via an `inputs.sources` render input. Only the pre-existing clone/cache machinery exists today. |
| [ephemeral-workspace-pool](./ephemeral-workspace-pool.md) | Leased/reaped/recycled workspaces: a `WorkspaceLease` model + `/lease` endpoints, a TTL+lease reaper sweep, and a warm pool (`operator.workspace_pool.<template>` + `acquire`). Only the pre-existing TTL/`expired` computation exists today. |
| [local-platform-instance](./local-platform-instance.md) | A central local operator that hosts workspaces and agents: a `stacks/local` platform template, containerized workspace dev stacks, per-workspace agent services, and the develop→reconcile loop. Largely cross-repo; depends on the entries below. |
| [operator-backup-restore](./operator-backup-restore.md) | Snapshot a stack's data plane: a `backup.Backend` interface + registry (localfs-tarball/restic/s3), a `manifest.BackupBackend` field, a snapshot catalog in the `store` layer, and `stack backup`/`restore`/`backup ls` + `workspace create --restore` across CLI + REST. |

## Remaining pieces of shipped features

| Proposal | Shipped | Remaining |
|---|---|---|
| [operator-file-tools](./operator-file-tools.md) | Scoped file read/write across REST/GraphQL/client/CLI on `internal/store` (v0.7.3). | Optional `GET /files/list` (directory listing) and `DELETE /files` — the `store` layer already exposes `Lister`/`Deleter`; deferred until a consumer needs them. |
| [per-service-log-streaming](./per-service-log-streaming.md) | Streaming-follow primitive, per-service WS socket, `LogStreamer` seam + ephemeral dev backend, REST `log_stream` descriptor, GraphQL `logStream` field, `--log-backend` selector. | A durable **production** log backend behind the `LogStreamer` seam (`prodStreamer` is a fail-closed stub); optional `LogLine.Ts` field. |
| [edge-ingress-caddy](./edge-ingress-caddy.md) | Caddy edge backend, route/connection token model, `/edge/verify` forward-auth, two-tier operator auth, `serviceEndpoint`/`ingressStatus` (v0.5.6/v0.5.8). | Per-field token **scope enforcement** — scope is carried but advisory; no path gates mutations by it yet. (The Django host-backend rewrite is cross-repo.) |
| [stack-update-template-sync](./stack-update-template-sync.md) | `stack update --template [--dry-run]` structural re-render from the Copier template, local CLI (v0.5.9/v0.5.12). | Full **3-way conflict detection** (locally-edited template keys should fail with a structured conflict, not template-wins); exposure on the remote REST/GraphQL surfaces. |
