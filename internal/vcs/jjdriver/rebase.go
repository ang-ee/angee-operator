package jjdriver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ang-ee/angee-operator/api"
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

// Rebase rebases the slot onto ref (destination): `jj rebase -b @ --onto
// <ref>` in the slot, which moves every commit of the slot's own that ref
// lacks, the working-copy commit with its uncommitted changes included, and
// their descendants. It first checks that those are the slot's alone
// (CheckRebase). jj records a conflict in the rebased commits and exits 0
// (plan D5), so the conflicts are read after it: the result is OK, and
// Conflicted with ConflictFiles, the files of the conflicted commits the slot
// now has beyond ref. A failure that is not a conflict is jj's error. The slot stays usable
// either way: jj never stops mid-rebase. When jj made the rebase but its
// conflicts cannot be read after, the result is OK and the error says so.
func (d Driver) Rebase(ctx context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	return d.rebase(ctx, s, destination(s, ref))
}

func (d Driver) rebase(ctx context.Context, s vcs.Slot, onto revset) (api.GitOpResult, error) {
	result := api.GitOpResult{ConflictFiles: []string{}}
	if err := d.checkScope(ctx, s, onto); err != nil {
		return result, err
	}
	_, stderr, err := d.jj.snapshotOutputs(ctx, s.Path, "rebase", "-b", workingCopy.String(), "--onto", onto.String())
	result.Message = logctx.RedactText(strings.TrimSpace(string(stderr)))
	if err != nil {
		return result, err
	}
	result.OK = true
	own, err := d.conflictsBeyond(ctx, s.Path, onto)
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
	return result, nil
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
// rewrites only the slot's own commits (checkScope).
func (d Driver) CheckRebase(ctx context.Context, s vcs.Slot, onto string) error {
	return d.checkScope(ctx, s, destination(s, onto))
}

// checkScope refuses a rebase of the slot onto onto that would rewrite
// another checkout's work (plan D12), or what jj will not rewrite. `jj rebase
// -b @ --onto B` rewrites (B..@):: , the slot's own commits and every
// descendant of them, and moves the bookmarks on them. So it refuses, with a
// *vcs.SharedCommitsError, when that set holds another jj workspace's
// working-copy commit (one stacked on the slot's commits, or sharing its
// working-copy commit), or a commit at which another git worktree of the
// same primary has a branch checked out: jj would move the branch under that
// worktree, which git then shows as changes there. And it refuses, with a
// *vcs.ImmutableCommitsError, when the set holds an immutable commit (one a
// tag or an untracked remote bookmark holds, say), which jj's rebase would
// refuse only once it runs. The slot is snapshotted first, and every read
// pinned to that snapshot.
//
// The slot's own working-copy commit is the one jj names `@` here; jj does
// not say which workspace the slot is, so when another workspace's
// working-copy commit is that same commit, every workspace holding it is
// named.
func (d Driver) checkScope(ctx context.Context, s vcs.Slot, onto revset) error {
	op, err := d.snapshot(ctx, s.Path)
	if err != nil {
		return err
	}
	wc, err := d.workingCopy(ctx, s.Path, op)
	if err != nil {
		return err
	}
	rewritten := onto.to(workingCopy).descendants()
	scope, err := d.commits(ctx, s.Path, op, rewritten)
	if err != nil {
		return err
	}
	frozen, err := d.commits(ctx, s.Path, op, rewritten.and(immutable))
	if err != nil {
		return err
	}
	if len(frozen) > 0 {
		refused := &vcs.ImmutableCommitsError{}
		for _, c := range frozen {
			refused.Commits = append(refused.Commits, shortID(c.Commit.CommitID))
		}
		return refused
	}
	shared := &vcs.SharedCommitsError{}
	inScope := map[string]bool{}
	for _, c := range scope {
		inScope[c.Commit.CommitID] = true
		if c.Commit.CommitID == wc.Commit.CommitID && len(c.WorkingCopies) <= 1 {
			continue
		}
		shared.Workspaces = append(shared.Workspaces, c.WorkingCopies...)
	}
	if ok, err := colocated(s.Path); err != nil {
		return err
	} else if ok {
		worktrees, err := d.git.Worktrees(ctx, s.Path)
		if err != nil {
			return fmt.Errorf("list the git worktrees of jj slot %s: %w", s.Path, err)
		}
		own := vcs.CanonicalPath(s.Path)
		for _, w := range worktrees {
			// The slot's own worktree is on a branch only when someone
			// switched it with git; jj moves that branch with the slot.
			if w.Branch != "" && inScope[w.Head] && vcs.CanonicalPath(w.Path) != own {
				shared.Branches = append(shared.Branches, vcs.CheckedOutBranch{Branch: w.Branch, Path: w.Path})
			}
		}
	}
	if len(shared.Workspaces) == 0 && len(shared.Branches) == 0 {
		return nil
	}
	slices.Sort(shared.Workspaces)
	shared.Workspaces = slices.Compact(shared.Workspaces)
	return shared
}

// shortID is a commit id cut to the length jj shows by default.
func shortID(id string) string { return id[:min(len(id), 12)] }
