package jjdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// base is main on origin, the fixture slot's base.
var base = remoteBookmark("main", "origin")

// TestStatus reads slots the way the service will, ported from the
// 2026-10-05 probes: a clean slot, an edit no jj command has recorded yet
// (dirty, D7), commits ahead of the base or the upstream, a moved base, and
// a rebase that records a conflict and exits 0 (D5).
func TestStatus(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		branch string
		// prepare changes the fixture and returns the Status to expect.
		prepare func(f *fixture) vcs.Status
	}{
		{
			name: "clean",
			prepare: func(f *fixture) vcs.Status {
				return vcs.Status{CurrentRef: f.shortID(f.slot, base), Pushed: true}
			},
		},
		{
			// Nothing has snapshotted the edit: only Status's own read can
			// see it.
			name: "an edit no jj command has recorded",
			prepare: func(f *fixture) vcs.Status {
				f.write(f.slot, "new.txt", "unrecorded\n")
				return vcs.Status{CurrentRef: f.shortID(f.slot, base), Dirty: true, UnpushedReason: "uncommitted changes"}
			},
		},
		{
			name: "ahead of the base with no upstream",
			prepare: func(f *fixture) vcs.Status {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				return vcs.Status{
					CurrentRef:     f.shortID(f.slot, workingCopy.parents()),
					Ahead:          1,
					UnpushedReason: "1 commit(s) ahead of base ref main@origin with no upstream",
				}
			},
		},
		{
			name:   "on its upstream",
			branch: "feature",
			prepare: func(f *fixture) vcs.Status {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
				f.jj(f.slot, "git", "push", "--bookmark", "feature")
				return vcs.Status{CurrentRef: "feature", Upstream: "feature@origin", Pushed: true}
			},
		},
		{
			// The bookmark stays on the pushed commit: it is still the slot's
			// branch, and the new commit is ahead of it.
			name:   "ahead of its upstream",
			branch: "feature",
			prepare: func(f *fixture) vcs.Status {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
				f.jj(f.slot, "git", "push", "--bookmark", "feature")
				f.commit(f.slot, "g.txt", "more\n", "second change")
				return vcs.Status{
					CurrentRef:     "feature",
					Upstream:       "feature@origin",
					Ahead:          1,
					UnpushedReason: "1 commit(s) ahead of feature@origin",
				}
			},
		},
		{
			name: "behind the base",
			prepare: func(f *fixture) vcs.Status {
				want := vcs.Status{CurrentRef: f.shortID(f.slot, base), Behind: 1, BaseBehind: 1, Pushed: true}
				f.advanceBase("g.txt", "upstream\n")
				return want
			},
		},
		{
			// Counted against its upstream, the slot is level; only BaseBehind
			// shows that the base moved.
			name:   "the base moved under a published slot",
			branch: "feature",
			prepare: func(f *fixture) vcs.Status {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
				f.jj(f.slot, "git", "push", "--bookmark", "feature")
				f.advanceBase("g.txt", "upstream\n")
				return vcs.Status{CurrentRef: "feature", Upstream: "feature@origin", BaseBehind: 1, Pushed: true}
			},
		},
		{
			// jj rebase records the conflict in the commit and exits 0; the
			// working-copy commit on top inherits it.
			name: "a rebase that conflicts",
			prepare: func(f *fixture) vcs.Status {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.advanceBase("f.txt", "one\nupstream\n")
				f.jj(f.slot, "rebase", "-b", "@", "--onto", "main@origin")
				return vcs.Status{
					CurrentRef:     f.shortID(f.slot, workingCopy.parents()),
					Ahead:          1,
					UnpushedReason: "1 commit(s) ahead of base ref main@origin with no upstream",
					Conflicted:     true,
					ConflictPaths:  []string{"f.txt"},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			want := tc.prepare(f)
			want.ChangeID = f.changeID(f.slot, workingCopy)
			got, err := driver().Status(ctx, f.slotAt(tc.branch))
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Status() = %#v\nwant %#v", got, want)
			}
		})
	}
}

