package jjdriver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// latestOp is the id of the repository's latest operation, read in dir
// without snapshotting.
func (f *fixture) latestOp(dir string) string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(dir, "--ignore-working-copy", "op", "log", "--no-graph", "-n", "1", "-T", "id"))
}

// bookmarkAt is the commit local bookmark name points at in the slot, or ""
// when there is none.
func (f *fixture) bookmarkAt(name string) string {
	f.t.Helper()
	return f.commitID(f.slot, localBookmark(name))
}

// PrepareWorktree sets the slot's bookmark on the commit under its working
// copy, creating it, or moving it forward, and RestoreBookmark puts it back:
// deleted when it was created, set back when it was moved. A bookmark there
// already is left alone.
func TestPrepareWorktreeSetsTheBookmarkAndRestoresIt(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	s.Primary = f.primary
	f.commit(f.slot, "g.txt", "mine\n", "first")
	first := f.commitID(f.slot, workingCopy.parents())

	h, err := d.PrepareWorktree(ctx, s)
	if err != nil || h != (Handoff{Tip: first, Branch: "feature", Moved: true}) {
		t.Fatalf("PrepareWorktree() with no bookmark = %+v, %v, want it created at %s", h, err, first)
	}
	if got := f.bookmarkAt("feature"); got != first {
		t.Fatalf("bookmark feature at %q, want %s", got, first)
	}
	if err := d.RestoreBookmark(ctx, s, h); err != nil {
		t.Fatalf("RestoreBookmark() error = %v", err)
	}
	if got := f.bookmarkAt("feature"); got != "" {
		t.Fatalf("bookmark feature at %q after the restore, want it deleted", got)
	}

	f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
	f.commit(f.slot, "h.txt", "more\n", "second")
	second := f.commitID(f.slot, workingCopy.parents())
	h, err = d.PrepareWorktree(ctx, s)
	if err != nil || h != (Handoff{Tip: second, Branch: "feature", Previous: first, Moved: true}) {
		t.Fatalf("PrepareWorktree() behind = %+v, %v, want it moved from %s to %s", h, err, first, second)
	}
	if err := d.RestoreBookmark(ctx, s, h); err != nil {
		t.Fatalf("RestoreBookmark() error = %v", err)
	}
	if got := f.bookmarkAt("feature"); got != first {
		t.Fatalf("bookmark feature at %q after the restore, want it back at %s", got, first)
	}

	f.jj(f.slot, "bookmark", "set", "feature", "-r", "@-")
	if h, err = d.PrepareWorktree(ctx, s); err != nil || h.Moved || h.Previous != second {
		t.Fatalf("PrepareWorktree() at the tip = %+v, %v, want nothing moved", h, err)
	}

	unborn := s
	unborn.Branch = ""
	if h, err = d.PrepareWorktree(ctx, unborn); err != nil || h != (Handoff{Tip: second}) {
		t.Fatalf("PrepareWorktree() on no branch = %+v, %v, want the tip alone", h, err)
	}
	if tip, err := d.Tip(ctx, f.slot); err != nil || tip != second {
		t.Fatalf("Tip() = %q, %v, want %s", tip, err, second)
	}
	paths, err := d.TrackedPaths(ctx, f.slot, second)
	if err != nil || !slices.Equal(paths, []string{"f.txt", "g.txt", "h.txt"}) {
		t.Fatalf("TrackedPaths() = %q, %v", paths, err)
	}
}

