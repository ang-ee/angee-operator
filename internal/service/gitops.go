package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
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
		state, err := p.sourceState(ctx, stack, name)
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
			commits, cerr := p.sourceCommits(ctx, state.Path, withCommits)
			switch {
			case cerr == nil:
				state.Commits = commits
			case ctx.Err() == nil:
				logctx.From(ctx).Warn(fmt.Sprintf("read the commits of source %s: %v", name, cerr))
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
		countGitOpsState(&topology.Summary, source.State, source.Pushed, source.Conflicted)
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
			// Mode is the effective one, so a jj slot, with or without
			// `mode: jj`, is not counted, though git registers it as a
			// worktree of a colocated cache.
			if link.Kind == "git" && link.Mode == manifest.WorkspaceSourceModeWorktree {
				topology.Summary.Worktrees++
			}
			countGitOpsState(&topology.Summary, link.State, link.Pushed, link.Conflicted)
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
		JJSlotState:    source.JJSlotState,
	}
}

// countGitOpsState counts a source or link of the topology by its state, and
// as unpushed or conflicted beside it.
func countGitOpsState(summary *api.GitOpsSummary, state string, pushed, conflicted bool) {
	normalized := strings.ToLower(state)
	// A clean or behind source can still hold commits of its own, such as a
	// detached HEAD with no base to count against.
	if !pushed && normalized != "error" {
		summary.Unpushed++
	}
	// jj records a conflict in a commit and goes on, so a jj slot in any
	// state can hold one.
	if conflicted {
		summary.Conflicted++
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
	stack, wsSource, source, path, err := p.workspaceSourceTarget(ctx, "fetch", workspaceName, slot)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if source.Kind != "git" && wsSource.Mode == manifest.WorkspaceSourceModeJJ {
		// A local store is the user's: angee never fetches into it, as
		// sync-base does not.
		return api.WorkspaceSourceStatus{}, &InvalidInputError{Field: "slot", Reason: fmt.Sprintf("source slot %q is a jj slot of local store %q, which angee never fetches into; rebase the slot onto its base as the store has it, with `angee workspace source rebase %s %s %s`", slot, wsSource.Source, workspaceName, slot, cmp.Or(workspaceSourceBaseRef(source, wsSource), "<base>"))}
	}
	if source.Kind != "git" {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q is not a git source", workspaceName, slot)
	}
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	if err := p.requireJJSlot(ctx, target); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	step := gitStep{action: "fetching", object: fmt.Sprintf("source %q", wsSource.Source), remote: driver.FetchURL(ctx, target), cause: CauseFetchFailed, slot: slot, source: wsSource.Source}
	if err := driver.Fetch(ctx, target); err != nil {
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
	stack, wsSource, source, path, err := p.workspaceSourceTarget(ctx, "pull", workspaceName, slot)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if source.Kind != "git" && wsSource.Mode != manifest.WorkspaceSourceModeJJ {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q is not a git source", workspaceName, slot)
	}
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	if err := requireSlotReady(ctx, driver, target, vcs.VerbPull, workspaceName, slot, wsSource.Source); err != nil {
		if target.Mode == vcs.ModeJJ && errors.Is(err, vcs.ErrUnsupported) {
			named := stack.Workspaces[workspaceName].Sources[slot].Mode != ""
			return api.WorkspaceSourceStatus{}, jjSlotNotPulled(err, workspaceName, slot, wsSource.Source, source, target, named)
		}
		return api.WorkspaceSourceStatus{}, err
	}
	step := gitStep{action: "pulling", object: fmt.Sprintf("source slot %q", slot), remote: driver.FetchURL(ctx, target), cause: CauseFetchFailed, slot: slot, source: wsSource.Source}
	if err := driver.Pull(ctx, target); err != nil {
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
	stack, wsSource, source, path, err := p.workspaceSourceTarget(ctx, "push", workspaceName, slot)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if source.Kind != "git" && wsSource.Mode != manifest.WorkspaceSourceModeJJ {
		return api.WorkspaceSourceStatus{}, fmt.Errorf("workspace %q source %q is not a git source", workspaceName, slot)
	}
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	if err := p.requireJJSlot(ctx, target); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if err := requireSlotReady(ctx, driver, target, vcs.VerbPush, workspaceName, slot, wsSource.Source); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	plan, err := planWorkspaceGitPush(ctx, driver, target, source, workspaceName, slot, wsSource, ref)
	if err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	if err := plan.run(ctx); err != nil {
		return api.WorkspaceSourceStatus{}, err
	}
	return p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack), nil
}

// workspaceSourceTarget resolves the slot verb names, for it to act on: the
// stack, the slot with its effective mode (withEffectiveMode), its source and
// its path, which must be materialized. A jj slot, one that names no mode
// under `vcs: jj` included, is refused for a verb not in jjSlotVerbs
// (jjSlotNotYet), so a verb added later never runs on one unawares.
func (p *Platform) workspaceSourceTarget(ctx context.Context, verb, workspaceName, slot string) (*manifest.Stack, manifest.WorkspaceSource, manifest.Source, string, error) {
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
	wsSource = withEffectiveMode(stack, wsSource)
	if wsSource.Mode == manifest.WorkspaceSourceModeJJ && !slices.Contains(jjSlotVerbs, verb) {
		refusal := jjSlotNotYet(verb, workspaceName, []string{slot}, []string{path})
		refusal.Source = wsSource.Source
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", refusal
	}
	if err := requireWorkspaceSlotsOnDisk(workspaceName, []workspaceSlotPath{{slot: slot, path: path, source: source, mode: wsSource.Mode}}); err != nil {
		return nil, manifest.WorkspaceSource{}, manifest.Source{}, "", err
	}
	return stack, wsSource, source, path, nil
}

// jjSlotVerbs are the slot verbs that reach a jj slot's driver: every one
// today (phase 8 of the jj plan). The driver refuses the ones that have no
// meaning there itself: pull (a jj slot takes its base in by a rebase), and
// the three that drive a merge or rebase stopped on conflicts, which a jj
// slot never has. A verb not listed is refused on a jj slot (jjSlotNotYet).
var jjSlotVerbs = []string{"fetch", "pull", "push", "publish", "diff", "merge", "rebase", "merge-abort", "rebase-abort", "rebase-continue"}

// workspaceSlotOperation names a verb on one workspace source slot, such as
// `Pull for workspace "src" slot "app"`.
func workspaceSlotOperation(name, verb, workspace, slot string) operation {
	return operation{name: name, title: fmt.Sprintf("%s for workspace %q slot %q", verb, workspace, slot), workspace: workspace, slot: slot}
}
