package cli

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

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/copierx"
	"github.com/fyltr/angee/internal/service"
)

type platformClient interface {
	StackInit(context.Context, string, string, map[string]string, bool) (service.StackInitResult, error)
	StackTemplateQuestions(context.Context, string) (map[string]copierx.Input, copierx.Inputs, error)
	StackUpdate(context.Context) error
	StackDestroy(context.Context, bool) error
	StackBuild(context.Context, []string) error
	StackUp(context.Context, []string, bool) error
	StackUpForeground(context.Context, []string, bool, io.Writer, io.Writer) error
	StackDevForeground(context.Context, bool, io.Writer, io.Writer) error
	StackDown(context.Context) error
	StackLogs(context.Context, []string, bool) (<-chan string, error)
	StackStatus(context.Context) (api.StackStatusResponse, error)
	StackCompile(context.Context) (*service.CompiledStack, error)
	StackPrepare(context.Context) (*service.CompiledStack, error)
	ServiceInit(context.Context, api.ServiceInitRequest) error
	ServiceCreate(context.Context, api.ServiceCreateRequest) (api.ServiceState, error)
	ServiceUpdate(context.Context, api.ServiceInitRequest) error
	ServiceDestroy(context.Context, string, bool) error
	ServiceList(context.Context) ([]api.ServiceState, error)
	ServiceStart(context.Context, []string) error
	ServiceStop(context.Context, []string) error
	ServiceRestart(context.Context, []string) error
	JobList(context.Context) ([]api.JobState, error)
	JobRun(context.Context, string, map[string]string) ([]byte, error)
	SourceList(context.Context) ([]api.SourceState, error)
	SourceFetch(context.Context, string) (api.SourceState, error)
	SourceStatus(context.Context, string) (api.SourceState, error)
	SourcePull(context.Context, string) (api.SourceState, error)
	SourcePush(context.Context, string, string) (api.SourceState, error)
	WorkspaceCreate(context.Context, api.WorkspaceCreateRequest) (api.WorkspaceRef, error)
	WorkspaceList(context.Context) ([]api.WorkspaceRef, error)
	WorkspaceGet(context.Context, string) (api.WorkspaceRef, error)
	WorkspaceStatus(context.Context, string) (api.WorkspaceStatusResponse, error)
	WorkspaceUpdate(context.Context, string, map[string]string, string) (api.WorkspaceRef, error)
	WorkspaceDestroy(context.Context, string, bool) error
	WorkspaceLogs(context.Context, string, bool) (<-chan string, error)
	WorkspaceGitStatus(context.Context, string) ([]api.SourceState, error)
	WorkspacePush(context.Context, string, string) ([]api.SourceState, error)
	WorkspaceSyncBase(context.Context, string, string) ([]api.SourceState, error)
	SecretsList(context.Context) ([]api.SecretRef, error)
	SecretGet(context.Context, string) (api.SecretRef, error)
	SecretValue(context.Context, string) (api.SecretValueResponse, error)
	SecretSet(context.Context, string, string) (api.SecretRef, error)
	SecretDelete(context.Context, string) error
	WorkspaceCreatePreflight(context.Context, api.WorkspaceCreateRequest) (api.WorkspaceCreatePreflightResponse, error)
	WorkspaceSourceFetch(context.Context, string, string) (api.WorkspaceSourceStatus, error)
	WorkspaceSourcePull(context.Context, string, string) (api.WorkspaceSourceStatus, error)
	WorkspaceSourcePush(context.Context, string, string, string) (api.WorkspaceSourceStatus, error)
	WorkspaceSourceDiff(context.Context, string, string, string) ([]api.DiffFile, error)
	WorkspaceSourceMerge(context.Context, string, string, string) (api.GitOpResult, error)
	WorkspaceSourceRebase(context.Context, string, string, string) (api.GitOpResult, error)
	WorkspaceSourceMergeAbort(context.Context, string, string) (api.GitOpResult, error)
	WorkspaceSourceRebaseAbort(context.Context, string, string) (api.GitOpResult, error)
	WorkspaceSourceRebaseContinue(context.Context, string, string) (api.GitOpResult, error)
	WorkspaceSourcePublish(context.Context, string, string, string, string) (api.GitOpResult, error)
	SourceDiff(context.Context, string, string) ([]api.DiffFile, error)
	GitOpsTopology(context.Context) (api.GitOpsTopologyResponse, error)
	GitOpsTopologyWithCommits(context.Context, int) (api.GitOpsTopologyResponse, error)
	Templates(context.Context) ([]api.TemplateDescriptor, error)
	Template(context.Context, string) (api.TemplateDescriptor, error)
}

