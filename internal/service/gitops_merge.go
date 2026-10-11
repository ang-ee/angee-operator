package service

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// WorkspaceSourceMerge merges `ref` into the workspace source slot's
// current branch with `--no-ff` (mirrors the safe-default a human would
// pick). On conflict the worktree is left in conflicted state for the
// caller to resolve and the response carries the conflicting paths.
func (p *Platform) WorkspaceSourceMerge(ctx context.Context, workspace, slot, ref string) (result api.GitOpResult, err error) {
	op := workspaceSlotOperation("workspace.source-merge", "Merge", workspace, slot)
	defer op.annotate(&err)
	if strings.TrimSpace(ref) == "" {
		return api.GitOpResult{}, &InvalidInputError{Field: "ref", Reason: "merge ref is required"}
	}
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: fmt.Sprintf("merging %s into", ref)}, workspace, slot, func(ctx context.Context, d vcs.Driver, s vcs.Slot) (api.GitOpResult, error) {
		return d.Merge(ctx, s, ref)
	})
}

// WorkspaceSourceRebase rebases the current branch onto `ref`. On conflict
// the worktree stays in the rebasing state; callers must
// `workspaceSourceRebaseContinue` or `workspaceSourceRebaseAbort`.
func (p *Platform) WorkspaceSourceRebase(ctx context.Context, workspace, slot, ref string) (result api.GitOpResult, err error) {
	op := workspaceSlotOperation("workspace.source-rebase", "Rebase", workspace, slot)
	defer op.annotate(&err)
	if strings.TrimSpace(ref) == "" {
		return api.GitOpResult{}, &InvalidInputError{Field: "ref", Reason: "rebase ref is required"}
	}
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: fmt.Sprintf("rebasing onto %s", ref)}, workspace, slot, func(ctx context.Context, d vcs.Driver, s vcs.Slot) (api.GitOpResult, error) {
		return d.Rebase(ctx, s, ref)
	})
}

// WorkspaceSourceMergeAbort aborts an in-progress merge.
func (p *Platform) WorkspaceSourceMergeAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	op := workspaceSlotOperation("workspace.source-merge-abort", "Abort merge", workspace, slot)
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: "aborting the merge in"}, workspace, slot, func(ctx context.Context, d vcs.Driver, s vcs.Slot) (api.GitOpResult, error) {
		return d.MergeAbort(ctx, s)
	})
}

// WorkspaceSourceRebaseAbort aborts an in-progress rebase.
func (p *Platform) WorkspaceSourceRebaseAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	op := workspaceSlotOperation("workspace.source-rebase-abort", "Abort rebase", workspace, slot)
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: "aborting the rebase in"}, workspace, slot, func(ctx context.Context, d vcs.Driver, s vcs.Slot) (api.GitOpResult, error) {
		return d.RebaseAbort(ctx, s)
	})
}

// WorkspaceSourceRebaseContinue continues an in-progress rebase after the
// caller has resolved conflicts in the worktree.
func (p *Platform) WorkspaceSourceRebaseContinue(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	op := workspaceSlotOperation("workspace.source-rebase-continue", "Continue rebase", workspace, slot)
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: "continuing the rebase in"}, workspace, slot, func(ctx context.Context, d vcs.Driver, s vcs.Slot) (api.GitOpResult, error) {
		return d.RebaseContinue(ctx, s)
	})
}

