// Package vcs is the seam between the service and the version control system
// behind a workspace slot. The service owns what the manifest says about a
// slot (its path, its base, the branch it must be on) and which verbs may run;
// a Driver owns how the slot's checkout answers for it and carries the verbs
// out. Git is the only driver today; jj slots add a second one.
package vcs

import (
	"context"
	"errors"

	"github.com/ang-ee/angee-operator/api"
)

// Slot identifies one slot checkout and the facts a driver needs about it.
type Slot struct {
	Path    string // the checkout
	Branch  string // the branch the checkout must be on; empty when any ref will do
	BaseRef string // the slot's ref, else its source's default ref; may be empty
	// BaseRemote is the remote BaseRef comes from: the source's clone remote.
	// Empty lets the driver pick (git: origin).
	BaseRemote string
}

// Status is what a driver reads from a slot's checkout.
type Status struct {
	CurrentRef string
	Upstream   string // empty without an upstream
	Dirty      bool
	// Ahead and Behind count against Upstream when it is set, else against
	// BaseRef, or BaseRef's remote counterpart when there is no local one.
	Ahead, Behind int
	// MismatchReason is set when the checkout is not on Slot.Branch.
	MismatchReason string
	// Pushed reports that nothing in the checkout is held by it alone, so
	// destroying it loses no work. UnpushedReason says what is held when it is
	// not.
	Pushed         bool
	UnpushedReason string
}

// Sync methods for Driver.SyncBase.
const (
	SyncMerge  = "merge"
	SyncRebase = "rebase"
)

var (
	// ErrBranchExists is returned by Driver.CreateBranch when the branch
	// exists.
	ErrBranchExists = errors.New("branch already exists")
	// ErrBaseNotFound is wrapped by Driver.ResolveBase when the slot's base
	// exists neither locally nor on a remote.
	ErrBaseNotFound = errors.New("base not found")
	// ErrUnsupported is wrapped by a driver method that has no meaning for its
	// kind of slot, such as aborting a merge in a version control system that
	// never stops mid-merge. The service reports it as invalid input.
	ErrUnsupported = errors.New("not supported for this slot")
)

// Driver reads and acts on one kind of slot checkout. The service decides from
// the manifest and from what the driver reads whether a verb may run and what
// it targets; the driver carries the verb out. Errors are the version control
// system's own, for the service to report.
type Driver interface {
	// Status reads the checkout. On error, the returned Status holds what was
	// read before the failure; with a *CountError, that includes Pushed and
	// UnpushedReason.
	Status(ctx context.Context, s Slot) (Status, error)
	// Diff returns the checkout's uncommitted changes when ref is empty, else
	// the committed range from the checkout to ref.
	Diff(ctx context.Context, s Slot, ref string) ([]api.DiffFile, error)

	// BranchMismatch says how the checkout differs from Slot.Branch, or
	// returns "" when it is on it or Slot.Branch is empty.
	BranchMismatch(ctx context.Context, s Slot) (string, error)
	// DirtyPaths lists the checkout's uncommitted changes.
	DirtyPaths(ctx context.Context, s Slot) ([]string, error)
	// CurrentBranch returns the branch the checkout is on; onBranch is false
	// when it is on none.
	CurrentBranch(ctx context.Context, s Slot) (branch string, onBranch bool, err error)
	// Upstream returns the branch the checkout's branch tracks, if any.
	Upstream(ctx context.Context, s Slot) (upstream string, ok bool, err error)
	// CommitsBeyondBase counts the checkout's commits that neither Slot.BaseRef
	// nor its remote counterpart holds. known is false when that cannot be
	// counted.
	CommitsBeyondBase(ctx context.Context, s Slot) (ahead int, known bool)
	// FetchURL is the URL of the remote a fetch or pull names in a message,
	// or "" when no single one does.
	FetchURL(ctx context.Context, s Slot) string
	// PushURL is the URL of remote, or of the remote a push goes to when
	// remote is empty; "" when it cannot be resolved.
	PushURL(ctx context.Context, s Slot, remote string) string
	// ResolveBase returns the ref Slot.BaseRef is synced from after a fetch:
	// its remote counterpart when there is one. A base that does not exist
	// is an error wrapping ErrBaseNotFound.
	ResolveBase(ctx context.Context, s Slot) (string, error)

	// Fetch updates what the checkout knows of its remotes.
	Fetch(ctx context.Context, s Slot) error
	// Pull fast-forwards the checkout's branch to its upstream.
	Pull(ctx context.Context, s Slot) error
	// Push pushes ref to the push remote, or the branch to its upstream when
	// ref is empty.
	Push(ctx context.Context, s Slot, ref string) error
	// PushSetUpstream pushes branch to the push remote and tracks it there.
	PushSetUpstream(ctx context.Context, s Slot, branch string) error
	// SyncBase brings onto, a ref ResolveBase returned, into the checkout's
	// branch by method (SyncMerge or SyncRebase). A conflict is a result, not
	// an error.
	SyncBase(ctx context.Context, s Slot, onto, method string) (api.GitOpResult, error)
	// Merge merges ref into the checkout's branch, always recording a merge.
	// A conflict is a result, not an error.
	Merge(ctx context.Context, s Slot, ref string) (api.GitOpResult, error)
	// Rebase rebases the checkout's branch onto ref. A conflict is a result,
	// not an error.
	Rebase(ctx context.Context, s Slot, ref string) (api.GitOpResult, error)
	// MergeAbort, RebaseAbort and RebaseContinue drive a checkout that Merge
	// or Rebase left stopped on conflicts. MergeAbort and RebaseAbort return
	// it to where the operation started; RebaseContinue goes on once the
	// conflicts are resolved, and may stop on the next. A driver whose
	// version control system never leaves an operation in progress returns
	// an error wrapping ErrUnsupported.
	MergeAbort(ctx context.Context, s Slot) (api.GitOpResult, error)
	RebaseAbort(ctx context.Context, s Slot) (api.GitOpResult, error)
	RebaseContinue(ctx context.Context, s Slot) (api.GitOpResult, error)
	// CreateBranch creates branch at the checkout's current commit and
	// switches to it, or returns ErrBranchExists.
	CreateBranch(ctx context.Context, s Slot, branch string) error
	// Publish pushes branch to remote under the same name and tracks it.
	Publish(ctx context.Context, s Slot, remote, branch string) (api.GitOpResult, error)
}

// CountError reports that Status decided whether a checkout is pushed but
// could not count it against its upstream or base, for example because the
// base branch was deleted. The Status returned with it has a valid Pushed and
// UnpushedReason; its Ahead and Behind are not set.
type CountError struct {
	Err error
}

func (e *CountError) Error() string { return e.Err.Error() }

func (e *CountError) Unwrap() error { return e.Err }
