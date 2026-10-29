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
	"strings"

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
	// ModeJJ is a jj workspace of Slot.Primary named Slot.Name, the jj
	// driver's one kind of slot.
	ModeJJ = "jj"
)

// Slot identifies one slot checkout and the facts a driver needs about it.
type Slot struct {
	Path string // the checkout
	// Mode, Primary, CloneURL and Name say how the checkout is made; only the
	// lifecycle (Exists, Add, Remove) reads them, except that ModeJJ also
	// selects the jj driver for a slot's reads and verbs. Mode is ModeWorktree
	// or ModeClone, for the git driver, or ModeJJ. Primary is the repository a
	// worktree or a jj slot is a checkout of: its source's cache, or a local
	// source's jj store; CloneURL is the URL a clone is cloned from, as the
	// remote BaseRemote names.
	Mode     string
	Primary  string
	CloneURL string
	// Name is the slot's name in Primary, for a primary that keeps its
	// checkouts by name: the jj workspace name. The service computes it
	// (`<stack>--<workspace>`, plus `--<slot>` when one workspace has two
	// slots of one repository; plan Q2) and does not store it. The jj
	// lifecycle reads it; jj's reads and verbs run in the slot, where jj
	// knows its own workspace, and the git driver ignores it.
	Name    string
	Branch  string // the branch the checkout must be on; empty when any ref will do
	BaseRef string // the slot's ref, else its source's default ref; may be empty
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
	// IsEmptyDirectory reports whether the entry is an empty real directory.
	// It fails when there is no entry or it is not a real directory.
	IsEmptyDirectory() (bool, error)
	// MkdirAll makes the path a directory, creating missing parents through
	// the capability, and keeps an existing real directory. From then on the
	// capability holds that directory, for a checkout made in place.
	MkdirAll(mode fs.FileMode) error
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
	// them unset. The service hands them to the API as api.JJSlotState.

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
	// UntrackedPaths lists the new paths jj reports leaving out of the
	// working-copy commit, relative to the checkout as jj lists them (a
	// directory ends in "/"): files over its snapshot.max-new-file-size or
	// outside its snapshot.auto-track, never ignored ones. Only the checkout
	// holds them, so they make it Dirty, as git's untracked files do. jj
	// neither records nor reports a nested repository or a new file outside
	// the checkout's sparse patterns, so those are not here, and do not make
	// it Dirty.
	UntrackedPaths []string
}

// Sync methods for Driver.SyncBase. SyncDefault leaves the choice to the
// driver: its version control system's natural way to bring a base in (git:
// a merge).
const (
	SyncDefault = ""
	SyncMerge   = "merge"
	SyncRebase  = "rebase"
)

// Verb names a verb whose preconditions Driver.Ready answers.
type Verb string

// The verbs Driver.Ready answers.
const (
	// VerbSyncBase is sync-base: bringing the slot's base into it.
	VerbSyncBase Verb = "sync-base"
	// VerbRebase is a slot's rebase onto a ref a caller names.
	VerbRebase Verb = "rebase"
	// VerbPull is pull: bringing the commits of the checkout's upstream into
	// it.
	VerbPull Verb = "pull"
	// VerbPush is push, of one slot or of every slot of a workspace: sending
	// the checkout's commits to its remote.
	VerbPush Verb = "push"
	// VerbPublish is publish: sending the checkout's commits to a remote under
	// a branch, which it creates where it does not exist.
	VerbPublish Verb = "publish"
	// VerbMerge is merge: recording a merge of a ref into the checkout.
	VerbMerge Verb = "merge"
	// VerbDestroy is destroying the workspace that holds the checkout. What
	// the checkout holds that nothing else does is the destroy guard's to
	// read, from Status; Ready answers what else must hold.
	VerbDestroy Verb = "destroy"
	// VerbRepair is repair's check of a checkout it has just made: that the
	// verbs after it can run there.
	VerbRepair Verb = "repair"
)

// BranchMismatchError is returned by Driver.Ready when the checkout is not on
// Slot.Branch, which the verb needs. Reason says how it differs; Fix is the
// version-control tool's own command that puts it back, for a message.
type BranchMismatchError struct {
	Reason string
	Fix    string
}

