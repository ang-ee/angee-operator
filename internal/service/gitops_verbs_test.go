package service

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// With origin a fork and a canonical upstream, the "nothing to push" and
// "nothing to publish" checks count against the base on the canonical remote:
// a new slot level with upstream's release has nothing of its own, though the
// fork's release differs, so neither push nor publish pushes it anywhere.
func TestNothingToPushOrPublishCountsAgainstTheCanonicalBase(t *testing.T) {
	ctx := t.Context()
	b := newBaseFixture(t, remotesFixture.forkAndCanonicalUpstream)
	if _, err := b.p.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: b.template, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}

	if _, err := b.p.WorkspacePush(ctx, "feature-a", ""); err != nil {
		t.Fatalf("WorkspacePush() error = %v", err)
	}
	if _, err := b.p.WorkspaceSourcePush(ctx, "feature-a", "app", ""); err != nil {
		t.Fatalf("WorkspaceSourcePush() error = %v", err)
	}
	result, err := b.p.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "")
	if want := "nothing to publish: no commits beyond release; pass a branch to publish anyway"; err != nil || !result.OK || result.Message != want {
		t.Fatalf("WorkspaceSourcePublish() = %#v, %v, want OK with %q", result, err, want)
	}
	for _, remote := range []string{b.fork, b.upstream} {
		if pushed := strings.TrimSpace(runGitOutput(t, "", "ls-remote", remote, "refs/heads/feature-a")); pushed != "" {
			t.Fatalf("feature-a was pushed to %s (%s), want nothing pushed", remote, pushed)
		}
	}
}

// gitIdentity configures a committer in dir, which a CI runner's git lacks.
func gitIdentity(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
}

func gitRev(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(runGitOutput(t, dir, "rev-parse", rev))
}

// pushUpstreamChange commits name with contents on main in the fixture's seed
// clone and pushes it.
func pushUpstreamChange(t *testing.T, fixture *repairFixture, name, contents string) {
	t.Helper()
	seed := filepath.Join(fixture.base, "seed")
	mustWriteFile(t, filepath.Join(seed, name), contents)
	runGit(t, seed, "add", name)
	runGit(t, seed, "commit", "-m", "upstream "+name)
	runGit(t, seed, "push", "origin", "main")
}

// commitInSlot commits name with contents in the slot at dir.
func commitInSlot(t *testing.T, dir, name, contents string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(dir, name), contents)
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", "slot "+name)
}

// A merge always records a merge commit (--no-ff), made by angee's own
// identity without an editor; a conflict leaves the paths git reports,
// non-ASCII names unquoted, and abort restores the slot.
func TestWorkspaceSourceMergeRecordsAMergeCommitAndReportsRawConflictPaths(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	app := fixture.slotPath("app")
	gitIdentity(t, app)
	pushUpstreamChange(t, fixture, "upstream.txt", "upstream\n")
	if _, err := fixture.platform.WorkspaceSourceFetch(ctx, "feature-a", "app"); err != nil {
		t.Fatalf("WorkspaceSourceFetch() error = %v", err)
	}

	result, err := fixture.platform.WorkspaceSourceMerge(ctx, "feature-a", "app", "origin/main")
	if err != nil || !result.OK || result.Conflicted || len(result.ConflictFiles) != 0 || result.ConflictFiles == nil {
		t.Fatalf("WorkspaceSourceMerge() = %#v, %v, want OK with an empty conflict list", result, err)
	}
	if parents := strings.Fields(runGitOutput(t, app, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Fatalf("HEAD parents = %v, want a merge commit although a fast-forward was possible", parents)
	}
	if got := strings.TrimSpace(runGitOutput(t, app, "log", "-1", "--format=%an <%ae>|%cn <%ce>")); got != "angee <angee@example.invalid>|angee <angee@example.invalid>" {
		t.Fatalf("merge commit identity = %q, want angee's", got)
	}

	before := gitRev(t, app, "HEAD")
	pushUpstreamChange(t, fixture, "résumé.md", "upstream\n")
	commitInSlot(t, app, "résumé.md", "slot\n")
	runGit(t, app, "fetch", "origin")
	result, err = fixture.platform.WorkspaceSourceMerge(ctx, "feature-a", "app", "origin/main")
	if err != nil || result.OK || !result.Conflicted || !slices.Equal(result.ConflictFiles, []string{"résumé.md"}) {
		t.Fatalf("WorkspaceSourceMerge() = %#v, %v, want a conflict on résumé.md", result, err)
	}
	aborted, err := fixture.platform.WorkspaceSourceMergeAbort(ctx, "feature-a", "app")
	if err != nil || !aborted.OK || aborted.Conflicted {
		t.Fatalf("WorkspaceSourceMergeAbort() = %#v, %v, want OK", aborted, err)
	}
	if got := gitRev(t, app, "HEAD~1"); got != before {
		t.Fatalf("HEAD~1 after abort = %s, want the pre-merge commit %s", got, before)
	}
}

// A merge git refuses to start is a GIT_FAILED error naming the step, not a
// conflict.
func TestWorkspaceSourceMergeOfAnUnknownRefFails(t *testing.T) {
	fixture := newRepairFixture(t)
	result, err := fixture.platform.WorkspaceSourceMerge(context.Background(), "feature-a", "app", "no-such-ref")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeGitFailed || opErr.Slot != "app" || opErr.Source != "app" || result.Conflicted || result.OK {
		t.Fatalf("WorkspaceSourceMerge() = %#v, %+v, want GIT_FAILED without a conflict", result, opErr)
	}
	if want := `Merge for workspace "feature-a" slot "app": merging no-such-ref into source slot "app" failed.`; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("message = %q, want it to start %q", err.Error(), want)
	}
}

