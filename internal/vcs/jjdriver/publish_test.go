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

// remoteBranch is the commit branch points at in the bare repository at
// remote, or "" when it has no such branch.
func (f *fixture) remoteBranch(remote, branch string) string {
	f.t.Helper()
	out := runTool(f.t, f.root, nil, "git", "ls-remote", remote, "refs/heads/"+branch)
	sha, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
	return sha
}

// commitID is jj's full commit id of r, read in dir without snapshotting.
func (f *fixture) commitID(dir string, r revset) string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(dir, "--ignore-working-copy", "log", "--no-graph", "-r", r.String(), "-T", `commit_id ++ "\n"`))
}

// publish plans, prepares and carries out a publish of the slot under branch,
// failing the test on any error.
func (f *fixture) publish(s vcs.Slot, intent vcs.PublishIntent) vcs.PublishPlan {
	f.t.Helper()
	ctx := context.Background()
	d := driver()
	plan, err := d.PlanPublish(ctx, s, intent)
	if err != nil {
		f.t.Fatalf("PlanPublish(%+v) error = %v", intent, err)
	}
	if err := d.PreparePublish(ctx, s, plan); err != nil {
		f.t.Fatalf("PreparePublish(%+v) error = %v", plan, err)
	}
	if result, err := d.Publish(ctx, s, plan); err != nil || !result.OK {
		f.t.Fatalf("Publish(%+v) = %+v, %v, want it pushed", plan, result, err)
	}
	return plan
}

// The first publish of a slot's commits creates the remote branch: the plan
// names git's push remote and its URL, the branch to create and the commit
// under the working copy; nothing changes until PreparePublish sets the
// bookmark there and Publish pushes it, which jj then tracks, so the slot
// reads as pushed against it and a second publish has nothing to send.
func TestPublishCreatesTheRemoteBranch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	tip := f.commitID(f.slot, workingCopy.parents())

	if err := d.Ready(ctx, s, vcs.VerbPublish); err != nil {
		t.Fatalf("Ready(publish) of a clean slot error = %v", err)
	}
	plan, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"})
	if err != nil {
		t.Fatalf("PlanPublish() error = %v", err)
	}
	if want := (vcs.PublishPlan{Remote: "origin", URL: f.remote, Branch: "feature", Create: true, Commit: tip}); plan != want {
		t.Fatalf("PlanPublish() = %+v, want %+v", plan, want)
	}
	if got := f.remoteBranch(f.remote, "feature"); got != "" {
		t.Fatalf("the remote has feature at %s before the push", got)
	}
	if err := d.PreparePublish(ctx, s, plan); err != nil {
		t.Fatalf("PreparePublish() error = %v", err)
	}
	if got := f.commitID(f.slot, localBookmark("feature")); got != tip {
		t.Fatalf("bookmark feature at %q, want the slot's commit %s", got, tip)
	}
	result, err := d.Publish(ctx, s, plan)
	if err != nil || !result.OK || !strings.Contains(result.Message, "feature") {
		t.Fatalf("Publish() = %+v, %v, want OK with jj's account of the push", result, err)
	}
	if got := f.remoteBranch(f.remote, "feature"); got != tip {
		t.Fatalf("the remote has feature at %q, want %s", got, tip)
	}
	status, err := d.Status(ctx, s)
	if err != nil || status.Upstream != "feature@origin" || status.Ahead != 0 || !status.Pushed {
		t.Fatalf("Status() after the publish = %+v, %v, want pushed against feature@origin", status, err)
	}
	_, err = d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"})
	var nothing *vcs.NothingToPublishError
	if !errors.As(err, &nothing) || !errors.Is(err, vcs.ErrNothingToPublish) || nothing.Reason != "feature@origin already holds every commit of the slot" {
		t.Fatalf("PlanPublish() of a published slot error = %v, want nothing to publish", err)
	}
	if url := d.PushURL(ctx, s, ""); url != f.remote {
		t.Fatalf("PushURL() = %q, want the remote %q", url, f.remote)
	}
}

