package gitdriver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// Status reports a checkout the way the service shows it: off its branch,
// dirty, or counted against its upstream or base, with what only the checkout
// holds as the unpushed reason. A linked worktree (a worktree slot) keeps its
// branches and tags in the repository it was added from; a repository of its
// own (a clone slot, a cache) loses them with the checkout, so only what a
// remote holds counts for it.
func TestStatus(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// linked: the checkout is a worktree added, detached at main, from
		// the clone, instead of the clone itself.
		linked  bool
		slot    vcs.Slot // Path is filled in
		prepare func(t *testing.T, dir, remote string)
		want    vcs.Status
		// detached: CurrentRef is a commit, only checked to be set.
		detached bool
		// countErr: Status decided Pushed but could not count.
		countErr bool
		// readErr: Status failed with what it read so far.
		readErr bool
	}{
		{
			name: "clean on its upstream",
			want: vcs.Status{CurrentRef: "main", Upstream: "origin/main", Pushed: true},
		},
		{
			name:    "ahead of its upstream",
			prepare: func(t *testing.T, dir, _ string) { commit(t, dir, "change.txt") },
			want:    vcs.Status{CurrentRef: "main", Upstream: "origin/main", Ahead: 1, UnpushedReason: "1 commit(s) ahead of origin/main"},
		},
		{
			name:    "behind its upstream",
			prepare: func(t *testing.T, dir, remote string) { advanceRemote(t, dir, remote, "main") },
			want:    vcs.Status{CurrentRef: "main", Upstream: "origin/main", Behind: 1, Pushed: true},
		},
		{
			name: "diverged from its upstream",
			prepare: func(t *testing.T, dir, remote string) {
				advanceRemote(t, dir, remote, "main")
				commit(t, dir, "change.txt")
			},
			want: vcs.Status{CurrentRef: "main", Upstream: "origin/main", Ahead: 1, Behind: 1, UnpushedReason: "1 commit(s) ahead of origin/main"},
		},
		{
			name:    "dirty",
			prepare: func(t *testing.T, dir, _ string) { write(t, filepath.Join(dir, "change.txt")) },
			want:    vcs.Status{CurrentRef: "main", Dirty: true, UnpushedReason: "uncommitted changes"},
		},
		{
			name: "off its branch",
			slot: vcs.Slot{Branch: "feature"},
			want: vcs.Status{
				CurrentRef:     "main",
				MismatchReason: `current branch/ref "main", expected workspace branch "feature"`,
				UnpushedReason: `current branch/ref "main", expected workspace branch "feature"`,
			},
		},
		{
			name:    "upstream branch deleted",
			prepare: func(t *testing.T, dir, _ string) { run(t, dir, "config", "branch.main.merge", "refs/heads/gone") },
			want:    vcs.Status{CurrentRef: "main", Upstream: "origin/gone"},
			readErr: true,
		},
		{
			name:   "worktree: own commit without an upstream",
			linked: true,
			prepare: func(t *testing.T, dir, _ string) {
				run(t, dir, "switch", "-c", "feature")
				commit(t, dir, "change.txt")
			},
			want: vcs.Status{CurrentRef: "feature", Ahead: 1, UnpushedReason: "1 commit(s) ahead of base ref main with no upstream"},
		},
		{
			// As after sync-base: the commit is the remote base's, not the
			// checkout's, so it is ahead of the local base but pushed.
			name:   "worktree: moved to the remote base without an upstream",
			linked: true,
			prepare: func(t *testing.T, dir, remote string) {
				run(t, dir, "switch", "-c", "feature")
				advanceRemote(t, dir, remote, "main")
				run(t, dir, "merge", "--ff-only", "origin/main")
			},
			want: vcs.Status{CurrentRef: "feature", Ahead: 1, Pushed: true},
		},
		{
			// Without a base there is nothing to measure the branch's own
			// commits by, so nothing is decided and destroy is refused.
			name:   "worktree: own commit without an upstream on a deleted base",
			linked: true,
			slot:   vcs.Slot{BaseRef: "gone"},
			prepare: func(t *testing.T, dir, _ string) {
				run(t, dir, "switch", "-c", "feature")
				commit(t, dir, "change.txt")
			},
			want:    vcs.Status{CurrentRef: "feature"},
			readErr: true,
		},
		{
			name:     "worktree: detached with a commit no branch holds",
			linked:   true,
			prepare:  func(t *testing.T, dir, _ string) { commit(t, dir, "change.txt") },
			want:     vcs.Status{Ahead: 1, UnpushedReason: "1 commit(s) on a detached HEAD that no branch holds"},
			detached: true,
		},
		{
			// The branch stays in the repository the worktree came from.
			name:   "worktree: detached with a commit its branch holds",
			linked: true,
			prepare: func(t *testing.T, dir, _ string) {
				run(t, dir, "switch", "-c", "mywork")
				commit(t, dir, "change.txt")
				run(t, dir, "switch", "--detach")
			},
			want:     vcs.Status{Ahead: 1, Pushed: true},
			detached: true,
		},
		{
			name:     "worktree: detached at its base",
			linked:   true,
			want:     vcs.Status{Pushed: true},
			detached: true,
		},
		{
			name:   "worktree: base only on the remote",
			linked: true,
			slot:   vcs.Slot{BaseRef: "develop"},
			prepare: func(t *testing.T, dir, remote string) {
				advanceRemote(t, dir, remote, "develop")
				run(t, dir, "switch", "--detach", "origin/develop")
				commit(t, dir, "change.txt")
			},
			want:     vcs.Status{Ahead: 1, UnpushedReason: "1 commit(s) on a detached HEAD that no branch holds"},
			detached: true,
		},
		{
			// Nothing to count against, but the commit is still the checkout's
			// alone: the state reads clean while Pushed is false.
			name:     "worktree: detached with no base",
			linked:   true,
			slot:     vcs.Slot{BaseRef: "-"},
			prepare:  func(t *testing.T, dir, _ string) { commit(t, dir, "change.txt") },
			want:     vcs.Status{UnpushedReason: "1 commit(s) on a detached HEAD that no branch holds"},
			detached: true,
		},
		{
			// The base branch was merged, deleted and pruned: there is nothing
			// to count against, but the verdict stands.
			name:     "worktree: detached on a deleted base",
			linked:   true,
			slot:     vcs.Slot{BaseRef: "gone"},
			want:     vcs.Status{Pushed: true},
			detached: true,
			countErr: true,
		},
		{
			name:     "worktree: detached with a commit on a deleted base",
			linked:   true,
			slot:     vcs.Slot{BaseRef: "gone"},
			prepare:  func(t *testing.T, dir, _ string) { commit(t, dir, "change.txt") },
			want:     vcs.Status{UnpushedReason: "1 commit(s) on a detached HEAD that no branch holds"},
			detached: true,
			countErr: true,
		},
		{
			name: "clone: own commit without an upstream",
			prepare: func(t *testing.T, dir, _ string) {
				run(t, dir, "switch", "-c", "feature")
				commit(t, dir, "change.txt")
			},
			want: vcs.Status{CurrentRef: "feature", Ahead: 1, UnpushedReason: "1 commit(s) that no remote branch or tag holds, with no upstream"},
		},
		{
			// Everything is on a remote branch, so the deleted base only
			// stops the count.
			name:     "clone: nothing of its own without an upstream on a deleted base",
			slot:     vcs.Slot{BaseRef: "gone"},
			prepare:  func(t *testing.T, dir, _ string) { run(t, dir, "switch", "-c", "feature") },
			want:     vcs.Status{CurrentRef: "feature", Pushed: true},
			countErr: true,
		},
		{
			name: "clone: own commit pushed without an upstream",
			prepare: func(t *testing.T, dir, _ string) {
				run(t, dir, "switch", "-c", "feature")
				commit(t, dir, "change.txt")
				run(t, dir, "-c", "push.autoSetupRemote=false", "push", "origin", "feature")
			},
			want: vcs.Status{CurrentRef: "feature", Ahead: 1, Pushed: true},
		},
		{
			name: "clone: moved to the remote base without an upstream",
			prepare: func(t *testing.T, dir, remote string) {
				run(t, dir, "switch", "-c", "feature")
				advanceRemote(t, dir, remote, "main")
				run(t, dir, "merge", "--ff-only", "origin/main")
			},
			want: vcs.Status{CurrentRef: "feature", Ahead: 1, Pushed: true},
		},
		{
			// The branch goes with the clone, so it holds nothing safely.
			name: "clone: detached with a commit only its own branch holds",
			prepare: func(t *testing.T, dir, _ string) {
				run(t, dir, "switch", "-c", "mywork")
				commit(t, dir, "change.txt")
				run(t, dir, "switch", "--detach")
			},
			want:     vcs.Status{Ahead: 1, UnpushedReason: "1 commit(s) on a detached HEAD that no remote branch or tag holds"},
			detached: true,
		},
		{
			// A clone pinned at a release tag that no branch holds.
			name: "clone: detached at a remote tag",
			prepare: func(t *testing.T, dir, remote string) {
				other := filepath.Join(t.TempDir(), "other")
				run(t, "", "clone", remote, other)
				identify(t, other)
				run(t, other, "switch", "--detach")
				commit(t, other, "release.txt")
				run(t, other, "tag", "v1")
				run(t, other, "push", "origin", "v1")
				run(t, dir, "fetch", "origin", "--tags")
				run(t, dir, "switch", "--detach", "v1")
			},
			want:     vcs.Status{Ahead: 1, Pushed: true},
			detached: true,
		},
		{
			name:     "clone: detached at its base",
			prepare:  func(t *testing.T, dir, _ string) { run(t, dir, "switch", "--detach") },
			want:     vcs.Status{Pushed: true},
			detached: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, remote := clone(t)
			if tc.linked {
				worktree := filepath.Join(filepath.Dir(dir), "slot")
				run(t, dir, "worktree", "add", "--detach", worktree)
				dir = worktree
			}
			if tc.prepare != nil {
				tc.prepare(t, dir, remote)
			}
			slot := tc.slot
			slot.Path = dir
			switch slot.BaseRef {
			case "":
				slot.BaseRef = "main"
			case "-":
				slot.BaseRef = ""
			}
			got, err := New(git.New()).Status(ctx, slot)
			var countErr *vcs.CountError
			switch {
			case tc.countErr:
				if !errors.As(err, &countErr) {
					t.Fatalf("Status() error = %v, want a *vcs.CountError", err)
				}
			case tc.readErr:
				if err == nil || errors.As(err, &countErr) {
					t.Fatalf("Status() error = %v, want a read error", err)
				}
			case err != nil:
				t.Fatalf("Status() error = %v", err)
			}
			if tc.detached {
				if got.CurrentRef == "" {
					t.Fatal("Status() of a detached checkout has no CurrentRef")
				}
				got.CurrentRef = ""
			}
			if got != tc.want {
				t.Fatalf("Status() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// clone returns a checkout of a fresh remote with one commit on main, tracking
// origin/main, and the remote.
func clone(t *testing.T) (dir, remote string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	dir = filepath.Join(base, "checkout")
	run(t, base, "init", "--bare", "--initial-branch=main", remote)
	run(t, base, "clone", remote, dir)
	identify(t, dir)
	commit(t, dir, "README.md")
	run(t, dir, "push", "-u", "origin", "main")
	return dir, remote
}

// advanceRemote pushes one commit to branch on remote from another clone,
// starting branch at main when it is new, and fetches it into dir.
func advanceRemote(t *testing.T, dir, remote, branch string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	run(t, "", "clone", remote, other)
	identify(t, other)
	run(t, other, "switch", "-C", branch)
	commit(t, other, branch+".txt")
	run(t, other, "push", "origin", branch)
	run(t, dir, "fetch", "origin")
}

func identify(t *testing.T, dir string) {
	t.Helper()
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test User")
	run(t, dir, "config", "commit.gpgsign", "false")
}

func commit(t *testing.T, dir, name string) {
	t.Helper()
	write(t, filepath.Join(dir, name))
	run(t, dir, "add", name)
	run(t, dir, "commit", "-m", "add "+name)
}

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(filepath.Base(path)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
