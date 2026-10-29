package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// remoteBranch is the commit branch points at on the fixture's remote, or ""
// when it has no such branch.
func (f *jjFixture) remoteBranch(branch string) string {
	f.t.Helper()
	sha, _, _ := strings.Cut(strings.TrimSpace(runGitOutput(f.t, "", "ls-remote", f.remote, "refs/heads/"+branch)), "\t")
	return sha
}

// jjSlots creates workspace ws with a jj slot of app per name, at subpath
// name on branch ws-<name>, and returns their paths by name.
func (f *jjFixture) jjSlots(names ...string) map[string]string {
	f.t.Helper()
	slots := map[string]string{}
	for _, name := range names {
		slots[name] = f.app(fmt.Sprintf("mode: jj\nbranch: ws-%s\nref: main\nsubpath: %s", name, name))
	}
	if err := f.create(f.template("jj", slots), "ws", false); err != nil {
		f.t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	paths := map[string]string{}
	for _, name := range names {
		paths[name] = filepath.Join(f.workspace("ws"), name)
	}
	return paths
}

// workspace push publishes each jj slot whose manifest names its mode under
// its branch to the remote it pushes to, and only the slot with commits:
// one with none is left alone. A slot that names no mode under `vcs: jj` (a
// work-state `.work`) is skipped (plan Q7), and so is a jj slot of a `kind:
// local` store, the user's; the per-slot verbs still reach both.
func TestWorkspacePushPublishesTheJJSlotsWithCommits(t *testing.T) {
	f := newJJFixture(t, manifest.VCSJJ)
	ctx := context.Background()
	store, storeRemote := filepath.Join(f.base, "store"), filepath.Join(f.base, "store.git")
	runGit(t, f.base, "init", "-q", "--bare", "-b", "main", storeRemote)
	runGit(t, f.base, "init", "-q", "-b", "main", store)
	runGit(t, store, "remote", "add", "origin", storeRemote)
	f.jj(store, "git", "init", "--colocate")
	f.jjCommit(store, "f.txt", "one\n", "base")
	f.jj(store, "bookmark", "create", "main", "-r", "@-")
	if err := f.create(f.template("mixed", map[string]string{
		"a":     f.app("mode: jj\nbranch: ws-a\nref: main\nsubpath: a"),
		"b":     f.app("mode: jj\nbranch: ws-b\nref: main\nsubpath: b"),
		"notes": "source: notes\nkind: local\npath: " + store + "\ndefault_ref: main\nmode: jj\nbranch: ws-notes\nsubpath: notes",
		"work":  f.app("branch: ws-work\nref: main\nsubpath: .work"),
	}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	a, notes, work := filepath.Join(f.workspace("ws"), "a"), filepath.Join(f.workspace("ws"), "notes"), filepath.Join(f.workspace("ws"), ".work")
	f.jjCommit(a, "a.txt", "a\n", "a's change")
	f.jjCommit(notes, "n.txt", "notes\n", "the store's slot")
	f.jjCommit(work, "notes.txt", "notes\n", "notes")

	states, err := f.platform.WorkspacePush(ctx, "ws", "")
	if err != nil || len(states) != 2 || states[0].Slot != "a" || states[1].Slot != "b" {
		t.Fatalf("WorkspacePush() = %+v, %v, want slots a and b", states, err)
	}
	if got := states[0]; !got.Pushed || got.Upstream != "ws-a@origin" || got.Ahead != 0 {
		t.Errorf("slot a after the push = %+v, want it pushed against ws-a@origin", got)
	}
	if got, want := f.remoteBranch("ws-a"), f.revision(a, "@-", "commit_id"); got != want {
		t.Errorf("the remote has ws-a at %q, want a's commit %s", got, want)
	}
	for _, branch := range []string{"ws-b", "ws-work"} {
		if got := f.remoteBranch(branch); got != "" {
			t.Errorf("workspace push pushed %s at %s, want it left alone", branch, got)
		}
	}
	if status, err := f.platform.WorkspaceSourcePush(ctx, "ws", "work", ""); err != nil || !status.Pushed || status.Mode != manifest.WorkspaceSourceModeJJ {
		t.Fatalf("WorkspaceSourcePush(work) = %+v, %v, want the slot with no mode pushed by its own verb", status, err)
	}
	if got, want := f.remoteBranch("ws-work"), f.revision(work, "@-", "commit_id"); got != want {
		t.Errorf("the remote has ws-work at %q, want work's commit %s", got, want)
	}
	if out := runGitOutput(t, "", "ls-remote", storeRemote, "refs/heads/ws-notes"); out != "" {
		t.Fatalf("workspace push published the local store's slot: %s", out)
	}
	if result, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "notes", "", ""); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourcePublish(notes) = %+v, %v, want the store's slot published by its own verb", result, err)
	}
	if sha, _, _ := strings.Cut(strings.TrimSpace(runGitOutput(t, "", "ls-remote", storeRemote, "refs/heads/ws-notes")), "\t"); sha != f.revision(notes, "@-", "commit_id") {
		t.Errorf("the store's remote has ws-notes at %q, want the slot's commit", sha)
	}
}

// Publishing to a remote other than the branch's default records it, as
// `git push -u` does (owner, 2026-10-29): the slot's status then reads pushed
// against that remote, and a later publish with no remote goes there.
func TestPublishToAnotherRemoteIsFollowedAfter(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	slot := f.jjSlots("a")["a"]
	fork := filepath.Join(f.base, "fork.git")
	runGit(t, f.base, "init", "-q", "--bare", "-b", "main", fork)
	runGit(t, f.cache(), "remote", "add", "fork", fork)
	forkBranch := func() string {
		sha, _, _ := strings.Cut(strings.TrimSpace(runGitOutput(t, "", "ls-remote", fork, "refs/heads/ws-a")), "\t")
		return sha
	}
	f.jjCommit(slot, "a.txt", "a\n", "a's change")
	if result, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "fork", ""); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourcePublish(fork) = %+v, %v", result, err)
	}
	if got := f.slotStatus("ws", "a"); !got.Pushed || got.Upstream != "ws-a@fork" {
		t.Fatalf("status after the publish to fork = %+v, want pushed against ws-a@fork", got)
	}
	f.jjCommit(slot, "b.txt", "b\n", "more")
	if result, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "", ""); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourcePublish() = %+v, %v", result, err)
	}
	if got, want := forkBranch(), f.revision(slot, "@-", "commit_id"); got != want {
		t.Fatalf("fork has ws-a at %q, want the later commit %s", got, want)
	}
	if got := f.remoteBranch("ws-a"); got != "" {
		t.Fatalf("origin has ws-a at %s, want the later publish sent to fork", got)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "ws", false); err != nil {
		t.Fatalf("WorkspaceDestroy() error = %v, want the slot pushed to fork let go", err)
	}
}

