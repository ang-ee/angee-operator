package service

import (
	"context"
	"errors"
	"fmt"
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
// (default "origin") under the named branch (default: the current branch),
// setting upstream tracking if not already configured. Useful for
// publishing a workspace branch for the first time so an external
// reviewer can open a PR against it.
//
// For a git source, two cases differ from a plain push. Without an explicit
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
	if remote == "" {
		remote = "origin"
	}
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	currentBranch, onBranch, err := driver.CurrentBranch(ctx, target)
	if err != nil {
		return api.GitOpResult{}, fmt.Errorf("resolve HEAD branch in %s: %w", path, err)
	}
	explicit := branch != ""
	if branch == "" {
		branch = wsSource.Branch
	}
	if branch == "" && onBranch {
		branch = currentBranch
	}
	isGit := source.Kind == "git"
	// The count is of HEAD, so it only speaks for the push when HEAD is what
	// would be pushed.
	if isGit && !explicit && (!onBranch || branch == currentBranch) {
		_, hasUpstream, err := driver.Upstream(ctx, target)
		if err != nil {
			return api.GitOpResult{}, err
		}
		if !hasUpstream {
			if ahead, known := driver.CommitsBeyondBase(ctx, target); known && ahead == 0 {
				return api.GitOpResult{OK: true, ConflictFiles: []string{}, Message: fmt.Sprintf("nothing to publish: no commits beyond %s; pass a branch to publish anyway", target.BaseRef)}, nil
			}
		}
	}
	if branch == "" {
		// Detached HEAD has no branch name to fall back on, and publishing
		// it as "HEAD" would create refs/heads/HEAD on the remote, so the
		// caller must name the branch.
		return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: "worktree is in detached HEAD; pass an explicit branch"}
	}
	if isGit && !onBranch {
		// A slot cut without a branch starts detached; publishing names its
		// branch, so create it at HEAD for the push below to track.
		if err := driver.CreateBranch(ctx, target, branch); errors.Is(err, vcs.ErrBranchExists) {
			return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: fmt.Sprintf("worktree is in detached HEAD and branch %q already exists; switch to it or pass another branch", branch)}
		} else if err != nil {
			return api.GitOpResult{}, fmt.Errorf("create branch %q in %s: %w", branch, path, err)
		}
	}
	step := gitStep{action: fmt.Sprintf("pushing branch %q of", branch), object: fmt.Sprintf("source slot %q", slot), remote: driver.PushURL(ctx, target, remote), cause: CausePushFailed, slot: slot, source: wsSource.Source}
	result, err = driver.Publish(ctx, target, remote, branch)
	return result, gitFailure(err, step)
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