func (e *BranchMismatchError) Error() string { return "not on its branch: " + e.Reason }

// UncommittedError is returned by Driver.Ready when the verb needs a checkout
// with no uncommitted changes and it has some, which Paths lists. Fix is the
// version-control tool's own command that records them, for a message, when
// it has one that does.
type UncommittedError struct {
	Paths []string
	Fix   string
}

func (e *UncommittedError) Error() string {
	return "uncommitted changes in " + strings.Join(e.Paths, ", ")
}

// NotReadyError is returned by Driver.Ready when the checkout cannot take the
// verb for a reason of its version control's own, which Reason says, such as
// a jj working-copy commit with two parents, which leaves no one commit to
// publish. Fix, when set, is the tool's own command that changes that.
type NotReadyError struct {
	Reason string
	Fix    string
}

func (e *NotReadyError) Error() string { return e.Reason }

// UntrackedPathsError is returned by Driver.Ready when the verb would move
// the checkout under files only it holds and its version control does not
// track (for jj, the paths `jj status` lists as untracked: over
// snapshot.max-new-file-size, or outside snapshot.auto-track). A rebase
// whose new base adds such a path keeps the local file, and the next
// snapshot records it as a change that undoes the base's. Paths lists them.
type UntrackedPathsError struct {
	Paths []string
}

func (e *UntrackedPathsError) Error() string {
	return "untracked paths only the checkout holds: " + NamePaths(e.Paths, 10)
}

// Warning is what a version-control tool warned of while it made an
// operation, in its own words: jj's "1 of those updates were skipped", say,
// when the working copy held a file (an ignored one) where the new commit
// has one, which the next snapshot then records as a change. A driver
// returns it beside an OK result.
type Warning struct {
	Text string
}

func (e *Warning) Error() string { return e.Text }

// CheckedOutBranch is a branch a git worktree of a primary has checked out,
// and where.
type CheckedOutBranch struct {
	Branch string
	Path   string
}

// SharedCommitsError is returned by Driver.CheckRebase, and by a rebase that
// checks it, when the commits rebasing the checkout would rewrite are not the
// checkout's alone (plan D12): jj rewrites the descendants of what it moves,
// so another checkout's work built on them would be rewritten too. Workspaces
// names the jj workspaces whose working-copy commit is among them, and
// Branches the git worktree branches that point at one, which jj would move
// under their worktrees.
type SharedCommitsError struct {
	Workspaces []string
	Branches   []CheckedOutBranch
}

func (e *SharedCommitsError) Error() string {
	var held []string
	for _, name := range e.Workspaces {
		held = append(held, fmt.Sprintf("jj workspace %q", name))
	}
	for _, branch := range e.Branches {
		held = append(held, fmt.Sprintf("branch %q checked out at %s", branch.Branch, branch.Path))
	}
	return "the rebase would rewrite commits that other checkouts hold: " + strings.Join(held, ", ")
}

// ImmutableCommitsError is returned by Driver.CheckRebase, and by a rebase
// that checks it, when the commits the rebase would rewrite include ones the
// version control system refuses to rewrite: for jj, the ancestors of its
// immutable_heads() (trunk(), a tag, an untracked remote bookmark). Commits
// lists them by short id.
type ImmutableCommitsError struct {
	Commits []string
}

func (e *ImmutableCommitsError) Error() string {
	return "the rebase would rewrite commits jj holds immutable: " + strings.Join(e.Commits, ", ")
}

// UnpushableCommitsError is returned by Driver.PlanPublish, and by
// Driver.PlanPush where a push publishes, when commits it would send to the
// remote are ones the version control system refuses to push (plan D6): for
// jj, a commit with conflicts (Conflicted) or with no description
// (Undescribed), each listed by short commit id. Nothing is pushed or
// changed.
type UnpushableCommitsError struct {
	Conflicted  []string
	Undescribed []string
}