// Commits jj will not push are refused as structured errors before anything
// is pushed (plan D6): undescribed_commits and conflicted_commits, naming
// each commit, PRECONDITION_FAILED. workspace push plans every slot first, so
// the slot sorted before the refused one is not pushed either.
func TestPublishRefusesCommitsJJWillNotPush(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	slots := f.jjSlots("a", "b")
	f.jjCommit(slots["a"], "f.txt", "one\nslot a\n", "a's change")
	mustWriteFile(t, filepath.Join(slots["b"], "b.txt"), "b\n")
	f.jj(slots["b"], "new")
	undescribed := f.revision(slots["b"], "@-", "commit_id.short()")

	for name, call := range map[string]func() error{
		"workspace push": func() error { _, err := f.platform.WorkspacePush(ctx, "ws", ""); return err },
		"publish":        func() error { _, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "b", "", ""); return err },
		"push":           func() error { _, err := f.platform.WorkspaceSourcePush(ctx, "ws", "b", ""); return err },
	} {
		err := call()
		var refused *vcs.UnpushableCommitsError
		opErr := AsOperationError(err)
		if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseUndescribedCommits || opErr.Slot != "b" || !errors.As(err, &refused) ||
			!slices.Equal(refused.Undescribed, []string{undescribed}) || !strings.Contains(err.Error(), `source slot "b" was not pushed: commits jj will not push: no description on `+undescribed) ||
			!strings.Contains(opErr.Hint, "jj describe") {
			t.Errorf("%s: error = %v (%+v), want undescribed_commits naming %s", name, err, opErr, undescribed)
		}
	}
	for _, branch := range []string{"ws-a", "ws-b"} {
		if got := f.remoteBranch(branch); got != "" {
			t.Errorf("a refused push pushed %s at %s", branch, got)
		}
	}

	f.jj(slots["b"], "describe", "-r", "@-", "-m", "b's change")
	f.moveBase("f.txt", "one\nupstream\n")
	if _, err := f.platform.WorkspaceSyncBase(ctx, "ws", ""); err != nil {
		t.Fatalf("WorkspaceSyncBase() error = %v", err)
	}
	conflicted := f.revision(slots["a"], "@-", "commit_id.short()")
	_, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "", "")
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseConflictedCommits || opErr.Slot != "a" ||
		!strings.Contains(err.Error(), "conflicts in "+conflicted) || !strings.Contains(opErr.Hint, "Resolve the conflicts") {
		t.Fatalf("WorkspaceSourcePublish(a) error = %v (%+v), want conflicted_commits naming %s", err, opErr, conflicted)
	}
	if result, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "b", "", ""); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourcePublish(b) once described = %+v, %v, want it published", result, err)
	}
}

