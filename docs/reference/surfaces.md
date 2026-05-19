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
| `StackTemplateQuestions` | Yes | No | No | Interactive local prompt flow. |
| `StackUpdate` | Yes | Yes | Yes | - |
| `StackDestroy` | Yes | Yes | Yes | - |
| `StackPrepare` | Yes | Yes | Yes | - |
| `StackCompile` | Yes | No | No | Internal compile flow; remote surfaces use `StackPrepare`. |
| `StackStatus` | Yes | Yes | Yes | - |
| `StackBuild` | Yes | Yes | Yes | - |
| `StackUp` | Yes | Yes | Yes | - |
| `StackUpForeground` | Yes | No | No | Local-only streaming process. |
| `StackDev` | Yes | Yes | Yes | Remote adapter calls non-foreground runtime flow. |
| `StackDevForeground` | Yes | No | No | Local-only streaming process. |
| `StackDown` | Yes | Yes | Yes | - |
| `StackLogs` | Yes | Yes | No | GraphQL uses bounded `StackLogsLimited`. |
| `StackLogsLimited` | No | No | Yes | GraphQL snapshot guardrail. |
| `ServiceInit` | Yes | Yes | Yes | - |
| `ServiceUpdate` | Yes | Yes | Yes | - |
| `ServiceDestroy` | Yes | Yes | Yes | - |
| `ServiceList` | Yes | Yes | Yes | - |
| `ServiceStart` | Yes | Yes | Yes | - |
| `ServiceStop` | Yes | Yes | Yes | - |
| `ServiceRestart` | Yes | Yes | Yes | - |
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
| `GitOpsTopology` | No | Yes | Yes | CLI gap; REST `GET /gitops/topology`. |
| `GitOpsTopologyWithCommits` | No | Yes | Yes | CLI gap; REST `GET /gitops/topology?with_commits=N`. |
| `SourceDiff` | No | Yes | Yes | CLI gap; REST `GET /sources/{name}/diff?ref=...`. |
| `WorkspaceSourceDiff` | No | Yes | Yes | CLI gap; REST `GET /workspaces/{name}/sources/{slot}/diff?ref=...`. |
| `WorkspaceSourceFetch` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/fetch`. |
| `WorkspaceSourcePull` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/pull`. |
| `WorkspaceSourcePush` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/push`. |
| `WorkspaceSourceMerge` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/merge`. |
| `WorkspaceSourceRebase` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/rebase`. |
| `WorkspaceSourceMergeAbort` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/merge-abort`. |
| `WorkspaceSourceRebaseAbort` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/rebase-abort`. |
| `WorkspaceSourceRebaseContinue` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/rebase-continue`. |
| `WorkspaceSourcePublish` | No | Yes | Yes | CLI gap; REST `POST /workspaces/{name}/sources/{slot}/publish`. |
| `WorkspaceCreatePreflight` | No | Yes | Yes | CLI gap; REST `POST /workspaces/preflight`. |
| `Templates` | No | Yes | Yes | CLI gap; REST `GET /templates`. |
| `Template` | No | Yes | Yes | CLI gap; REST `GET /templates/{ref...}`. |

When adding a new exported `Platform` method, update this table in the same
change. `internal/service/surface_matrix_test.go` verifies that every exported
method is classified here.
