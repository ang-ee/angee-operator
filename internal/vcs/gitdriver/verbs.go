package gitdriver

import (
	"context"
	"errors"
	"fmt"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// Ready answers a verb's preconditions. sync-base, pull and push need a
// checkout that must be on a branch (Slot.Branch) to be on it, and to have no
// uncommitted changes, which a merge or rebase would mix into the base's and
// a push would leave behind. Destroy, and repair's check of a slot it made,
// need only the branch: the destroy guard reads what the checkout holds from
// Status. A slot's own rebase, publish and merge need nothing: git refuses a
// rebase over uncommitted changes, and a merge that would overwrite one,
// itself, and publish pushes what is committed, creating the branch on a
// detached HEAD.
func (d Driver) Ready(ctx context.Context, s vcs.Slot, verb vcs.Verb) error {
	switch verb {
	case vcs.VerbSyncBase, vcs.VerbPull, vcs.VerbPush:
		if err := d.onBranch(ctx, s); err != nil {
			return err
		}
		paths, err := d.DirtyPaths(ctx, s)
		if err != nil {
			return err
		}
		if len(paths) > 0 {
			return &vcs.UncommittedError{Paths: paths}
		}
		return nil
	case vcs.VerbDestroy, vcs.VerbRepair:
		return d.onBranch(ctx, s)
	case vcs.VerbRebase, vcs.VerbPublish, vcs.VerbMerge:
		return nil
	}
	return fmt.Errorf("no preconditions for %s on a git slot: %w", verb, vcs.ErrUnsupported)
}

// onBranch refuses a checkout that is not on Slot.Branch, when it must be on
// one, with git's own command that puts it back.
func (d Driver) onBranch(ctx context.Context, s vcs.Slot) error {
	if s.Branch == "" {
		return nil
	}
	reason, err := d.BranchMismatch(ctx, s)
	if err != nil {
		return err
	}
	if reason != "" {
		return &vcs.BranchMismatchError{Reason: reason, Fix: fmt.Sprintf("git -C %s switch %s", s.Path, s.Branch)}
	}
	return nil
}

// SyncMethod syncs by merging unless rebasing is asked for.
func (Driver) SyncMethod(method string) (string, error) {
	switch method {
	case vcs.SyncDefault, vcs.SyncMerge:
		return vcs.SyncMerge, nil
	case vcs.SyncRebase:
		return vcs.SyncRebase, nil
	}
	return "", fmt.Errorf("unknown sync method %q: %w", method, vcs.ErrUnsupported)
}

// BranchMismatch says how the checkout differs from the branch it must be on.
func (d Driver) BranchMismatch(ctx context.Context, s vcs.Slot) (string, error) {
	currentRef, err := d.git.CurrentRef(ctx, s.Path)
	if err != nil {
		return "", err
	}
	return branchMismatch(currentRef, s.Branch), nil
}

// DirtyPaths lists the checkout's uncommitted changes, as git status sees them.
func (d Driver) DirtyPaths(ctx context.Context, s vcs.Slot) ([]string, error) {
	return d.git.DirtyPaths(ctx, s.Path)
}

// FetchURL is the URL of the remote the slot's base comes from
// (Slot.BaseRemote), else of origin, else of the checkout's only remote.
func (d Driver) FetchURL(ctx context.Context, s vcs.Slot) string {
	for _, remote := range []string{s.BaseRemote, "origin"} {
		if remote == "" {
			continue
		}
		if url, ok, err := d.git.RemoteURL(ctx, s.Path, remote); err == nil && ok {
			return url
		}
	}
	remotes, err := d.git.Remotes(ctx, s.Path)
	if err != nil || len(remotes) != 1 {
		return ""
	}
	if url, ok, err := d.git.RemoteURL(ctx, s.Path, remotes[0]); err == nil && ok {
		return url
	}
	return ""
}

// PushURL is the URL of remote, or of the remote git resolves a push from the
// checkout to when remote is empty.
func (d Driver) PushURL(ctx context.Context, s vcs.Slot, remote string) string {
	if remote == "" {
		var err error
		if remote, err = d.git.PushRemote(ctx, s.Path); err != nil {
			return ""
		}
	}
	url, ok, err := d.git.RemoteURL(ctx, s.Path, remote)
	if err != nil || !ok {
		return ""
	}
	return url
}

// ResolveBase returns the ref the slot's base is synced from after a fetch,
// as git.Client.SyncBaseRef resolves it: the remote branch the local base
// tracks, else the base on Slot.BaseRemote, else on another remote, else the
// local base. When none exists, the error, in git.Client's words, is a
// vcs.ErrBaseNotFound; a git command that failed is returned as it is.
func (d Driver) ResolveBase(ctx context.Context, s vcs.Slot) (string, error) {
	ref, err := d.git.SyncBaseRef(ctx, s.Path, s.BaseRef, s.BaseRemote)
	var command *git.CommandError
	if err != nil && !errors.As(err, &command) {
		return "", baseNotFound{err}
	}
	return ref, err
}

// baseNotFound is a missing base, reported in git.Client's words.
type baseNotFound struct{ err error }

func (e baseNotFound) Error() string { return e.err.Error() }

func (e baseNotFound) Unwrap() []error { return []error{e.err, vcs.ErrBaseNotFound} }

// Fetch fetches every remote, pruning deleted branches.
func (d Driver) Fetch(ctx context.Context, s vcs.Slot) error {
	return d.git.Fetch(ctx, s.Path)
}

// Pull fast-forwards the checkout's branch to its upstream.
func (d Driver) Pull(ctx context.Context, s vcs.Slot) error {
	return d.git.Pull(ctx, s.Path)
}

// PlanPush decides a push without pushing. An explicit ref is pushed as
// given. Otherwise a branch with an upstream pushes to it, and one without
// pushes branch and tracks it, unless HEAD holds nothing of its own. The
// count is of HEAD, so it only speaks for the push when HEAD is what would be
// pushed: on no branch, or on the branch the manifest names (or any branch
// when it names none).
func (d Driver) PlanPush(ctx context.Context, s vcs.Slot, ref, branch string) (vcs.PushPlan, error) {
	if ref != "" {
		return vcs.PushPlan{Ref: ref}, nil
	}
	current, onBranch, err := d.git.CurrentBranch(ctx, s.Path)
	if err != nil {
		return vcs.PushPlan{}, err
	}
	_, hasUpstream, err := d.git.Upstream(ctx, s.Path)
	if err != nil || hasUpstream {
		return vcs.PushPlan{}, err
	}
	if !onBranch || branch == "" || current == branch {
		if ahead, known := commitsBeyondBase(ctx, d.git, s.Path, s.BaseRef, s.BaseRemote); known && ahead == 0 {
			return vcs.PushPlan{Skip: true}, nil
		}
	}
	if !onBranch {
		return vcs.PushPlan{}, vcs.ErrDetachedWithWork
	}
	return vcs.PushPlan{Track: branch}, nil
}

// Push carries out a plan: the branch to track pushed with -u, else the ref,
// or the branch to its upstream when the plan names no ref. A skipped plan
// pushes nothing.
func (d Driver) Push(ctx context.Context, s vcs.Slot, plan vcs.PushPlan) error {
	switch {
	case plan.Skip:
		return nil
	case plan.Track != "":
		return d.git.PushSetUpstream(ctx, s.Path, plan.Track)
	default:
		return d.git.Push(ctx, s.Path, plan.Ref)
	}
}

// SyncBase merges onto into the checkout's branch, or rebases the branch onto
// it; vcs.SyncDefault merges. Unlike Merge and Rebase, it runs as whoever
// runs the operator, with their environment: syncing replays or merges their
// own work.
func (d Driver) SyncBase(ctx context.Context, s vcs.Slot, onto, method string) (api.GitOpResult, error) {
	var err error
	switch method {
	case vcs.SyncDefault, vcs.SyncMerge:
		err = d.git.Merge(ctx, s.Path, onto)
	case vcs.SyncRebase:
		err = d.git.Rebase(ctx, s.Path, onto)
	default:
		return api.GitOpResult{}, fmt.Errorf("unknown sync method %q", method)
	}
	return d.syncResult(ctx, s.Path, err)
}

// CheckRebase lets every rebase run: git rebases only the checkout's branch,
// which git lets no other worktree check out, and leaves the commits it
// replays where they were for anything else built on them.
func (Driver) CheckRebase(context.Context, vcs.Slot, string) error { return nil }

// Merge merges ref with --no-ff, so a merge commit is recorded even when the
// branch could fast-forward. The commit is the user's (commitIdentity).
func (d Driver) Merge(ctx context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, d.commitIdentity(ctx, s.Path), "merge", "--no-ff", "--no-edit", git.EndOfOptions, ref)
}