// TestStatusCurrentRef names the slot's branch only when its bookmark points
// at one of the slot's own commits, and otherwise the commit the slot is on:
// never another bookmark, and never the branch's bookmark left behind in the
// base.
func TestStatusCurrentRef(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		branch  string
		prepare func(f *fixture)
		// onBranch: CurrentRef is the branch; else the short id of @-.
		onBranch bool
	}{
		{name: "no branch, no bookmark"},
		{
			name:   "a branch with no bookmark yet",
			branch: "feature",
			prepare: func(f *fixture) {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
			},
		},
		{
			name:   "the branch's bookmark on a commit of the slot",
			branch: "feature",
			prepare: func(f *fixture) {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
			},
			onBranch: true,
		},
		{
			name:   "the branch's bookmark in the base",
			branch: "feature",
			prepare: func(f *fixture) {
				f.jj(f.slot, "bookmark", "create", "feature", "-r", "main@origin")
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
			},
		},
		{
			name: "another bookmark on a commit of the slot",
			prepare: func(f *fixture) {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.jj(f.slot, "bookmark", "create", "other", "-r", "@-")
			},
		},
		{
			name:   "another bookmark, and the branch's in the base",
			branch: "main",
			prepare: func(f *fixture) {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.jj(f.slot, "bookmark", "create", "other", "-r", "@-")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.prepare != nil {
				tc.prepare(f)
			}
			want := tc.branch
			if !tc.onBranch {
				want = f.shortID(f.slot, workingCopy.parents())
			}
			got, err := driver().Status(ctx, f.slotAt(tc.branch))
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if got.CurrentRef != want {
				t.Fatalf("Status().CurrentRef = %q, want %q", got.CurrentRef, want)
			}
		})
	}
}

// TestStatusUpstreamFollowsGitsPushRemote counts a slot against the remote git
// pushes its branch to from the slot (the branch's pushRemote,
// remote.pushDefault, the branch's remote, origin, the only remote), as
// publish resolves it for git slots, so the manifest's push_default (which
// reaches the source as remote.pushDefault) applies to jj slots too. jj's own
// git.push setting, which jj would follow and git does not know, has no
// effect.
func TestStatusUpstreamFollowsGitsPushRemote(t *testing.T) {
	ctx := context.Background()
	// published sets up a primary on remoteName with a fork added, and a
	// commit on bookmark feature pushed to the fork only.
	published := func(t *testing.T, remoteName string) *fixture {
		f := newFixture(t, cloneRemote(remoteName))
		fork := f.remote + "-fork.git"
		f.git(f.primary, "init", "-q", "--bare", "-b", "main", fork)
		f.git(f.primary, "remote", "add", "fork", fork)
		f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
		f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
		f.jj(f.slot, "git", "push", "--remote", "fork", "--bookmark", "feature")
		return f
	}
	for _, tc := range []struct {
		name       string
		remoteName string
		configure  func(f *fixture)
		upstream   string
	}{
		{
			// git resolves origin, which has no feature.
			name:       "jj's git.push has no effect",
			remoteName: "origin",
			configure: func(f *fixture) {
				f.jj(f.primary, "config", "set", "--repo", "git.push", "fork")
			},
		},
		{
			name:       "remote.pushDefault",
			remoteName: "origin",
			configure: func(f *fixture) {
				f.git(f.primary, "config", "remote.pushDefault", "fork")
			},
			upstream: "feature@fork",
		},
		{
			name:       "the branch's pushRemote before remote.pushDefault",
			remoteName: "origin",
			configure: func(f *fixture) {
				f.git(f.primary, "config", "remote.pushDefault", "origin")
				f.git(f.primary, "config", "branch.feature.pushRemote", "fork")
			},
			upstream: "feature@fork",
		},
		{
			// Several remotes, none of them origin, and no push default: git
			// names none.
			name:       "no push remote",
			remoteName: "upstream",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := published(t, tc.remoteName)
			if tc.configure != nil {
				tc.configure(f)
			}
			got, err := driver().Status(ctx, f.slotAt("feature"))
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if got.Upstream != tc.upstream || got.Pushed != (tc.upstream != "") {
				t.Fatalf("Status() = %#v, want upstream %q, pushed %v", got, tc.upstream, tc.upstream != "")
			}
		})
	}

	t.Run("the only remote, not named origin", func(t *testing.T) {
		f := newFixture(t, cloneRemote("upstream"))
		f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
		f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
		f.jj(f.slot, "git", "push", "--bookmark", "feature")
		got, err := driver().Status(ctx, f.slotAt("feature"))
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if got.Upstream != "feature@upstream" || !got.Pushed {
			t.Fatalf("Status() = %#v, want upstream feature@upstream, pushed", got)
		}
	})
}