// A bookmark off the slot's history is refused, naming what to merge: the
// bookmark when it is one commit, the stray head of a conflicted one; one
// conflicted between targets on the slot's history is resolved at the tip,
// and the restore leaves it there.
func TestPrepareWorktreeRefusesABookmarkOffTheSlotsHistory(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	s.Primary = f.primary
	f.jj(f.primary, "new", f.base().String(), "-m", "elsewhere")
	elsewhere := f.commitID(f.primary, workingCopy)
	f.jj(f.primary, "new")
	f.commit(f.slot, "g.txt", "mine\n", "first")
	f.jj(f.slot, "bookmark", "create", "feature", "-r", commitOf(elsewhere).String())

	_, err := d.PrepareWorktree(ctx, s)
	var moved *vcs.BranchMovedError
	if !errors.As(err, &moved) || moved.Remote != "" || len(moved.Heads) != 0 || !slices.Equal(moved.Commits, []string{shortID(elsewhere)}) {
		t.Fatalf("PrepareWorktree() over a bookmark elsewhere error = %+v, want it refused naming %s", err, shortID(elsewhere))
	}

	f.jj(f.slot, "bookmark", "set", "feature", "--allow-backwards", "-r", "@-")
	created := f.latestOp(f.slot)
	f.commit(f.slot, "h.txt", "more\n", "second")
	f.jj(f.slot, "bookmark", "set", "feature", "-r", "@-")
	f.jj(f.slot, "--at-op", created, "bookmark", "set", "feature", "--allow-backwards", "-r", commitOf(elsewhere).String())
	if _, err := d.PrepareWorktree(ctx, s); !errors.As(err, &moved) || !slices.Equal(moved.Heads, []string{shortID(elsewhere)}) {
		t.Fatalf("PrepareWorktree() over a conflicted bookmark error = %+v, want the stray head named", err)
	}

	f.jj(f.slot, "new", "@-", commitOf(elsewhere).String(), "-m", "merge")
	f.jj(f.slot, "new")
	merge := f.commitID(f.slot, workingCopy.parents())
	h, err := d.PrepareWorktree(ctx, s)
	if err != nil || !h.Resolved || !h.Moved || h.Tip != merge {
		t.Fatalf("PrepareWorktree() over a conflicted bookmark both of whose targets are merged = %+v, %v, want it resolved at %s", h, err, merge)
	}
	if err := d.RestoreBookmark(ctx, s, h); err != nil || f.bookmarkAt("feature") != merge {
		t.Fatalf("RestoreBookmark() of a resolved bookmark = %v, bookmark at %s, want it left at %s", err, f.bookmarkAt("feature"), merge)
	}
}

// A slot whose working copy holds something, or whose commit under it is
// conflicted, is not readied.
func TestPrepareWorktreeRefusesAWorkingCopyThatHoldsSomething(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	s.Primary = f.primary
	f.write(f.slot, "wip.txt", "wip\n")
	var uncommitted *vcs.UncommittedError
	if _, err := d.PrepareWorktree(ctx, s); !errors.As(err, &uncommitted) || !slices.Equal(uncommitted.Paths, []string{"wip.txt"}) {
		t.Fatalf("PrepareWorktree() with a change error = %v, want it uncommitted", err)
	}
	f.jj(f.slot, "commit", "-m", "wip")
	f.jj(f.slot, "describe", "-m", "next")
	var notReady *vcs.NotReadyError
	if _, err := d.PrepareWorktree(ctx, s); !errors.As(err, &notReady) {
		t.Fatalf("PrepareWorktree() over a described working copy error = %v, want it not ready", err)
	}
	if got := f.bookmarkAt("feature"); got != "" {
		t.Fatalf("a refused PrepareWorktree set bookmark feature at %s", got)
	}
}

// A bookmark another workspace moved after PrepareWorktree set it is left
// where that workspace put it, wrapping ErrBookmarkMoved.
func TestRestoreBookmarkLeavesABookmarkMovedSince(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	s.Primary = f.primary
	f.commit(f.slot, "g.txt", "mine\n", "first")
	h, err := d.PrepareWorktree(ctx, s)
	if err != nil || !h.Moved {
		t.Fatalf("PrepareWorktree() = %+v, %v", h, err)
	}
	f.jj(f.primary, "new", f.base().String(), "-m", "elsewhere")
	f.jj(f.primary, "bookmark", "set", "feature", "--allow-backwards", "-r", "@")
	moved := f.commitID(f.primary, localBookmark("feature"))
	if err := d.RestoreBookmark(ctx, s, h); !errors.Is(err, ErrBookmarkMoved) {
		t.Fatalf("RestoreBookmark() of a bookmark moved since error = %v, want ErrBookmarkMoved", err)
	}
	if got := f.commitID(f.primary, localBookmark("feature")); got != moved {
		t.Fatalf("bookmark feature at %s, want it left at %s", got, moved)
	}
}
