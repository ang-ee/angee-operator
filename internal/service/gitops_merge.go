package service

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// WorkspaceSourceMerge merges `ref` into the workspace source slot's
// current branch with `--no-ff` (mirrors the safe-default a human would
// pick). On conflict the worktree is left in conflicted state for the
// caller to resolve and the response carries the conflicting paths. A jj
// slot, whose working copy must hold no changes (vcs.Driver.Ready), gets the
// merge as the commit under a fresh working copy (`jj new @- <ref>`, then
// `jj new`), so a publish after it sends the merge; ref is in jj's spelling
// (`main@origin`), and a conflict is recorded in the merge commit and
// answered as OK and conflicted.
func (p *Platform) WorkspaceSourceMerge(ctx context.Context, workspace, slot, ref string) (result api.GitOpResult, err error) {
	op := workspaceSlotOperation("workspace.source-merge", "Merge", workspace, slot)
	defer op.annotate(&err)
	if strings.TrimSpace(ref) == "" {
		return api.GitOpResult{}, &InvalidInputError{Field: "ref", Reason: "merge ref is required"}
	}
	return p.runReadyWorkspaceGitOp(ctx, op, gitStep{action: fmt.Sprintf("merging %s into", ref)}, workspace, slot, vcs.VerbMerge, func(ctx context.Context, d vcs.Driver, s vcs.Slot) (api.GitOpResult, error) {
		return d.Merge(ctx, s, ref)
	})
}

