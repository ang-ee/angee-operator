package jjdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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

// TestStatusWithoutBase reads a slot whose base exists nowhere, such as a base
// branch deleted after it merged. Like the git driver, Status keeps the
// verdict wherever it needs no base, with a *vcs.CountError wrapping
// vcs.ErrBaseNotFound, so the destroy guard reads both drivers alike: a dirty
// slot is not pushed, and a slot with an upstream is pushed when nothing is
// ahead of it. A clean slot with no upstream has only the base to measure its
// own commits by, so it gets the base's error and no verdict, as a git branch
// with no upstream does.
func TestStatusWithoutBase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch string
		// prepare changes the fixture and returns the Status to expect, less
		// CurrentRef (always `@-`'s short id: the base bounds the bookmark
		// search) and ChangeID.
		prepare func(f *fixture) vcs.Status
		// decided: the verdict comes with a *vcs.CountError; else the error
		// is the base's alone.
		decided bool
	}{
		{
			name: "dirty",
			prepare: func(f *fixture) vcs.Status {
				f.write(f.slot, "new.txt", "unrecorded\n")
				return vcs.Status{Dirty: true, UnpushedReason: "uncommitted changes"}
			},
			decided: true,
		},
		{
			name: "holding a file jj does not snapshot",
			prepare: func(f *fixture) vcs.Status {
				f.jj(f.primary, "config", "set", "--repo", "snapshot.max-new-file-size", "16")
				f.write(f.slot, "only-here.bin", strings.Repeat("x", 200))
				return vcs.Status{
					Dirty:          true,
					UntrackedPaths: []string{"only-here.bin"},
					UnpushedReason: "untracked paths jj does not snapshot: only-here.bin",
				}
			},
			decided: true,
		},
		{
			name:    "clean, with no upstream",
			prepare: func(*fixture) vcs.Status { return vcs.Status{} },
		},
		{
			name: "a commit of its own, with no upstream",
			prepare: func(f *fixture) vcs.Status {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				return vcs.Status{}
			},
		},
		{
			name:   "on its upstream",
			branch: "feature",
			prepare: func(f *fixture) vcs.Status {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
				f.jj(f.slot, "git", "push", "--bookmark", "feature")
				return vcs.Status{Upstream: "feature@origin", Pushed: true}
			},
			decided: true,
		},
		{
			name:   "ahead of its upstream",
			branch: "feature",
			prepare: func(f *fixture) vcs.Status {
				f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
				f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
				f.jj(f.slot, "git", "push", "--bookmark", "feature")
				f.commit(f.slot, "g.txt", "more\n", "second change")
				return vcs.Status{Upstream: "feature@origin", Ahead: 1, UnpushedReason: "1 commit(s) ahead of feature@origin"}
			},
			decided: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			want := tc.prepare(f)
			want.CurrentRef = f.shortID(f.slot, workingCopy.parents())
			want.ChangeID = f.changeID(f.slot, workingCopy)
			slot := f.slotAt(tc.branch)
			slot.BaseRef = "gone"
			got, err := driver().Status(context.Background(), slot)
			if !errors.Is(err, vcs.ErrBaseNotFound) {
				t.Fatalf("Status() error = %v, want vcs.ErrBaseNotFound", err)
			}
			var countErr *vcs.CountError
			if decided := errors.As(err, &countErr); decided != tc.decided {
				t.Fatalf("Status() error = %#v: a *vcs.CountError is %v, want %v", err, decided, tc.decided)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Status() = %#v\nwant %#v", got, want)
			}
		})
	}
}