// The first publish of a jj slot creates its remote branch; after a
// sync-base rebase rewrote the commit, the next publish force-updates it,
// with jj's lease, and the slot reads as pushed each time.
func TestPublishAfterSyncBaseForceUpdatesTheRemoteBranch(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	slot := f.jjSlots("a")["a"]
	f.jjCommit(slot, "a.txt", "a\n", "a's change")
	first := f.revision(slot, "@-", "commit_id")

	result, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "", "")
	if err != nil || !result.OK || !strings.Contains(result.Message, "ws-a") {
		t.Fatalf("WorkspaceSourcePublish() = %+v, %v, want the branch created", result, err)
	}
	if got := f.remoteBranch("ws-a"); got != first {
		t.Fatalf("the remote has ws-a at %q, want %s", got, first)
	}
	if got := f.slotStatus("ws", "a"); !got.Pushed || got.Upstream != "ws-a@origin" {
		t.Fatalf("status after the first publish = %+v, want pushed against ws-a@origin", got)
	}
	if result, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "", ""); err != nil || !result.OK || result.Message != "nothing to publish: ws-a@origin already holds every commit of the slot" {
		t.Fatalf("WorkspaceSourcePublish() of a published slot = %+v, %v, want nothing to publish", result, err)
	}

	f.moveBase("f.txt", "one\nupstream\n")
	if _, err := f.platform.WorkspaceSyncBase(ctx, "ws", ""); err != nil {
		t.Fatalf("WorkspaceSyncBase() error = %v", err)
	}
	rewritten := f.revision(slot, "@-", "commit_id")
	if rewritten == first {
		t.Fatal("sync-base did not rewrite the slot's commit")
	}
	if got := f.slotStatus("ws", "a"); got.Pushed {
		t.Fatalf("status after the rebase = %+v, want the rewritten commit unpushed", got)
	}
	result, err = f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "", "")
	if err != nil || !result.OK || !strings.Contains(result.Message, "sideways") {
		t.Fatalf("WorkspaceSourcePublish() after the rebase = %+v, %v, want the branch moved sideways", result, err)
	}
	if got := f.remoteBranch("ws-a"); got != rewritten {
		t.Fatalf("the remote has ws-a at %q, want the rewritten %s", got, rewritten)
	}
	if got := f.slotStatus("ws", "a"); !got.Pushed {
		t.Fatalf("status after the second publish = %+v, want it pushed", got)
	}
}

// A merge into a jj slot leaves the merge under a fresh working copy, so the
// publish after it pushes the merge. The ref is in jj's spelling; git's
// names nothing.
func TestMergeThenPublishPushesTheMerge(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	slot := f.jjSlots("a")["a"]
	f.jjCommit(slot, "a.txt", "a\n", "a's change")
	own := f.revision(slot, "@-", "commit_id")
	f.moveBase("h.txt", "upstream\n")
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "ws", "a"); err != nil {
		t.Fatalf("WorkspaceSourceFetch() error = %v", err)
	}
	if _, err := f.platform.WorkspaceSourceMerge(ctx, "ws", "a", "origin/main"); err == nil {
		t.Fatal("WorkspaceSourceMerge(origin/main) error = nil, want git's spelling refused by jj")
	}
	result, err := f.platform.WorkspaceSourceMerge(ctx, "ws", "a", "main@origin")
	if err != nil || !result.OK || result.Conflicted {
		t.Fatalf("WorkspaceSourceMerge() = %+v, %v, want a clean merge", result, err)
	}
	moved := strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main"))
	if got := f.revision(slot, "@-", `parents.map(|p| p.commit_id()).join(",") ++ "|" ++ description`); got != own+","+moved+"|Merge main@origin" {
		t.Fatalf("@- = %q, want the merge of %s and %s", got, own, moved)
	}
	merge := f.revision(slot, "@-", "commit_id")
	if result, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "", ""); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourcePublish() = %+v, %v", result, err)
	}
	if got := f.remoteBranch("ws-a"); got != merge {
		t.Fatalf("the remote has ws-a at %q, want the merge %s", got, merge)
	}
}

