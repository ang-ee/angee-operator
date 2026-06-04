# Surface Matrix

This matrix classifies exported `service.Platform` methods across the local CLI,
REST operator, and GraphQL operator surfaces. `Internal` means the method is a
helper used by adapters or tests and should not be exposed directly.

| Platform method | CLI | REST | GraphQL | Omit reason |
| --- | --- | --- | --- | --- |
| `Root` | Internal | Internal | Internal | Adapter helper. |
| `LoadStack` | Internal | Internal | Internal | File-loading primitive; callers expose specific operations. |
| `EmptyStack` | Internal | Internal | Internal | Construction helper for stack init/tests. |
| `StackInit` | Yes | Yes | Yes | - |
| `StackUpdate` | Yes | Yes | Yes | - |
| `StackUpdateFromTemplate` | Yes | No | No | Local re-render of `angee.yaml` from its template behind `stack update --template`; not yet on remote surfaces. |
| `StackDestroy` | Yes | Yes | Yes | - |
| `StackPrepare` | Yes | Yes | Yes | - |
| `StackCompile` | Yes | No | No | Internal compile flow; remote surfaces use `StackPrepare`. |
| `StackStatus` | Yes | Yes | Yes | - |
| `StackBuild` | Yes | Yes | Yes | - |
| `StackUp` | Yes | Yes | Yes | - |
| `StackUpForeground` | Yes | Yes | No | Foreground stream; REST `GET /stack/up/stream` streams the combined output. |
| `StackDev` | Yes | Yes | Yes | Remote adapter calls non-foreground runtime flow. |
| `StackDevForeground` | Yes | Yes | No | Foreground stream; REST `GET /stack/dev/stream` streams the combined output. |
| `StackDown` | Yes | Yes | Yes | - |
| `StackLogs` | Yes | Yes | No | GraphQL uses bounded `StackLogsLimited`. |
| `StackLogsLimited` | No | No | Yes | GraphQL snapshot guardrail. |
| `ServiceInit` | Yes | Yes | Yes | - |
| `ServiceCreate` | Yes | Yes | Yes | Template-based: `angee service create`; REST `POST /services/create`; GraphQL `serviceCreate`. |
| `ServiceUpdate` | Yes | Yes | Yes | - |
| `ServiceDestroy` | Yes | Yes | Yes | - |
| `ServiceList` | Yes | Yes | Yes | - |
| `ServiceEndpoint` | No | Yes | Yes | REST `GET /services/{name}/endpoint`; GraphQL `serviceEndpoint` ingress route lookup. |
| `ServiceUp` | Yes | Yes | Yes | Create-and-start; idempotent across never-created services. |
| `ServiceStart` | Yes | Yes | Yes | - |
| `ServiceStop` | Yes | Yes | Yes | - |
| `ServiceRestart` | Yes | Yes | Yes | - |
| `IngressStatus` | No | Yes | Yes | REST `GET /ingress/status`; GraphQL `ingressStatus` route summary. |
| `JobList` | Yes | Yes | Yes | - |
| `JobRun` | Yes | Yes | Yes | - |
| `SourceList` | Yes | Yes | Yes | - |
| `SourceFetch` | Yes | Yes | Yes | - |
| `SourceStatus` | Yes | Yes | Yes | - |
| `SourcePull` | Yes | Yes | Yes | - |
| `SourcePush` | Yes | Yes | Yes | - |
| `WorkspaceCreate` | Yes | Yes | Yes | - |
| `WorkspaceList` | Yes | Yes | Yes | - |
| `WorkspaceGet` | Yes | Yes | Yes | - |
| `WorkspaceStatus` | Yes | Yes | Yes | - |
| `WorkspaceUpdate` | Yes | Yes | Yes | - |
| `WorkspaceDestroy` | Yes | Yes | Yes | - |
| `WorkspaceLogs` | Yes | Yes | No | GraphQL uses bounded `WorkspaceLogsLimited`. |
| `WorkspaceLogsLimited` | No | No | Yes | GraphQL snapshot guardrail. |
| `WorkspaceGitStatus` | Yes | Yes | Yes | - |
| `WorkspacePush` | Yes | Yes | Yes | - |
| `WorkspaceSyncBase` | Yes | Yes | Yes | - |
| `GitOpsTopology` | Yes | Yes | Yes | `angee gitops topology`; REST `GET /gitops/topology`. |
| `GitOpsTopologyWithCommits` | Yes | Yes | Yes | `angee gitops topology --with-commits N`; REST `GET /gitops/topology?with_commits=N`. |
| `SourceDiff` | Yes | Yes | Yes | `angee source diff <name>`; REST `GET /sources/{name}/diff?ref=...`. |
| `WorkspaceSourceDiff` | Yes | Yes | Yes | `angee workspace source diff <ws> <slot>`; REST `GET /workspaces/{name}/sources/{slot}/diff?ref=...`. |
| `WorkspaceSourceFetch` | Yes | Yes | Yes | `angee workspace source fetch`; REST `POST /workspaces/{name}/sources/{slot}/fetch`. |
| `WorkspaceSourcePull` | Yes | Yes | Yes | `angee workspace source pull`; REST `POST /workspaces/{name}/sources/{slot}/pull`. |
| `WorkspaceSourcePush` | Yes | Yes | Yes | `angee workspace source push`; REST `POST /workspaces/{name}/sources/{slot}/push`. |
| `WorkspaceSourceMerge` | Yes | Yes | Yes | `angee workspace source merge`; REST `POST /workspaces/{name}/sources/{slot}/merge`. |
| `WorkspaceSourceRebase` | Yes | Yes | Yes | `angee workspace source rebase`; REST `POST /workspaces/{name}/sources/{slot}/rebase`. |
| `WorkspaceSourceMergeAbort` | Yes | Yes | Yes | `angee workspace source merge-abort`; REST `POST /workspaces/{name}/sources/{slot}/merge-abort`. |
| `WorkspaceSourceRebaseAbort` | Yes | Yes | Yes | `angee workspace source rebase-abort`; REST `POST /workspaces/{name}/sources/{slot}/rebase-abort`. |
| `WorkspaceSourceRebaseContinue` | Yes | Yes | Yes | `angee workspace source rebase-continue`; REST `POST /workspaces/{name}/sources/{slot}/rebase-continue`. |
| `WorkspaceSourcePublish` | Yes | Yes | Yes | `angee workspace source publish`; REST `POST /workspaces/{name}/sources/{slot}/publish`. |
| `WorkspaceCreatePreflight` | Yes | Yes | Yes | `angee workspace preflight`; REST `POST /workspaces/preflight`. |
| `Templates` | Yes | Yes | Yes | `angee template list`; REST `GET /templates`. |
| `Template` | Yes | Yes | Yes | `angee template get`; REST `GET /templates/{ref...}`. |
| `SecretsList` | Yes | Yes | Yes | CLI `angee secret list`; REST `GET /secrets`. |
| `SecretGet` | Yes | Yes | Yes | CLI `angee secret get`; REST `GET /secrets/{name}`. |
| `SecretValue` | Yes | Yes | Yes | CLI `angee secret reveal`; REST `GET /secrets/{name}/value`. Privileged value-read. |
| `SecretSet` | Yes | Yes | Yes | CLI `angee secret set`; REST `POST /secrets/{name}`. |
| `SecretDelete` | Yes | Yes | Yes | CLI `angee secret delete`; REST `DELETE /secrets/{name}`. |

When adding a new exported `Platform` method, update this table in the same
change. `internal/service/surface_matrix_test.go` verifies that every exported
method is classified here.
