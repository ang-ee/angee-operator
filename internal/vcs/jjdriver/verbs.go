package jjdriver

import (
	"context"
	"fmt"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// noOperationInProgress is the error of the verbs that drive a merge or
// rebase stopped on conflicts: jj never stops one, it records the conflict
// in the commit and goes on, so a jj slot never has one to abort or
// continue. It wraps vcs.ErrUnsupported, which the service reports as
// invalid input.
func noOperationInProgress(verb string) error {
	return fmt.Errorf("%s: a jj slot never has a merge or rebase in progress, since jj records a conflict in the commit and goes on: %w", verb, vcs.ErrUnsupported)
}

// neverPulled is the error of a pull of a jj slot. jj has no pull: a jj slot
// takes in its base by being rebased onto it, which sync-base does, and the
// slot's own rebase onto a ref the caller names. It wraps vcs.ErrUnsupported.
func neverPulled() error {
	return fmt.Errorf("a jj slot is never pulled: jj takes in a base by rebasing the slot onto it, as %s does: %w", vcs.VerbSyncBase, vcs.ErrUnsupported)
}

// Ready answers a verb's preconditions on a jj slot. Each read snapshots the
// slot first.
//   - sync-base and rebase: no untracked paths (`jj status`'s, plan Q10),
//     refused with a *vcs.UntrackedPathsError: they are work only the slot
//     holds, and a rebase whose new base adds the same path keeps the local
//     file, which the next snapshot records as a change undoing the base's.
//     The working-copy commit rides along with the rebase, and there is no
//     branch to be on.
//   - publish, push and merge: no uncommitted changes and no untracked
//     paths, since each acts on the commit under the working copy, `@-`:
//     publish and push send it, and merge makes a commit on it and starts a
//     new working copy, so what the working copy holds, recorded or not, is
//     left out. Changes in the working-copy commit are a
//     *vcs.UncommittedError with jj's own command that commits them;
//     without any, untracked paths are a *vcs.UntrackedPathsError.
//   - destroy, and repair's check of a slot it made: none, and nothing runs.
//     The destroy guard reads the slot's own work from Status, and a jj slot
//     keeps no branch checked out.
//   - pull: refused (neverPulled), and nothing runs.
func (d Driver) Ready(ctx context.Context, s vcs.Slot, verb vcs.Verb) error {
	switch verb {
	case vcs.VerbSyncBase, vcs.VerbRebase:
		untracked, err := d.untracked(ctx, s.Path)
		if err != nil {
			return err
		}
		if len(untracked) > 0 {
			return &vcs.UntrackedPathsError{Paths: untracked}
		}
		return nil
	case vcs.VerbPublish, vcs.VerbPush, vcs.VerbMerge:
		changed, untracked, err := d.uncommitted(ctx, s.Path)
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			return &vcs.UncommittedError{Paths: changed, Fix: fmt.Sprintf("jj -R %s commit -m <message>", s.Path)}
		}
		if len(untracked) > 0 {
			return &vcs.UntrackedPathsError{Paths: untracked}
		}
		return nil
	case vcs.VerbDestroy, vcs.VerbRepair:
		return nil
	case vcs.VerbPull:
		return neverPulled()
	}
	return fmt.Errorf("no preconditions for %s on a jj slot: %w", verb, vcs.ErrUnsupported)
}

// SyncMethod syncs a jj slot by rebasing it. A merge is refused: a jj slot
// is not synced that way (plan D11), and rebasing when a merge was asked for
// would be worse than refusing.
func (Driver) SyncMethod(method string) (string, error) {
	switch method {
	case vcs.SyncDefault, vcs.SyncRebase:
		return vcs.SyncRebase, nil
	case vcs.SyncMerge:
		return "", fmt.Errorf("a jj slot is synced by rebasing, never by merging: %w", vcs.ErrUnsupported)
	}
	return "", fmt.Errorf("unknown sync method %q: %w", method, vcs.ErrUnsupported)
}

// BranchMismatch reports none: a jj slot keeps no branch checked out, and
// its bookmark moves with the commits it points at, so it is never off it.
func (Driver) BranchMismatch(context.Context, vcs.Slot) (string, error) {
	return "", nil
}

// DirtyPaths lists what the slot holds that no commit under its working copy
// does: the paths its working-copy commit changes, then the untracked paths
// jj leaves out of it (plan Q10), which only the slot holds. It snapshots the
// slot first, as every read does (D7).
func (d Driver) DirtyPaths(ctx context.Context, s vcs.Slot) ([]string, error) {
	changed, untracked, err := d.uncommitted(ctx, s.Path)
	if err != nil {
		return nil, err
	}
	return append(changed, untracked...), nil
}

// uncommitted snapshots the slot through `jj status` and returns the paths
// its working-copy commit changes and the untracked paths the report lists,
// both as that snapshot left them.
func (d Driver) uncommitted(ctx context.Context, dir string) (changed, untracked []string, err error) {
	untracked, op, err := d.statusSnapshot(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	out, err := d.jj.read(ctx, dir, op, "log", "--no-graph", "-r", workingCopy.String(), "-T", changedFilesTemplate)
	if err != nil {
		return nil, nil, err
	}
	lists, err := decodeLines[[]string](out)
	if err != nil {
		return nil, nil, fmt.Errorf("jj log -r @ in %s: %w", dir, err)
	}
	if len(lists) != 1 {
		return nil, nil, fmt.Errorf("jj log -r @ in %s printed %d lists of changed paths, want 1", dir, len(lists))
	}
	return lists[0], untracked, nil
}

// Pull is refused: a jj slot is never pulled (neverPulled).
func (Driver) Pull(context.Context, vcs.Slot) error { return neverPulled() }

// MergeAbort has nothing to abort on a jj slot.
func (Driver) MergeAbort(context.Context, vcs.Slot) (api.GitOpResult, error) {
	return api.GitOpResult{}, noOperationInProgress("merge-abort")
}

// RebaseAbort has nothing to abort on a jj slot.
func (Driver) RebaseAbort(context.Context, vcs.Slot) (api.GitOpResult, error) {
	return api.GitOpResult{}, noOperationInProgress("rebase-abort")
}

// RebaseContinue has nothing to continue on a jj slot.
func (Driver) RebaseContinue(context.Context, vcs.Slot) (api.GitOpResult, error) {
	return api.GitOpResult{}, noOperationInProgress("rebase-continue")
}