func (e *UnpushableCommitsError) Error() string {
	var reasons []string
	if len(e.Conflicted) > 0 {
		reasons = append(reasons, "conflicts in "+strings.Join(e.Conflicted, ", "))
	}
	if len(e.Undescribed) > 0 {
		reasons = append(reasons, "no description on "+strings.Join(e.Undescribed, ", "))
	}
	return "commits jj will not push: " + strings.Join(reasons, "; ")
}

// BranchTakenError is returned by Driver.PlanPublish, and by Driver.PlanPush
// where a push publishes, when the branch to publish under is not the
// checkout's to set: a git worktree of the same primary has it checked out
// (Worktree), which moving it would leave on no branch, or the remote already
// has a branch of that name that the repository does not track (Remote), so
// jj would not push over it. Nothing is changed.
type BranchTakenError struct {
	Branch   string
	Worktree string
	Remote   string
}

func (e *BranchTakenError) Error() string {
	if e.Worktree != "" {
		return fmt.Sprintf("branch %q is checked out by the git worktree at %s", e.Branch, e.Worktree)
	}
	return fmt.Sprintf("remote %q already has a branch %q that this repository does not track", e.Remote, e.Branch)
}

// BranchMovedError is returned by Driver.PlanPublish, and by Driver.PlanPush
// where a push publishes, when the branch holds work the checkout does not:
// commits that are not versions of the checkout's own, on a remote (Remote
// set: the one publishing goes to, or another whose branch holds them) or
// on the repository's own branch (Remote empty), as a
// collaborator's push leaves it once fetched. Publishing would move the
// branch past them and drop them from the remote. Commits lists them by
// short id. Nothing is changed.
//
// Remote, when set, also says that the branch on that remote holds every one
// of Commits, so merging it takes them in. Heads is set instead when no one
// branch names a commit that holds them all (the repository's own branch is
// conflicted, and no remote's holds them): the commits of Commits no other
// descends from, by short id, each of which the checkout merges in turn.
type BranchMovedError struct {
	Branch  string
	Remote  string
	Commits []string
	Heads   []string
}

func (e *BranchMovedError) Error() string {
	where := fmt.Sprintf("branch %q", e.Branch)
	if e.Remote != "" {
		where = fmt.Sprintf("branch %q on %s", e.Branch, e.Remote)
	}
	return where + " holds commits the slot does not: " + strings.Join(e.Commits, ", ")
}

// NothingToPublishError is ErrNothingToPublish with the driver's account of
// what the checkout's commits were measured against, for a driver that says
// more than "no commits beyond the base".
type NothingToPublishError struct {
	Reason string
}

func (e *NothingToPublishError) Error() string { return "nothing to publish: " + e.Reason }

func (e *NothingToPublishError) Unwrap() error { return ErrNothingToPublish }

// ToolError is a failed version-control command as a driver returns it, so a
// message can quote the tool's own words without knowing which tool ran.
type ToolError interface {
	error
	// Tool names the tool that failed, such as "jj".
	Tool() string
	// Line is the tool's own line naming the failure.
	Line() string
	// Output is everything the tool said, redacted.
	Output() string
}

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
	// upstream, and no commits beyond its base. Reason, when set, is the
	// driver's own account of it.
	Skip   bool
	Reason string
	// Publish, when set, is the publish the push carries out, for a driver
	// whose push is a publish (jj): Push makes the plan's branch where the
	// checkout stands and pushes it, as PreparePublish and Publish do. The
	// service checks it against the manifest's rules for its remote, as it
	// checks a publish.
	Publish *PublishPlan
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
	// Commit, when set, is the commit PlanPublish checked and the branch is
	// to publish (jj: the slot's working-copy commit's parent). PreparePublish
	// refuses when the checkout no longer stands there, so what is pushed is
	// what was checked. Empty for git, which pushes the branch as it stands.
	Commit string
	// RecordRemote reports that Remote is not the one the checkout pushes
	// Branch to by default, so Publish records it as the branch's push
	// remote, as git's push --set-upstream records an upstream: the
	// checkout's status, and later pushes, then follow it. Only a driver
	// whose push remote is otherwise git's default (jj) sets it.
	RecordRemote bool
	// Unmanaged carries PublishIntent.Unmanaged: the checkout is of a store
	// the operator does not manage, a user's, whose git config Publish never
	// writes, so a RecordRemote plan warns that it did not record the remote.
	Unmanaged bool
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
	// ErrNameTaken is wrapped by Driver.Add when Slot.Primary already has a
	// checkout under Slot.Name, which the slot would collide with.
	ErrNameTaken = errors.New("name already taken in the primary")
	// ErrCheckoutMoved is wrapped by Driver.PreparePublish, and by
	// Driver.Push where a push publishes, when the checkout moved after its
	// plan was made, so the plan no longer says what would be pushed. Nothing
	// is changed; a new plan reads the checkout afresh.
	ErrCheckoutMoved = errors.New("the checkout moved since its push was planned")
	// ErrUnsupported is wrapped by a driver method that has no meaning for its
	// kind of slot, such as aborting a merge in a version control system that
	// never stops mid-merge. The service reports it as invalid input.
	ErrUnsupported = errors.New("not supported for this slot")
)