// A slot with no commits beyond its base has nothing to publish, a named
// branch or not, and nothing to push; one with commits and no branch to push
// them under is refused (vcs.ErrDetachedWithWork), and publish needs a
// branch.
func TestPublishNeedsCommitsAndABranch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("")
	_, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature", Explicit: true})
	var nothing *vcs.NothingToPublishError
	if !errors.As(err, &nothing) || nothing.Reason != "no commits beyond main@origin" {
		t.Fatalf("PlanPublish() of a slot with no commits error = %v, want nothing beyond main@origin", err)
	}
	for _, branch := range []string{"", "feature"} {
		plan, err := d.PlanPush(ctx, s, "", branch)
		if err != nil || !plan.Skip || plan.Reason != "no commits beyond main@origin" || plan.Publish != nil {
			t.Fatalf("PlanPush(%q) of a slot with no commits = %+v, %v, want it skipped", branch, plan, err)
		}
	}
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	if _, err := d.PlanPush(ctx, s, "", ""); !errors.Is(err, vcs.ErrDetachedWithWork) {
		t.Fatalf("PlanPush() of commits with no branch error = %v, want ErrDetachedWithWork", err)
	}
	if _, err := d.PlanPublish(ctx, s, vcs.PublishIntent{}); !errors.Is(err, vcs.ErrNoBranch) {
		t.Fatalf("PlanPublish() with no branch error = %v, want ErrNoBranch", err)
	}
	if got := f.remoteBranch(f.remote, "feature"); got != "" {
		t.Fatalf("a refused publish pushed feature at %s", got)
	}
}

// Commits jj would refuse to push are refused before anything changes,
// naming each (plan D6): a conflicted one, and ones with no description,
// empty or not. No bookmark is set and nothing is pushed.
func TestPublishRefusesCommitsJJWillNotPush(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		prepare func(f *fixture) (conflicted, undescribed []string)
	}{
		{"a conflicted commit", func(f *fixture) ([]string, []string) {
			f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
			f.advanceBase("f.txt", "one\nupstream\n")
			if result, err := driver().Rebase(ctx, f.slotAt("feature"), "main@origin"); err != nil || !result.Conflicted {
				f.t.Fatalf("Rebase() = %+v, %v, want a recorded conflict", result, err)
			}
			return []string{f.shortID(f.slot, workingCopy.parents())}, nil
		}},
		{"undescribed commits", func(f *fixture) ([]string, []string) {
			f.write(f.slot, "g.txt", "undescribed\n")
			f.jj(f.slot, "new")
			changed := f.shortID(f.slot, workingCopy.parents())
			f.jj(f.slot, "new")
			return nil, []string{f.shortID(f.slot, workingCopy.parents()), changed}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			d := driver()
			s := f.slotAt("feature")
			conflicted, undescribed := tc.prepare(f)
			_, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"})
			var refused *vcs.UnpushableCommitsError
			if !errors.As(err, &refused) || !slices.Equal(refused.Conflicted, conflicted) || !slices.Equal(refused.Undescribed, undescribed) {
				t.Fatalf("PlanPublish() error = %v (%+v), want conflicted %q and undescribed %q", err, refused, conflicted, undescribed)
			}
			for _, id := range append(conflicted, undescribed...) {
				if !strings.Contains(err.Error(), id) {
					t.Errorf("PlanPublish() error = %q, want it to name %s", err, id)
				}
			}
			if _, err := d.PlanPush(ctx, s, "", "feature"); !errors.As(err, &refused) {
				t.Errorf("PlanPush() error = %v, want the same refusal", err)
			}
			if got := f.jj(f.slot, "--ignore-working-copy", "bookmark", "list", "feature"); got != "" {
				t.Errorf("a refused publish set bookmark feature: %q", got)
			}
			if got := f.remoteBranch(f.remote, "feature"); got != "" {
				t.Errorf("a refused publish pushed feature at %s", got)
			}
		})
	}
}

