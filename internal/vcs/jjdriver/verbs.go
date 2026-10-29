package jjdriver

import (
	"context"
	"fmt"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// notYet is the error of every verb phase 8 has not implemented on a jj slot,
// and of the git-shaped reads the service asks before them.
func notYet(verb string) error {
	return fmt.Errorf("%s on a jj slot is not implemented yet: %w", verb, vcs.ErrUnsupported)
}

// noOperationInProgress is the error of the verbs that drive a merge or
// rebase stopped on conflicts: jj never stops one, it records the conflict
// in the commit and goes on, so a jj slot never has one to abort or
// continue. It wraps vcs.ErrUnsupported, which the service reports as
// invalid input.
func noOperationInProgress(verb string) error {
	return fmt.Errorf("%s: a jj slot never has a merge or rebase in progress, since jj records a conflict in the commit and goes on: %w", verb, vcs.ErrUnsupported)
}

// Ready lets sync-base run on a jj slot as it stands: the working-copy commit
// rides along with the rebase, and there is no branch to be on.
func (Driver) Ready(_ context.Context, _ vcs.Slot, verb vcs.Verb) error {
	if verb != vcs.VerbSyncBase {
		return notYet("the preconditions of " + string(verb))
	}
	return nil
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

// BranchMismatch is not implemented yet: phase 8 answers the verb
// preconditions per driver.
func (Driver) BranchMismatch(context.Context, vcs.Slot) (string, error) {
	return "", notYet("the branch check")
}

// DirtyPaths is not implemented yet: phase 8 answers the verb preconditions
// per driver.
func (Driver) DirtyPaths(context.Context, vcs.Slot) ([]string, error) {
	return nil, notYet("the uncommitted-changes check")
}

// PushURL names no remote yet.
func (Driver) PushURL(context.Context, vcs.Slot, string) string { return "" }

// Pull is not implemented yet.
func (Driver) Pull(context.Context, vcs.Slot) error { return notYet("pull") }

// PlanPush is not implemented yet.
func (Driver) PlanPush(context.Context, vcs.Slot, string, string) (vcs.PushPlan, error) {
	return vcs.PushPlan{}, notYet("push")
}

// Push is not implemented yet.
func (Driver) Push(context.Context, vcs.Slot, vcs.PushPlan) error { return notYet("push") }

// Merge is not implemented yet.
func (Driver) Merge(context.Context, vcs.Slot, string) (api.GitOpResult, error) {
	return api.GitOpResult{}, notYet("merge")
}

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

// PlanPublish is not implemented yet.
func (Driver) PlanPublish(context.Context, vcs.Slot, vcs.PublishIntent) (vcs.PublishPlan, error) {
	return vcs.PublishPlan{}, notYet("publish")
}

// PreparePublish is not implemented yet.
func (Driver) PreparePublish(context.Context, vcs.Slot, vcs.PublishPlan) error {
	return notYet("publish")
}

// Publish is not implemented yet.
func (Driver) Publish(context.Context, vcs.Slot, vcs.PublishPlan) (api.GitOpResult, error) {
	return api.GitOpResult{}, notYet("publish")
}