// A rebase that stops on conflicts can be aborted back to where it started,
// or continued without an editor once resolved; the replayed commit is
// committed by angee.
func TestWorkspaceSourceRebaseConflictAbortAndContinue(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	app := fixture.slotPath("app")
	gitIdentity(t, app)
	pushUpstreamChange(t, fixture, "README.md", "upstream change\n")
	commitInSlot(t, app, "README.md", "slot change\n")
	runGit(t, app, "fetch", "origin")
	before := gitRev(t, app, "HEAD")

	result, err := fixture.platform.WorkspaceSourceRebase(ctx, "feature-a", "app", "origin/main")
	if err != nil || result.OK || !result.Conflicted || !slices.Equal(result.ConflictFiles, []string{"README.md"}) {
		t.Fatalf("WorkspaceSourceRebase() = %#v, %v, want a conflict on README.md", result, err)
	}
	aborted, err := fixture.platform.WorkspaceSourceRebaseAbort(ctx, "feature-a", "app")
	if err != nil || !aborted.OK {
		t.Fatalf("WorkspaceSourceRebaseAbort() = %#v, %v, want OK", aborted, err)
	}
	if got := gitRev(t, app, "HEAD"); got != before {
		t.Fatalf("HEAD after abort = %s, want %s", got, before)
	}

	if result, err = fixture.platform.WorkspaceSourceRebase(ctx, "feature-a", "app", "origin/main"); err != nil || !result.Conflicted {
		t.Fatalf("WorkspaceSourceRebase() again = %#v, %v, want a conflict", result, err)
	}
	mustWriteFile(t, filepath.Join(app, "README.md"), "resolved\n")
	runGit(t, app, "add", "README.md")
	continued, err := fixture.platform.WorkspaceSourceRebaseContinue(ctx, "feature-a", "app")
	if err != nil || !continued.OK || continued.Conflicted {
		t.Fatalf("WorkspaceSourceRebaseContinue() = %#v, %v, want OK", continued, err)
	}
	if got, want := gitRev(t, app, "HEAD~1"), gitRev(t, app, "origin/main"); got != want {
		t.Fatalf("HEAD~1 after continue = %s, want origin/main %s", got, want)
	}
	if got := strings.TrimSpace(runGitOutput(t, app, "log", "-1", "--format=%cn <%ce>")); got != "angee <angee@example.invalid>" {
		t.Fatalf("rebased commit committer = %q, want angee's", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, app, "rev-parse", "--abbrev-ref", "HEAD")); got != "feature-a" {
		t.Fatalf("branch after continue = %q, want feature-a", got)
	}
}

// Fetch and pull bring the slot's upstream in: pull fast-forwards the slot's
// branch to what was pushed to it.
func TestWorkspaceSourceFetchAndPullFollowTheUpstream(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	app := fixture.slotPath("app")
	runGit(t, app, "push", "-u", "origin", "feature-a")
	seed := filepath.Join(fixture.base, "seed")
	runGit(t, seed, "fetch", "origin")
	runGit(t, seed, "switch", "-c", "feature-a", "origin/feature-a")
	mustWriteFile(t, filepath.Join(seed, "pushed.txt"), "pushed\n")
	runGit(t, seed, "add", "pushed.txt")
	runGit(t, seed, "commit", "-m", "pushed")
	runGit(t, seed, "push", "origin", "feature-a")
	pushed := gitRev(t, seed, "HEAD")

	status, err := fixture.platform.WorkspaceSourceFetch(ctx, "feature-a", "app")
	if err != nil || status.Behind != 1 || status.State != "behind" {
		t.Fatalf("WorkspaceSourceFetch() = %#v, %v, want the slot one behind its upstream", status, err)
	}
	status, err = fixture.platform.WorkspaceSourcePull(ctx, "feature-a", "app")
	if err != nil || status.Behind != 0 || status.State != "clean" {
		t.Fatalf("WorkspaceSourcePull() = %#v, %v, want the slot caught up", status, err)
	}
	if got := gitRev(t, app, "HEAD"); got != pushed {
		t.Fatalf("HEAD after pull = %s, want %s", got, pushed)
	}
}

