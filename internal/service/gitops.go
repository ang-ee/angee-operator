package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// gitOpsTopologyCommitsMax caps the per-source commit window the topology
// query is willing to walk. Clients passing a higher value have it clamped
// silently so a hostile or buggy `withCommits=1e9` doesn't fan out a long
// git-log walk per git source.
const gitOpsTopologyCommitsMax = 1000

func (p *Platform) GitOpsTopology(ctx context.Context) (api.GitOpsTopologyResponse, error) {
	return p.GitOpsTopologyWithCommits(ctx, 0)
}

// GitOpsTopologyWithCommits returns the same topology as GitOpsTopology
// plus per-source commit history capped at `withCommits` per source.
// Pass 0 to skip commit population — that path matches the cheap default
// the polling subscription relies on.
//
// Negative values are rejected (the caller is asking for nonsense); the
// upper bound is clamped to gitOpsTopologyCommitsMax so a hostile
// `withCommits=1e9` doesn't fan out a long git-log walk per source.
func (p *Platform) GitOpsTopologyWithCommits(ctx context.Context, withCommits int) (api.GitOpsTopologyResponse, error) {
	if withCommits < 0 {
		return api.GitOpsTopologyResponse{}, &InvalidInputError{Field: "withCommits", Reason: "must be non-negative"}
	}
	if withCommits > gitOpsTopologyCommitsMax {
		withCommits = gitOpsTopologyCommitsMax
	}
	if err := ctx.Err(); err != nil {
		return api.GitOpsTopologyResponse{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.GitOpsTopologyResponse{}, err
	}
	sources := make([]api.SourceState, 0, len(stack.Sources))
	for _, name := range sortedKeys(stack.Sources) {
		if err := ctx.Err(); err != nil {
			return api.GitOpsTopologyResponse{}, err
		}
		source := stack.Sources[name]
		state, err := p.sourceState(ctx, name, source)
		if err != nil {
			state = api.SourceState{
				Name:   name,
				Kind:   source.Kind,
				Path:   p.sourcePath(name, source),
				State:  "error",
				Pushed: true,
				Error:  err.Error(),
			}
		}
		if withCommits > 0 && source.Kind == "git" && state.Exists {
			if commits, cerr := p.sourceCommits(ctx, state.Path, withCommits); cerr == nil {
				state.Commits = commits
			}
		}
		sources = append(sources, state)
	}
	topology := api.GitOpsTopologyResponse{
		Root:       p.root,
		Name:       stack.Name,
		Sources:    sources,
		Workspaces: []api.WorkspaceStatusResponse{},
		Links:      []api.GitOpsLink{},
		Summary: api.GitOpsSummary{
			Sources:    len(sources),
			Workspaces: len(stack.Workspaces),
		},
	}
	for _, source := range sources {
		countGitOpsState(&topology.Summary, source.State, source.Pushed)
	}
	for _, name := range sortedKeys(stack.Workspaces) {
		if err := ctx.Err(); err != nil {
			return api.GitOpsTopologyResponse{}, err
		}
		status := p.workspaceStatus(ctx, name, stack.Workspaces[name], stack)
		if err := ctx.Err(); err != nil {
			return api.GitOpsTopologyResponse{}, err
		}
		topology.Workspaces = append(topology.Workspaces, status)
		for _, source := range status.Sources {
			link := gitOpsLinkFromWorkspaceSource(status.Name, source)
			topology.Links = append(topology.Links, link)
			if link.Kind == "git" && link.Mode == manifest.WorkspaceSourceModeWorktree {
				topology.Summary.Worktrees++
			}
			countGitOpsState(&topology.Summary, link.State, link.Pushed)
		}
	}
	return topology, nil
}

func gitOpsLinkFromWorkspaceSource(workspace string, source api.WorkspaceSourceStatus) api.GitOpsLink {
	return api.GitOpsLink{
		ID:             workspace + ":" + source.Slot,
		Source:         source.Source,
		Workspace:      workspace,
		Slot:           source.Slot,
		Kind:           source.Kind,
		Mode:           source.Mode,
		Branch:         source.Branch,
		Ref:            source.Ref,
		Path:           source.Path,
		Exists:         source.Exists,
		State:          source.State,
		CurrentRef:     source.CurrentRef,
		Dirty:          source.Dirty,
		Upstream:       source.Upstream,
		Ahead:          source.Ahead,
		Behind:         source.Behind,
		Pushed:         source.Pushed,
		UnpushedReason: source.UnpushedReason,
		Error:          source.Error,
	}
}

func countGitOpsState(summary *api.GitOpsSummary, state string, pushed bool) {
	normalized := strings.ToLower(state)
	if !pushed && (normalized == "dirty" || normalized == "ahead" || normalized == "diverged" || normalized == workspaceSourceStateBranchMismatch) {
		summary.Unpushed++
	}
	switch normalized {
	case "clean", "ready":
		summary.Clean++
	case "dirty":
		summary.Dirty++
	case "ahead":
		summary.Ahead++
	case "behind":
		summary.Behind++
	case "diverged":
		summary.Diverged++
	case workspaceSourceStateBranchMismatch:
		summary.BranchMismatch++
	case "missing":
		summary.Missing++
	case "error":
		summary.Error++
	}
}

func (p *Platform) WorkspaceSourceFetch(ctx context.Context, workspaceName, slot string) (status api.WorkspaceSourceStatus, err error) {
	defer workspaceSlotOperation("workspace.source-fetch", "Fetch", workspaceName, slot).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	defer release()
	stack, wsSource, source, path, err := p.workspaceSourceTarget(ctx, workspaceName, slot)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if source.Kind != "git" {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q is not a git source", workspaceName, slot)
	}
	client := p.gitClient()
	step := gitStep{action: "fetching", object: fmt.Sprintf("source %q", wsSource.Source), remote: originURL(ctx, client, path), cause: CauseFetchFailed, slot: slot, source: wsSource.Source}
	if err := client.Fetch(ctx, path); err != nil {
		return api.WorkspaceSourceStatus{}, gitFailure(err, step)
	}
	return p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack), nil
}

