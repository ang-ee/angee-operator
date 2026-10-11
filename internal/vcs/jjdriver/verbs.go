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
// in the commit and goes on.
func noOperationInProgress(verb string) error {
	return fmt.Errorf("%s: a jj slot never has a merge or rebase in progress: %w", verb, vcs.ErrUnsupported)
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

// Exists is not implemented yet: phase 5 step 6 adds the jj slot lifecycle.
func (Driver) Exists(context.Context, vcs.Slot) (bool, error) {
	return false, notYet("the registration check")
}

// Add is not implemented yet: phase 5 step 6 adds the jj slot lifecycle.
func (Driver) Add(context.Context, vcs.Slot, vcs.Dest, func(context.Context, string) error) error {
	return notYet("creating the slot")
}

// Remove is not implemented yet: phase 5 step 6 adds the jj slot lifecycle.
func (Driver) Remove(context.Context, vcs.Slot, vcs.Dest) error {
	return notYet("removing the slot")
}

// FetchURL names no remote yet.
func (Driver) FetchURL(context.Context, vcs.Slot) string { return "" }

// PushURL names no remote yet.
func (Driver) PushURL(context.Context, vcs.Slot, string) string { return "" }

// ResolveBase is not implemented yet.
func (Driver) ResolveBase(context.Context, vcs.Slot) (string, error) {
	return "", notYet("resolving the base")
}

// Fetch is not implemented yet.
func (Driver) Fetch(context.Context, vcs.Slot) error { return notYet("fetch") }

// Pull is not implemented yet.
func (Driver) Pull(context.Context, vcs.Slot) error { return notYet("pull") }

// PlanPush is not implemented yet.
func (Driver) PlanPush(context.Context, vcs.Slot, string, string) (vcs.PushPlan, error) {
	return vcs.PushPlan{}, notYet("push")
}

// Push is not implemented yet.
func (Driver) Push(context.Context, vcs.Slot, vcs.PushPlan) error { return notYet("push") }

// SyncBase is not implemented yet.
func (Driver) SyncBase(context.Context, vcs.Slot, string, string) (api.GitOpResult, error) {
	return api.GitOpResult{}, notYet("sync-base")
}

// Merge is not implemented yet.
func (Driver) Merge(context.Context, vcs.Slot, string) (api.GitOpResult, error) {
	return api.GitOpResult{}, notYet("merge")
}

// Rebase is not implemented yet.
func (Driver) Rebase(context.Context, vcs.Slot, string) (api.GitOpResult, error) {
	return api.GitOpResult{}, notYet("rebase")
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
