package jjdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// pushBase moves main on the remote from the fixture's seed, setting name to
// content, without fetching it anywhere.
func (f *fixture) pushBase(name, content string) {
	f.t.Helper()
	f.write(f.seed, name, content)
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-q", "-m", "upstream "+name)
	f.git(f.seed, "push", "-q", "origin", "HEAD:main")
}

// TestSyncBase brings a moved base into a slot the way sync-base does:
// Fetch in the slot reaches the remote, ResolveBase names main on it, and
// SyncBase by default rebases the slot's commit and its unrecorded edit onto
// it, which ride along. Nothing conflicts, and the slot is no longer behind.
func TestSyncBase(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	f.write(f.slot, "wip.txt", "unrecorded\n")
	f.pushBase("f.txt", "one\nupstream\n")

	if url := d.FetchURL(ctx, s); url != f.remote {
		t.Errorf("FetchURL() = %q, want the remote %q", url, f.remote)
	}
	if err := d.Fetch(ctx, s); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	onto, err := d.ResolveBase(ctx, s)
	if err != nil || onto != "main@origin" {
		t.Fatalf("ResolveBase() = %q, %v, want main@origin", onto, err)
	}
	if err := d.CheckRebase(ctx, s, onto); err != nil {
		t.Fatalf("CheckRebase() error = %v", err)
	}
	result, err := d.SyncBase(ctx, s, onto, vcs.SyncDefault)
	if err != nil || !result.OK || result.Conflicted || len(result.ConflictFiles) != 0 || !strings.Contains(result.Message, "Rebased") {
		t.Fatalf("SyncBase() = %+v, %v, want a clean rebase with jj's account of it", result, err)
	}
	if got, want := f.shortID(f.slot, revset("(@-)-")), f.shortID(f.slot, base); got != want {
		t.Errorf("the slot's commit sits on %s, want the moved base %s", got, want)
	}
	if got := f.jj(f.slot, "--ignore-working-copy", "file", "show", "-r", "@", "wip.txt"); got != "unrecorded\n" {
		t.Errorf("the working-copy commit holds wip.txt %q, want the edit carried along", got)
	}
	status, err := d.Status(ctx, s)
	if err != nil || status.Behind != 0 || status.Ahead != 1 || !status.Dirty || status.Conflicted {
		t.Errorf("Status() after the sync = %+v, %v, want one commit ahead, dirty, nothing behind", status, err)
	}
}

// A rebase that conflicts is a result: jj records the conflict in the commit,
// so the result is OK and Conflicted, with the files, and the slot stays
// usable, reading as conflicted.
func TestSyncBaseThatConflicts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("")
	f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
	f.advanceBase("f.txt", "one\nupstream\n")

	result, err := d.SyncBase(ctx, s, "main@origin", vcs.SyncRebase)
	if err != nil || !result.OK || !result.Conflicted || !slices.Equal(result.ConflictFiles, []string{"f.txt"}) {
		t.Fatalf("SyncBase() = %+v, %v, want OK and conflicted in f.txt", result, err)
	}
	if !strings.Contains(result.Message, "conflict") {
		t.Errorf("SyncBase() message = %q, want jj's account of the conflict", result.Message)
	}
	status, err := d.Status(ctx, s)
	if err != nil || !status.Conflicted || !slices.Equal(status.ConflictPaths, []string{"f.txt"}) || status.Behind != 0 {
		t.Fatalf("Status() after the conflict = %+v, %v, want it conflicted in f.txt and not behind", status, err)
	}
	f.jj(f.slot, "status")
	f.write(f.slot, "f.txt", "one\nresolved\n")
	f.jj(f.slot, "squash", "-m", "slot change")
	if status, err := d.Status(ctx, s); err != nil || status.Conflicted {
		t.Errorf("Status() once resolved = %+v, %v, want no conflict", status, err)
	}
}