// TestStatusCountsFilesJJDoesNotSnapshot settles open question Q10 of the plan
// as it recommends: a new file jj refuses to snapshot (one over
// snapshot.max-new-file-size) or one snapshot.auto-track leaves out never
// reaches the working-copy commit, the snapshot exits 0 and `@` stays empty,
// yet only the slot holds the file. Status takes jj's own report of it, the
// untracked paths of `jj status`, as dirty, so the destroy guard does not
// delete it; git is not asked (plan section 2). An ignored file is neither.
func TestStatusCountsFilesJJDoesNotSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  map[string]string
		prepare func(f *fixture)
		// untracked is the paths to expect; changed, that `@` holds changes
		// of its own too.
		untracked []string
		changed   bool
	}{
		{
			name:      "a new file over snapshot.max-new-file-size",
			config:    map[string]string{"snapshot.max-new-file-size": "16"},
			prepare:   func(f *fixture) { f.write(f.slot, "only-here.bin", strings.Repeat("x", 200)) },
			untracked: []string{"only-here.bin"},
		},
		{
			name:      "a new file snapshot.auto-track leaves out",
			config:    map[string]string{"snapshot.auto-track": "none()"},
			prepare:   func(f *fixture) { f.write(f.slot, "only-here.bin", strings.Repeat("x", 200)) },
			untracked: []string{"only-here.bin"},
		},
		{
			// jj config set reads a value that is not a bare word as TOML,
			// so the fileset is a TOML literal string.
			name:   "a directory snapshot.auto-track leaves out, beside a file it tracks",
			config: map[string]string{"snapshot.auto-track": `'~root:"local"'`},
			prepare: func(f *fixture) {
				f.write(f.slot, "tracked.txt", "recorded\n")
				if err := os.MkdirAll(filepath.Join(f.slot, "local", "deep"), 0o755); err != nil {
					f.t.Fatalf("MkdirAll() error = %v", err)
				}
				f.write(filepath.Join(f.slot, "local"), "a.txt", "left out\n")
				f.write(filepath.Join(f.slot, "local", "deep"), "b.txt", "left out\n")
			},
			untracked: []string{"local/"},
			changed:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			for key, value := range tc.config {
				f.jj(f.primary, "config", "set", "--repo", key, value)
			}
			tc.prepare(f)
			got, err := driver().Status(context.Background(), f.slotAt(""))
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if got.Dirty != true || got.Pushed || !slices.Equal(got.UntrackedPaths, tc.untracked) {
				t.Fatalf("Status() = %#v, want dirty and unpushed with untracked paths %q", got, tc.untracked)
			}
			if want := dirtyReason(tc.changed, tc.untracked); got.UnpushedReason != want {
				t.Fatalf("Status().UnpushedReason = %q, want %q", got.UnpushedReason, want)
			}
			// Status only reads jj's report: the files stay out of the
			// working-copy commit.
			for file := range strings.SplitSeq(strings.TrimSpace(f.jj(f.slot, "--ignore-working-copy", "file", "list", "-r", "@")), "\n") {
				for _, path := range tc.untracked {
					if file == path || strings.HasSuffix(path, "/") && strings.HasPrefix(file, path) {
						t.Fatalf("the working-copy commit holds %s after Status, want it left untracked", file)
					}
				}
			}
		})
	}

	t.Run("an ignored file", func(t *testing.T) {
		f := newFixture(t)
		f.jj(f.primary, "config", "set", "--repo", "snapshot.max-new-file-size", "16")
		f.commit(f.slot, ".gitignore", "*.bin\n", "ignore binaries")
		f.write(f.slot, "ignored.bin", strings.Repeat("x", 200))
		got, err := driver().Status(context.Background(), f.slotAt(""))
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if got.Dirty || got.UntrackedPaths != nil {
			t.Fatalf("Status() = %#v, want an ignored file neither dirty nor untracked", got)
		}
	})
}