// Driver reads and acts on one kind of slot checkout. The service decides from
// the manifest and from what the driver reads whether a verb may run and what
// it targets; the driver carries the verb out. Errors are the version control
// system's own, for the service to report.
//
// The primary's lock (plan D9) is the caller's. Add and Remove of a slot that
// has a Primary change what the primary keeps of its checkouts (git: adding a
// worktree and pruning registrations; jj: adding and forgetting a workspace,
// and dropping its git worktree registration), so the caller holds the lock
// around them. Exists, the reads and the verbs take none.
type Driver interface {
	// Exists reports whether Slot.Primary lists the slot's checkout: git by
	// Slot.Path, jj by Slot.Name. A clone is a repository of its own, which
	// nothing lists. For git, a worktree listed at the slot's own path is a
	// leftover of an earlier create, which the caller may Remove before Add.
	// For jj it means only that the name is taken in the primary, possibly
	// by another live slot (one that moved, or another stack's whose name
	// collides), which Remove would forget, deleting its .git. So a jj slot
	// never goes through the git leftover removal (addWorktreeSlot): the
	// service decides what a taken name means (addJJSlot, plan phase 6 step
	// 4).
	Exists(ctx context.Context, s Slot) (bool, error)
	// Add makes the slot's checkout at Slot.Path, writing there only through
	// d, which holds nothing or an empty directory: a worktree of
	// Slot.Primary on Slot.Branch (created at the base unless it exists) or,
	// without a branch, detached at the base; a clone of Slot.CloneURL at
	// the base; or a jj workspace of Slot.Primary named Slot.Name, made in
	// place on the bookmark Slot.Branch when it exists and at the base
	// otherwise, creating no bookmark. configure, when not nil, runs on the
	// new checkout before it reaches Slot.Path, so nothing there can use it
	// unconfigured; a driver that cannot keep that promise refuses it. Add
	// leaves nothing behind when it fails.
	Add(ctx context.Context, s Slot, d Dest, configure func(ctx context.Context, checkout string) error) error
	// Remove removes the checkout through d and drops whatever Slot.Primary
	// keeps of it. For a git worktree that is `git worktree prune`, which
	// drops every registration in the cache whose checkout is missing, not
	// only this slot's; a jj workspace is forgotten once its directory is
	// gone, and only its own git registration dropped.
	Remove(ctx context.Context, s Slot, d Dest) error

	// Status reads the checkout. On error, the returned Status holds what was
	// read before the failure; with a *CountError, that includes Pushed and
	// UnpushedReason.
	Status(ctx context.Context, s Slot) (Status, error)
	// Diff returns the checkout's uncommitted changes when ref is empty, else
	// the committed range from the checkout to ref.
	Diff(ctx context.Context, s Slot, ref string) ([]api.DiffFile, error)

	// Ready reports whether verb may run on the checkout as it stands: nil,
	// or why not, as a *BranchMismatchError, an *UncommittedError, an
	// *UntrackedPathsError or a *NotReadyError, for the service to word. What a verb needs is the
	// driver's to say: a git sync-base, pull or push needs the checkout on
	// Slot.Branch and clean, while a jj slot's working-copy commit rides
	// along with its rebase, but not files jj does not track, and a jj
	// publish, push or merge acts on the commits under the working copy, so
	// it needs the working copy clean and nothing untracked. A verb the
	// driver does not run, or does not know, is an error wrapping
	// ErrUnsupported.
	Ready(ctx context.Context, s Slot, verb Verb) error
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
	// with commits of its own is refused with ErrDetachedWithWork. A driver
	// whose push is a publish (jj) plans one to the remote the checkout
	// pushes to, under ref, else branch, and returns it as the plan's
	// Publish, refusing as PlanPublish refuses.
	PlanPush(ctx context.Context, s Slot, ref, branch string) (PushPlan, error)
	// Push carries out a plan PlanPush returned.
	Push(ctx context.Context, s Slot, plan PushPlan) error
	// SyncMethod resolves a sync-base method for the driver's checkouts:
	// SyncDefault to its own natural sync (git: SyncMerge), and SyncMerge or
	// SyncRebase to itself. A method the driver cannot sync by is an error
	// wrapping ErrUnsupported. It runs nothing, so a caller can resolve every
	// slot's method before any slot is touched.
	SyncMethod(method string) (string, error)
	// SyncBase brings onto, a ref ResolveBase returned, into the checkout's
	// branch by method (SyncMerge, SyncRebase, or SyncDefault for the
	// driver's own choice). A conflict is a result, not an error, as for
	// Rebase.
	SyncBase(ctx context.Context, s Slot, onto, method string) (api.GitOpResult, error)
	// CheckRebase reports whether rebasing the checkout onto onto, a ref
	// ResolveBase returned or one a caller named, would rewrite only what is
	// the checkout's own and may be rewritten: nil, a *SharedCommitsError
	// naming the other checkouts that hold some of it, or an
	// *ImmutableCommitsError naming what the version control system will not
	// rewrite. The rebases check it themselves too;
	// a caller rebasing several slots asks each first, so a refusal leaves
	// none rebased. A git rebase rewrites only the checkout's own branch,
	// which no other worktree can have checked out, so the git driver
	// answers nil.
	CheckRebase(ctx context.Context, s Slot, onto string) error
	// Merge merges ref into the checkout's branch, always recording a merge.
	// A conflict is a result, not an error.
	Merge(ctx context.Context, s Slot, ref string) (api.GitOpResult, error)
	// Rebase rebases the checkout's branch onto ref. A conflict is a result,
	// not an error: OK and Conflicted when the version control system
	// records it in the commits and goes on (jj), Conflicted alone when the
	// checkout stopped mid-rebase (git). An error with OK set means the
	// rebase was made, with something its caller must hear: a *Warning the
	// tool printed, or a failure to read what the rebase left.
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
	// have with *UnknownRemoteError, commits the remote would be sent that
	// the version control system will not push with *UnpushableCommitsError,
	// and otherwise with ErrNothingToPublish (or a *NothingToPublishError),
	// ErrNoBranch or ErrBranchExists.
	PlanPublish(ctx context.Context, s Slot, intent PublishIntent) (PublishPlan, error)
	// PreparePublish gets the checkout ready for a plan PlanPublish returned:
	// it creates the plan's branch where the checkout stands when the plan
	// says so, or, for a plan with a Commit, sets the branch there, refusing
	// a checkout that no longer stands on it. Its errors are the checkout's,
	// not the remote's.
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
// UnpushedReason; what it could not read is not set.
//
// Each driver keeps its verdict this way wherever the verdict needs no base:
// git for a detached worktree or a clone, jj for a dirty slot or one with an
// upstream (Err then wraps ErrBaseNotFound). Where only the base measures
// what the checkout holds (git: a branch with no upstream; jj: a clean slot
// with no upstream), Status returns the failure itself, with no verdict, and
// the checkout is not pushed.
type CountError struct {
	Err error
}

func (e *CountError) Error() string { return e.Err.Error() }

func (e *CountError) Unwrap() error { return e.Err }
