package service

import (
	"context"
	"io"

	"github.com/fyltr/angee/api"
)

// API is the canonical control-plane operation surface for one stack root.
//
// It is the single contract that every adapter dispatches through: the CLI,
// the REST operator, and the GraphQL operator all hold an API rather than a
// concrete type, and both the in-process *Platform and the remote HTTP client
// (internal/platformclient.RemoteClient) implement it. Keeping the contract
// here — beside the types it speaks (api.* DTOs plus the service-local
// StackInitResult and CompiledStack) — is what keeps the three transports DRY
// and prevents the interface drift that a per-adapter copy invites.
//
// Genuinely host-local operations are deliberately NOT on API: Root,
// LoadStack, and EmptyStack return in-memory handles meaningless over the
// wire; StackUpdateFromTemplate re-renders the local template tree; and the
// CLI-host commands `doctor` and `workspace open` are not Platform methods at
// all. Callers that need those reach for the concrete *Platform.
//
// The foreground StackUpForeground/StackDevForeground methods take io.Writer
// sinks that live on the caller's side, so they cross transports cleanly: the
// local implementation writes process output to them, and the remote client
// streams the operator's chunked HTTP response into them.
type API interface {
	StackAPI
	RuntimeAPI
	ServiceAPI
	JobAPI
	WorkspaceAPI
	SourceAPI
	WorkspaceSourceAPI
	GitOpsAPI
	SecretsAPI
	IngressAPI
	TemplateAPI
}

// StackAPI covers stack lifecycle and compilation.
type StackAPI interface {
	StackInit(ctx context.Context, template, targetPath string, inputs map[string]string, force bool) (StackInitResult, error)
	StackUpdate(ctx context.Context) error
	StackDestroy(ctx context.Context, purge bool) error
	StackPrepare(ctx context.Context) (*CompiledStack, error)
	StackCompile(ctx context.Context) (*CompiledStack, error)
	StackStatus(ctx context.Context) (api.StackStatusResponse, error)
}

// RuntimeAPI covers bringing services up/down and tailing logs. The
// foreground variants stream output to the supplied writers; a transport may
// coalesce stdout and stderr into a single stream (the remote client does, as
// HTTP delivers one chunked body), so callers must not rely on the two being
// kept separate.
type RuntimeAPI interface {
	StackBuild(ctx context.Context, services []string) error
	StackUp(ctx context.Context, services []string, build bool) error
	StackUpForeground(ctx context.Context, services []string, build bool, stdout, stderr io.Writer) error
	StackDev(ctx context.Context, build bool) error
	StackDevForeground(ctx context.Context, build bool, stdout, stderr io.Writer) error
	StackDown(ctx context.Context) error
	StackLogs(ctx context.Context, services []string, follow bool) (<-chan string, error)
	StackLogsLimited(ctx context.Context, services []string, follow bool, maxBytes int) (<-chan string, error)
	ServiceUp(ctx context.Context, names []string) error
	ServiceStart(ctx context.Context, names []string) error
	ServiceStop(ctx context.Context, names []string) error
	ServiceRestart(ctx context.Context, names []string) error
}

// ServiceAPI covers service manifest CRUD and template-based creation.
type ServiceAPI interface {
	ServiceList(ctx context.Context) ([]api.ServiceState, error)
	ServiceInit(ctx context.Context, req api.ServiceInitRequest) error
	ServiceUpdate(ctx context.Context, req api.ServiceInitRequest) error
	ServiceDestroy(ctx context.Context, name string, stop bool) error
	ServiceCreate(ctx context.Context, req api.ServiceCreateRequest) (api.ServiceState, error)
}

// JobAPI covers job discovery and invocation.
type JobAPI interface {
	JobList(ctx context.Context) ([]api.JobState, error)
	JobRun(ctx context.Context, name string, inputs map[string]string) ([]byte, error)
}