// TestStatusKnownLimitWhatJJNeitherRecordsNorReports pins what Q10's answer
// does not cover, found in its review: jj leaves a directory holding a
// repository of its own, and a new file outside the slot's sparse patterns,
// out of the working-copy commit and out of `jj status` alike. Status then
// reports a slot holding only those clean and pushed, and the destroy guard
// would delete them. The plan records this under Q10 for the owner; this test
// changes with that decision.
func TestStatusKnownLimitWhatJJNeitherRecordsNorReports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(f *fixture)
	}{
		{
			name: "a nested git repository",
			prepare: func(f *fixture) {
				nested := filepath.Join(f.slot, "nested")
				f.git(f.slot, "init", "-q", nested)
				f.write(nested, "work.txt", "only here\n")
			},
		},
		{
			name: "a new file outside the sparse patterns",
			prepare: func(f *fixture) {
				f.jj(f.slot, "sparse", "set", "--clear", "--add", "f.txt")
				outside := filepath.Join(f.slot, "outside")
				if err := os.Mkdir(outside, 0o755); err != nil {
					f.t.Fatalf("Mkdir() error = %v", err)
				}
				f.write(outside, "new.txt", "only here\n")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.prepare(f)
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

// TestParseUntracked reads the untracked listing of `jj status` reports as
// jj 0.45 prints them, and fails closed on a report it does not recognize,
// so a jj that rewords or rearranges the report cannot make a slot holding
// only untracked paths read as clean.
func TestParseUntracked(t *testing.T) {
	const commitLines = "Working copy  (@) : nzpzszsr 0efbe793 (empty) (no description set)\n" +
		"Parent commit (@-): pzkqsxmm e1024b09 base\n"
	for _, tc := range []struct {
		name    string
		report  string
		want    []string
		wantErr bool
	}{
		{name: "no changes", report: statusNoChanges + "\n" + commitLines},
		{name: "changes only", report: statusChanges + "\nM f.txt\n" + commitLines},
		{
			name:   "untracked only",
			report: statusUntracked + "\n? big.bin\n? d/\n" + commitLines,
			want:   []string{"big.bin", "d/"},
		},
		{
			// Paths print as they are; the commit lines end the listing.
			name:   "changes and untracked",
			report: statusChanges + "\nA small.txt\n" + statusUntracked + "\n? sp ace \"q\".txt\n? ünï.txt\n" + commitLines,
			want:   []string{"sp ace \"q\".txt", "ünï.txt"},
		},
		{
			// What follows the commit's line is not read, whatever it says.
			name:   "conflicts after the commit's lines",
			report: statusUntracked + "\n? big.bin\n" + commitLines + "Warning: There are unresolved conflicts at these paths:\nf.txt    2-sided conflict\n",
			want:   []string{"big.bin"},
		},
		{
			// A line in the listing that starts no entry is the rest of a
			// name holding a line break.
			name:   "a name with a line break",
			report: statusUntracked + "\n? nl\nname\n? after.txt\n" + commitLines,
			want:   []string{"nl\nname", "after.txt"},
		},
		{name: "a reworded untracked listing", report: "Untracked files:\n? big.bin\n" + commitLines, wantErr: true},
		{
			// A jj that both rewords the listing and says there are no changes
			// would otherwise pass for one with no untracked paths.
			name:    "a reworded untracked listing after no changes",
			report:  statusNoChanges + "\nUntracked files:\n? big.bin\n" + commitLines,
			wantErr: true,
		},
		{name: "a line nothing explains", report: statusNoChanges + "\nSomething new\n" + commitLines, wantErr: true},
		{name: "a listing with no path", report: statusUntracked + "\n" + commitLines, wantErr: true},
		{name: "nothing", report: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUntracked([]byte(tc.report))
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseUntracked() error = %v, want error %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseUntracked() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDirtyReason names what only a dirty slot holds, and at most
// maxReasonPaths untracked paths, quoting a name that does not print.
func TestDirtyReason(t *testing.T) {
	many := make([]string, maxReasonPaths+2)
	for i := range many {
		many[i] = "p" + strconv.Itoa(i)
	}
	for _, tc := range []struct {
		changed   bool
		untracked []string
		want      string
	}{
		{changed: true, want: "uncommitted changes"},
		{untracked: []string{"a.bin", "d/"}, want: "untracked paths jj does not snapshot: a.bin, d/"},
		{changed: true, untracked: []string{"a.bin"}, want: "uncommitted changes, and untracked paths jj does not snapshot: a.bin"},
		{untracked: many, want: "untracked paths jj does not snapshot: " + strings.Join(many[:maxReasonPaths], ", ") + " and 2 more"},
		// jj prints names as they are; the reason quotes one that does not
		// print.
		{untracked: []string{"nl\nname", "esc\x1b[31m", "ünï.txt"}, want: `untracked paths jj does not snapshot: "nl\nname", "esc\x1b[31m", ünï.txt`},
	} {
		if got := dirtyReason(tc.changed, tc.untracked); got != tc.want {
			t.Errorf("dirtyReason(%v, %q) = %q, want %q", tc.changed, tc.untracked, got, tc.want)
		}
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