// Rebase onto a ref a caller names takes jj's spelling: name@remote is a
// remote bookmark, quoted, so a name with revset syntax names nothing.
func TestRebaseOntoANamedRef(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	f.advanceBase("f.txt", "one\nupstream\n")
	if result, err := d.Rebase(ctx, s, "main@origin"); err != nil || !result.OK || result.Conflicted {
		t.Fatalf("Rebase(main@origin) = %+v, %v, want a clean rebase", result, err)
	}
	if got, want := f.shortID(f.slot, revset("(@-)-")), f.shortID(f.slot, base); got != want {
		t.Errorf("the slot's commit sits on %s, want main@origin %s", got, want)
	}
	before := f.shortID(f.slot, workingCopy.parents())
	if _, err := d.Rebase(ctx, s, "main@origin | root()"); err == nil {
		t.Fatal("Rebase() onto a revset error = nil, want the name refused")
	}
	if got := f.shortID(f.slot, workingCopy.parents()); got != before {
		t.Errorf("a refused rebase moved the slot to %s, from %s", got, before)
	}
}

// A rebase is refused, before anything is rewritten, when what it would
// rewrite is not the slot's own: another jj workspace stacked on the slot's
// commits, one that shares its working-copy commit, or a branch a git
// worktree of the same primary has checked out at one of them. A checkout
// that only shares the base is no obstacle.
func TestRebaseRefusesSharedCommits(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// prepare makes another checkout hold some of the slot's commits and
		// returns the error to expect.
		prepare func(f *fixture, own string) *vcs.SharedCommitsError
	}{
		{"a workspace stacked on the slot", func(f *fixture, own string) *vcs.SharedCommitsError {
			f.jj(f.primary, "workspace", "add", "--colocate", "--name", "stacked", "-r", own, filepath.Join(f.root, "stacked"))
			return &vcs.SharedCommitsError{Workspaces: []string{"stacked"}}
		}},
		{"a workspace on the slot's working-copy commit", func(f *fixture, _ string) *vcs.SharedCommitsError {
			other := filepath.Join(f.root, "other")
			f.jj(f.primary, "workspace", "add", "--colocate", "--name", "other", "-r", f.base().String(), other)
			f.jj(other, "edit", f.changeID(f.slot, workingCopy))
			return &vcs.SharedCommitsError{Workspaces: []string{"other", f.name}}
		}},
		{"a git worktree's branch", func(f *fixture, own string) *vcs.SharedCommitsError {
			tree := filepath.Join(f.root, "tree")
			f.git(f.primary, "worktree", "add", "-q", "-b", "topic", tree, own)
			return &vcs.SharedCommitsError{Branches: []vcs.CheckedOutBranch{{Branch: "topic", Path: mustEval(t, tree)}}}
		}},
		{"the slot's own branch", func(f *fixture, _ string) *vcs.SharedCommitsError {
			f.git(f.slot, "switch", "-q", "-c", "mine")
			return nil
		}},
		{"a workspace on the base", func(f *fixture, _ string) *vcs.SharedCommitsError {
			f.jj(f.primary, "workspace", "add", "--colocate", "--name", "beside", "-r", f.base().String(), filepath.Join(f.root, "beside"))
			f.git(f.primary, "worktree", "add", "-q", "-b", "elsewhere", filepath.Join(f.root, "elsewhere"), "origin/main")
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			d := driver()
			s := f.slotAt("")
			f.commit(f.slot, "g.txt", "mine\n", "slot change")
			own := f.shortID(f.slot, workingCopy.parents())
			want := tc.prepare(f, own)
			f.advanceBase("f.txt", "one\nupstream\n")

			err := d.CheckRebase(ctx, s, "main@origin")
			if want == nil {
				if err != nil {
					t.Fatalf("CheckRebase() error = %v, want none", err)
				}
				if result, err := d.SyncBase(ctx, s, "main@origin", vcs.SyncDefault); err != nil || !result.OK {
					t.Fatalf("SyncBase() = %+v, %v, want a rebase", result, err)
				}
				return
			}
			var shared *vcs.SharedCommitsError
			if !errors.As(err, &shared) || !slices.Equal(shared.Workspaces, want.Workspaces) || !slices.Equal(shared.Branches, want.Branches) {
				t.Fatalf("CheckRebase() error = %#v, want %#v", err, want)
			}
			for _, call := range []func() error{
				func() error { _, err := d.SyncBase(ctx, s, "main@origin", vcs.SyncDefault); return err },
				func() error { _, err := d.Rebase(ctx, s, "main@origin"); return err },
			} {
				if err := call(); !errors.As(err, &shared) {
					t.Fatalf("rebase error = %v, want the same refusal", err)
				}
			}
			if got := f.shortID(f.slot, workingCopy.parents()); got != own {
				t.Errorf("a refused rebase rewrote the slot's commit %s into %s", own, got)
			}
		})
	}
}

