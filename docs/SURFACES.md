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
| `WorkspaceStart` | Yes | Yes | Yes | - |
| `WorkspaceStop` | Yes | Yes | Yes | - |
| `WorkspaceGitStatus` | Yes | Yes | Yes | - |
| `WorkspacePush` | Yes | Yes | Yes | - |
| `WorkspaceSyncBase` | Yes | Yes | Yes | - |
| `GitOpsTopology` | No | No | Yes | Gap: currently GraphQL-only topology view. |
| `WorkspaceSourceFetch` | No | No | Yes | Gap: currently GraphQL-only per-workspace source operation. |
| `WorkspaceSourcePull` | No | No | Yes | Gap: currently GraphQL-only per-workspace source operation. |
| `WorkspaceSourcePush` | No | No | Yes | Gap: currently GraphQL-only per-workspace source operation. |

When adding a new exported `Platform` method, update this table in the same
change. `internal/service/surface_matrix_test.go` verifies that every exported
method is classified here.