// Publish after a sync-base rebase force-updates the remote branch to the
// rewritten commits, with jj's lease (the remote branch is where the slot
// last saw it). The rewritten range holds the base's own commits too, one
// with no description here: the remote has them, so they are not checked,
// as jj does not check them.
func TestPublishAfterASyncBaseRebaseUpdatesTheRemoteBranch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	first := f.publish(s, vcs.PublishIntent{Branch: "feature"})

	f.write(f.seed, "h.txt", "upstream\n")
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-q", "--allow-empty-message", "-m", "")
	f.git(f.seed, "push", "-q", "origin", "HEAD:main")
	if err := d.Fetch(ctx, s); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if result, err := d.SyncBase(ctx, s, "main@origin", vcs.SyncDefault); err != nil || !result.OK || result.Conflicted {
		t.Fatalf("SyncBase() = %+v, %v, want a clean rebase", result, err)
	}
	if status, err := d.Status(ctx, s); err != nil || status.Pushed {
		t.Fatalf("Status() after the rebase = %+v, %v, want the rewritten commit unpushed", status, err)
	}

	second, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"})
	if err != nil || second.Create || second.Commit == first.Commit {
		t.Fatalf("PlanPublish() after the rebase = %+v, %v, want the rewritten commit on the existing branch", second, err)
	}
	if err := d.PreparePublish(ctx, s, second); err != nil {
		t.Fatalf("PreparePublish() error = %v", err)
	}
	result, err := d.Publish(ctx, s, second)
	if err != nil || !result.OK || !strings.Contains(result.Message, "sideways") {
		t.Fatalf("Publish() = %+v, %v, want the branch moved sideways", result, err)
	}
	if got := f.remoteBranch(f.remote, "feature"); got != second.Commit {
		t.Fatalf("the remote has feature at %q, want the rewritten %s", got, second.Commit)
	}
	if status, err := d.Status(ctx, s); err != nil || !status.Pushed {
		t.Fatalf("Status() after the publish = %+v, %v, want it pushed", status, err)
	}
}

// A remote branch that moved since the slot last fetched it refuses the push
// with jj's lease, which comes back as jj's own error with its standard
// error, and leaves the remote branch where it was.
func TestPublishRefusedByTheLease(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	f.publish(s, vcs.PublishIntent{Branch: "feature"})
	f.git(f.seed, "fetch", "-q", "origin")
	f.git(f.seed, "switch", "-q", "-c", "theirs", "origin/feature")
	f.write(f.seed, "z.txt", "theirs\n")
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-q", "-m", "theirs")
	f.git(f.seed, "push", "-q", "origin", "HEAD:feature")
	moved := f.remoteBranch(f.remote, "feature")

	f.commit(f.slot, "h.txt", "more\n", "more")
	plan, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"})
	if err != nil {
		t.Fatalf("PlanPublish() error = %v", err)
	}
	if err := d.PreparePublish(ctx, s, plan); err != nil {
		t.Fatalf("PreparePublish() error = %v", err)
	}
	result, err := d.Publish(ctx, s, plan)
	var command *CommandError
	if !errors.As(err, &command) || result.OK || command.Tool() != "jj" || !strings.HasPrefix(command.Line(), "Error:") || !strings.Contains(result.Message, "unexpectedly moved") {
		t.Fatalf("Publish() over a moved remote branch = %+v, %v, want jj's refusal", result, err)
	}
	if got := f.remoteBranch(f.remote, "feature"); got != moved {
		t.Fatalf("the remote has feature at %q, want it left at %s", got, moved)
	}
}

// The remote is git's push remote for the branch in the slot's git config
// (plan Q8), else the one the caller names; one the slot does not have is
// refused, saying whether the caller named it, and with several remotes and
// nothing naming one, there is none to default to.
func TestPublishRemote(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	fork := filepath.Join(f.root, "fork.git")
	f.git(f.root, "init", "-q", "--bare", "-b", "main", fork)
	f.git(f.primary, "remote", "add", "fork", fork)
	f.git(f.primary, "config", "remote.pushDefault", "fork")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")

	if plan, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"}); err != nil || plan.Remote != "fork" || plan.URL != fork {
		t.Fatalf("PlanPublish() = %+v, %v, want git's push default, fork", plan, err)
	}
	if plan, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Remote: "origin", Branch: "feature"}); err != nil || plan.Remote != "origin" || plan.URL != f.remote {
		t.Fatalf("PlanPublish(origin) = %+v, %v, want origin", plan, err)
	}
	var unknown *vcs.UnknownRemoteError
	if _, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Remote: "ghost", Branch: "feature"}); !errors.As(err, &unknown) || unknown.Resolved {
		t.Fatalf("PlanPublish(ghost) error = %v, want an unknown remote the caller named", err)
	}
	f.git(f.primary, "config", "remote.pushDefault", "ghost")
	if _, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"}); !errors.As(err, &unknown) || !unknown.Resolved || unknown.Remote != "ghost" {
		t.Fatalf("PlanPublish() to a push default it lacks error = %v, want an unknown remote the slot named", err)
	}
	f.git(f.primary, "config", "--unset", "remote.pushDefault")
	f.git(f.primary, "remote", "rename", "origin", "upstream")
	var none *vcs.NoPushRemoteError
	if _, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"}); !errors.As(err, &none) || !strings.Contains(none.Reason, "multiple git remotes") {
		t.Fatalf("PlanPublish() with two remotes and no default error = %v, want no push remote", err)
	}
	f.publish(s, vcs.PublishIntent{Remote: "fork", Branch: "feature"})
	if got, want := f.remoteBranch(fork, "feature"), f.commitID(f.slot, workingCopy.parents()); got != want {
		t.Fatalf("fork has feature at %q, want %s", got, want)
	}
}

