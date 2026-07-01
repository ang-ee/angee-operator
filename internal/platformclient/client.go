// Package platformclient provides RemoteClient, an HTTP implementation of
// service.API that talks to a running Angee operator. It is the remote half of
// the single control-plane contract: the CLI selects between an in-process
// *service.Platform and a *RemoteClient, and every command dispatches through
// service.API without knowing which transport it holds.
package platformclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
	"github.com/ang-ee/angee-operator/internal/service"
)

// listQueryValues encodes a query.Args as the `?query=<json>` parameter the
// operator's list endpoints accept. An empty Args yields no parameter.
func listQueryValues(q query.Args) url.Values {
	lq := queryfields.FromArgs(q)
	if lq.Filter == nil && len(lq.Sorting) == 0 && lq.Paging == nil {
		return nil
	}
	data, err := json.Marshal(lq)
	if err != nil {
		return nil
	}
	return url.Values{"query": []string{string(data)}}
}

// RemoteClient implements service.API against a remote operator over HTTP.
type RemoteClient struct {
	baseURL string
	client  *http.Client
}

// RemoteClient is the over-the-wire implementation of the platform contract.
var _ service.API = (*RemoteClient)(nil)

// RemoteError is a non-2xx response from the operator, carrying the HTTP
// status and the decoded api.ErrorResponse body.
type RemoteError struct {
	Status int
	Body   api.ErrorResponse
}

func (e *RemoteError) Error() string {
	message := e.Body.Error
	if message == "" {
		message = http.StatusText(e.Status)
	}
	return fmt.Sprintf("operator returned HTTP %d: %s", e.Status, message)
}

// RemoteNotFound is a RemoteError with HTTP 404 status.
type RemoteNotFound struct {
	RemoteError
}

// RemoteConflict is a RemoteError with HTTP 409 status.
type RemoteConflict struct {
	RemoteError
}

// RemoteInvalidInput is a RemoteError with HTTP 400 status.
type RemoteInvalidInput struct {
	RemoteError
}

// New constructs a RemoteClient for the operator at baseURL.
func New(baseURL string) *RemoteClient {
	return &RemoteClient{baseURL: strings.TrimRight(baseURL, "/"), client: http.DefaultClient}
}

// Ping reports whether the operator is reachable by issuing a short
// GET /healthz request. It returns nil as soon as the operator answers with
// any HTTP response (the server is up), and the transport error otherwise
// (connection refused, DNS failure, timeout). Callers use this to decide
// whether to fall back to local execution when an operator URL is configured
// but the server is down. The probe is bounded by a short timeout so a hung
// operator does not stall the caller indefinitely.
func (p *RemoteClient) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint("/healthz", nil), nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	// Drain before closing so the keep-alive connection can be reused by the
	// StackInit request that follows to the same operator.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil
}

func (p *RemoteClient) StackInit(ctx context.Context, template string, targetPath string, inputs map[string]string, force bool) (service.StackInitResult, error) {
	req := api.StackInitRequest{Template: template, Path: targetPath, Inputs: inputs, Force: force, Yes: true}
	var resp service.StackInitResult
	if err := p.doJSON(ctx, http.MethodPost, "/stack/init", nil, req, &resp); err != nil {
		return service.StackInitResult{}, err
	}
	return resp, nil
}

func (p *RemoteClient) StackUpdate(ctx context.Context) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/update", nil, nil, nil)
}

func (p *RemoteClient) StackDestroy(ctx context.Context, purge bool) error {
	query := url.Values{}
	if purge {
		query.Set("purge", "true")
	}
	return p.doJSON(ctx, http.MethodPost, "/stack/destroy", query, nil, nil)
}

func (p *RemoteClient) StackBuild(ctx context.Context, services []string) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/build", nil, api.StackRuntimeRequest{Services: services}, nil)
}

func (p *RemoteClient) StackUp(ctx context.Context, services []string, build bool) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/up", nil, api.StackRuntimeRequest{Services: services, Build: build}, nil)
}

// StackUpForeground brings the stack up on the operator and streams its
// combined output into stdout. The operator can only deliver one chunked
// body, so stderr is folded into stdout (matching the local foreground
// behavior of `angee up`). A client disconnect (ctx cancel) stops streaming
// but leaves the started services running on the operator host.
func (p *RemoteClient) StackUpForeground(ctx context.Context, services []string, build bool, stdout io.Writer, _ io.Writer) error {
	query := url.Values{}
	for _, service := range services {
		query.Add("service", service)
	}
	if build {
		query.Set("build", "true")
	}
	return p.streamTo(ctx, "/stack/up/stream", query, stdout)
}