// Each network verb on a slot that fails names the remote it used: fetch and
// pull the origin, push the push remote, publish the remote it was given.
func TestWorkspaceSourceNetworkFailuresNameTheirRemote(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	app := fixture.slotPath("app")
	gitIdentity(t, app)
	commitInSlot(t, app, "own.txt", "own\n")
	missing := filepath.Join(fixture.base, "missing.git")
	cache := filepath.Join(fixture.root, ".cache", "app")
	runGit(t, cache, "remote", "set-url", "origin", missing)
	other := filepath.Join(fixture.base, "other-missing.git")
	runGit(t, cache, "remote", "add", "other", other)

	for _, tc := range []struct {
		name      string
		run       func() error
		cause     string
		remote    string // "" for origin's missing URL
		wantStart string
	}{
		{"fetch", func() error { _, err := fixture.platform.WorkspaceSourceFetch(ctx, "feature-a", "app"); return err },
			CauseFetchFailed, "", `Fetch for workspace "feature-a" slot "app": fetching source "app" (` + missing + `) failed.`},
		{"pull", func() error { _, err := fixture.platform.WorkspaceSourcePull(ctx, "feature-a", "app"); return err },
			CauseFetchFailed, "", `Pull for workspace "feature-a" slot "app": pulling source slot "app" (` + missing + `) failed.`},
		{"push", func() error { _, err := fixture.platform.WorkspaceSourcePush(ctx, "feature-a", "app", ""); return err },
			CausePushFailed, "", `Push for workspace "feature-a" slot "app": pushing source slot "app" (` + missing + `) failed.`},
		{"publish", func() error {
			_, err := fixture.platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "")
			return err
		}, CausePushFailed, "", `Publish for workspace "feature-a" slot "app": pushing branch "feature-a" of source slot "app" (` + missing + `) failed.`},
		{"publish to another remote", func() error {
			_, err := fixture.platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "other", "")
			return err
		}, CausePushFailed, other, `Publish for workspace "feature-a" slot "app": pushing branch "feature-a" of source slot "app" (` + other + `) failed.`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := tc.remote
			if remote == "" {
				remote = missing
			}
			err := tc.run()
			opErr := AsOperationError(err)
			if opErr == nil || opErr.Code != CodeGitFailed || opErr.Cause != tc.cause || opErr.Remote != remote || opErr.Slot != "app" || opErr.Source != "app" {
				t.Fatalf("error = %+v, want GIT_FAILED %s naming %s", opErr, tc.cause, remote)
			}
			if !strings.HasPrefix(err.Error(), tc.wantStart) {
				t.Fatalf("message = %q, want it to start %q", err.Error(), tc.wantStart)
			}
		})
	}
}

// Sync-base by rebase replays the slot's own commits onto the fetched base,
// as the user (not angee), and a rebase that stops on conflicts is
// merge_conflict naming the step and the files, non-ASCII names unquoted.
func TestWorkspaceSyncBaseByRebase(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	app := fixture.slotPath("app")
	gitIdentity(t, app)
	pushUpstreamChange(t, fixture, "upstream.txt", "upstream\n")
	commitInSlot(t, app, "own.txt", "own\n")

	states, err := fixture.platform.WorkspaceSyncBase(ctx, "feature-a", "rebase")
	if err != nil || len(states) != 1 || states[0].Slot != "app" {
		t.Fatalf("WorkspaceSyncBase(rebase) = %#v, %v, want the app slot's state", states, err)
	}
	if got, want := gitRev(t, app, "HEAD~1"), gitRev(t, app, "origin/main"); got != want {
		t.Fatalf("HEAD~1 = %s, want origin/main %s", got, want)
	}
	if parents := strings.Fields(runGitOutput(t, app, "log", "-1", "--format=%P")); len(parents) != 1 {
		t.Fatalf("HEAD parents = %v, want a linear history", parents)
	}
	if got := strings.TrimSpace(runGitOutput(t, app, "log", "-1", "--format=%cn <%ce>")); got != "Test User <test@example.com>" {
		t.Fatalf("replayed commit committer = %q, want the user", got)
	}

	pushUpstreamChange(t, fixture, "résumé.md", "upstream change\n")
	commitInSlot(t, app, "résumé.md", "slot change\n")
	_, err = fixture.platform.WorkspaceSyncBase(ctx, "feature-a", "rebase")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeGitFailed || opErr.Cause != CauseMergeConflict || !slices.Equal(opErr.Paths, []string{"résumé.md"}) || opErr.Detail == "" {
		t.Fatalf("WorkspaceSyncBase(rebase) error = %+v, want merge_conflict on résumé.md with git's output", opErr)
	}
	if want := `Sync base for workspace "feature-a": rebasing onto origin/main source slot "app" stopped on conflicts in résumé.md.`; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("message = %q, want it to start %q", err.Error(), want)
	}
}