// PreparePublish sets the bookmark on what PlanPublish checked: a slot that
// committed more since is refused, and nothing is set, so an unchecked
// commit is never pushed.
func TestPreparePublishRefusesASlotThatMoved(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	plan, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"})
	if err != nil {
		t.Fatalf("PlanPublish() error = %v", err)
	}
	f.write(f.slot, "h.txt", "later\n")
	f.jj(f.slot, "new")
	if err := d.PreparePublish(ctx, s, plan); !errors.Is(err, vcs.ErrCheckoutMoved) {
		t.Fatalf("PreparePublish() of a slot that moved error = %v, want vcs.ErrCheckoutMoved", err)
	}
	if got := f.jj(f.slot, "--ignore-working-copy", "bookmark", "list", "feature"); got != "" {
		t.Fatalf("a refused PreparePublish set bookmark feature: %q", got)
	}
}

// A push of a jj slot is a publish to the remote it pushes to, under the
// ref the caller names, else the slot's branch.
func TestPushPublishes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	tip := f.commitID(f.slot, workingCopy.parents())
	for _, tc := range []struct{ ref, branch string }{{"", "feature"}, {"topic", "topic"}} {
		plan, err := d.PlanPush(ctx, s, tc.ref, "feature")
		if err != nil || plan.Skip || plan.Publish == nil || plan.Publish.Branch != tc.branch || plan.Publish.Remote != "origin" || plan.Publish.Commit != tip {
			t.Fatalf("PlanPush(%q) = %+v, %v, want a publish of %s to origin", tc.ref, plan, err, tc.branch)
		}
		if err := d.Push(ctx, s, plan); err != nil {
			t.Fatalf("Push(%+v) error = %v", plan, err)
		}
		if got := f.remoteBranch(f.remote, tc.branch); got != tip {
			t.Fatalf("the remote has %s at %q, want %s", tc.branch, got, tip)
		}
	}
	if plan, err := d.PlanPush(ctx, s, "", "feature"); err != nil || !plan.Skip || plan.Reason != "feature@origin already holds every commit of the slot" {
		t.Fatalf("PlanPush() of a pushed slot = %+v, %v, want it skipped", plan, err)
	}
	if err := d.Push(ctx, s, vcs.PushPlan{Skip: true}); err != nil {
		t.Fatalf("Push() of a skipped plan error = %v", err)
	}
}