// StackDevForeground brings every service up on the operator and streams the
// combined output into stdout. Same single-stream/disconnect semantics as
// StackUpForeground.
func (p *RemoteClient) StackDevForeground(ctx context.Context, build bool, stdout io.Writer, _ io.Writer) error {
	query := url.Values{}
	if build {
		query.Set("build", "true")
	}
	return p.streamTo(ctx, "/stack/dev/stream", query, stdout)
}

func (p *RemoteClient) StackDev(ctx context.Context, build bool) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/dev", nil, api.StackRuntimeRequest{Build: build}, nil)
}

func (p *RemoteClient) StackDown(ctx context.Context) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/down", nil, nil, nil)
}

func (p *RemoteClient) StackLogs(ctx context.Context, services []string, _ bool) (<-chan string, error) {
	query := url.Values{}
	for _, service := range services {
		query.Add("service", service)
	}
	return p.stream(ctx, "/stack/logs", query)
}

// StackLogsLimited delegates to the operator's /stack/logs stream. The
// operator emits a finite, already-bounded snapshot, so follow and maxBytes
// are not forwarded — the same degradation StackLogs applies to follow.
func (p *RemoteClient) StackLogsLimited(ctx context.Context, services []string, _ bool, _ int) (<-chan string, error) {
	return p.StackLogs(ctx, services, false)
}

// StreamServiceLogs is not proxied by the HTTP remote client: the per-service
// structured log stream is served over the operator's
// /services/{name}/logs/stream WebSocket, which a consumer connects to directly
// using the descriptor + token from the service-info endpoint.
func (p *RemoteClient) StreamServiceLogs(_ context.Context, _ string, _ int) (<-chan api.LogLine, error) {
	return nil, errors.New("per-service log streaming is served over the operator WebSocket, not the remote client")
}

func (p *RemoteClient) StackStatus(ctx context.Context) (api.StackStatusResponse, error) {
	var status api.StackStatusResponse
	if err := p.doJSON(ctx, http.MethodGet, "/stack/status", nil, nil, &status); err != nil {
		return api.StackStatusResponse{}, err
	}
	return status, nil
}

// StackCompile maps to the operator's /stack/prepare. The operator has no
// compile-without-write endpoint, so a remote `stack compile` writes runtime
// files as a side effect (see the plan's flagged follow-up).
func (p *RemoteClient) StackCompile(ctx context.Context) (*service.CompiledStack, error) {
	return p.StackPrepare(ctx)
}

func (p *RemoteClient) StackPrepare(ctx context.Context) (*service.CompiledStack, error) {
	var compiled service.CompiledStack
	if err := p.doJSON(ctx, http.MethodPost, "/stack/prepare", nil, nil, &compiled); err != nil {
		return nil, err
	}
	return &compiled, nil
}

func (p *RemoteClient) ServiceInit(ctx context.Context, req api.ServiceInitRequest) error {
	return p.doJSON(ctx, http.MethodPost, "/services", nil, req, nil)
}

func (p *RemoteClient) ServiceCreate(ctx context.Context, req api.ServiceCreateRequest) (api.ServiceState, error) {
	var state api.ServiceState
	if err := p.doJSON(ctx, http.MethodPost, "/services/create", nil, req, &state); err != nil {
		return api.ServiceState{}, err
	}
	return state, nil
}

func (p *RemoteClient) ServiceUpdate(ctx context.Context, req api.ServiceInitRequest) error {
	return p.doJSON(ctx, http.MethodPatch, "/services/"+url.PathEscape(req.Name), nil, req, nil)
}

func (p *RemoteClient) ServiceDestroy(ctx context.Context, name string, _ bool) error {
	return p.doJSON(ctx, http.MethodPost, "/services/"+url.PathEscape(name)+"/destroy", nil, nil, nil)
}

