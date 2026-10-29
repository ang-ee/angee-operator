package jjdriver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// fetchOperation names the fetch in a timeout error.
const fetchOperation = "jj git fetch --all-remotes"

// Fetch snapshots the slot and fetches every remote of its repository into
// it (`jj git fetch --all-remotes` in the slot), as the git driver fetches
// every remote. The slot shares its primary's store, so this is the
// primary's fetch too: the remote bookmarks it updates are every slot's of
// that primary. ANGEE_GIT_TIMEOUT bounds it. Like every slot verb it takes no
// primary lock (plan D9): jj merges concurrent operations, and git's ref
// transactions keep each ref update whole.
func (d Driver) Fetch(ctx context.Context, s vcs.Slot) error {
	_, err := d.jj.network(ctx, fetchOperation, s.Path, "git", "fetch", "--all-remotes")
	return err
}

// FetchURL is the URL of the remote the slot's base comes from
// (Slot.BaseRemote), else of origin, else of the slot's only remote, as git
// names them in the slot's colocated git config; "" for a slot without git.
func (d Driver) FetchURL(ctx context.Context, s vcs.Slot) string {
	if ok, err := colocated(s.Path); err != nil || !ok {
		return ""
	}
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

// ResolveBase names the slot's base B as Status counts against it: Slot.BaseRef
// on Slot.BaseRemote (origin when empty), else the local bookmark, as the
// repository's latest operation has them. It is a name in jj's spelling
// (`main@origin`, `main`), which SyncBase, Rebase and CheckRebase take back
// as that bookmark (destination). A base that exists neither way wraps
// vcs.ErrBaseNotFound.
func (d Driver) ResolveBase(ctx context.Context, s vcs.Slot) (string, error) {
	op, err := d.latestOperation(ctx, s.Path)
	if err != nil {
		return "", err
	}
	marks, err := d.bookmarks(ctx, s.Path, op.ID, s.BaseRef)
	if err != nil {
		return "", err
	}
	base, err := resolveBase(marks, s.BaseRef, s.BaseRemote)
	if err != nil {
		return "", err
	}
	return base.name, nil
}

// destination is the revision a rebase of the slot onto onto lands on, always
// exactly one commit, so the several targets of a conflicted bookmark are an
// error, never a merge of them, which `--onto` would make. A name ResolveBase
// gives the slot's base is that bookmark: a remote bookmark, or the local
// one, never a tag or commit of that name. Any other ref is resolved as jj
// resolves a name (refRevision).
func destination(s vcs.Slot, onto string) revset {
	if s.BaseRef != "" {
		for _, c := range baseCandidates(s.BaseRef, s.BaseRemote) {
			if c.name == onto {
				return c.revset.single()
			}
		}
	}
	return refRevision(onto).single()
}

// SyncBase rebases the slot onto onto, a name ResolveBase returned, as Rebase
// does: jj's sync is a rebase (SyncMethod), so a merge is refused.
func (d Driver) SyncBase(ctx context.Context, s vcs.Slot, onto, method string) (api.GitOpResult, error) {
	if _, err := d.SyncMethod(method); err != nil {
		return api.GitOpResult{ConflictFiles: []string{}}, err
	}
	return d.rebase(ctx, s, destination(s, onto))
}

// Rebase rebases the slot onto ref (destination): `jj rebase -b <change>
// --onto <commit>` in the slot, which moves every commit of the slot's own
// that ref lacks, the working-copy commit with its uncommitted changes
// included, and their descendants. It first checks that those are the slot's
// alone and may be rewritten (checkScope), and then rebases exactly what it
// checked: the working-copy change and the base commit that check read, each
// required to be one commit, so an agent's `jj edit` or a moved bookmark in
// between, or a change that became divergent, fails rather than rebase what
// was not checked. jj records a conflict in the rebased commits and exits 0
// (plan D5), so the conflicts are read after it: the result is OK, and
// Conflicted with ConflictFiles, the files of the conflicted commits the slot
// now has beyond ref. A failure that is not a conflict is jj's error. The
// slot stays usable either way: jj never stops mid-rebase. When jj made the
// rebase but warned (it skipped updating a file the working copy held, which
// the next snapshot records as a change), or its conflicts cannot be read
// after, the result is OK with an error saying so: a *vcs.Warning in jj's
// words, or the read's failure.
func (d Driver) Rebase(ctx context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	return d.rebase(ctx, s, destination(s, ref))
}

func (d Driver) rebase(ctx context.Context, s vcs.Slot, onto revset) (api.GitOpResult, error) {
	pin, err := d.checkScope(ctx, s, onto)
	if err != nil {
		return api.GitOpResult{ConflictFiles: []string{}}, err
	}
	return d.rebasePinned(ctx, s, pin)
}

// rebasePinned rebases what checkScope checked (pin), and reads what the
// rebase left.
func (d Driver) rebasePinned(ctx context.Context, s vcs.Slot, pin pinned) (api.GitOpResult, error) {
	result := api.GitOpResult{ConflictFiles: []string{}}
	_, stderr, err := d.jj.snapshotOutputs(ctx, s.Path, "rebase", "-b", pin.branch().String(), "--onto", pin.destination().String())
	result.Message = logctx.RedactText(strings.TrimSpace(string(stderr)))
	if err != nil {
		return result, err
	}
	result.OK = true
	own, err := d.conflictsBeyond(ctx, s.Path, pin.destination())
	if err != nil {
		return result, fmt.Errorf("jj rebased %s, but reading the conflicts it left failed: %w", s.Path, err)
	}
	for _, c := range own {
		if c.Conflict {
			result.Conflicted = true
			result.ConflictFiles = append(result.ConflictFiles, c.ConflictedFiles...)
		}
	}
	slices.Sort(result.ConflictFiles)
	result.ConflictFiles = slices.Compact(result.ConflictFiles)
	if warned := warnings(result.Message); warned != "" {
		return result, &vcs.Warning{Text: warned}
	}
	return result, nil
}

// conflictsWarning starts jj's warning that a command left conflicts, which
// Conflicted reports already (jj 0.45's wording; a reworded one is carried as
// a warning, which only repeats it).
const conflictsWarning = "Warning: There are unresolved conflicts"

// warnings is what jj warned of in a mutation's report: each "Warning:" line
// and the lines after it, up to the next warning, but for the warning that
// conflicts were left.
func warnings(report string) string {
	var carried []string
	carrying := false
	for line := range strings.SplitSeq(report, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Warning:") {
			carrying = !strings.HasPrefix(line, conflictsWarning)
		}
		if carrying && line != "" {
			carried = append(carried, line)
		}
	}
	return strings.Join(carried, " ")
}

