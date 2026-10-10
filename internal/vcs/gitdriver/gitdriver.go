// Package gitdriver is the vcs.Driver for git checkouts: worktree and clone
// slots, and the source caches they come from.
package gitdriver

import (
	"context"
	"fmt"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// Driver reads git checkouts through a git.Client.
type Driver struct {
	git git.Client
}

var _ vcs.Driver = Driver{}

// New returns a driver that runs git through client.
func New(client git.Client) Driver {
	return Driver{git: client}
}

// Status reads the checkout. One off its branch or with uncommitted changes
// is reported as such without counting commits.
func (d Driver) Status(ctx context.Context, s vcs.Slot) (vcs.Status, error) {
	var status vcs.Status
	currentRef, err := d.git.CurrentRef(ctx, s.Path)
	if err != nil {
		return status, err
	}
	status.CurrentRef = currentRef
	if status.Dirty, err = d.git.Dirty(ctx, s.Path); err != nil {
		return status, err
	}
	if reason := BranchMismatch(currentRef, s.Branch); reason != "" {
		status.MismatchReason = reason
		status.UnpushedReason = reason
		return status, nil
	}
	if status.Dirty {
		status.UnpushedReason = "uncommitted changes"
		return status, nil
	}
	upstream, hasUpstream, err := d.git.Upstream(ctx, s.Path)
	if err != nil {
		return status, err
	}
	countBase := s.BaseRef
	var bases []string
	if hasUpstream {
		status.Upstream = upstream
		countBase = upstream
	} else if bases = baseRefs(ctx, d.git, s.Path, s.BaseRef); len(bases) > 0 {
		countBase = bases[0]
	}
	reason, decided, err := d.pushedVerdict(ctx, s, hasUpstream, bases)
	if err != nil {
		return status, err
	}
	if decided {
		status.Pushed = reason == ""
		status.UnpushedReason = reason
	}
	if countBase != "" {
		ahead, behind, err := d.git.AheadBehind(ctx, s.Path, countBase)
		if err != nil {
			if decided {
				return status, &vcs.CountError{Err: err}
			}
			return status, err
		}
		status.Ahead, status.Behind = ahead, behind
	}
	if !decided {
		status.Pushed = status.Ahead == 0
		switch {
		case status.Pushed:
		case hasUpstream:
			status.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of %s", status.Ahead, countBase)
		default:
			status.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of base ref %s with no upstream", status.Ahead, countBase)
		}
	}
	return status, nil
}

// pushedVerdict says which commits only a clean checkout on its branch holds,
// when that does not depend on counting against its upstream: on a detached
// HEAD, and without an upstream. decided is false when the count decides.
func (d Driver) pushedVerdict(ctx context.Context, s vcs.Slot, hasUpstream bool, bases []string) (reason string, decided bool, err error) {
	_, onBranch, err := d.git.CurrentBranch(ctx, s.Path)
	if err != nil {
		return "", false, err
	}
	if onBranch && hasUpstream {
		return "", false, nil
	}
	linked, err := d.git.LinkedWorktree(ctx, s.Path)
	if err != nil {
		return "", false, err
	}
	if !linked {
		// A repository of its own (a clone slot, a cache) takes its branches
		// with it, so only what a remote branch or a tag holds is safe. Tags
		// count because git does not tell a fetched tag from a local one.
		held, err := d.git.CountNotOnRemotesOrTags(ctx, s.Path)
		if err != nil {
			return "", false, err
		}
		switch {
		case held == 0:
			return "", true, nil
		case onBranch:
			return fmt.Sprintf("%d commit(s) that no remote branch or tag holds, with no upstream", held), true, nil
		default:
			return fmt.Sprintf("%d commit(s) on a detached HEAD that no remote branch or tag holds", held), true, nil
		}
	}
	// A linked worktree's branches and tags stay in the repository it was
	// added from, so a detached HEAD loses only what no branch, remote branch
	// or tag also holds.
	if !onBranch {
		held, err := d.git.CountNotOnAnyRef(ctx, s.Path)
		if err != nil {
			return "", false, err
		}
		if held > 0 {
			return fmt.Sprintf("%d commit(s) on a detached HEAD that no branch holds", held), true, nil
		}
		return "", true, nil
	}
	beyond, known := countBeyond(ctx, d.git, s.Path, bases)
	if !known {
		return "", false, nil
	}
	if beyond > 0 {
		return fmt.Sprintf("%d commit(s) ahead of base ref %s with no upstream", beyond, s.BaseRef), true, nil
	}
	return "", true, nil
}

// Diff returns the checkout's uncommitted changes when ref is empty, else the
// committed range from HEAD to ref.
func (d Driver) Diff(ctx context.Context, s vcs.Slot, ref string) ([]api.DiffFile, error) {
	files, err := d.git.Diff(ctx, s.Path, ref)
	if err != nil {
		return nil, err
	}
	out := make([]api.DiffFile, 0, len(files))
	for _, f := range files {
		out = append(out, convertDiffFile(f))
	}
	return out, nil
}

// BranchMismatch says how a checkout on currentRef differs from the branch it
// must be on, or returns "" when branch is empty or matches.
func BranchMismatch(currentRef, branch string) string {
	if branch == "" || currentRef == branch {
		return ""
	}
	return fmt.Sprintf("current branch/ref %q, expected workspace branch %q", currentRef, branch)
}

// CommitsBeyondBase counts the commits reachable from HEAD at dir that neither
// base nor its remote counterpart (<remote>/<base>, origin's first) reaches.
// The cache's local base only moves on `source pull`, while sync-base moves a
// slot to the remote one, so counting against either alone would take the
// remote's commits for the slot's own. known is false when base is empty,
// neither ref exists, or the count cannot be read.
func CommitsBeyondBase(ctx context.Context, client git.Client, dir, base string) (ahead int, known bool) {
	return countBeyond(ctx, client, dir, baseRefs(ctx, client, dir, base))
}

// baseRefs returns the refs at dir that hold base: base itself when it exists,
// then its remote counterpart when that is another ref.
func baseRefs(ctx context.Context, client git.Client, dir, base string) []string {
	if base == "" {
		return nil
	}
	var refs []string
	if client.RefExists(ctx, dir, base) {
		refs = append(refs, base)
	}
	if remote, err := client.SyncBaseRef(ctx, dir, base); err == nil && remote != base {
		refs = append(refs, remote)
	}
	return refs
}

func countBeyond(ctx context.Context, client git.Client, dir string, bases []string) (int, bool) {
	if len(bases) == 0 {
		return 0, false
	}
	ahead, err := client.CountNotOn(ctx, dir, bases...)
	if err != nil {
		return 0, false
	}
	return ahead, true
}