// WorkspaceSourceRebase rebases the current branch onto `ref`. On conflict
// a git worktree stays in the rebasing state; callers must
// `workspaceSourceRebaseContinue` or `workspaceSourceRebaseAbort`. A jj slot
// is rebased with `jj rebase -b @ --onto <ref>` (ref in jj's spelling, such
// as `main@origin`), its working-copy commit included: a conflict is
// recorded in the commits and reported as OK and conflicted, and a rebase
// that would rewrite another checkout's commits is refused (shared_commits).
func (p *Platform) WorkspaceSourceRebase(ctx context.Context, workspace, slot, ref string) (result api.GitOpResult, err error) {
	op := workspaceSlotOperation("workspace.source-rebase", "Rebase", workspace, slot)
	defer op.annotate(&err)
	if strings.TrimSpace(ref) == "" {
		return api.GitOpResult{}, &InvalidInputError{Field: "ref", Reason: "rebase ref is required"}
	}
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: fmt.Sprintf("rebasing onto %s", ref)}, workspace, slot, func(ctx context.Context, d vcs.Driver, s vcs.Slot) (api.GitOpResult, error) {
		if s.Mode == vcs.ModeJJ {
			if err := p.requireJJSlots(ctx); err != nil {
				return api.GitOpResult{}, err
			}
		}
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
// Without a remote it publishes to the remote the slot's driver resolves:
// the push remote git itself would use (the branch's pushRemote, then
// remote.pushDefault, which the cache takes from the source's push_default,
// then the branch's remote, origin, or the only remote), for a jj slot too
// (plan Q8). When git names none, the caller is asked to pass one. A remote
// the slot does not have is refused, and so are the canonical remote and a
// branch that matches none of the target remote's declared `branches`
// patterns, all before anything changes.
//
// The slot's driver decides what is published (vcs.Driver.PlanPublish), once
// it says the slot is ready (vcs.Driver.Ready). For a git source, two cases
// differ from a plain push. Without an explicit branch, a slot with no
// upstream and no commits beyond its base has nothing to publish and is left
// alone (naming the branch publishes it anyway). A slot on a detached HEAD
// gets the named branch created at HEAD first; if the push then fails, the
// slot stays on that branch and a retry pushes it. A jj slot must hold no
// uncommitted changes, since publish sends the commit under its working
// copy, `@-`: its commits since the branch's remote bookmark, else since its
// base, are refused when jj would not push some (a conflict, no
// description), are nothing to publish when there are none, and otherwise
// get the branch set on `@-` and pushed (`jj git push`), which force-updates,
// with jj's lease, a remote branch a rebase rewrote.
func (p *Platform) WorkspaceSourcePublish(ctx context.Context, workspace, slot, remote, branch string) (result api.GitOpResult, err error) {
	defer workspaceSlotOperation("workspace.source-publish", "Publish", workspace, slot).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.GitOpResult{}, err
	}
	defer release()
	_, wsSource, source, path, err := p.workspaceSourceTarget(ctx, "publish", workspace, slot)
	if err != nil {
		return api.GitOpResult{}, err
	}
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	if err := p.requireJJSlot(ctx, target); err != nil {
		return api.GitOpResult{}, err
	}
	if err := requireSlotReady(ctx, driver, target, vcs.VerbPublish, workspace, slot, wsSource.Source); err != nil {
		return api.GitOpResult{}, err
	}
	intent := vcs.PublishIntent{Remote: remote, Branch: branch, Explicit: branch != "", Unmanaged: source.Kind != "git"}
	if intent.Branch == "" {
		intent.Branch = wsSource.Branch
	}
	plan, err := driver.PlanPublish(ctx, target, intent)
	var unknownRemote *vcs.UnknownRemoteError
	var noPushRemote *vcs.NoPushRemoteError
	var nothing *vcs.NothingToPublishError
	switch {
	case err == nil:
	case errors.As(err, &unknownRemote) && unknownRemote.Resolved:
		return api.GitOpResult{}, &InvalidInputError{Field: "remote", Reason: fmt.Sprintf("source slot %q pushes to remote %q by its own configuration, but has no such remote; pass a remote, or fix the slot's push remote", slot, unknownRemote.Remote)}
	case errors.As(err, &unknownRemote):
		return api.GitOpResult{}, &InvalidInputError{Field: "remote", Reason: fmt.Sprintf("source slot %q has no remote %q", slot, unknownRemote.Remote)}
	case errors.As(err, &noPushRemote):
		return api.GitOpResult{}, &InvalidInputError{Field: "remote", Reason: fmt.Sprintf("source slot %q has no push remote to default to (%s); pass a remote, or declare push_default", slot, noPushRemote.Reason)}
	case errors.As(err, &nothing):
		return api.GitOpResult{OK: true, ConflictFiles: []string{}, Message: nothing.Error()}, nil
	case errors.Is(err, vcs.ErrNothingToPublish):
		return api.GitOpResult{OK: true, ConflictFiles: []string{}, Message: fmt.Sprintf("nothing to publish: no commits beyond %s; pass a branch to publish anyway", target.BaseRef)}, nil
	case errors.Is(err, vcs.ErrNoBranch) && target.Mode == vcs.ModeJJ:
		// A jj slot is on no branch: its commits go under the one named.
		return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: "the jj slot names no branch to publish its commits under; pass an explicit branch"}
	case errors.Is(err, vcs.ErrNoBranch):
		// A detached HEAD has no branch name to fall back on, so the caller
		// must name the branch.
		return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: "worktree is in detached HEAD; pass an explicit branch"}
	case errors.Is(err, vcs.ErrBranchExists):
		return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: fmt.Sprintf("worktree is in detached HEAD and branch %q already exists; switch to it or pass another branch", intent.Branch)}
	case errors.Is(err, vcs.ErrBaseNotFound):
		return api.GitOpResult{}, baseNotFound(err, slot, wsSource.Source, target.BaseRef)
	default:
		if refusal := unpushableCommits(err, slot, wsSource.Source); refusal != nil {
			return api.GitOpResult{}, refusal
		}
		return api.GitOpResult{}, gitFailure(err, gitStep{action: "planning the publish of", object: fmt.Sprintf("source slot %q", slot), slot: slot, source: wsSource.Source})
	}
	// The manifest's rules for the target remote are checked before the
	// driver creates or sets a branch, so a refused publish changes nothing.
	// git's push block on the canonical remote still refuses any push made
	// outside angee.
	if err := publishRulesRefusal(source, wsSource.Source, plan); err != nil {
		return api.GitOpResult{}, err
	}
	// A slot cut without a branch starts detached, and a jj slot is on none;
	// publishing names its branch, so the driver creates or sets it for the
	// push below.
	if err := driver.PreparePublish(ctx, target, plan); err != nil {
		action := fmt.Sprintf("setting branch %q in", plan.Branch)
		if plan.Create {
			action = fmt.Sprintf("creating branch %q in", plan.Branch)
		}
		return api.GitOpResult{}, gitFailure(err, gitStep{action: action, object: fmt.Sprintf("source slot %q", slot), slot: slot, source: wsSource.Source})
	}
	step := gitStep{action: fmt.Sprintf("pushing branch %q of", plan.Branch), object: fmt.Sprintf("source slot %q", slot), remote: plan.URL, cause: CausePushFailed, slot: slot, source: wsSource.Source}
	result, err = driver.Publish(ctx, target, plan)
	return result, gitFailure(err, step)
}