// Publish, push and merge need a slot with nothing uncommitted: changes in
// its working-copy commit, listed with jj's command that commits them, or
// untracked paths jj leaves out of it, which only the slot holds. DirtyPaths
// lists both. Destroy needs nothing.
func TestReadyRefusesUncommittedWork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	f.jj(f.primary, "config", "set", "--repo", "snapshot.max-new-file-size", "16")
	verbs := []vcs.Verb{vcs.VerbPublish, vcs.VerbPush, vcs.VerbMerge}
	for _, verb := range verbs {
		if err := d.Ready(ctx, s, verb); err != nil {
			t.Fatalf("Ready(%s) of a clean slot error = %v", verb, err)
		}
	}
	f.write(f.slot, "big.bin", strings.Repeat("x", 200))
	if paths, err := d.DirtyPaths(ctx, s); err != nil || !slices.Equal(paths, []string{"big.bin"}) {
		t.Fatalf("DirtyPaths() = %q, %v, want the untracked big.bin", paths, err)
	}
	for _, verb := range verbs {
		var untracked *vcs.UntrackedPathsError
		if err := d.Ready(ctx, s, verb); !errors.As(err, &untracked) || !slices.Equal(untracked.Paths, []string{"big.bin"}) {
			t.Errorf("Ready(%s) with an untracked file error = %v, want it named", verb, err)
		}
	}
	f.write(f.slot, "wip.txt", "unrecorded\n")
	if paths, err := d.DirtyPaths(ctx, s); err != nil || !slices.Equal(paths, []string{"wip.txt", "big.bin"}) {
		t.Fatalf("DirtyPaths() = %q, %v, want the change, then the untracked file", paths, err)
	}
	for _, verb := range verbs {
		var uncommitted *vcs.UncommittedError
		if err := d.Ready(ctx, s, verb); !errors.As(err, &uncommitted) || !slices.Equal(uncommitted.Paths, []string{"wip.txt"}) || uncommitted.Fix != "jj -R "+f.slot+" commit -m <message>" {
			t.Errorf("Ready(%s) with a change error = %v, want the change and jj's command", verb, err)
		}
	}
	if err := d.Ready(ctx, s, vcs.VerbDestroy); err != nil {
		t.Errorf("Ready(destroy) error = %v, want none", err)
	}
	f.jj(f.slot, "commit", "-m", "wip")
	f.jj(f.slot, "file", "untrack", "big.bin")
	if err := os.Remove(filepath.Join(f.slot, "big.bin")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	f.jj(f.slot, "new", "@-", f.base().String())
	for _, verb := range verbs {
		var notReady *vcs.NotReadyError
		if err := d.Ready(ctx, s, verb); !errors.As(err, &notReady) || !strings.Contains(notReady.Reason, "2 parents") || notReady.Fix != "jj -R "+f.slot+" new <commit>" {
			t.Errorf("Ready(%s) of a working copy with two parents error = %v, want it not ready", verb, err)
		}
	}
}

// A branch that is not the slot's to set is refused before anything changes:
// one the remote has but the repository does not track (someone else's,
// fetched), which jj would not push over, and one a git worktree of the
// primary has checked out, the primary's own included, which setting the
// bookmark would leave on no branch (plan D12).
func TestPublishRefusesABranchThatIsNotTheSlots(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	f.git(f.seed, "push", "-q", "origin", "HEAD:theirs")
	if err := d.Fetch(ctx, s); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	tree := filepath.Join(f.root, "tree")
	f.git(f.primary, "worktree", "add", "-q", "-b", "topic", tree, "main")

	for _, tc := range []struct {
		branch string
		want   vcs.BranchTakenError
	}{
		{"theirs", vcs.BranchTakenError{Branch: "theirs", Remote: "origin"}},
		{"topic", vcs.BranchTakenError{Branch: "topic", Worktree: tree}},
		{"main", vcs.BranchTakenError{Branch: "main", Worktree: f.primary}},
	} {
		_, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: tc.branch})
		var taken *vcs.BranchTakenError
		if !errors.As(err, &taken) || taken.Branch != tc.want.Branch || taken.Remote != tc.want.Remote || vcs.CanonicalPath(taken.Worktree) != vcs.CanonicalPath(tc.want.Worktree) {
			t.Errorf("PlanPublish(%s) error = %v, want %+v", tc.branch, err, tc.want)
		}
		if _, err := d.PlanPush(ctx, s, tc.branch, ""); !errors.As(err, &taken) {
			t.Errorf("PlanPush(%s) error = %v, want the same refusal", tc.branch, err)
		}
	}
	if got := f.jj(f.slot, "--ignore-working-copy", "bookmark", "list", "theirs"); strings.Contains(got, "theirs:") {
		t.Errorf("a refused publish set a local bookmark theirs: %q", got)
	}
	if got := f.gitOut(tree, "symbolic-ref", "--short", "HEAD"); got != "topic" {
		t.Errorf("the worktree is on %q after the refusal, want topic", got)
	}
}