// WorkspaceAPI covers workspace lifecycle, status, logs, and aggregate git ops.
type WorkspaceAPI interface {
	WorkspaceCreate(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceRef, error)
	WorkspaceList(ctx context.Context) ([]api.WorkspaceRef, error)
	WorkspaceGet(ctx context.Context, name string) (api.WorkspaceRef, error)
	WorkspaceStatus(ctx context.Context, name string) (api.WorkspaceStatusResponse, error)
	WorkspaceUpdate(ctx context.Context, name string, inputs map[string]string, ttl string) (api.WorkspaceRef, error)
	WorkspaceDestroy(ctx context.Context, name string, purge bool) error
	WorkspaceLogs(ctx context.Context, name string, follow bool) (<-chan string, error)
	WorkspaceLogsLimited(ctx context.Context, name string, follow bool, maxBytes int) (<-chan string, error)
	WorkspaceCreatePreflight(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceCreatePreflightResponse, error)
	WorkspaceGitStatus(ctx context.Context, name string) ([]api.SourceState, error)
	WorkspacePush(ctx context.Context, name, ref string) ([]api.SourceState, error)
	WorkspaceSyncBase(ctx context.Context, name, method string) ([]api.SourceState, error)
}

// SourceAPI covers top-level source materialization and git inspection.
type SourceAPI interface {
	SourceList(ctx context.Context) ([]api.SourceState, error)
	SourceStatus(ctx context.Context, name string) (api.SourceState, error)
	SourceFetch(ctx context.Context, name string) (api.SourceState, error)
	SourcePull(ctx context.Context, name string) (api.SourceState, error)
	SourcePush(ctx context.Context, name, ref string) (api.SourceState, error)
	SourceDiff(ctx context.Context, name, ref string) ([]api.DiffFile, error)
}

// WorkspaceSourceAPI covers per-workspace source slot git operations.
type WorkspaceSourceAPI interface {
	WorkspaceSourceFetch(ctx context.Context, workspace, slot string) (api.WorkspaceSourceStatus, error)
	WorkspaceSourcePull(ctx context.Context, workspace, slot string) (api.WorkspaceSourceStatus, error)
	WorkspaceSourcePush(ctx context.Context, workspace, slot, ref string) (api.WorkspaceSourceStatus, error)
	WorkspaceSourceDiff(ctx context.Context, workspace, slot, ref string) ([]api.DiffFile, error)
	WorkspaceSourceMerge(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error)
	WorkspaceSourceRebase(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error)
	WorkspaceSourceMergeAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error)
	WorkspaceSourceRebaseAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error)
	WorkspaceSourceRebaseContinue(ctx context.Context, workspace, slot string) (api.GitOpResult, error)
	WorkspaceSourcePublish(ctx context.Context, workspace, slot, remote, branch string) (api.GitOpResult, error)
}

// GitOpsAPI covers the aggregate sources/workspaces topology view.
type GitOpsAPI interface {
	GitOpsTopology(ctx context.Context) (api.GitOpsTopologyResponse, error)
	GitOpsTopologyWithCommits(ctx context.Context, withCommits int) (api.GitOpsTopologyResponse, error)
}

// SecretsAPI covers secret declaration metadata and backend mutation.
type SecretsAPI interface {
	SecretsList(ctx context.Context) ([]api.SecretRef, error)
	SecretGet(ctx context.Context, name string) (api.SecretRef, error)
	SecretValue(ctx context.Context, name string) (api.SecretValueResponse, error)
	SecretSet(ctx context.Context, name, value string) (api.SecretRef, error)
	SecretDelete(ctx context.Context, name string) error
}

// IngressAPI covers resolved service endpoints and ingress status.
type IngressAPI interface {
	ServiceEndpoint(ctx context.Context, name string) (*api.ServiceEndpoint, error)
	IngressStatus(ctx context.Context) (*api.IngressStatus, error)
}

// TemplateAPI covers template discovery and descriptor introspection.
type TemplateAPI interface {
	Templates(ctx context.Context) ([]api.TemplateDescriptor, error)
	Template(ctx context.Context, ref string) (api.TemplateDescriptor, error)
}

// Platform is the in-process implementation of API.
var _ API = (*Platform)(nil)
