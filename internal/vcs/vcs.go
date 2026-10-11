// Package vcs is the seam between the service and the version control system
// behind a workspace slot. The service owns what the manifest says about a
// slot (its path, its base, the branch it must be on) and which verbs may run;
// a Driver owns how the slot's checkout answers for it, carries the verbs out,
// and makes and removes the checkout. Git is the only driver today; jj slots
// add a second one.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/ang-ee/angee-operator/api"
)

// Slot modes: how a slot's checkout is made, which Driver.Add and
// Driver.Remove follow.
const (
	// ModeWorktree is a checkout linked to Slot.Primary, which keeps its
	// branches and lists it among its checkouts.
	ModeWorktree = "worktree"
	// ModeClone is a repository of its own, cloned from Slot.CloneURL.
	ModeClone = "clone"
)

// Slot identifies one slot checkout and the facts a driver needs about it.
type Slot struct {
	Path string // the checkout
	// Mode, Primary and CloneURL say how the checkout is made; only the
	// lifecycle (Exists, Add, Remove) reads them. Mode is ModeWorktree or
	// ModeClone. Primary is the repository a worktree is a checkout of, its
	// source's cache; CloneURL is the URL a clone is cloned from, as the
	// remote BaseRemote names.
	Mode     string
	Primary  string
	CloneURL string
	Branch   string // the branch the checkout must be on; empty when any ref will do
	BaseRef  string // the slot's ref, else its source's default ref; may be empty
	// BaseRemote is the remote BaseRef comes from: the source's clone remote.
	// Empty lets the driver pick (git: origin).
	BaseRemote string
}

// Dest is the capability a driver makes every write to a slot's path
// through. The service opens it on Slot.Path, inside the stack root, before
// it calls Driver.Add or Driver.Remove, and closes it; a driver never opens
// the slot path itself. copierx.GuardedPath satisfies it.
type Dest interface {
	// Lstat returns the entry at the path as the capability holds it, and
	// whether there is one; it fails when the entry changed since.
	Lstat() (fs.FileInfo, bool, error)
	// ReplaceFrom installs a copy of the tree at stage as the path.
	ReplaceFrom(ctx context.Context, stage string) error
	// VerifyPathIdentity confirms that path, looked up by name, still leads
	// to the entry the capability installed.
	VerifyPathIdentity(path string) error
	// RemoveAll removes the entry at the path, if any.
	RemoveAll() error
}

// Status is what a driver reads from a slot's checkout.
type Status struct {
	CurrentRef string
	Upstream   string // empty without an upstream
	Dirty      bool
	// Ahead and Behind count against Upstream when it is set, else against
	// the base, which each driver resolves from BaseRef its own way: git
	// counts against the local BaseRef, or its remote counterpart when there
	// is no local one; jj against the bookmark on BaseRemote, else the local
	// bookmark.
	Ahead, Behind int
	// MismatchReason is set when the checkout is not on Slot.Branch.
	MismatchReason string
	// Pushed reports that nothing in the checkout is held by it alone, so
	// destroying it loses no work. UnpushedReason says what is held when it is
	// not.
	Pushed         bool
	UnpushedReason string

	// The fields below are read from jj slots only; the git driver leaves
	// them unset, and the API does not carry them yet.

	// ChangeID is the change id of the slot's working-copy commit.
	ChangeID string
	// BaseBehind counts the commits the base has that the slot lacks, so a
	// moved base shows even when Behind counts against an upstream.
	BaseBehind int
	// Conflicted reports a conflicted commit among the slot's own (jj records
	// a conflict in the commit and goes on), and ConflictPaths lists the
	// conflicted files, sorted.
	Conflicted    bool
	ConflictPaths []string
}

// Sync methods for Driver.SyncBase. SyncDefault leaves the choice to the
// driver: its version control system's natural way to bring a base in (git:
// a merge).
const (
	SyncDefault = ""
	SyncMerge   = "merge"
	SyncRebase  = "rebase"
)

// PushPlan is what pushing a slot will do, decided by Driver.PlanPush before
// anything is pushed, so a push of several slots can refuse before any of
// them is pushed.
type PushPlan struct {
	// Ref is pushed as it is when the caller named one.
	Ref string
	// Track is the branch to push and track there, for a checkout whose
	// branch has nothing it pushes to yet.
	Track string
	// Skip reports that the checkout has nothing of its own to push: no
	// upstream, and no commits beyond its base.
	Skip bool
}

// PublishIntent is what a caller asked publish for.
type PublishIntent struct {
	// Remote is the remote to publish to. Empty lets the driver resolve the
	// remote its version control system pushes the checkout to (git: its
	// push remote, else origin).
	Remote string
	// Branch is the branch to publish under; empty publishes the branch the
	// checkout is on.
	Branch string
	// Explicit reports that the caller named Branch, rather than the manifest
	// supplying it; a checkout with nothing of its own is then published
	// anyway.
	Explicit bool
	// Unmanaged marks a checkout the operator does not manage as a
	// repository, such as a local source's directory: publish pushes its
	// branch as it stands, without the nothing-to-publish check and without
	// creating a branch.
	Unmanaged bool
}

