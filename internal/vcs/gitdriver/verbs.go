package gitdriver

import (
	"context"
	"errors"
	"fmt"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

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

// CurrentBranch returns the branch HEAD is on; onBranch is false on a
// detached HEAD.
func (d Driver) CurrentBranch(ctx context.Context, s vcs.Slot) (string, bool, error) {
	return d.git.CurrentBranch(ctx, s.Path)
}

// Upstream returns the upstream of the checkout's branch, if it has one.
func (d Driver) Upstream(ctx context.Context, s vcs.Slot) (string, bool, error) {
	return d.git.Upstream(ctx, s.Path)
}

// CommitsBeyondBase counts the commits HEAD reaches that neither the slot's
// base nor its remote counterpart on Slot.BaseRemote reaches
// (commitsBeyondBase).
func (d Driver) CommitsBeyondBase(ctx context.Context, s vcs.Slot) (int, bool) {
	return commitsBeyondBase(ctx, d.git, s.Path, s.BaseRef, s.BaseRemote)
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

// Push pushes ref to the push remote, or the branch to its upstream when ref
// is empty.
func (d Driver) Push(ctx context.Context, s vcs.Slot, ref string) error {
	return d.git.Push(ctx, s.Path, ref)
}

// PushSetUpstream pushes branch to the push remote and tracks it there.
func (d Driver) PushSetUpstream(ctx context.Context, s vcs.Slot, branch string) error {
	return d.git.PushSetUpstream(ctx, s.Path, branch)
}

// SyncBase merges onto into the checkout's branch, or rebases the branch onto
// it. Unlike Merge and Rebase, it runs as whoever runs the operator, with
// their environment: syncing replays or merges their own work.
func (d Driver) SyncBase(ctx context.Context, s vcs.Slot, onto, method string) (api.GitOpResult, error) {
	var err error
	switch method {
	case vcs.SyncMerge:
		err = d.git.Merge(ctx, s.Path, onto)
	case vcs.SyncRebase:
		err = d.git.Rebase(ctx, s.Path, onto)
	default:
		return api.GitOpResult{}, fmt.Errorf("unknown sync method %q", method)
	}
	return d.syncResult(ctx, s.Path, err)
}

// Merge merges ref with --no-ff, so a merge commit is recorded even when the
// branch could fast-forward.
func (d Driver) Merge(ctx context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, "merge", "--no-ff", "--no-edit", git.EndOfOptions, ref)
}

// Rebase rebases the checkout's branch onto ref. On conflict the checkout
// stays in the rebasing state until RebaseContinue or RebaseAbort.
func (d Driver) Rebase(ctx context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, "rebase", git.EndOfOptions, ref)
}

// MergeAbort aborts an in-progress merge.
func (d Driver) MergeAbort(ctx context.Context, s vcs.Slot) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, "merge", "--abort")
}

// RebaseAbort aborts an in-progress rebase.
func (d Driver) RebaseAbort(ctx context.Context, s vcs.Slot) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, "rebase", "--abort")
}

// RebaseContinue continues an in-progress rebase without opening an editor.
func (d Driver) RebaseContinue(ctx context.Context, s vcs.Slot) (api.GitOpResult, error) {
	return d.runOp(ctx, s.Path, "-c", "core.editor=true", "rebase", "--continue")
}

// CreateBranch creates branch at HEAD and switches to it. An existing branch
// of that name is not reused: it may point elsewhere.
func (d Driver) CreateBranch(ctx context.Context, s vcs.Slot, branch string) error {
	if d.git.RefExists(ctx, s.Path, "refs/heads/"+branch) {
		return vcs.ErrBranchExists
	}
	_, err := d.git.Run(ctx, s.Path, "switch", "-c", branch)
	return err
}

// Publish pushes branch to remote and sets it as the branch's upstream. The
// push is a network operation like any other: ANGEE_GIT_TIMEOUT bounds it, so
// a stalled remote fails instead of hanging.
func (d Driver) Publish(ctx context.Context, s vcs.Slot, remote, branch string) (api.GitOpResult, error) {
	var result api.GitOpResult
	err := git.RunNetworkOperation(ctx, "git push --set-upstream "+remote+" "+branch, s.Path, func(ctx context.Context) error {
		var runErr error
		// A full refspec, so the branch is only ever a branch name: git refuses
		// one that carries refspec syntax (":" deletes, "+" forces).
		refspec := "refs/heads/" + branch + ":refs/heads/" + branch
		result, runErr = d.runOp(ctx, s.Path, "push", "--set-upstream", git.EndOfOptions, remote, refspec)
		return runErr
	})
	return result, err
}