func (p *RemoteClient) ServiceList(ctx context.Context, q query.Args) ([]api.ServiceState, int, error) {
	var resp api.ServiceListResponse
	if err := p.doJSON(ctx, http.MethodGet, "/services", listQueryValues(q), nil, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Nodes, resp.TotalCount, nil
}

func (p *RemoteClient) ServiceUp(ctx context.Context, names []string) error {
	return p.serviceAction(ctx, names, "up")
}

func (p *RemoteClient) ServiceStart(ctx context.Context, names []string) error {
	return p.serviceAction(ctx, names, "start")
}

func (p *RemoteClient) ServiceStop(ctx context.Context, names []string) error {
	return p.serviceAction(ctx, names, "stop")
}

func (p *RemoteClient) ServiceRestart(ctx context.Context, names []string) error {
	return p.serviceAction(ctx, names, "restart")
}

func (p *RemoteClient) serviceAction(ctx context.Context, names []string, action string) error {
	for _, name := range names {
		if err := p.doJSON(ctx, http.MethodPost, "/services/"+url.PathEscape(name)+"/"+action, nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (p *RemoteClient) ServiceEndpoint(ctx context.Context, name string) (*api.ServiceEndpoint, error) {
	var endpoint api.ServiceEndpoint
	if err := p.doJSON(ctx, http.MethodGet, "/services/"+url.PathEscape(name)+"/endpoint", nil, nil, &endpoint); err != nil {
		return nil, err
	}
	return &endpoint, nil
}

func (p *RemoteClient) IngressStatus(ctx context.Context) (*api.IngressStatus, error) {
	var status api.IngressStatus
	if err := p.doJSON(ctx, http.MethodGet, "/ingress/status", nil, nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (p *RemoteClient) JobList(ctx context.Context, q query.Args) ([]api.JobState, int, error) {
	var resp api.JobListResponse
	if err := p.doJSON(ctx, http.MethodGet, "/jobs", listQueryValues(q), nil, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Nodes, resp.TotalCount, nil
}

func (p *RemoteClient) JobRun(ctx context.Context, name string, inputs map[string]string) ([]byte, error) {
	return p.doBytes(ctx, http.MethodPost, "/jobs/"+url.PathEscape(name)+"/run", nil, api.JobRunRequest{Inputs: inputs})
}

func (p *RemoteClient) SourceList(ctx context.Context, q query.Args) ([]api.SourceState, int, error) {
	var resp api.SourceListResponse
	if err := p.doJSON(ctx, http.MethodGet, "/sources", listQueryValues(q), nil, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Nodes, resp.TotalCount, nil
}

func (p *RemoteClient) SourceFetch(ctx context.Context, name string) (api.SourceState, error) {
	return p.sourceOperation(ctx, name, "fetch")
}

func (p *RemoteClient) SourceStatus(ctx context.Context, name string) (api.SourceState, error) {
	var state api.SourceState
	if err := p.doJSON(ctx, http.MethodGet, "/sources/"+url.PathEscape(name)+"/status", nil, nil, &state); err != nil {
		return api.SourceState{}, err
	}
	return state, nil
}

func (p *RemoteClient) SourcePull(ctx context.Context, name string) (api.SourceState, error) {
	return p.sourceOperation(ctx, name, "pull")
}

func (p *RemoteClient) SourcePush(ctx context.Context, name string, ref string) (api.SourceState, error) {
	var state api.SourceState
	if err := p.doJSON(ctx, http.MethodPost, "/sources/"+url.PathEscape(name)+"/push", nil, api.SourceOperationRequest{Ref: ref}, &state); err != nil {
		return api.SourceState{}, err
	}
	return state, nil
}

func (p *RemoteClient) sourceOperation(ctx context.Context, name string, action string) (api.SourceState, error) {
	var state api.SourceState
	if err := p.doJSON(ctx, http.MethodPost, "/sources/"+url.PathEscape(name)+"/"+action, nil, nil, &state); err != nil {
		return api.SourceState{}, err
	}
	return state, nil
}

func (p *RemoteClient) WorkspaceCreate(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceRef, error) {
	var ref api.WorkspaceRef
	if err := p.doJSON(ctx, http.MethodPost, "/workspaces", nil, req, &ref); err != nil {
		return api.WorkspaceRef{}, err
	}
	return ref, nil
}

func (p *RemoteClient) WorkspaceList(ctx context.Context, q query.Args) ([]api.WorkspaceRef, int, error) {
	var resp api.WorkspaceListResponse
	if err := p.doJSON(ctx, http.MethodGet, "/workspaces", listQueryValues(q), nil, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Nodes, resp.TotalCount, nil
}

func (p *RemoteClient) WorkspaceGet(ctx context.Context, name string) (api.WorkspaceRef, error) {
	var ref api.WorkspaceRef
	if err := p.doJSON(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(name), nil, nil, &ref); err != nil {
		return api.WorkspaceRef{}, err
	}
	return ref, nil
}

func (p *RemoteClient) WorkspaceStatus(ctx context.Context, name string) (api.WorkspaceStatusResponse, error) {
	var status api.WorkspaceStatusResponse
	if err := p.doJSON(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(name)+"/status", nil, nil, &status); err != nil {
		return api.WorkspaceStatusResponse{}, err
	}
	return status, nil
}

func (p *RemoteClient) WorkspaceUpdate(ctx context.Context, name string, inputs map[string]string, ttl string) (api.WorkspaceRef, error) {
	var ref api.WorkspaceRef
	req := api.WorkspaceUpdateRequest{Inputs: inputs, TTL: ttl}
	if err := p.doJSON(ctx, http.MethodPatch, "/workspaces/"+url.PathEscape(name), nil, req, &ref); err != nil {
		return api.WorkspaceRef{}, err
	}
	return ref, nil
}

func (p *RemoteClient) WorkspaceDestroy(ctx context.Context, name string, purge bool) error {
	query := url.Values{}
	if purge {
		query.Set("purge", "true")
	}
	return p.doJSON(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(name)+"/destroy", query, nil, nil)
}

func (p *RemoteClient) WorkspaceLogs(ctx context.Context, name string, _ bool) (<-chan string, error) {
	return p.stream(ctx, "/workspaces/"+url.PathEscape(name)+"/logs", nil)
}

// WorkspaceLogsLimited delegates to the operator's workspace logs stream;
// follow and maxBytes are not forwarded (see StackLogsLimited).
func (p *RemoteClient) WorkspaceLogsLimited(ctx context.Context, name string, _ bool, _ int) (<-chan string, error) {
	return p.WorkspaceLogs(ctx, name, false)
}

func (p *RemoteClient) WorkspaceGitStatus(ctx context.Context, name string) ([]api.SourceState, error) {
	var states []api.SourceState
	if err := p.doJSON(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(name)+"/git", nil, nil, &states); err != nil {
		return nil, err
	}
	return states, nil
}

func (p *RemoteClient) WorkspacePush(ctx context.Context, name string, ref string) ([]api.SourceState, error) {
	var states []api.SourceState
	if err := p.doJSON(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(name)+"/push", nil, api.SourceOperationRequest{Ref: ref}, &states); err != nil {
		return nil, err
	}
	return states, nil
}

func (p *RemoteClient) WorkspaceSyncBase(ctx context.Context, name string, method string) ([]api.SourceState, error) {
	var states []api.SourceState
	req := api.WorkspaceSyncBaseRequest{Method: method}
	if err := p.doJSON(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(name)+"/sync-base", nil, req, &states); err != nil {
		return nil, err
	}
	return states, nil
}

func (p *RemoteClient) SecretsList(ctx context.Context, q query.Args) ([]api.SecretRef, int, error) {
	var resp api.SecretListResponse
	if err := p.doJSON(ctx, http.MethodGet, "/secrets", listQueryValues(q), nil, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Nodes, resp.TotalCount, nil
}

func (p *RemoteClient) SecretGet(ctx context.Context, name string) (api.SecretRef, error) {
	var ref api.SecretRef
	if err := p.doJSON(ctx, http.MethodGet, "/secrets/"+url.PathEscape(name), nil, nil, &ref); err != nil {
		return api.SecretRef{}, err
	}
	return ref, nil
}

func (p *RemoteClient) SecretValue(ctx context.Context, name string) (api.SecretValueResponse, error) {
	var resp api.SecretValueResponse
	if err := p.doJSON(ctx, http.MethodGet, "/secrets/"+url.PathEscape(name)+"/value", nil, nil, &resp); err != nil {
		return api.SecretValueResponse{}, err
	}
	return resp, nil
}

func (p *RemoteClient) SecretSet(ctx context.Context, name, value string) (api.SecretRef, error) {
	var ref api.SecretRef
	body := api.SecretSetRequest{Value: value}
	if err := p.doJSON(ctx, http.MethodPost, "/secrets/"+url.PathEscape(name), nil, body, &ref); err != nil {
		return api.SecretRef{}, err
	}
	return ref, nil
}

func (p *RemoteClient) SecretDelete(ctx context.Context, name string) error {
	return p.doJSON(ctx, http.MethodDelete, "/secrets/"+url.PathEscape(name), nil, nil, nil)
}

func (p *RemoteClient) FileRead(ctx context.Context, source, path string) (api.FileContent, error) {
	var out api.FileContent
	q := url.Values{"source": {source}, "path": {path}}
	if err := p.doJSON(ctx, http.MethodGet, "/files", q, nil, &out); err != nil {
		return api.FileContent{}, err
	}
	return out, nil
}

func (p *RemoteClient) FileWrite(ctx context.Context, source, path, content, etag string) (api.FileRef, error) {
	var out api.FileRef
	q := url.Values{"source": {source}, "path": {path}}
	body := api.FileWriteRequest{Content: content, Etag: etag}
	if err := p.doJSON(ctx, http.MethodPut, "/files", q, body, &out); err != nil {
		return api.FileRef{}, err
	}
	return out, nil
}

func (p *RemoteClient) WorkspaceCreatePreflight(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceCreatePreflightResponse, error) {
	var resp api.WorkspaceCreatePreflightResponse
	if err := p.doJSON(ctx, http.MethodPost, "/workspaces/preflight", nil, req, &resp); err != nil {
		return api.WorkspaceCreatePreflightResponse{}, err
	}
	return resp, nil
}

func (p *RemoteClient) WorkspaceSourceFetch(ctx context.Context, workspace, slot string) (api.WorkspaceSourceStatus, error) {
	var state api.WorkspaceSourceStatus
	if err := p.doJSON(ctx, http.MethodPost, slotPath(workspace, slot, "fetch"), nil, nil, &state); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return state, nil
}

func (p *RemoteClient) WorkspaceSourcePull(ctx context.Context, workspace, slot string) (api.WorkspaceSourceStatus, error) {
	var state api.WorkspaceSourceStatus
	if err := p.doJSON(ctx, http.MethodPost, slotPath(workspace, slot, "pull"), nil, nil, &state); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return state, nil
}

func (p *RemoteClient) WorkspaceSourcePush(ctx context.Context, workspace, slot, ref string) (api.WorkspaceSourceStatus, error) {
	var state api.WorkspaceSourceStatus
	body := api.SourceOperationRequest{Ref: ref}
	if err := p.doJSON(ctx, http.MethodPost, slotPath(workspace, slot, "push"), nil, body, &state); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return state, nil
}

func (p *RemoteClient) WorkspaceSourceDiff(ctx context.Context, workspace, slot, ref string) ([]api.DiffFile, error) {
	var files []api.DiffFile
	query := refQuery(ref)
	if err := p.doJSON(ctx, http.MethodGet, slotPath(workspace, slot, "diff"), query, nil, &files); err != nil {
		return nil, err
	}
	return files, nil
}

func (p *RemoteClient) WorkspaceSourceMerge(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "merge", api.WorkspaceSourceGitOpRequest{Ref: ref})
}

func (p *RemoteClient) WorkspaceSourceRebase(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "rebase", api.WorkspaceSourceGitOpRequest{Ref: ref})
}

func (p *RemoteClient) WorkspaceSourceMergeAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "merge-abort", api.WorkspaceSourceGitOpRequest{})
}

func (p *RemoteClient) WorkspaceSourceRebaseAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "rebase-abort", api.WorkspaceSourceGitOpRequest{})
}

func (p *RemoteClient) WorkspaceSourceRebaseContinue(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "rebase-continue", api.WorkspaceSourceGitOpRequest{})
}

func (p *RemoteClient) WorkspaceSourcePublish(ctx context.Context, workspace, slot, remote, branch string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "publish", api.WorkspaceSourceGitOpRequest{Remote: remote, Branch: branch})
}

func (p *RemoteClient) gitOp(ctx context.Context, workspace, slot, op string, body api.WorkspaceSourceGitOpRequest) (api.GitOpResult, error) {
	var result api.GitOpResult
	if err := p.doJSON(ctx, http.MethodPost, slotPath(workspace, slot, op), nil, body, &result); err != nil {
		return api.GitOpResult{}, err
	}
	return result, nil
}

func (p *RemoteClient) SourceDiff(ctx context.Context, name, ref string) ([]api.DiffFile, error) {
	var files []api.DiffFile
	query := refQuery(ref)
	if err := p.doJSON(ctx, http.MethodGet, "/sources/"+url.PathEscape(name)+"/diff", query, nil, &files); err != nil {
		return nil, err
	}
	return files, nil
}

func (p *RemoteClient) GitOpsTopology(ctx context.Context) (api.GitOpsTopologyResponse, error) {
	return p.GitOpsTopologyWithCommits(ctx, 0)
}

func (p *RemoteClient) GitOpsTopologyWithCommits(ctx context.Context, withCommits int) (api.GitOpsTopologyResponse, error) {
	var topo api.GitOpsTopologyResponse
	var query url.Values
	if withCommits > 0 {
		query = url.Values{"with_commits": []string{fmt.Sprintf("%d", withCommits)}}
	}
	if err := p.doJSON(ctx, http.MethodGet, "/gitops/topology", query, nil, &topo); err != nil {
		return api.GitOpsTopologyResponse{}, err
	}
	return topo, nil
}

func (p *RemoteClient) Templates(ctx context.Context, q query.Args) ([]api.TemplateDescriptor, int, error) {
	var resp api.TemplateListResponse
	if err := p.doJSON(ctx, http.MethodGet, "/templates", listQueryValues(q), nil, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Nodes, resp.TotalCount, nil
}

// MintConnectionToken is not part of service.API: minting belongs to the
// operator's auth surface, not the platform contract. Callers reach it on the
// concrete *RemoteClient, and only when an operator URL is configured.
func (p *RemoteClient) MintConnectionToken(ctx context.Context, req api.MintConnectionTokenRequest) (api.ConnectionTokenResponse, error) {
	var resp api.ConnectionTokenResponse
	if err := p.doJSON(ctx, http.MethodPost, "/tokens/mint", nil, req, &resp); err != nil {
		return api.ConnectionTokenResponse{}, err
	}
	return resp, nil
}

func (p *RemoteClient) Template(ctx context.Context, ref string) (api.TemplateDescriptor, error) {
	var desc api.TemplateDescriptor
	// `ref` may contain slashes (e.g. workspaces/dev-pr); the REST route
	// accepts the full ref as a path suffix, so concatenate without
	// path-escaping the slash separators.
	if err := p.doJSON(ctx, http.MethodGet, "/templates/"+strings.TrimPrefix(ref, "/"), nil, nil, &desc); err != nil {
		return api.TemplateDescriptor{}, err
	}
	return desc, nil
}

func slotPath(workspace, slot, op string) string {
	return "/workspaces/" + url.PathEscape(workspace) + "/sources/" + url.PathEscape(slot) + "/" + op
}

func refQuery(ref string) url.Values {
	if ref == "" {
		return nil
	}
	return url.Values{"ref": []string{ref}}
}

func (p *RemoteClient) doJSON(ctx context.Context, method, path string, query url.Values, in any, out any) error {
	body, err := jsonBody(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, p.endpoint(path, query), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return operatorHTTPError(resp.StatusCode, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (p *RemoteClient) doBytes(ctx context.Context, method, path string, query url.Values, in any) ([]byte, error) {
	body, err := jsonBody(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, p.endpoint(path, query), body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, operatorHTTPError(resp.StatusCode, data)
	}
	return data, nil
}

func (p *RemoteClient) stream(ctx context.Context, path string, query url.Values) (<-chan string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint(path, query), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, readErr
		}
		return nil, operatorHTTPError(resp.StatusCode, data)
	}
	out := make(chan string)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			out <- scanner.Text() + "\n"
		}
	}()
	return out, nil
}

// streamTo copies a chunked text stream into w as it arrives, returning any
// pre-stream HTTP error. A cancelled ctx aborts the in-flight request, which
// unblocks the copy.
func (p *RemoteClient) streamTo(ctx context.Context, path string, query url.Values, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint(path, query), nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return readErr
		}
		return operatorHTTPError(resp.StatusCode, data)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func (p *RemoteClient) endpoint(path string, query url.Values) string {
	endpoint := p.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	return endpoint
}

func jsonBody(value any) (io.Reader, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

func operatorHTTPError(status int, data []byte) error {
	var body api.ErrorResponse
	if err := json.Unmarshal(data, &body); err == nil && body.Error != "" {
		base := RemoteError{Status: status, Body: body}
		switch status {
		case http.StatusNotFound:
			return &RemoteNotFound{RemoteError: base}
		case http.StatusConflict:
			return &RemoteConflict{RemoteError: base}
		case http.StatusBadRequest:
			return &RemoteInvalidInput{RemoteError: base}
		default:
			return &base
		}
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		text = http.StatusText(status)
	}
	return &RemoteError{Status: status, Body: api.ErrorResponse{Error: text}}
}

// AsConflict reports whether err is a RemoteConflict and, when kind is
// non-empty, whether its Kind matches. It is the exported successor to the
// CLI's former remoteConflict helper.
func AsConflict(err error, kind string) (*RemoteConflict, bool) {
	var conflict *RemoteConflict
	if !errors.As(err, &conflict) {
		return nil, false
	}
	return conflict, kind == "" || conflict.Body.Kind == kind
}