// A rebase that would rewrite a commit jj holds immutable, here one of the
// slot's own that a tag holds, is refused before it runs, naming the commit,
// and nothing is rewritten.
func TestRebaseRefusesImmutableCommits(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	own := f.jjOut(f.slot, "commit_id", workingCopy.parents())
	f.git(f.primary, "tag", "v1", own)
	f.advanceBase("f.txt", "one\nupstream\n")
	for _, call := range []func() error{
		func() error { return d.CheckRebase(ctx, f.slotAt(""), "main@origin") },
		func() error { _, err := d.SyncBase(ctx, f.slotAt(""), "main@origin", vcs.SyncDefault); return err },
	} {
		var frozen *vcs.ImmutableCommitsError
		if err := call(); !errors.As(err, &frozen) || !slices.Equal(frozen.Commits, []string{own[:12]}) {
			t.Fatalf("rebase error = %v, want the tagged commit %s refused", err, own[:12])
		}
	}
	if got := f.jjOut(f.slot, "commit_id", workingCopy.parents()); got != own {
		t.Errorf("a refused rebase rewrote %s into %s", own, got)
	}
}

// A rebase jj fails for a reason angee does not check first, here a ref that
// names nothing, is jj's error, in jj's words.
func TestRebaseFailureIsJJsError(t *testing.T) {
	result, err := driver().Rebase(context.Background(), newFixture(t).slotAt(""), "nosuch@origin")
	var command *CommandError
	if !errors.As(err, &command) || result.OK || !strings.HasPrefix(command.Line(), "Error: ") || !strings.Contains(command.Line(), "nosuch") {
		t.Fatalf("Rebase() = %+v, %v, want jj's error naming the ref", result, err)
	}
}

// ResolveBase reports a base that exists nowhere as vcs.ErrBaseNotFound, and
// falls back to the local bookmark where the remote has none.
func TestResolveBase(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("")
	s.BaseRef = "gone"
	if _, err := d.ResolveBase(ctx, s); !errors.Is(err, vcs.ErrBaseNotFound) {
		t.Fatalf("ResolveBase() of a missing base error = %v, want vcs.ErrBaseNotFound", err)
	}
	f.jj(f.primary, "bookmark", "create", "local-only", "-r", f.base().String())
	s.BaseRef = "local-only"
	if onto, err := d.ResolveBase(ctx, s); err != nil || onto != "local-only" {
		t.Fatalf("ResolveBase() = %q, %v, want the local bookmark", onto, err)
	}
	if got := destination(s, "local-only"); got != localBookmark("local-only").single() {
		t.Errorf("destination() = %s, want the local bookmark, exactly", got)
	}
}

// jjOut is the template field of r, read in dir without snapshotting.
func (f *fixture) jjOut(dir, field string, r revset) string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(dir, "--ignore-working-copy", "log", "--no-graph", "-r", r.String(), "-T", field))
}

// TestCommandErrorLine quotes jj's "Error:" line, else its first.
func TestCommandErrorLine(t *testing.T) {
	for stderr, want := range map[string]string{
		"Warning: x\nError: Commit 0123 is immutable\nHint: y": "Error: Commit 0123 is immutable",
		"something odd\nmore": "something odd",
		"":                    "",
	} {
		e := &CommandError{Stderr: stderr, Err: os.ErrInvalid}
		if got := e.Line(); got != want || e.Tool() != "jj" || e.Output() != stderr {
			t.Errorf("Line() of %q = %q, want %q", stderr, got, want)
		}
	}
}