// Publish, push and merge of a jj slot act on the commit under its working
// copy, so they refuse what the working copy holds: changes, with jj's
// command that commits them, and untracked paths jj leaves out of it. Nothing
// is pushed or merged.
func TestJJSlotVerbsRefuseUncommittedWork(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	slot := f.jjSlots("a")["a"]
	f.jjCommit(slot, "a.txt", "a\n", "a's change")
	f.moveBase("h.txt", "upstream\n")
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "ws", "a"); err != nil {
		t.Fatalf("WorkspaceSourceFetch() error = %v", err)
	}
	calls := map[string]func() error{
		"publish":        func() error { _, err := f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "", ""); return err },
		"push":           func() error { _, err := f.platform.WorkspaceSourcePush(ctx, "ws", "a", ""); return err },
		"workspace push": func() error { _, err := f.platform.WorkspacePush(ctx, "ws", ""); return err },
		"merge":          func() error { _, err := f.platform.WorkspaceSourceMerge(ctx, "ws", "a", "main@origin"); return err },
	}

	mustWriteFile(t, filepath.Join(slot, "wip.txt"), "unrecorded\n")
	hint := fmt.Sprintf("Commit the working copy first with `jj -R %s commit -m <message>`, or restore the changes, then retry.", slot)
	for name, call := range calls {
		err := call()
		if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseUncommittedChanges || opErr.Hint != hint ||
			!slices.Equal(opErr.Paths, []string{"wip.txt"}) || !strings.Contains(err.Error(), `source slot "a" has uncommitted changes in wip.txt. `+hint) {
			t.Errorf("%s with a change: error = %v (%+v), want uncommitted_changes with jj's command", name, err, opErr)
		}
	}
	f.jj(slot, "commit", "-m", "wip")
	tip := f.revision(slot, "@-", "commit_id")

	f.jj(f.cache(), "config", "set", "--repo", "snapshot.max-new-file-size", "16")
	mustWriteFile(t, filepath.Join(slot, "big.bin"), strings.Repeat("x", 200))
	for name, call := range calls {
		err := call()
		verb := strings.TrimPrefix(name, "workspace ")
		if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseUntrackedPaths || !slices.Equal(opErr.Paths, []string{"big.bin"}) ||
			!strings.Contains(err.Error(), verb+" leaves out, since no commit holds them: big.bin") {
			t.Errorf("%s with an untracked file: error = %v (%+v), want untracked_paths", name, err, opErr)
		}
	}
	if got := f.revision(slot, "@-", "commit_id"); got != tip {
		t.Errorf("a refused merge moved @- from %s to %s", tip, got)
	}
	if got := f.remoteBranch("ws-a"); got != "" {
		t.Errorf("a refused push pushed ws-a at %s", got)
	}
}

// A jj slot's diff against a ref takes jj's spelling: `main@origin` is the
// remote bookmark, past the local main once fetched.
func TestWorkspaceSourceDiffOfAJJSlot(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	slot := f.jjSlots("a")["a"]
	f.jjCommit(slot, "f.txt", "one\nslot\n", "slot change")
	f.moveBase("h.txt", "upstream\n")
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "ws", "a"); err != nil {
		t.Fatalf("WorkspaceSourceFetch() error = %v", err)
	}
	files, err := f.platform.WorkspaceSourceDiff(ctx, "ws", "a", "main@origin")
	if err != nil {
		t.Fatalf("WorkspaceSourceDiff(main@origin) error = %v", err)
	}
	var paths []string
	for _, file := range files {
		paths = append(paths, file.NewPath)
	}
	if !slices.Equal(paths, []string{"f.txt", "h.txt"}) {
		t.Fatalf("WorkspaceSourceDiff(main@origin) paths = %q, want f.txt and the moved base's h.txt", paths)
	}
	mustWriteFile(t, filepath.Join(slot, "wip.txt"), "unrecorded\n")
	if files, err := f.platform.WorkspaceSourceDiff(ctx, "ws", "a", ""); err != nil || len(files) != 1 || files[0].NewPath != "wip.txt" {
		t.Fatalf("WorkspaceSourceDiff() = %+v, %v, want the uncommitted wip.txt", files, err)
	}
	// git's spelling names nothing in jj: jj's failure, quoted, not an
	// internal error.
	_, err = f.platform.WorkspaceSourceDiff(ctx, "ws", "a", "origin/main")
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeGitFailed || opErr.Tool != "jj" || !strings.HasPrefix(opErr.ToolLine, "Error:") {
		t.Fatalf("WorkspaceSourceDiff(origin/main) error = %v (%+v), want GIT_FAILED quoting jj", err, opErr)
	}
}