// Rebase rebases the checkout's branch onto ref. On conflict the checkout
// stays in the rebasing state until RebaseContinue or RebaseAbort. The
// replayed commits keep their authors and are committed by the user
// (commitIdentity).
func (d Driver) Rebase(ctx context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, d.commitIdentity(ctx, s.Path), "rebase", git.EndOfOptions, ref)
}

// MergeAbort aborts an in-progress merge.
func (d Driver) MergeAbort(ctx context.Context, s vcs.Slot) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, nil, "merge", "--abort")
}

// RebaseAbort aborts an in-progress rebase.
func (d Driver) RebaseAbort(ctx context.Context, s vcs.Slot) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, nil, "rebase", "--abort")
}

// RebaseContinue continues an in-progress rebase without opening an editor,
// committing as Rebase does.
func (d Driver) RebaseContinue(ctx context.Context, s vcs.Slot) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, d.commitIdentity(ctx, s.Path), "-c", "core.editor=true", "rebase", "--continue")
}

// PlanPublish decides a publish without changing anything. The remote is the
// intent's, else git's push remote for the checkout (git.Client.PushRemote:
// the branch's pushRemote, remote.pushDefault, the branch's remote, origin,
// the only remote); when git names none, that is a *vcs.NoPushRemoteError
// with git's reason. A remote the checkout has no URL for is refused. The
// branch is the intent's, else the one HEAD is on. Unless the caller named
// the branch, a branch that tracks nothing and holds no commit beyond the
// slot's base has nothing to publish; the count is of HEAD, so it only speaks
// when HEAD is what would be pushed. On a detached HEAD the branch is created at HEAD
// first, and an existing branch of that name is not reused: it may point
// elsewhere. An unmanaged checkout skips both rules.
func (d Driver) PlanPublish(ctx context.Context, s vcs.Slot, intent vcs.PublishIntent) (vcs.PublishPlan, error) {
	current, onBranch, err := d.git.CurrentBranch(ctx, s.Path)
	if err != nil {
		return vcs.PublishPlan{}, fmt.Errorf("resolve HEAD branch in %s: %w", s.Path, err)
	}
	remote := intent.Remote
	if remote == "" {
		if remote, err = d.git.PushRemote(ctx, s.Path); err != nil {
			var none *git.NoPushRemoteError
			if errors.As(err, &none) {
				return vcs.PublishPlan{}, &vcs.NoPushRemoteError{Reason: none.Reason}
			}
			return vcs.PublishPlan{}, fmt.Errorf("resolve the push remote in %s: %w", s.Path, err)
		}
	}
	url, ok, err := d.git.RemoteURL(ctx, s.Path, remote)
	if err != nil {
		return vcs.PublishPlan{}, err
	}
	if !ok {
		return vcs.PublishPlan{}, &vcs.UnknownRemoteError{Remote: remote, Resolved: intent.Remote == ""}
	}
	branch := intent.Branch
	if branch == "" && onBranch {
		branch = current
	}
	if !intent.Unmanaged && !intent.Explicit && (!onBranch || branch == current) {
		_, hasUpstream, err := d.git.Upstream(ctx, s.Path)
		if err != nil {
			return vcs.PublishPlan{}, err
		}
		if !hasUpstream {
			if ahead, known := commitsBeyondBase(ctx, d.git, s.Path, s.BaseRef, s.BaseRemote); known && ahead == 0 {
				return vcs.PublishPlan{}, vcs.ErrNothingToPublish
			}
		}
	}
	if branch == "" {
		// Publishing a detached HEAD as "HEAD" would create refs/heads/HEAD
		// on the remote.
		return vcs.PublishPlan{}, vcs.ErrNoBranch
	}
	plan := vcs.PublishPlan{Remote: remote, URL: url, Branch: branch}
	if !intent.Unmanaged && !onBranch {
		if d.git.RefExists(ctx, s.Path, "refs/heads/"+branch) {
			return vcs.PublishPlan{}, vcs.ErrBranchExists
		}
		plan.Create = true
	}
	return plan, nil
}