// publishRulesRefusal refuses a publish the manifest's rules for its remote
// forbid, before anything changes: one to a remote the source declares
// canonical (fetch-only), or of a branch that matches none of the remote's
// declared `branches`. A remote the manifest does not declare has no rules.
func publishRulesRefusal(source manifest.Source, sourceName string, plan vcs.PublishPlan) error {
	declared := source.DeclaredRemotes()[plan.Remote]
	if declared.Canonical {
		return &InvalidInputError{Field: "remote", Reason: fmt.Sprintf("remote %q of source %q is canonical (fetch-only) in angee.yaml; publish to another remote", plan.Remote, sourceName)}
	}
	if len(declared.Branches) > 0 && !matchesAnyBranchPattern(declared.Branches, plan.Branch) {
		return &InvalidInputError{Field: "branch", Reason: fmt.Sprintf("remote %q of source %q accepts only branches matching %s; %q matches none", plan.Remote, sourceName, strings.Join(declared.Branches, ", "), plan.Branch)}
	}
	return nil
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
// here. The verb a refusal names is the operation's, as the CLI spells it
// (`merge-abort` for workspace.source-merge-abort).
func (p *Platform) runWorkspaceGitOp(ctx context.Context, op operation, step gitStep, workspace, slot string, run slotOp) (api.GitOpResult, error) {
	return p.runReadyWorkspaceGitOp(ctx, op, step, workspace, slot, "", run)
}

// runReadyWorkspaceGitOp is runWorkspaceGitOp for an operation whose
// preconditions the slot's driver answers (vcs.Driver.Ready) as verb, which
// it asks first, on a machine that can run a jj slot's jj (requireJJSlot);
// an empty verb asks nothing.
func (p *Platform) runReadyWorkspaceGitOp(ctx context.Context, op operation, step gitStep, workspace, slot string, verb vcs.Verb, run slotOp) (result api.GitOpResult, err error) {
	defer op.annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.GitOpResult{}, err
	}
	defer release()
	_, wsSource, source, path, err := p.workspaceSourceTarget(ctx, strings.TrimPrefix(op.name, "workspace.source-"), workspace, slot)
	if err != nil {
		return api.GitOpResult{}, err
	}
	step.object = fmt.Sprintf("source slot %q", slot)
	step.slot = slot
	step.source = wsSource.Source
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	if verb != "" {
		if err := p.requireJJSlot(ctx, target); err != nil {
			return api.GitOpResult{}, err
		}
		if err := requireSlotReady(ctx, driver, target, verb, workspace, slot, wsSource.Source); err != nil {
			return api.GitOpResult{}, err
		}
	}
	result, err = run(ctx, driver, target)
	if refusal := rebaseRefusal(err, workspace, slot, wsSource.Source); refusal != nil {
		return result, refusal
	}
	if err != nil && result.OK {
		// The operation was made; what failed is reading what it left.
		step.action = "reading what " + step.action + " left in"
	}
	return result, gitFailure(err, step)
}