type remotePlatform struct {
	baseURL string
	client  *http.Client
}

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

type RemoteNotFound struct {
	RemoteError
}

type RemoteConflict struct {
	RemoteError
}

type RemoteInvalidInput struct {
	RemoteError
}

func newRemotePlatform(baseURL string) *remotePlatform {
	return &remotePlatform{baseURL: strings.TrimRight(baseURL, "/"), client: http.DefaultClient}
}

func (p *remotePlatform) StackInit(ctx context.Context, template string, targetPath string, inputs map[string]string, force bool) (service.StackInitResult, error) {
	req := api.StackInitRequest{Template: template, Path: targetPath, Inputs: inputs, Force: force, Yes: true}
	var resp service.StackInitResult
	if err := p.doJSON(ctx, http.MethodPost, "/stack/init", nil, req, &resp); err != nil {
		return service.StackInitResult{}, err
	}
	return resp, nil
}

func (p *remotePlatform) StackTemplateQuestions(context.Context, string) (map[string]copierx.Input, copierx.Inputs, error) {
	return nil, nil, nil
}

func (p *remotePlatform) StackUpdate(ctx context.Context) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/update", nil, nil, nil)
}

func (p *remotePlatform) StackDestroy(ctx context.Context, purge bool) error {
	query := url.Values{}
	if purge {
		query.Set("purge", "true")
	}
	return p.doJSON(ctx, http.MethodPost, "/stack/destroy", query, nil, nil)
}

func (p *remotePlatform) StackBuild(ctx context.Context, services []string) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/build", nil, api.StackRuntimeRequest{Services: services}, nil)
}

func (p *remotePlatform) StackUp(ctx context.Context, services []string, build bool) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/up", nil, api.StackRuntimeRequest{Services: services, Build: build}, nil)
}

func (p *remotePlatform) StackUpForeground(ctx context.Context, services []string, build bool, _ io.Writer, _ io.Writer) error {
	return p.StackUp(ctx, services, build)
}

func (p *remotePlatform) StackDevForeground(ctx context.Context, build bool, _ io.Writer, _ io.Writer) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/dev", nil, api.StackRuntimeRequest{Build: build}, nil)
}

func (p *remotePlatform) StackDown(ctx context.Context) error {
	return p.doJSON(ctx, http.MethodPost, "/stack/down", nil, nil, nil)
}

func (p *remotePlatform) StackLogs(ctx context.Context, services []string, _ bool) (<-chan string, error) {
	query := url.Values{}
	for _, service := range services {
		query.Add("service", service)
	}
	return p.stream(ctx, "/stack/logs", query)
}

func (p *remotePlatform) StackStatus(ctx context.Context) (api.StackStatusResponse, error) {
	var status api.StackStatusResponse
	if err := p.doJSON(ctx, http.MethodGet, "/stack/status", nil, nil, &status); err != nil {
		return api.StackStatusResponse{}, err
	}
	return status, nil
}

func (p *remotePlatform) StackCompile(ctx context.Context) (*service.CompiledStack, error) {
	return p.StackPrepare(ctx)
}

func (p *remotePlatform) StackPrepare(ctx context.Context) (*service.CompiledStack, error) {
	var compiled service.CompiledStack
	if err := p.doJSON(ctx, http.MethodPost, "/stack/prepare", nil, nil, &compiled); err != nil {
		return nil, err
	}
	return &compiled, nil
}

func (p *remotePlatform) ServiceInit(ctx context.Context, req api.ServiceInitRequest) error {
	return p.doJSON(ctx, http.MethodPost, "/services", nil, req, nil)
}

func (p *remotePlatform) ServiceCreate(ctx context.Context, req api.ServiceCreateRequest) (api.ServiceState, error) {
	var state api.ServiceState
	if err := p.doJSON(ctx, http.MethodPost, "/services/create", nil, req, &state); err != nil {
		return api.ServiceState{}, err
	}
	return state, nil
}

func (p *remotePlatform) ServiceUpdate(ctx context.Context, req api.ServiceInitRequest) error {
	return p.doJSON(ctx, http.MethodPatch, "/services/"+url.PathEscape(req.Name), nil, req, nil)
}

func (p *remotePlatform) ServiceDestroy(ctx context.Context, name string, _ bool) error {
	return p.doJSON(ctx, http.MethodPost, "/services/"+url.PathEscape(name)+"/destroy", nil, nil, nil)
}