// PreparePublish creates the plan's branch at HEAD and switches to it, when
// the plan says to. If the push then fails, the checkout stays on that branch
// and a retry pushes it.
func (d Driver) PreparePublish(ctx context.Context, s vcs.Slot, plan vcs.PublishPlan) error {
	if !plan.Create {
		return nil
	}
	if _, err := d.git.Run(ctx, s.Path, "switch", "-c", plan.Branch); err != nil {
		return fmt.Errorf("create branch %q in %s: %w", plan.Branch, s.Path, err)
	}
	return nil
}

// Publish pushes the plan's branch to its remote and sets it as the branch's
// upstream. The push is a network operation like any other:
// ANGEE_GIT_TIMEOUT bounds it, so a stalled remote fails instead of hanging.
func (d Driver) Publish(ctx context.Context, s vcs.Slot, plan vcs.PublishPlan) (api.GitOpResult, error) {
	var result api.GitOpResult
	err := git.RunNetworkOperation(ctx, "git push --set-upstream "+plan.Remote+" "+plan.Branch, s.Path, func(ctx context.Context) error {
		var runErr error
		// A full refspec, so the branch is only ever a branch name: git refuses
		// one that carries refspec syntax (":" deletes, "+" forces).
		refspec := "refs/heads/" + plan.Branch + ":refs/heads/" + plan.Branch
		result, runErr = d.runOp(ctx, s.Path, nil, "push", "--set-upstream", git.EndOfOptions, plan.Remote, refspec)
		return runErr
	})
	return result, err
}