// workspace push over a workspace with a git worktree slot and a jj slot of
// one cache pushes both, each its own way: the worktree's branch with git,
// the jj slot's commits published under its branch.
func TestWorkspacePushPushesGitAndJJSlotsAlike(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("mixed", map[string]string{
		"a":    f.app("mode: jj\nbranch: ws-a\nref: main\nsubpath: a"),
		"tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
	}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	a, tree := filepath.Join(f.workspace("ws"), "a"), filepath.Join(f.workspace("ws"), "tree")
	f.jjCommit(a, "a.txt", "a\n", "a's change")
	mustWriteFile(t, filepath.Join(tree, "t.txt"), "tree\n")
	runGit(t, tree, "add", ".")
	runGit(t, tree, "commit", "-q", "-m", "tree's change")

	states, err := f.platform.WorkspacePush(ctx, "ws", "")
	if err != nil || len(states) != 2 || !states[0].Pushed || !states[1].Pushed {
		t.Fatalf("WorkspacePush() = %+v, %v, want both slots pushed", states, err)
	}
	if got, want := f.remoteBranch("ws-a"), f.revision(a, "@-", "commit_id"); got != want {
		t.Errorf("the remote has ws-a at %q, want a's commit %s", got, want)
	}
	if got, want := f.remoteBranch("ws"), strings.TrimSpace(runGitOutput(t, tree, "rev-parse", "HEAD")); got != want {
		t.Errorf("the remote has ws at %q, want the worktree's commit %s", got, want)
	}

	// The worktree slot has branch ws checked out: publishing the jj slot
	// under it would move it from under the worktree.
	_, err = f.platform.WorkspaceSourcePublish(ctx, "ws", "a", "", "ws")
	var invalid *InvalidInputError
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeInvalidInput || !errors.As(err, &invalid) || invalid.Field != "branch" ||
		!strings.Contains(err.Error(), `branch "ws" is checked out by the git worktree at `) {
		t.Fatalf("WorkspaceSourcePublish(--branch ws) error = %v (%+v), want the branch refused", err, opErr)
	}
	if got := strings.TrimSpace(runGitOutput(t, tree, "symbolic-ref", "--short", "HEAD")); got != "ws" {
		t.Errorf("the worktree slot is on %q after the refusal, want ws", got)
	}
}

// A jj slot of a `kind: local` store is never fetched into: the store is the
// user's. The refusal points at the slot's rebase onto its base.
func TestWorkspaceSourceFetchRefusesALocalStoreSlot(t *testing.T) {
	f := newJJFixture(t, "")
	store := filepath.Join(f.base, "store")
	runGit(t, f.base, "init", "-q", "-b", "main", store)
	f.jj(store, "git", "init", "--colocate")
	f.jjCommit(store, "f.txt", "one\n", "base")
	f.jj(store, "bookmark", "create", "main", "-r", "@-")
	if err := f.create(f.template("local", map[string]string{"notes": "source: notes\nkind: local\npath: " + store + "\ndefault_ref: main\nmode: jj\nsubpath: notes"}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	_, err := f.platform.WorkspaceSourceFetch(context.Background(), "ws", "notes")
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeInvalidInput || !strings.Contains(err.Error(), "angee never fetches into") ||
		!strings.Contains(err.Error(), "`angee workspace source rebase ws notes main`") {
		t.Fatalf("WorkspaceSourceFetch() error = %v (%+v), want INVALID_INPUT pointing at the rebase", err, opErr)
	}
}