// PublishPlan is what publishing a slot does, decided by Driver.PlanPublish
// before anything changes.
type PublishPlan struct {
	Remote string
	URL    string // Remote's URL, for messages
	Branch string
	// Create reports that Branch does not exist yet and PreparePublish creates
	// it where the checkout stands.
	Create bool
}

var (
	// ErrBranchExists is returned by Driver.PlanPublish when the branch to
	// create already exists.
	ErrBranchExists = errors.New("branch already exists")
	// ErrNoBranch is returned by Driver.PlanPublish when the checkout is on no
	// branch and the caller named none.
	ErrNoBranch = errors.New("no branch to publish")
	// ErrNothingToPublish is returned by Driver.PlanPublish when the checkout
	// has nothing of its own: no upstream and no commits beyond its base.
	ErrNothingToPublish = errors.New("nothing to publish")
	// ErrDetachedWithWork is returned by Driver.PlanPush when the checkout is
	// on no branch but holds commits of its own, so there is no branch to
	// push them on.
	ErrDetachedWithWork = errors.New("no branch to push")
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
	// Exists reports whether Slot.Primary lists a checkout at Slot.Path, as a
	// leftover of an earlier create can still be listed. A clone is a
	// repository of its own, which nothing lists.
	Exists(ctx context.Context, s Slot) (bool, error)
	// Add makes the slot's checkout at Slot.Path, writing there only through
	// d, which holds nothing or an empty directory: a worktree of
	// Slot.Primary on Slot.Branch (created at the base unless it exists) or,
	// without a branch, detached at the base; or a clone of Slot.CloneURL at
	// the base. configure, when not nil, runs on the new checkout before it
	// reaches Slot.Path, so nothing there can use it unconfigured. Add leaves
	// nothing behind when it fails.
	Add(ctx context.Context, s Slot, d Dest, configure func(ctx context.Context, checkout string) error) error
	// Remove removes the checkout through d and drops whatever Slot.Primary
	// keeps of it. For a git worktree that is `git worktree prune`, which
	// drops every registration in the cache whose checkout is missing, not
	// only this slot's.
	Remove(ctx context.Context, s Slot, d Dest) error

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
	// PlanPush decides what pushing the checkout does without pushing: ref as
	// it is when the caller named one, else the checkout's branch to what it
	// tracks, or, untracked, branch (the manifest's branch for the slot) to
	// track. A checkout with nothing of its own is skipped; one on no branch
	// with commits of its own is refused with ErrDetachedWithWork.
	PlanPush(ctx context.Context, s Slot, ref, branch string) (PushPlan, error)
	// Push carries out a plan PlanPush returned.
	Push(ctx context.Context, s Slot, plan PushPlan) error
	// SyncBase brings onto, a ref ResolveBase returned, into the checkout's
	// branch by method (SyncMerge, SyncRebase, or SyncDefault for the
	// driver's own choice). A conflict is a result, not an error.
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
	// PlanPublish decides what publishing the checkout does without changing
	// anything: the remote (the intent's, else the one the checkout pushes
	// to), the branch to publish (the intent's, else the checkout's) and
	// whether it must be created. It refuses a remote the checkout does not
	// have with *UnknownRemoteError, and otherwise with ErrNothingToPublish,
	// ErrNoBranch or ErrBranchExists.
	PlanPublish(ctx context.Context, s Slot, intent PublishIntent) (PublishPlan, error)
	// PreparePublish gets the checkout ready for a plan PlanPublish returned:
	// it creates the plan's branch where the checkout stands when the plan
	// says so. Its errors are the checkout's, not the remote's.
	PreparePublish(ctx context.Context, s Slot, plan PublishPlan) error
	// Publish pushes the plan's branch to its remote under the same name and
	// tracks it there.
	Publish(ctx context.Context, s Slot, plan PublishPlan) (api.GitOpResult, error)
}

// UnknownRemoteError is returned by Driver.PlanPublish for a remote the
// checkout does not have. Resolved marks a remote the checkout's own push
// configuration named, rather than the caller.
type UnknownRemoteError struct {
	Remote   string
	Resolved bool
}

func (e *UnknownRemoteError) Error() string { return fmt.Sprintf("no remote %q", e.Remote) }

// NoPushRemoteError is returned by Driver.PlanPublish when the caller names
// no remote and the checkout's push configuration resolves none. Reason is
// the version-control tool's own account, saying what to set.
type NoPushRemoteError struct {
	Reason string
}

func (e *NoPushRemoteError) Error() string { return "no push remote: " + e.Reason }

// CountError reports that Status decided whether a checkout is pushed but
// could not count it against its upstream or base, for example because the
// base branch was deleted. The Status returned with it has a valid Pushed and
// UnpushedReason; its Ahead and Behind are not set.
type CountError struct {
	Err error
}

func (e *CountError) Error() string { return e.Err.Error() }

func (e *CountError) Unwrap() error { return e.Err }
