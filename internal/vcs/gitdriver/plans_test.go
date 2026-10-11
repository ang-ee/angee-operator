package gitdriver

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// PlanPublish decides from the checkout alone and changes nothing: what it
// publishes, whether it creates the branch, and what it refuses. An unmanaged
// checkout skips the nothing-to-publish check and never has a branch created.
func TestPlanPublish(t *testing.T) {
	ctx := context.Background()
	d := New(git.New())
	for _, tc := range []struct {
		name     string
		detached bool
		ahead    bool
		upstream bool
		intent   vcs.PublishIntent
		want     vcs.PublishPlan
		wantErr  error
	}{
		{name: "nothing of its own", intent: vcs.PublishIntent{Remote: "origin"}, wantErr: vcs.ErrNothingToPublish},
		{name: "nothing of its own, branch named", intent: vcs.PublishIntent{Remote: "origin", Branch: "feature", Explicit: true}, want: vcs.PublishPlan{Remote: "origin", Branch: "feature"}},
		{name: "nothing of its own, unmanaged", intent: vcs.PublishIntent{Remote: "origin", Unmanaged: true}, want: vcs.PublishPlan{Remote: "origin", Branch: "feature"}},
		{name: "its own commit", ahead: true, intent: vcs.PublishIntent{Remote: "fork"}, want: vcs.PublishPlan{Remote: "fork", Branch: "feature"}},
		{name: "tracking", upstream: true, intent: vcs.PublishIntent{Remote: "origin"}, want: vcs.PublishPlan{Remote: "origin", Branch: "feature"}},
		{name: "detached, nothing of its own", detached: true, intent: vcs.PublishIntent{Remote: "origin", Branch: "manifest"}, wantErr: vcs.ErrNothingToPublish},
		{name: "detached, no branch", detached: true, ahead: true, intent: vcs.PublishIntent{Remote: "origin"}, wantErr: vcs.ErrNoBranch},
		{name: "detached, a branch to create", detached: true, ahead: true, intent: vcs.PublishIntent{Remote: "origin", Branch: "new", Explicit: true}, want: vcs.PublishPlan{Remote: "origin", Branch: "new", Create: true}},
		{name: "detached, an existing branch", detached: true, intent: vcs.PublishIntent{Remote: "origin", Branch: "main", Explicit: true}, wantErr: vcs.ErrBranchExists},
		{name: "detached, unmanaged", detached: true, intent: vcs.PublishIntent{Remote: "origin", Branch: "main", Unmanaged: true}, want: vcs.PublishPlan{Remote: "origin", Branch: "main"}},
		{name: "detached, unmanaged, no branch", detached: true, intent: vcs.PublishIntent{Remote: "origin", Unmanaged: true}, wantErr: vcs.ErrNoBranch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := clone(t)
			run(t, dir, "switch", "--no-track", "-c", "feature", "main")
			if tc.upstream {
				run(t, dir, "branch", "--set-upstream-to", "origin/main")
			}
			if tc.ahead {
				commit(t, dir, "change.txt")
			}
			if tc.detached {
				run(t, dir, "switch", "--detach")
			}
			s := vcs.Slot{Path: dir, BaseRef: "main", BaseRemote: "origin"}
			head := gitOutput(t, dir, "rev-parse", "HEAD")
			got, err := d.PlanPublish(ctx, s, tc.intent)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Fatalf("PlanPublish() = %+v, %v, want %+v, %v", got, err, tc.want, tc.wantErr)
			}
			if after := gitOutput(t, dir, "rev-parse", "HEAD"); after != head || (tc.detached && gitOutput(t, dir, "branch", "--show-current") != "") {
				t.Fatal("PlanPublish() changed the checkout")
			}
		})
	}
}

// PreparePublish creates the planned branch at HEAD and switches to it, and
// only when the plan says to.
func TestPreparePublishCreatesThePlannedBranch(t *testing.T) {
	ctx := context.Background()
	d := New(git.New())
	dir, _ := clone(t)
	run(t, dir, "switch", "--detach")
	s := vcs.Slot{Path: dir}
	if err := d.PreparePublish(ctx, s, vcs.PublishPlan{Remote: "origin", Branch: "new"}); err != nil {
		t.Fatalf("PreparePublish() without Create error = %v", err)
	}
	if got := gitOutput(t, dir, "branch", "--show-current"); got != "" {
		t.Fatalf("branch after PreparePublish() without Create = %q, want still detached", got)
	}
	if err := d.PreparePublish(ctx, s, vcs.PublishPlan{Remote: "origin", Branch: "new", Create: true}); err != nil {
		t.Fatalf("PreparePublish() error = %v", err)
	}
	if got := gitOutput(t, dir, "branch", "--show-current"); got != "new" {
		t.Fatalf("branch after PreparePublish() = %q, want new", got)
	}
}