// WorkspaceSourcePublish pushes the worktree's branch to the named remote
// under the named branch (default: the current branch), setting upstream
// tracking if not already configured. Useful for publishing a workspace
// branch for the first time so an external reviewer can open a PR against
// it.
//
// Without a remote it publishes to the remote the slot's driver resolves: for
// git, the push remote git itself would use (the branch's pushRemote, then
// remote.pushDefault, which the cache takes from the source's push_default,
// then the branch's remote, origin, or the only remote). When git names none,
// the caller is asked to pass one. A remote the slot does not have is
// refused, and so are the canonical remote and a branch that matches none of
// the target remote's declared `branches` patterns, all before anything
// changes.
//
// The slot's driver decides what is published (vcs.Driver.PlanPublish). For
// a git source, two cases differ from a plain push. Without an explicit
// branch, a slot with no upstream and no commits beyond its base has nothing
// to publish and is left alone (naming the branch publishes it anyway). A slot
// on a detached HEAD gets the named branch created at HEAD first; if the push
// then fails, the slot stays on that branch and a retry pushes it.
func (p *Platform) WorkspaceSourcePublish(ctx context.Context, workspace, slot, remote, branch string) (result api.GitOpResult, err error) {
	defer workspaceSlotOperation("workspace.source-publish", "Publish", workspace, slot).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.GitOpResult{}, err
	}
	defer release()
	_, wsSource, source, path, err := p.workspaceSourceTarget(ctx, workspace, slot)
	if err != nil {
		return api.GitOpResult{}, err
	}
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	intent := vcs.PublishIntent{Remote: remote, Branch: branch, Explicit: branch != "", Unmanaged: source.Kind != "git"}
	if intent.Branch == "" {
		intent.Branch = wsSource.Branch
	}
	plan, err := driver.PlanPublish(ctx, target, intent)
	var unknownRemote *vcs.UnknownRemoteError
	var noPushRemote *vcs.NoPushRemoteError
	switch {
	case errors.As(err, &unknownRemote) && unknownRemote.Resolved:
		return api.GitOpResult{}, &InvalidInputError{Field: "remote", Reason: fmt.Sprintf("source slot %q pushes to remote %q by its own configuration, but has no such remote; pass a remote, or fix the slot's push remote", slot, unknownRemote.Remote)}
	case errors.As(err, &unknownRemote):
		return api.GitOpResult{}, &InvalidInputError{Field: "remote", Reason: fmt.Sprintf("source slot %q has no remote %q", slot, unknownRemote.Remote)}
	case errors.As(err, &noPushRemote):
		return api.GitOpResult{}, &InvalidInputError{Field: "remote", Reason: fmt.Sprintf("source slot %q has no push remote to default to (%s); pass a remote, or declare push_default", slot, noPushRemote.Reason)}
	case errors.Is(err, vcs.ErrNothingToPublish):
		return api.GitOpResult{OK: true, ConflictFiles: []string{}, Message: fmt.Sprintf("nothing to publish: no commits beyond %s; pass a branch to publish anyway", target.BaseRef)}, nil
	case errors.Is(err, vcs.ErrNoBranch):
		// A detached HEAD has no branch name to fall back on, so the caller
		// must name the branch.
		return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: "worktree is in detached HEAD; pass an explicit branch"}
	case errors.Is(err, vcs.ErrBranchExists):
		return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: fmt.Sprintf("worktree is in detached HEAD and branch %q already exists; switch to it or pass another branch", intent.Branch)}
	case err != nil:
		return api.GitOpResult{}, err
	}
	// The manifest's rules for the target remote apply to a declared remote
	// only, and are checked before the driver creates a branch on a detached
	// slot, so a refused publish changes nothing. git's push block on the
	// canonical remote still refuses any push made outside angee.
	declared := source.DeclaredRemotes()[plan.Remote]
	if declared.Canonical {
		return api.GitOpResult{}, &InvalidInputError{Field: "remote", Reason: fmt.Sprintf("remote %q of source %q is canonical (fetch-only) in angee.yaml; publish to another remote", plan.Remote, wsSource.Source)}
	}
	if len(declared.Branches) > 0 && !matchesAnyBranchPattern(declared.Branches, plan.Branch) {
		return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: fmt.Sprintf("remote %q of source %q accepts only branches matching %s; %q matches none", plan.Remote, wsSource.Source, strings.Join(declared.Branches, ", "), plan.Branch)}
	}
	// A slot cut without a branch starts detached; publishing names its
	// branch, so the driver creates it for the push below to track.
	if err := driver.PreparePublish(ctx, target, plan); err != nil {
		return api.GitOpResult{}, err
	}
	step := gitStep{action: fmt.Sprintf("pushing branch %q of", plan.Branch), object: fmt.Sprintf("source slot %q", slot), remote: plan.URL, cause: CausePushFailed, slot: slot, source: wsSource.Source}
	result, err = driver.Publish(ctx, target, plan)
	return result, gitFailure(err, step)
}

// matchesAnyBranchPattern reports whether branch matches one of patterns, in
// path.Match syntax, where `*` does not cross a `/`. A pattern path.Match
// rejects counts as no match; ValidateSource refuses such patterns when the
// manifest loads, so none reaches here.
func matchesAnyBranchPattern(patterns []string, branch string) bool {
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, branch); err == nil && ok {
			return true
		}
	}
	return false
}

// slotOp is one driver operation on a slot that may stop on conflicts.
type slotOp func(ctx context.Context, d vcs.Driver, s vcs.Slot) (api.GitOpResult, error)

// runWorkspaceGitOp runs a local operation in a slot through its driver. step
// names the action for a failure's message; its object and slot are filled in
// here.
func (p *Platform) runWorkspaceGitOp(ctx context.Context, op operation, step gitStep, workspace, slot string, run slotOp) (result api.GitOpResult, err error) {
	defer op.annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.GitOpResult{}, err
	}
	defer release()
	_, wsSource, source, path, err := p.workspaceSourceTarget(ctx, workspace, slot)
	if err != nil {
		return api.GitOpResult{}, err
	}
	step.object = fmt.Sprintf("source slot %q", slot)
	step.slot = slot
	step.source = wsSource.Source
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	result, err = run(ctx, driver, target)
	return result, gitFailure(err, step)
}
