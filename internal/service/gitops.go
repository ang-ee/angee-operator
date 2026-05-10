package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/git"
	"github.com/fyltr/angee/internal/manifest"
)

func (p *Platform) GitOpsTopology(ctx context.Context) (api.GitOpsTopologyResponse, error) {
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
			if link.Kind == "git" && link.Mode == "worktree" {
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

func (p *Platform) WorkspaceSourceFetch(ctx context.Context, workspaceName, slot string) (api.WorkspaceSourceStatus, error) {
	stack, wsSource, source, path, err := p.workspaceSourceTarget(ctx, workspaceName, slot)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if source.Kind != "git" {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q is not a git source", workspaceName, slot)
	}
	if _, err := os.Stat(path); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if err := git.New().Fetch(ctx, path); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack), nil
}

func (p *Platform) WorkspaceSourcePull(ctx context.Context, workspaceName, slot string) (api.WorkspaceSourceStatus, error) {
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
	client := git.New()
	dirty, err := client.Dirty(ctx, path)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if dirty {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q has uncommitted changes", workspaceName, slot)
	}
	if err := client.Pull(ctx, path); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack), nil
}

func (p *Platform) WorkspaceSourcePush(ctx context.Context, workspaceName, slot, ref string) (api.WorkspaceSourceStatus, error) {
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
	client := git.New()
	dirty, err := client.Dirty(ctx, path)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if dirty {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q has uncommitted changes", workspaceName, slot)
	}
	pushRef := ref
	if pushRef == "" {
		pushRef = wsSource.Branch
	}
	if ref == "" {
		_, hasUpstream, upstreamErr := client.Upstream(ctx, path)
		if upstreamErr != nil {
			return api.WorkspaceSourceStatus{}, upstreamErr
		}
		if hasUpstream {
			err = client.Push(ctx, path, "")
		} else if pushRef != "" && wsSource.Branch != "" {
			err = client.PushSetUpstream(ctx, path, pushRef)
		} else {
			err = client.Push(ctx, path, pushRef)
		}
	} else {
		err = client.Push(ctx, path, pushRef)
	}
	if err != nil {
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
	path := filepath.Join(p.root, "workspaces", workspaceName, wsSource.Subpath)
	return stack, wsSource, source, path, nil
}
