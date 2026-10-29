package jjdriver

import (
	"context"
	"fmt"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// What `workspace source migrate` (plan phase 9) asks of the jj driver
// beyond the lifecycle: the service sets the slot aside, makes the other kind
// of checkout at its path and carries across what neither tracks; the
// driver reads and readies the jj side. None of it is on vcs.Driver, since a
// git slot needs none of it (plan D2).

// fileListTemplate is for `jj file list`: each tracked path, as a JSON
// string, relative to the workspace root and slash-separated.
const fileListTemplate = `json(path) ++ "\n"`

// Import imports the git refs of the primary, colocated with git, into jj
// (`jj git import`), so a branch a git worktree of the primary has checked
// out is a bookmark a jj slot can start on. It touches no working copy.
func (d Driver) Import(ctx context.Context, primary string) error {
	if primary == "" {
		return errNoPrimary
	}
	_, err := d.jj.store(ctx, primary, "git", "import")
	return err
}

// Tip returns the full commit id of the commit under the working copy of
// the jj workspace checked out at dir, `@-`, at the repository's latest
// operation, without snapshotting. A working-copy commit with parents other
// than one has none, and is a *vcs.NotReadyError.
func (d Driver) Tip(ctx context.Context, dir string) (string, error) {
	op, err := d.latestOperation(ctx, dir)
	if err != nil {
		return "", err
	}
	wc, err := d.workingCopy(ctx, dir, op.ID)
	if err != nil {
		return "", err
	}
	if len(wc.Commit.Parents) != 1 {
		return "", severalParents(dir, len(wc.Commit.Parents), string(vcs.VerbMigrate))
	}
	return wc.Commit.Parents[0], nil
}

// TrackedPaths lists the paths commit tracks, as `jj file list` lists them
// from the jj workspace at dir, relative to its root and slash-separated,
// without snapshotting.
func (d Driver) TrackedPaths(ctx context.Context, dir, commit string) ([]string, error) {
	out, err := d.jj.latest(ctx, dir, "file", "list", "-r", commitOf(commit).single().String(), "-T", fileListTemplate)
	if err != nil {
		return nil, err
	}
	return decodeLines[string](out)
}

// Handoff is what PrepareWorktree settled for a jj slot that becomes a git
// worktree: the commit under its working copy, which the worktree checks
// out, and the bookmark it set there, with where that bookmark was before,
// for RestoreBookmark.
type Handoff struct {
	// Tip is the full commit id of `@-`.
	Tip string
	// Branch is Slot.Branch, on Tip now; empty for a slot on no branch,
	// whose worktree is detached at Tip.
	Branch string
	// Previous is the commit Branch pointed at before, or "" when the
	// repository had no such bookmark; Moved reports that PrepareWorktree
	// created or moved it.
	Previous string
	Moved    bool
	// Resolved reports that Branch was conflicted, between targets all on
	// the slot's history (after a merge of them), and is resolved at Tip now.
	// RestoreBookmark leaves it there: jj cannot set a conflict back, and
	// every target it had is an ancestor of Tip.
	Resolved bool
}

// PrepareWorktree readies the jj slot to become a git worktree of its
// primary on Slot.Branch, which the worktree checks out (plan phase 9, the
// reverse direction's first steps), at one snapshot:
//   - the working copy must hold nothing: changes are a
//     *vcs.UncommittedError, a working-copy commit with parents other than
//     one, or with a description and no changes, a *vcs.NotReadyError, as
//     Ready answers for vcs.VerbMigrate, so what changed since it said so is
//     refused too (Ready, which reads jj status, also refuses untracked
//     paths);
//   - the commit under it, Tip, must not be conflicted, since git would check
//     out jj's conflict files (*vcs.NotReadyError);
//   - with no branch, nothing more: the worktree is detached at Tip;
//   - the branch must not be checked out by another git worktree of the
//     primary (*vcs.BranchTakenError), which git refuses;
//   - bookmark Branch must be on the slot's history, not conflicted, so the
//     worktree checks out the slot's own commits: a target off `::Tip` is a
//     *vcs.BranchMovedError naming what to merge (mergeableStray);
//   - it is then created at Tip, or moved forward there (`jj bookmark set`),
//     which git sees as the branch, jj exporting it.
func (d Driver) PrepareWorktree(ctx context.Context, s vcs.Slot) (Handoff, error) {
	op, err := d.snapshot(ctx, s.Path)
	if err != nil {
		return Handoff{}, err
	}
	wc, err := d.workingCopy(ctx, s.Path, op)
	if err != nil {
		return Handoff{}, err
	}
	switch {
	case !wc.Empty:
		changed, err := d.changedFiles(ctx, s.Path, op)
		if err != nil {
			return Handoff{}, err
		}
		return Handoff{}, uncommittedChanges(s.Path, changed)
	case len(wc.Commit.Parents) != 1:
		return Handoff{}, severalParents(s.Path, len(wc.Commit.Parents), string(vcs.VerbMigrate))
	case wc.Commit.Description != "":
		return Handoff{}, describedEmpty(s.Path, string(vcs.VerbMigrate))
	}
	h := Handoff{Tip: wc.Commit.Parents[0], Branch: s.Branch}
	tip, err := d.commits(ctx, s.Path, op, commitOf(h.Tip))
	if err != nil {
		return Handoff{}, err
	}
	if len(tip) == 1 && tip[0].Conflict {
		return Handoff{}, &vcs.NotReadyError{Reason: fmt.Sprintf("the commit under its working copy, %s, has conflicts, which a git worktree would check out as jj's conflict files; resolve them and squash the resolution into it first", shortID(h.Tip))}
	}
	if s.Branch == "" {
		return h, nil
	}
	if err := d.checkedOut(ctx, s, s.Branch); err != nil {
		return Handoff{}, err
	}
	marks, err := d.bookmarks(ctx, s.Path, op, s.Branch)
	if err != nil {
		return Handoff{}, err
	}
	for _, m := range marks {
		if m.Name == s.Branch && m.Remote == "" && m.present() {
			if len(m.Target) == 1 {
				h.Previous = m.Target[0]
			} else {
				h.Resolved = true
			}
			off := localBookmark(s.Branch).minus(commitOf(h.Tip).ancestors())
			stray, err := d.changes(ctx, s.Path, op, off)
			if err != nil {
				return Handoff{}, err
			}
			if len(stray) > 0 {
				moved := &vcs.BranchMovedError{Branch: s.Branch, Commits: shortIDs(commitIDs(stray))}
				if err := d.mergeableStray(ctx, s.Path, pending{op: op, marks: marks}, off, "", len(stray), moved); err != nil {
					return Handoff{}, err
				}
				return Handoff{}, moved
			}
		}
	}
	if h.Previous == h.Tip {
		return h, nil
	}
	if _, err := d.jj.snapshot(ctx, s.Path, "bookmark", "set", "-r", commitOf(h.Tip).String(), "--", quote(s.Branch)); err != nil {
		return Handoff{}, fmt.Errorf("set bookmark %q in %s: %w", s.Branch, s.Path, err)
	}
	h.Moved = true
	return h, nil
}

// RestoreBookmark puts back the bookmark PrepareWorktree created or moved,
// for a migrate that rolls back: deleted when the repository had none
// (`jj bookmark delete`, which leaves a remote bookmark it tracks as it was
// before, a local deletion), else set back where it was, backwards; one that
// was conflicted stays resolved (Handoff.Resolved). It runs in the slot's
// primary, touching no working copy.
func (d Driver) RestoreBookmark(ctx context.Context, s vcs.Slot, h Handoff) error {
	if !h.Moved || h.Branch == "" || h.Resolved {
		return nil
	}
	if s.Primary == "" {
		return errNoPrimary
	}
	args := []string{"bookmark", "delete", "--", exact(h.Branch)}
	if h.Previous != "" {
		args = []string{"bookmark", "set", "--allow-backwards", "-r", commitOf(h.Previous).String(), "--", quote(h.Branch)}
	}
	if _, err := d.jj.store(ctx, s.Primary, args...); err != nil {
		return fmt.Errorf("restore bookmark %q in %s: %w", h.Branch, s.Primary, err)
	}
	return nil
}