func (p *remotePlatform) ServiceList(ctx context.Context) ([]api.ServiceState, error) {
	var services []api.ServiceState
	if err := p.doJSON(ctx, http.MethodGet, "/services", nil, nil, &services); err != nil {
		return nil, err
	}
	return services, nil
}

func (p *remotePlatform) ServiceStart(ctx context.Context, names []string) error {
	return p.serviceAction(ctx, names, "start")
}

func (p *remotePlatform) ServiceStop(ctx context.Context, names []string) error {
	return p.serviceAction(ctx, names, "stop")
}

func (p *remotePlatform) ServiceRestart(ctx context.Context, names []string) error {
	return p.serviceAction(ctx, names, "restart")
}

func (p *remotePlatform) serviceAction(ctx context.Context, names []string, action string) error {
	for _, name := range names {
		if err := p.doJSON(ctx, http.MethodPost, "/services/"+url.PathEscape(name)+"/"+action, nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (p *remotePlatform) JobList(ctx context.Context) ([]api.JobState, error) {
	var jobs []api.JobState
	if err := p.doJSON(ctx, http.MethodGet, "/jobs", nil, nil, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (p *remotePlatform) JobRun(ctx context.Context, name string, inputs map[string]string) ([]byte, error) {
	return p.doBytes(ctx, http.MethodPost, "/jobs/"+url.PathEscape(name)+"/run", nil, api.JobRunRequest{Inputs: inputs})
}

func (p *remotePlatform) SourceList(ctx context.Context) ([]api.SourceState, error) {
	var sources []api.SourceState
	if err := p.doJSON(ctx, http.MethodGet, "/sources", nil, nil, &sources); err != nil {
		return nil, err
	}
	return sources, nil
}

func (p *remotePlatform) SourceFetch(ctx context.Context, name string) (api.SourceState, error) {
	return p.sourceOperation(ctx, name, "fetch")
}

func (p *remotePlatform) SourceStatus(ctx context.Context, name string) (api.SourceState, error) {
	var state api.SourceState
	if err := p.doJSON(ctx, http.MethodGet, "/sources/"+url.PathEscape(name)+"/status", nil, nil, &state); err != nil {
		return api.SourceState{}, err
	}
	return state, nil
}

func (p *remotePlatform) SourcePull(ctx context.Context, name string) (api.SourceState, error) {
	return p.sourceOperation(ctx, name, "pull")
}

func (p *remotePlatform) SourcePush(ctx context.Context, name string, ref string) (api.SourceState, error) {
	var state api.SourceState
	if err := p.doJSON(ctx, http.MethodPost, "/sources/"+url.PathEscape(name)+"/push", nil, api.SourceOperationRequest{Ref: ref}, &state); err != nil {
		return api.SourceState{}, err
	}
	return state, nil
}

func (p *remotePlatform) sourceOperation(ctx context.Context, name string, action string) (api.SourceState, error) {
	var state api.SourceState
	if err := p.doJSON(ctx, http.MethodPost, "/sources/"+url.PathEscape(name)+"/"+action, nil, nil, &state); err != nil {
		return api.SourceState{}, err
	}
	return state, nil
}

func (p *remotePlatform) WorkspaceCreate(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceRef, error) {
	var ref api.WorkspaceRef
	if err := p.doJSON(ctx, http.MethodPost, "/workspaces", nil, req, &ref); err != nil {
		return api.WorkspaceRef{}, err
	}
	return ref, nil
}

func (p *remotePlatform) WorkspaceList(ctx context.Context) ([]api.WorkspaceRef, error) {
	var refs []api.WorkspaceRef
	if err := p.doJSON(ctx, http.MethodGet, "/workspaces", nil, nil, &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

func (p *remotePlatform) WorkspaceGet(ctx context.Context, name string) (api.WorkspaceRef, error) {
	var ref api.WorkspaceRef
	if err := p.doJSON(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(name), nil, nil, &ref); err != nil {
		return api.WorkspaceRef{}, err
	}
	return ref, nil
}

func (p *remotePlatform) WorkspaceStatus(ctx context.Context, name string) (api.WorkspaceStatusResponse, error) {
	var status api.WorkspaceStatusResponse
	if err := p.doJSON(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(name)+"/status", nil, nil, &status); err != nil {
		return api.WorkspaceStatusResponse{}, err
	}
	return status, nil
}

func (p *remotePlatform) WorkspaceUpdate(ctx context.Context, name string, inputs map[string]string, ttl string) (api.WorkspaceRef, error) {
	var ref api.WorkspaceRef
	req := api.WorkspaceUpdateRequest{Inputs: inputs, TTL: ttl}
	if err := p.doJSON(ctx, http.MethodPatch, "/workspaces/"+url.PathEscape(name), nil, req, &ref); err != nil {
		return api.WorkspaceRef{}, err
	}
	return ref, nil
}

func (p *remotePlatform) WorkspaceDestroy(ctx context.Context, name string, purge bool) error {
	query := url.Values{}
	if purge {
		query.Set("purge", "true")
	}
	return p.doJSON(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(name)+"/destroy", query, nil, nil)
}

func (p *remotePlatform) WorkspaceLogs(ctx context.Context, name string, _ bool) (<-chan string, error) {
	return p.stream(ctx, "/workspaces/"+url.PathEscape(name)+"/logs", nil)
}

func (p *remotePlatform) WorkspaceGitStatus(ctx context.Context, name string) ([]api.SourceState, error) {
	var states []api.SourceState
	if err := p.doJSON(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(name)+"/git", nil, nil, &states); err != nil {
		return nil, err
	}
	return states, nil
}

func (p *remotePlatform) WorkspacePush(ctx context.Context, name string, ref string) ([]api.SourceState, error) {
	var states []api.SourceState
	if err := p.doJSON(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(name)+"/push", nil, api.SourceOperationRequest{Ref: ref}, &states); err != nil {
		return nil, err
	}
	return states, nil
}

func (p *remotePlatform) WorkspaceSyncBase(ctx context.Context, name string, method string) ([]api.SourceState, error) {
	var states []api.SourceState
	req := api.WorkspaceSyncBaseRequest{Method: method}
	if err := p.doJSON(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(name)+"/sync-base", nil, req, &states); err != nil {
		return nil, err
	}
	return states, nil
}

func (p *remotePlatform) SecretsList(ctx context.Context) ([]api.SecretRef, error) {
	var refs []api.SecretRef
	if err := p.doJSON(ctx, http.MethodGet, "/secrets", nil, nil, &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

func (p *remotePlatform) SecretGet(ctx context.Context, name string) (api.SecretRef, error) {
	var ref api.SecretRef
	if err := p.doJSON(ctx, http.MethodGet, "/secrets/"+url.PathEscape(name), nil, nil, &ref); err != nil {
		return api.SecretRef{}, err
	}
	return ref, nil
}

func (p *remotePlatform) SecretValue(ctx context.Context, name string) (api.SecretValueResponse, error) {
	var resp api.SecretValueResponse
	if err := p.doJSON(ctx, http.MethodGet, "/secrets/"+url.PathEscape(name)+"/value", nil, nil, &resp); err != nil {
		return api.SecretValueResponse{}, err
	}
	return resp, nil
}

func (p *remotePlatform) SecretSet(ctx context.Context, name, value string) (api.SecretRef, error) {
	var ref api.SecretRef
	body := api.SecretSetRequest{Value: value}
	if err := p.doJSON(ctx, http.MethodPost, "/secrets/"+url.PathEscape(name), nil, body, &ref); err != nil {
		return api.SecretRef{}, err
	}
	return ref, nil
}

func (p *remotePlatform) SecretDelete(ctx context.Context, name string) error {
	return p.doJSON(ctx, http.MethodDelete, "/secrets/"+url.PathEscape(name), nil, nil, nil)
}

func (p *remotePlatform) WorkspaceCreatePreflight(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceCreatePreflightResponse, error) {
	var resp api.WorkspaceCreatePreflightResponse
	if err := p.doJSON(ctx, http.MethodPost, "/workspaces/preflight", nil, req, &resp); err != nil {
		return api.WorkspaceCreatePreflightResponse{}, err
	}
	return resp, nil
}

func (p *remotePlatform) WorkspaceSourceFetch(ctx context.Context, workspace, slot string) (api.WorkspaceSourceStatus, error) {
	var state api.WorkspaceSourceStatus
	if err := p.doJSON(ctx, http.MethodPost, slotPath(workspace, slot, "fetch"), nil, nil, &state); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return state, nil
}

func (p *remotePlatform) WorkspaceSourcePull(ctx context.Context, workspace, slot string) (api.WorkspaceSourceStatus, error) {
	var state api.WorkspaceSourceStatus
	if err := p.doJSON(ctx, http.MethodPost, slotPath(workspace, slot, "pull"), nil, nil, &state); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return state, nil
}

func (p *remotePlatform) WorkspaceSourcePush(ctx context.Context, workspace, slot, ref string) (api.WorkspaceSourceStatus, error) {
	var state api.WorkspaceSourceStatus
	body := api.SourceOperationRequest{Ref: ref}
	if err := p.doJSON(ctx, http.MethodPost, slotPath(workspace, slot, "push"), nil, body, &state); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return state, nil
}

func (p *remotePlatform) WorkspaceSourceDiff(ctx context.Context, workspace, slot, ref string) ([]api.DiffFile, error) {
	var files []api.DiffFile
	query := refQuery(ref)
	if err := p.doJSON(ctx, http.MethodGet, slotPath(workspace, slot, "diff"), query, nil, &files); err != nil {
		return nil, err
	}
	return files, nil
}

func (p *remotePlatform) WorkspaceSourceMerge(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "merge", api.WorkspaceSourceGitOpRequest{Ref: ref})
}

func (p *remotePlatform) WorkspaceSourceRebase(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "rebase", api.WorkspaceSourceGitOpRequest{Ref: ref})
}

func (p *remotePlatform) WorkspaceSourceMergeAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "merge-abort", api.WorkspaceSourceGitOpRequest{})
}

func (p *remotePlatform) WorkspaceSourceRebaseAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "rebase-abort", api.WorkspaceSourceGitOpRequest{})
}

func (p *remotePlatform) WorkspaceSourceRebaseContinue(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "rebase-continue", api.WorkspaceSourceGitOpRequest{})
}

func (p *remotePlatform) WorkspaceSourcePublish(ctx context.Context, workspace, slot, remote, branch string) (api.GitOpResult, error) {
	return p.gitOp(ctx, workspace, slot, "publish", api.WorkspaceSourceGitOpRequest{Remote: remote, Branch: branch})
}

func (p *remotePlatform) gitOp(ctx context.Context, workspace, slot, op string, body api.WorkspaceSourceGitOpRequest) (api.GitOpResult, error) {
	var result api.GitOpResult
	if err := p.doJSON(ctx, http.MethodPost, slotPath(workspace, slot, op), nil, body, &result); err != nil {
		return api.GitOpResult{}, err
	}
	return result, nil
}

func (p *remotePlatform) SourceDiff(ctx context.Context, name, ref string) ([]api.DiffFile, error) {
	var files []api.DiffFile
	query := refQuery(ref)
	if err := p.doJSON(ctx, http.MethodGet, "/sources/"+url.PathEscape(name)+"/diff", query, nil, &files); err != nil {
		return nil, err
	}
	return files, nil
}

func (p *remotePlatform) GitOpsTopology(ctx context.Context) (api.GitOpsTopologyResponse, error) {
	return p.GitOpsTopologyWithCommits(ctx, 0)
}

func (p *remotePlatform) GitOpsTopologyWithCommits(ctx context.Context, withCommits int) (api.GitOpsTopologyResponse, error) {
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

func (p *remotePlatform) Templates(ctx context.Context) ([]api.TemplateDescriptor, error) {
	var refs []api.TemplateDescriptor
	if err := p.doJSON(ctx, http.MethodGet, "/templates", nil, nil, &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

func (p *remotePlatform) MintConnectionToken(ctx context.Context, req api.MintConnectionTokenRequest) (api.ConnectionTokenResponse, error) {
	var resp api.ConnectionTokenResponse
	if err := p.doJSON(ctx, http.MethodPost, "/tokens/mint", nil, req, &resp); err != nil {
		return api.ConnectionTokenResponse{}, err
	}
	return resp, nil
}

func (p *remotePlatform) Template(ctx context.Context, ref string) (api.TemplateDescriptor, error) {
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

func (p *remotePlatform) doJSON(ctx context.Context, method, path string, query url.Values, in any, out any) error {
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

func (p *remotePlatform) doBytes(ctx context.Context, method, path string, query url.Values, in any) ([]byte, error) {
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

func (p *remotePlatform) stream(ctx context.Context, path string, query url.Values) (<-chan string, error) {
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

func (p *remotePlatform) endpoint(path string, query url.Values) string {
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

func remoteConflict(err error, kind string) (*RemoteConflict, bool) {
	var conflict *RemoteConflict
	if !errors.As(err, &conflict) {
		return nil, false
	}
	return conflict, kind == "" || conflict.Body.Kind == kind
}