// Sync-base by merge of a diverged slot records the merge commit as the
// user, not angee.
func TestWorkspaceSyncBaseMergeCommitsAsTheUser(t *testing.T) {
	fixture := newRepairFixture(t)
	app := fixture.slotPath("app")
	gitIdentity(t, app)
	pushUpstreamChange(t, fixture, "upstream.txt", "upstream\n")
	commitInSlot(t, app, "own.txt", "own\n")

	if _, err := fixture.platform.WorkspaceSyncBase(context.Background(), "feature-a", "merge"); err != nil {
		t.Fatalf("WorkspaceSyncBase(merge) error = %v", err)
	}
	if parents := strings.Fields(runGitOutput(t, app, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Fatalf("HEAD parents = %v, want a merge commit", parents)
	}
	if got := strings.TrimSpace(runGitOutput(t, app, "log", "-1", "--format=%an <%ae>|%cn <%ce>")); got != "Test User <test@example.com>|Test User <test@example.com>" {
		t.Fatalf("merge commit identity = %q, want the user's", got)
	}
}

// Sync-base resolves every slot's base before it syncs any: a slot whose base
// exists nowhere refuses the sync naming that base, and a slot sorted before
// it, whose base moved on, is left as it was.
func TestWorkspaceSyncBaseResolvesEveryBaseBeforeSyncingAny(t *testing.T) {
	fixture := newTwoSlotFixture(t)
	app := fixture.slotPath("app")
	gitIdentity(t, app)
	fixture.declareSlot(t, "lib", nil, manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a", Ref: "no-such-base", Subpath: "lib"})
	pushUpstreamChange(t, fixture, "upstream.txt", "upstream\n")
	before := slotSnapshot(t, app)

	_, err := fixture.platform.WorkspaceSyncBase(context.Background(), "feature-a", "merge")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Slot != "lib" || opErr.Source != "lib" || opErr.Hint != `Check the base ref "no-such-base" of source slot "lib".` {
		t.Fatalf("WorkspaceSyncBase() error = %+v, want PRECONDITION_FAILED on lib's base", opErr)
	}
	if want := `Sync base for workspace "feature-a": source slot "lib": base ref "no-such-base" was not found after fetch. Check the base ref "no-such-base" of source slot "lib".`; err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if after := slotSnapshot(t, app); after != before {
		t.Fatalf("app slot changed from %q to %q, want it untouched", before, after)
	}
}

// Workspace push checks every slot is clean before pushing any: a dirty slot
// sorted after one with work to push refuses the push, and nothing is pushed.
func TestWorkspacePushChecksEverySlotIsCleanFirst(t *testing.T) {
	fixture := newTwoSlotFixture(t)
	app := fixture.slotPath("app")
	gitIdentity(t, app)
	commitInSlot(t, app, "own.txt", "own\n")
	mustWriteFile(t, filepath.Join(fixture.slotPath("lib"), "README.md"), "uncommitted\n")

	_, err := fixture.platform.WorkspacePush(context.Background(), "feature-a", "")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseUncommittedChanges || opErr.Slot != "lib" || !slices.Equal(opErr.Paths, []string{"README.md"}) {
		t.Fatalf("WorkspacePush() error = %+v, want uncommitted_changes on lib", opErr)
	}
	if pushed := strings.TrimSpace(runGitOutput(t, "", "ls-remote", filepath.Join(fixture.base, "remote.git"), "refs/heads/feature-a")); pushed != "" {
		t.Fatalf("app was pushed (%s), want nothing pushed", pushed)
	}
}