func (p *Platform) WorkspaceSourcePull(ctx context.Context, workspaceName, slot string) (status api.WorkspaceSourceStatus, err error) {
	defer workspaceSlotOperation("workspace.source-pull", "Pull", workspaceName, slot).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	defer release()
	stack, wsSource, source, path, err := p.workspaceSourceTarget(ctx, workspaceName, slot)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if source.Kind != "git" {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q is not a git source", workspaceName, slot)
	}
	if err := p.ensureWorkspaceGitSourceOnExpectedBranch(ctx, workspaceName, slot, source, wsSource); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	client := p.gitClient()
	if err := requireCleanCheckout(ctx, client, path, fmt.Sprintf("source slot %q", slot), slot, wsSource.Source); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	step := gitStep{action: "pulling", object: fmt.Sprintf("source slot %q", slot), remote: originURL(ctx, client, path), cause: CauseFetchFailed, slot: slot, source: wsSource.Source}
	if err := client.Pull(ctx, path); err != nil {
		return api.WorkspaceSourceStatus{}, gitFailure(err, step)
	}
	return p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack), nil
}

func (p *Platform) WorkspaceSourcePush(ctx context.Context, workspaceName, slot, ref string) (status api.WorkspaceSourceStatus, err error) {
	defer workspaceSlotOperation("workspace.source-push", "Push", workspaceName, slot).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	defer release()
	stack, wsSource, source, path, err := p.workspaceSourceTarget(ctx, workspaceName, slot)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if source.Kind != "git" {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q is not a git source", workspaceName, slot)
	}
	if err := p.ensureWorkspaceGitSourceOnExpectedBranch(ctx, workspaceName, slot, source, wsSource); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	client := p.gitClient()
	if err := requireCleanCheckout(ctx, client, path, fmt.Sprintf("source slot %q", slot), slot, wsSource.Source); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	plan, err := planWorkspaceGitPush(ctx, client, workspaceName, slot, path, source, wsSource, ref)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if err := plan.run(ctx, client, path); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack), nil
}

func (p *Platform) workspaceSourceTarget(ctx context.Context, workspaceName, slot string) (*manifest.Stack, manifest.WorkspaceSource, manifest.Source, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", err
	}
	workspace, ok := stack.Workspaces[workspaceName]
	if !ok {
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", &NotFoundError{Kind: "workspace", Name: workspaceName}
	}
	wsSource, ok := workspace.Sources[slot]
	if !ok {
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", &NotFoundError{Kind: "workspace-source", Name: slot}
	}
	source, ok := stack.Sources[wsSource.Source]
	if !ok {
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", &NotFoundError{Kind: "source", Name: wsSource.Source}
	}
	_, path, err := p.workspaceSourcePath(workspaceName, slot, wsSource)
	if err != nil {
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
	}
	if err := requireWorkspaceSlotsOnDisk(workspaceName, []workspaceSlotPath{{slot: slot, path: path, source: source}}); err != nil {
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", err
	}
	return stack, wsSource, source, path, nil
}

// workspaceSlotOperation names a verb on one workspace source slot, such as
// `Pull for workspace "src" slot "app"`.
func workspaceSlotOperation(name, verb, workspace, slot string) operation {
	return operation{name: name, title: fmt.Sprintf("%s for workspace %q slot %q", verb, workspace, slot), workspace: workspace, slot: slot}
}