// conflictsBeyond lists the slot's commits beyond onto, as the latest
// operation has them.
func (d Driver) conflictsBeyond(ctx context.Context, dir string, onto revset) ([]commitEntry, error) {
	op, err := d.latestOperation(ctx, dir)
	if err != nil {
		return nil, err
	}
	return d.commits(ctx, dir, op.ID, onto.to(workingCopy))
}

// CheckRebase reports whether rebasing the slot onto onto (destination)
// rewrites only the slot's own commits, and none jj holds immutable
// (checkScope).
func (d Driver) CheckRebase(ctx context.Context, s vcs.Slot, onto string) error {
	_, err := d.checkScope(ctx, s, destination(s, onto))
	return err
}

// pinned is what a rebase checked, for the rebase to act on: the change id of
// the slot's working-copy commit, and the commit id of its destination.
type pinned struct {
	change, onto string
}

// branch is the checked change, which must still be one commit.
func (p pinned) branch() revset { return changeOf(p.change).single() }

// destination is the checked destination commit.
func (p pinned) destination() revset { return commitOf(p.onto).single() }

// checkScope refuses a rebase of the slot onto onto that would rewrite
// another checkout's work (plan D12), or what jj will not rewrite, and
// returns what it checked (pinned). `jj rebase -b @ --onto B` rewrites
// (B..@):: , the slot's own commits and every descendant of them, and moves
// the bookmarks on them. So it refuses, with a *vcs.SharedCommitsError, when
// that set holds another jj workspace's working-copy commit (one stacked on
// the slot's commits, or sharing its working-copy commit), or a commit that a
// branch another git worktree of the primary has checked out points at, by
// its commit or by jj's bookmark of that name: jj would move the branch, and
// that worktree is left detached when jj exports it. And it refuses, with a
// *vcs.ImmutableCommitsError, when the set holds an immutable commit (one a
// tag or an untracked remote bookmark holds, say), which jj's rebase would
// refuse only once it runs. A bookmark on those commits that nothing has
// checked out moves with them, as jj moves it (owner, 2026-10-29).
//
// The git worktrees are the primary's, whether or not the slot has a .git of
// its own: they are listed from the repository jj's store is backed by (`jj
// git root`), before the slot is snapshotted, so a branch a worktree commits
// to meanwhile is still in the list, and found by its name. Then the slot is
// snapshotted, and every read pinned to that snapshot.
//
// The slot's own working-copy commit is the one jj names `@` here; jj does
// not say which workspace the slot is, so when another workspace's
// working-copy commit is that same commit, every workspace holding it is
// named.
func (d Driver) checkScope(ctx context.Context, s vcs.Slot, onto revset) (pinned, error) {
	worktrees, err := d.primaryWorktrees(ctx, s.Path)
	if err != nil {
		return pinned{}, err
	}
	op, err := d.snapshot(ctx, s.Path)
	if err != nil {
		return pinned{}, err
	}
	wc, err := d.workingCopy(ctx, s.Path, op)
	if err != nil {
		return pinned{}, err
	}
	base, err := d.commits(ctx, s.Path, op, onto)
	if err != nil {
		return pinned{}, err
	}
	if len(base) != 1 {
		return pinned{}, fmt.Errorf("jj log -r %s in %s printed %d commits, want 1", onto, s.Path, len(base))
	}
	rewritten := onto.to(workingCopy).descendants()
	scope, err := d.commits(ctx, s.Path, op, rewritten)
	if err != nil {
		return pinned{}, err
	}
	frozen, err := d.commits(ctx, s.Path, op, rewritten.and(immutable))
	if err != nil {
		return pinned{}, err
	}
	if len(frozen) > 0 {
		refused := &vcs.ImmutableCommitsError{}
		for _, c := range frozen {
			refused.Commits = append(refused.Commits, shortID(c.Commit.CommitID))
		}
		return pinned{}, refused
	}
	shared := &vcs.SharedCommitsError{}
	inScope, marked := map[string]bool{}, map[string]bool{}
	for _, c := range scope {
		inScope[c.Commit.CommitID] = true
		for _, b := range c.Bookmarks {
			if b.Remote == "" {
				marked[b.Name] = true
			}
		}
		if c.Commit.CommitID == wc.Commit.CommitID && len(c.WorkingCopies) <= 1 {
			continue
		}
		shared.Workspaces = append(shared.Workspaces, c.WorkingCopies...)
	}
	own := vcs.CanonicalPath(s.Path)
	for _, w := range worktrees {
		// The slot's own worktree is on a branch only when someone switched
		// it with git; jj moves that branch with the slot.
		if w.Branch != "" && (inScope[w.Head] || marked[w.Branch]) && vcs.CanonicalPath(w.Path) != own {
			shared.Branches = append(shared.Branches, vcs.CheckedOutBranch{Branch: w.Branch, Path: w.Path})
		}
	}
	if len(shared.Workspaces) > 0 || len(shared.Branches) > 0 {
		slices.Sort(shared.Workspaces)
		shared.Workspaces = slices.Compact(shared.Workspaces)
		return pinned{}, shared
	}
	return pinned{change: wc.Commit.ChangeID, onto: base[0].Commit.CommitID}, nil
}

// primaryWorktrees lists the git worktrees of the repository the slot's jj
// store is backed by (`jj --ignore-working-copy git root`): a colocated
// primary's .git, whether or not the slot has a .git of its own, or, for a
// store jj keeps without git's working tree, its internal repository, which
// has none. A store jj cannot name a git repository for fails the check.
func (d Driver) primaryWorktrees(ctx context.Context, slot string) ([]git.Worktree, error) {
	out, err := d.jj.store(ctx, slot, "git", "root")
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return nil, fmt.Errorf("jj git root in %s printed no repository", slot)
	}
	worktrees, err := d.git.Worktrees(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("list the git worktrees of %s, the repository of jj slot %s: %w", root, slot, err)
	}
	return worktrees, nil
}

// shortID is a commit id cut to the length jj shows by default.
func shortID(id string) string { return id[:min(len(id), 12)] }