// A bookmark moved elsewhere (sideways from the slot's commit) is not taken
// back: jj refuses to move it, and that refusal is jj's own error.
func TestPreparePublishLeavesABookmarkMovedElsewhere(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	f.jj(f.primary, "new", f.base().String(), "-m", "elsewhere")
	f.jj(f.primary, "bookmark", "create", "feature", "-r", "@")
	elsewhere := f.commitID(f.primary, localBookmark("feature"))
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	plan, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"})
	if err != nil {
		t.Fatalf("PlanPublish() error = %v", err)
	}
	err = d.PreparePublish(ctx, s, plan)
	var command *CommandError
	if !errors.As(err, &command) || !strings.Contains(command.Line(), "Refusing to move bookmark backwards or sideways") {
		t.Fatalf("PreparePublish() over a bookmark elsewhere error = %v, want jj's refusal", err)
	}
	if got := f.commitID(f.slot, localBookmark("feature")); got != elsewhere {
		t.Fatalf("bookmark feature at %s, want it left at %s", got, elsewhere)
	}
}

// A slot of a store jj keeps without git has no git config to name a push
// remote, so publish needs one named, whose URL comes from the repository jj
// keeps its store in; it then publishes as a colocated slot does.
func TestPublishFromAStoreWithoutGit(t *testing.T) {
	isolate(t)
	requireJJ(t)
	ctx := context.Background()
	root := t.TempDir()
	f := &fixture{t: t, root: root, remote: filepath.Join(root, "remote.git")}
	f.git(root, "init", "-q", "--bare", "-b", "main", f.remote)
	f.primary = filepath.Join(root, "store")
	f.jj(root, "git", "init", "--no-colocate", f.primary)
	f.jj(f.primary, "git", "remote", "add", "origin", f.remote)
	f.write(f.primary, "f.txt", "one\n")
	f.jj(f.primary, "commit", "-m", "base")
	f.jj(f.primary, "bookmark", "create", "main", "-r", "@-")
	d := driver()
	s := vcs.Slot{Path: filepath.Join(root, "slot"), Name: "stack--ws", Primary: f.primary, BaseRef: "main"}
	if err := d.Add(ctx, s, f.dest(s), nil); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	f.commit(s.Path, "g.txt", "mine\n", "slot change")

	var none *vcs.NoPushRemoteError
	if _, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: "feature"}); !errors.As(err, &none) {
		t.Fatalf("PlanPublish() with no remote error = %v, want no push remote", err)
	}
	// jj records the remote's path with symlinks resolved.
	if url := d.PushURL(ctx, s, "origin"); vcs.CanonicalPath(url) != vcs.CanonicalPath(f.remote) {
		t.Fatalf("PushURL(origin) = %q, want %q", url, f.remote)
	}
	var unknown *vcs.UnknownRemoteError
	if _, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Remote: "ghost", Branch: "feature"}); !errors.As(err, &unknown) {
		t.Fatalf("PlanPublish(ghost) error = %v, want an unknown remote", err)
	}
	plan := f.publish(s, vcs.PublishIntent{Remote: "origin", Branch: "feature"})
	if vcs.CanonicalPath(plan.URL) != vcs.CanonicalPath(f.remote) {
		t.Fatalf("plan URL = %q, want %q", plan.URL, f.remote)
	}
	if got := f.remoteBranch(f.remote, "feature"); got != plan.Commit {
		t.Fatalf("the remote has feature at %q, want %s", got, plan.Commit)
	}
}

// Publish names the remote and the bookmark to jj as exact patterns, since
// jj reads both as globs (a remote `*` would push to every remote), and a
// refusal's standard error comes back redacted, in the result's message and
// in jj's error alike (plan D6). The fake jj fails with a URL carrying
// credentials.
func TestPublishArgumentsAndRedaction(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.45.1\n", "fail": ""})
	plan := vcs.PublishPlan{Remote: "or*", Branch: "feat*", Commit: strings.Repeat("a", 40)}
	result, err := driver().Publish(context.Background(), vcs.Slot{Path: t.TempDir()}, plan)
	var command *CommandError
	if !errors.As(err, &command) || result.OK {
		t.Fatalf("Publish() = %+v, %v, want the fake jj's refusal", result, err)
	}
	for _, text := range []string{result.Message, command.Stderr, err.Error()} {
		if strings.Contains(text, "secret") || !strings.Contains(text, "example.com") {
			t.Errorf("refusal text %q, want the URL kept with its credentials redacted", text)
		}
	}
	runs := fakeRuns(t, home)
	if len(runs) != 1 || !slices.Contains(runs[0].Args, `--remote=exact:"or*"`) || !slices.Contains(runs[0].Args, `--bookmark=exact:"feat*"`) {
		t.Fatalf("jj ran %+v, want the remote and bookmark as exact patterns", runs)
	}
}