// TestStatusRecoversAStaleSlot reads a slot whose working copy another
// workspace changed: the primary rebases the slot onto a moved base, as
// sync-base will. The repository does not set snapshot.auto-update-stale, so
// a plain jj in the slot refuses the stale working copy; the driver passes
// the setting itself (D8, Q9), and the slot updates itself, files included.
// (A rewrite that leaves the working copy's files as they were, such as a new
// description on `@-`, does not make it stale.)
func TestStatusRecoversAStaleSlot(t *testing.T) {
	f := newFixture(t)
	f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
	f.advanceBase("g.txt", "upstream\n")
	f.jj(f.primary, "--ignore-working-copy", "rebase", "-b", workspaceCommit(f.name).String(), "--onto", base.String())
	if stderr := f.jjFails(f.slot, "log", "-r", "@"); !strings.Contains(stderr, "stale") {
		t.Fatalf("a plain jj in the rewritten slot said %q, want its stale working copy error", stderr)
	}

	got, err := driver().Status(context.Background(), f.slotAt(""))
	if err != nil {
		t.Fatalf("Status() of a stale slot error = %v", err)
	}
	if got.Ahead != 1 || got.Behind != 0 || got.Dirty {
		t.Fatalf("Status() = %#v, want 1 ahead, level with the moved base, clean", got)
	}
	if want := f.shortID(f.slot, workingCopy.parents()); got.CurrentRef != want {
		t.Fatalf("Status().CurrentRef = %q, want the rebased commit %q", got.CurrentRef, want)
	}
	if _, err := os.Stat(filepath.Join(f.slot, "g.txt")); err != nil {
		t.Fatalf("the slot did not take the base's file: %v", err)
	}
}

// TestStatusWithoutBase reports a base that exists nowhere as
// vcs.ErrBaseNotFound, with what Status read before it.
func TestStatusWithoutBase(t *testing.T) {
	f := newFixture(t)
	f.write(f.slot, "new.txt", "unrecorded\n")
	slot := f.slotAt("")
	slot.BaseRef = "gone"
	got, err := driver().Status(context.Background(), slot)
	if !errors.Is(err, vcs.ErrBaseNotFound) {
		t.Fatalf("Status() error = %v, want vcs.ErrBaseNotFound", err)
	}
	if !got.Dirty || got.ChangeID == "" {
		t.Fatalf("Status() = %#v, want the dirty working copy read before the base", got)
	}
}

// TestStatusKnownLimitFilesJJDoesNotSnapshot pins a known limit, open
// question Q10 of the plan. A new file jj refuses to snapshot (one over
// snapshot.max-new-file-size) or one snapshot.auto-track leaves out never
// reaches the working-copy commit: the snapshot exits 0 and `@` stays empty,
// so Status reports the slot clean and pushed while the file exists only in
// it. jj says so only in human-readable output (a "Refused to snapshot some
// files" warning, `jj status`'s untracked paths), and git is not asked (plan
// section 2). Phase 6 must settle Q10 before the destroy guard reads a jj
// slot's Pushed; this test changes with that decision.
func TestStatusKnownLimitFilesJJDoesNotSnapshot(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{name: "a new file over snapshot.max-new-file-size", key: "snapshot.max-new-file-size", value: "16"},
		{name: "a new file snapshot.auto-track leaves out", key: "snapshot.auto-track", value: "none()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.jj(f.primary, "config", "set", "--repo", tc.key, tc.value)
			f.write(f.slot, "only-here.bin", strings.Repeat("x", 200))
			got, err := driver().Status(context.Background(), f.slotAt(""))
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			want := vcs.Status{CurrentRef: f.shortID(f.slot, base), ChangeID: f.changeID(f.slot, workingCopy), Pushed: true}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Status() = %#v\nwant the known limit %#v", got, want)
			}
		})
	}
}