// PlanPush pushes a named ref as it is, a tracking branch to its upstream, an
// untracked one to the manifest's branch, and refuses or skips a detached
// HEAD by whether it holds commits of its own. Push carries the plan out.
func TestPlanPush(t *testing.T) {
	ctx := context.Background()
	d := New(git.New())
	for _, tc := range []struct {
		name     string
		detached bool
		ahead    bool
		upstream bool
		ref      string
		branch   string
		want     vcs.PushPlan
		wantErr  error
	}{
		{name: "a ref", ref: "v1", branch: "feature", want: vcs.PushPlan{Ref: "v1"}},
		{name: "tracking", upstream: true, ahead: true, branch: "feature", want: vcs.PushPlan{}},
		{name: "untracked with a commit", ahead: true, branch: "feature", want: vcs.PushPlan{Track: "feature"}},
		{name: "untracked without", branch: "feature", want: vcs.PushPlan{Skip: true}},
		{name: "untracked on another branch", branch: "other", want: vcs.PushPlan{Track: "other"}},
		{name: "detached without a commit", detached: true, branch: "feature", want: vcs.PushPlan{Skip: true}},
		{name: "detached with a commit", detached: true, ahead: true, branch: "feature", wantErr: vcs.ErrDetachedWithWork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := clone(t)
			run(t, dir, "switch", "--no-track", "-c", "feature", "main")
			if tc.upstream {
				run(t, dir, "branch", "--set-upstream-to", "origin/main")
			}
			if tc.ahead {
				commit(t, dir, "change.txt")
			}
			if tc.detached {
				run(t, dir, "switch", "--detach")
			}
			s := vcs.Slot{Path: dir, BaseRef: "main", BaseRemote: "origin"}
			got, err := d.PlanPush(ctx, s, tc.ref, tc.branch)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Fatalf("PlanPush() = %+v, %v, want %+v, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// The default sync is the git driver's own choice, a merge: the slot's branch
// gains the base's new commit, and its own commit stays on it.
func TestSyncBaseByDefaultMerges(t *testing.T) {
	ctx := context.Background()
	d := New(git.New())
	dir, remote := clone(t)
	commit(t, dir, "mine.txt")
	advanceRemote(t, dir, remote, "main")
	mine := gitOutput(t, dir, "rev-parse", "HEAD")
	result, err := d.SyncBase(ctx, vcs.Slot{Path: dir}, "origin/main", vcs.SyncDefault)
	if err != nil || result.Conflicted {
		t.Fatalf("SyncBase(SyncDefault) = %+v, %v, want a clean sync", result, err)
	}
	if got := gitOutput(t, dir, "rev-list", "--count", "--merges", "HEAD^..HEAD"); got != "1" {
		t.Fatalf("SyncBase(SyncDefault) recorded %s merge commit(s), want the merge git makes for a diverged branch", got)
	}
	if gitOutput(t, dir, "rev-parse", "HEAD^1") != mine {
		t.Fatal("SyncBase(SyncDefault) rewrote the branch's own commit, want it merged onto")
	}
}

// gitOutput runs git in dir and returns its trimmed output.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// Push of a plan to track pushes the branch with -u, so it tracks the remote
// branch afterwards; a skipped plan pushes nothing.
func TestPushCarriesOutThePlan(t *testing.T) {
	ctx := context.Background()
	d := New(git.New())
	dir, remote := clone(t)
	run(t, dir, "switch", "--no-track", "-c", "feature", "main")
	commit(t, dir, "change.txt")
	s := vcs.Slot{Path: dir}
	if err := d.Push(ctx, s, vcs.PushPlan{Skip: true, Track: "feature"}); err != nil {
		t.Fatalf("Push() of a skipped plan error = %v", err)
	}
	if got := gitOutput(t, "", "ls-remote", remote, "refs/heads/feature"); got != "" {
		t.Fatalf("a skipped plan pushed %q", got)
	}
	if err := d.Push(ctx, s, vcs.PushPlan{Track: "feature"}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if got := gitOutput(t, dir, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "origin/feature" {
		t.Fatalf("upstream after Push() = %q, want origin/feature", got)
	}
}