// TestStatusOfAMergeWorkingCopy reads a slot whose working-copy commit has
// two parents (`jj new A B`): CurrentRef is its first parent, and the counts
// run from both.
func TestStatusOfAMergeWorkingCopy(t *testing.T) {
	f := newFixture(t)
	f.commit(f.slot, "g.txt", "slot\n", "slot change")
	f.advanceBase("h.txt", "upstream\n")
	f.jj(f.slot, "new", "@-", base.String(), "-m", "merge the base")
	got, err := driver().Status(context.Background(), f.slotAt(""))
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	want := vcs.Status{
		CurrentRef:     f.shortID(f.slot, workingCopy.firstParent()),
		ChangeID:       f.changeID(f.slot, workingCopy),
		Ahead:          1,
		UnpushedReason: "1 commit(s) ahead of base ref main@origin with no upstream",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Status() = %#v\nwant %#v", got, want)
	}
}

// TestReadsArePinnedToTheSnapshot reads at the operation the snapshot
// recorded: a base that moves after it does not move under the reads of the
// same call, and the next call sees it.
func TestReadsArePinnedToTheSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	behind := workingCopy.parents().to(base)
	op, err := d.snapshot(ctx, f.slot)
	if err != nil {
		t.Fatalf("snapshot() error = %v", err)
	}
	f.advanceBase("g.txt", "upstream\n")
	if n, err := d.count(ctx, f.slot, op, behind); err != nil || n != 0 {
		t.Fatalf("count() at the snapshot's operation = %d, %v; want 0: the base moved after it", n, err)
	}
	next, err := d.snapshot(ctx, f.slot)
	if err != nil {
		t.Fatalf("snapshot() error = %v", err)
	}
	if n, err := d.count(ctx, f.slot, next, behind); err != nil || n != 1 {
		t.Fatalf("count() at the next snapshot = %d, %v; want 1", n, err)
	}
}

// TestTheGitPseudoRemoteIsNeverBaseOrUpstream: jj lists a colocated
// repository's own branches as bookmarks on remote "git", which no push or
// fetch reaches, so it is passed over even when the manifest or git's push
// configuration names a remote "git".
func TestTheGitPseudoRemoteIsNeverBaseOrUpstream(t *testing.T) {
	marks := []bookmark{
		{Name: "main", Remote: gitRemote, Target: []string{"a"}},
		{Name: "feature", Remote: gitRemote, Target: []string{"b"}},
	}
	if _, err := resolveBase(marks, "main", gitRemote); !errors.Is(err, vcs.ErrBaseNotFound) {
		t.Fatalf("resolveBase(main@git only) error = %v, want vcs.ErrBaseNotFound", err)
	}
	local := append(slices.Clone(marks), bookmark{Name: "main", Target: []string{"c"}})
	if got, err := resolveBase(local, "main", gitRemote); err != nil || got.name != "main" {
		t.Fatalf("resolveBase() = %#v, %v; want the local bookmark main", got, err)
	}

	isolate(t)
	dir := t.TempDir()
	runTool(t, dir, nil, "git", "init", "-q")
	runTool(t, dir, nil, "git", "remote", "add", gitRemote, filepath.Join(dir, "elsewhere.git"))
	if _, ok, err := driver().upstream(context.Background(), dir, "feature", marks); err != nil || ok {
		t.Fatalf("upstream() with git pushing to a remote named git = %v, %v; want none", ok, err)
	}
}
