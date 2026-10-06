package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// A slot declared after the workspace was created is missing on disk. The git
// verbs name it and point at repair instead of failing on a chdir; repair cuts
// it as create would, leaves the existing slot alone, and changes nothing the
// second time.
func TestWorkspaceRepairMaterializesNewlyDeclaredSlot(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	libRemote := fixture.seedRemote(t, "lib")
	fixture.declareSlot(t, "lib", &manifest.Source{Kind: "git", Repo: libRemote, DefaultRef: "main", CachePath: ".cache/lib"},
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a", Ref: "main", Subpath: "lib"})
	libPath := fixture.slotPath("lib")

	_, err := fixture.platform.WorkspaceSyncBase(ctx, "feature-a", "merge")
	assertMissingSlotError(t, err, "feature-a", `source slot "lib"`)

	result, err := fixture.platform.WorkspaceRepair(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceRepair() error = %v", err)
	}
	assertRepairActions(t, result, map[string]string{"app": api.WorkspaceRepairOK, "lib": api.WorkspaceRepairCreated})
	if !result.OK || result.Workspace != "feature-a" || result.Path != fixture.workspacePath() {
		t.Fatalf("WorkspaceRepair() = %+v, want an OK result for feature-a at %s", result, fixture.workspacePath())
	}
	lib := result.Slots[1]
	if lib.Path != libPath || lib.Reason != `worktree on branch "feature-a", base main` {
		t.Fatalf("lib outcome = %+v, want the worktree cut at %s", lib, libPath)
	}
	if lib.Status == nil || lib.Status.State != "clean" || lib.Status.CurrentRef != "feature-a" || !lib.Status.Exists {
		t.Fatalf("lib post-repair status = %+v, want a clean slot on feature-a", lib.Status)
	}
	if got := strings.TrimSpace(runGitOutput(t, libPath, "branch", "--show-current")); got != "feature-a" {
		t.Fatalf("lib branch = %q, want feature-a", got)
	}

	again, err := fixture.platform.WorkspaceRepair(ctx, "feature-a")
	if err != nil {
		t.Fatalf("second WorkspaceRepair() error = %v", err)
	}
	assertRepairActions(t, again, map[string]string{"app": api.WorkspaceRepairOK, "lib": api.WorkspaceRepairOK})
	if _, err := fixture.platform.WorkspaceSyncBase(ctx, "feature-a", "merge"); err != nil {
		t.Fatalf("WorkspaceSyncBase() after repair error = %v", err)
	}
}

// A slot on disk is never changed. A dirty, diverged or wrong-branch slot is
// reported as needing attention, and the repair still succeeds.
func TestWorkspaceRepairReportsExistingSlotsWithoutTouchingThem(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prepare    func(t *testing.T, slotPath, cache string)
		wantReason string
	}{
		{name: "dirty", wantReason: "uncommitted changes", prepare: func(t *testing.T, slotPath, _ string) {
			mustWriteFile(t, filepath.Join(slotPath, "scratch.txt"), "work in progress\n")
		}},
		{name: "wrong branch", wantReason: `branch mismatch: current branch/ref "other", expected workspace branch "feature-a"`, prepare: func(t *testing.T, slotPath, _ string) {
			runGit(t, slotPath, "switch", "-c", "other")
		}},
		{name: "diverged", wantReason: "diverged: 1 commit(s) ahead of main and 1 behind", prepare: func(t *testing.T, slotPath, cache string) {
			commitFile(t, slotPath, "slot.txt")
			commitFile(t, cache, "base.txt")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			platform, workspaceName, slotPath, cache := setupGitWorkspace(t)
			tc.prepare(t, slotPath, cache)
			before := slotSnapshot(t, slotPath)

			result, err := platform.WorkspaceRepair(ctx, workspaceName)
			if err != nil {
				t.Fatalf("WorkspaceRepair() error = %v, want slots needing attention reported, not failed", err)
			}
			assertRepairActions(t, result, map[string]string{"app": api.WorkspaceRepairNeedsAttention})
			if got := result.Slots[0].Reason; got != tc.wantReason {
				t.Fatalf("app reason = %q, want %q", got, tc.wantReason)
			}
			if !result.OK {
				t.Fatalf("WorkspaceRepair() OK = false, want true: nothing failed")
			}
			if after := slotSnapshot(t, slotPath); after != before {
				t.Fatalf("slot changed by repair:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// A slot that cannot be materialized is reported as failed and leaves nothing
// at its path, a slot nested inside it is not attempted, and the other slots
// are still repaired. A slot on disk whose source is no longer declared only
// needs attention and does not block the slots nested in it, and a second slot
// on an already claimed path fails. The error names the failed slots.
func TestWorkspaceRepairReportsFailedSlotAndKeepsOthers(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	libRemote := fixture.seedRemote(t, "lib")
	fixture.declareSlot(t, "lib", &manifest.Source{Kind: "git", Repo: libRemote, DefaultRef: "main", CachePath: ".cache/lib"},
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a", Ref: "main", Subpath: "lib"})
	fixture.declareSlot(t, "broken", &manifest.Source{Kind: "git", Repo: filepath.Join(fixture.base, "no-such-remote.git"), DefaultRef: "main", CachePath: ".cache/broken"},
		manifest.WorkspaceSource{Source: "broken", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a", Ref: "main", Subpath: "broken"})
	fixture.declareSlot(t, "a-nested", nil,
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeClone, Ref: "main", Subpath: "broken/nested"})
	fixture.declareSlot(t, "ghost", nil, manifest.WorkspaceSource{Source: "ghost", Subpath: "ghost"})
	fixture.declareSlot(t, "lib-twin", nil, manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeClone, Ref: "main", Subpath: "lib"})
	// app's source is dropped from the manifest while app stays on disk, and a
	// slot is declared inside it.
	fixture.declareSlot(t, "app-child", nil, manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeClone, Ref: "main", Subpath: "app/child"})
	stack, err := manifest.LoadFile(manifest.Path(fixture.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	delete(stack.Sources, "app")
	if err := manifest.SaveFile(manifest.Path(fixture.root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		result, err := fixture.platform.WorkspaceRepair(ctx, "feature-a")
		var repairErr *WorkspaceRepairError
		if !errors.As(err, &repairErr) {
			t.Fatalf("attempt %d: WorkspaceRepair() error = %v, want a *WorkspaceRepairError", attempt, err)
		}
		if len(repairErr.Failed) != 4 || !strings.Contains(err.Error(), `"broken"`) || !strings.Contains(err.Error(), `"ghost" (source "ghost" is not declared)`) || !strings.Contains(err.Error(), `"lib-twin" (shares its path with slot "lib")`) {
			t.Fatalf("attempt %d: error = %v, want it to name a-nested, broken, ghost and lib-twin", attempt, err)
		}
		wantCreated := api.WorkspaceRepairCreated
		if attempt == 2 {
			wantCreated = api.WorkspaceRepairOK
		}
		assertRepairActions(t, result, map[string]string{
			"a-nested":  api.WorkspaceRepairFailed,
			"app":       api.WorkspaceRepairNeedsAttention,
			"app-child": wantCreated,
			"broken":    api.WorkspaceRepairFailed,
			"ghost":     api.WorkspaceRepairFailed,
			"lib":       wantCreated,
			"lib-twin":  api.WorkspaceRepairFailed,
		})
		if result.OK {
			t.Fatalf("attempt %d: WorkspaceRepair() OK = true, want false with failed slots", attempt)
		}
		if got := result.Slots[0].Reason; got != `not attempted: it lies inside slot "broken", which failed` {
			t.Fatalf("attempt %d: a-nested reason = %q, want it skipped inside the failed slot", attempt, got)
		}
		if got := result.Slots[1].Reason; got != `source "app" is not declared` {
			t.Fatalf("attempt %d: app reason = %q, want its undeclared source reported", attempt, got)
		}
		for _, slot := range []string{"broken", "ghost"} {
			if _, statErr := os.Lstat(fixture.slotPath(slot)); !os.IsNotExist(statErr) {
				t.Fatalf("attempt %d: %s path after a failed repair: Lstat err = %v, want nothing there", attempt, slot, statErr)
			}
		}
		if got := strings.TrimSpace(runGitOutput(t, fixture.slotPath("lib"), "branch", "--show-current")); got != "feature-a" {
			t.Fatalf("attempt %d: lib branch = %q, want feature-a", attempt, got)
		}
	}
}

// A slot that is cut but does not end up on its branch (here a post-checkout
// hook moves it) is rolled back the way a failed create rolls back: nothing at
// its path and no worktree registration left in the cache.
func TestWorkspaceRepairRollsBackSlotOffItsBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	libRemote := fixture.seedRemote(t, "lib")
	cache := filepath.Join(fixture.root, ".cache", "lib")
	runGit(t, "", "clone", libRemote, cache)
	hooks := filepath.Join(fixture.base, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks) error = %v", err)
	}
	hook := "#!/bin/sh\n[ \"$(git rev-parse --abbrev-ref HEAD)\" = hijacked ] || git switch -q -c hijacked\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatalf("WriteFile(post-checkout) error = %v", err)
	}
	runGit(t, cache, "config", "core.hooksPath", hooks)
	fixture.declareSlot(t, "lib", &manifest.Source{Kind: "git", Repo: libRemote, DefaultRef: "main", CachePath: ".cache/lib"},
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a", Ref: "main", Subpath: "lib"})

	result, err := fixture.platform.WorkspaceRepair(ctx, "feature-a")
	var repairErr *WorkspaceRepairError
	if !errors.As(err, &repairErr) || !strings.Contains(err.Error(), `branch mismatch: current branch/ref "hijacked"`) {
		t.Fatalf("WorkspaceRepair() error = %v, want lib failed on its branch check", err)
	}
	assertRepairActions(t, result, map[string]string{"app": api.WorkspaceRepairOK, "lib": api.WorkspaceRepairFailed})
	if _, statErr := os.Lstat(fixture.slotPath("lib")); !os.IsNotExist(statErr) {
		t.Fatalf("lib path after rollback: Lstat err = %v, want nothing there", statErr)
	}
	if got := strings.Count(runGitOutput(t, cache, "worktree", "list", "--porcelain"), "worktree "); got != 1 {
		t.Fatalf("worktrees registered in the lib cache after rollback = %d, want 1 (the cache only)", got)
	}
}

// A branchless worktree slot is cut detached at its base, without creating a
// branch in the cache, and a clone slot is cloned; a branchless worktree with
// no base at all fails before anything is cloned.
func TestWorkspaceRepairCutsDetachedAndCloneSlots(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	libRemote := fixture.seedRemote(t, "lib")
	fixture.declareSlot(t, "lib", &manifest.Source{Kind: "git", Repo: libRemote, DefaultRef: "main", CachePath: ".cache/lib"},
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeWorktree, Ref: "main", Subpath: "lib"})
	fixture.declareSlot(t, "docs", nil,
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeClone, Ref: "main", Subpath: "docs"})
	fixture.declareSlot(t, "nobase", &manifest.Source{Kind: "git", Repo: libRemote, CachePath: ".cache/nobase"},
		manifest.WorkspaceSource{Source: "nobase", Mode: manifest.WorkspaceSourceModeWorktree, Subpath: "nobase"})

	result, err := fixture.platform.WorkspaceRepair(ctx, "feature-a")
	if err == nil || !strings.Contains(err.Error(), "a worktree without a branch needs a ref") {
		t.Fatalf("WorkspaceRepair() error = %v, want nobase refused for having no base", err)
	}
	assertRepairActions(t, result, map[string]string{"app": api.WorkspaceRepairOK, "docs": api.WorkspaceRepairCreated, "lib": api.WorkspaceRepairCreated, "nobase": api.WorkspaceRepairFailed})
	if got := result.Slots[1].Reason; got != "cloned at main" {
		t.Fatalf("docs reason = %q, want a clone", got)
	}
	if info, err := os.Stat(filepath.Join(fixture.slotPath("docs"), ".git")); err != nil || !info.IsDir() {
		t.Fatalf("docs .git: info=%v err=%v, want a clone's .git directory", info, err)
	}
	if got := result.Slots[2].Reason; got != "worktree detached at main" {
		t.Fatalf("lib reason = %q, want a detached cut", got)
	}
	libPath := fixture.slotPath("lib")
	assertDetached(t, libPath)
	remoteMain := strings.TrimSpace(runGitOutput(t, "", "--git-dir", libRemote, "rev-parse", "main"))
	if got := strings.TrimSpace(runGitOutput(t, libPath, "rev-parse", "HEAD")); got != remoteMain {
		t.Fatalf("lib HEAD = %s, want main's commit %s", got, remoteMain)
	}
	if got := strings.TrimSpace(runGitOutput(t, filepath.Join(fixture.root, ".cache", "lib"), "branch", "--format=%(refname:short)")); got != "main" {
		t.Fatalf("lib cache branches = %q, want only main", got)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.root, ".cache", "nobase")); !os.IsNotExist(statErr) {
		t.Fatalf("nobase cache: Stat err = %v, want nothing cloned", statErr)
	}
}

// Repair works on a workspace that exists: an undeclared one is not found, a
// declared one whose directory is missing is left to workspace create, and a
// file in its place is refused. It does not start while another mutation holds
// the lease or once its context is cancelled.
func TestWorkspaceRepairRequiresMaterializedWorkspace(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), ".angee")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	stack := &manifest.Stack{
		Version:    manifest.VersionCurrent,
		Kind:       manifest.KindStack,
		Name:       "test",
		Workspaces: map[string]manifest.Workspace{"src": {Template: "workspaces/src"}},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var notFound *NotFoundError
	if _, err := platform.WorkspaceRepair(ctx, "nope"); !errors.As(err, &notFound) {
		t.Fatalf("WorkspaceRepair(undeclared) error = %v, want NotFoundError", err)
	}
	var conflict *ConflictError
	if _, err := platform.WorkspaceRepair(ctx, "src"); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "angee workspace create src --template workspaces/src") {
		t.Fatalf("WorkspaceRepair(unmaterialized) error = %v, want a conflict pointing at workspace create", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "workspaces", "src")); !os.IsNotExist(statErr) {
		t.Fatalf("workspace directory after a refused repair: Stat err = %v, want it still absent", statErr)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if result, err := platform.WorkspaceRepair(cancelled, "src"); !errors.Is(err, context.Canceled) || len(result.Slots) != 0 {
		t.Fatalf("WorkspaceRepair(cancelled) = %+v, %v; want context.Canceled and no result", result, err)
	}
	_, release, err := platform.beginMutation(ctx, "test")
	if err != nil {
		t.Fatalf("beginMutation() error = %v", err)
	}
	var invalid *InvalidInputError
	if _, err := platform.WorkspaceRepair(ctx, "src"); !errors.As(err, &invalid) || !strings.Contains(err.Error(), "another stack mutation is active") {
		t.Fatalf("WorkspaceRepair() under a held lease error = %v, want the lease refusal", err)
	}
	release()

	if err := os.MkdirAll(filepath.Join(root, "workspaces"), 0o755); err != nil {
		t.Fatalf("MkdirAll(workspaces) error = %v", err)
	}
	mustWriteFile(t, filepath.Join(root, "workspaces", "src"), "not a directory\n")
	if _, err := platform.WorkspaceRepair(ctx, "src"); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "is not a real directory") {
		t.Fatalf("WorkspaceRepair() over a file error = %v, want a not-a-directory conflict", err)
	}
}

// A slot deleted from disk after create is cut again on its existing branch,
// which still holds the slot's commits.
func TestWorkspaceRepairRecreatesDeletedSlot(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	appPath := fixture.slotPath("app")
	commitFile(t, appPath, "work.txt")
	head := strings.TrimSpace(runGitOutput(t, appPath, "rev-parse", "HEAD"))
	if err := os.RemoveAll(appPath); err != nil {
		t.Fatalf("RemoveAll(app) error = %v", err)
	}

	result, err := fixture.platform.WorkspaceRepair(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceRepair() error = %v", err)
	}
	assertRepairActions(t, result, map[string]string{"app": api.WorkspaceRepairCreated})
	if got := strings.TrimSpace(runGitOutput(t, appPath, "branch", "--show-current")); got != "feature-a" {
		t.Fatalf("app branch = %q, want feature-a", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, appPath, "rev-parse", "HEAD")); got != head {
		t.Fatalf("app HEAD = %s, want the branch's commit %s", got, head)
	}
	if got := result.Slots[0].Status; got == nil || got.State != "ahead" {
		t.Fatalf("app status = %+v, want ahead of main with its own commit", got)
	}
}

// With ANGEE_ROOT inside another git repository, what is at a slot's path
// decides what repair does: an empty directory is cut into, a directory that
// is not the slot's checkout or a dangling link is left alone, and the git
// verbs refuse such slots instead of acting on the enclosing repository.
func TestWorkspaceRepairInspectsWhatIsAtTheSlotPath(t *testing.T) {
	ctx := context.Background()
	fixture := newRepairFixture(t)
	runGit(t, fixture.base, "init", "-q")
	runGit(t, fixture.base, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-q", "--allow-empty", "-m", "host")
	libRemote := fixture.seedRemote(t, "lib")
	fixture.declareSlot(t, "empty", &manifest.Source{Kind: "git", Repo: libRemote, DefaultRef: "main", CachePath: ".cache/lib"},
		manifest.WorkspaceSource{Source: "empty", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a", Ref: "main", Subpath: "empty"})
	fixture.declareSlot(t, "stray", nil, manifest.WorkspaceSource{Source: "empty", Mode: manifest.WorkspaceSourceModeClone, Ref: "main", Subpath: "stray"})
	fixture.declareSlot(t, "link", nil, manifest.WorkspaceSource{Source: "empty", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a-link", Ref: "main", Subpath: "link"})
	if err := os.MkdirAll(fixture.slotPath("empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll(empty) error = %v", err)
	}
	if err := os.MkdirAll(fixture.slotPath("stray"), 0o755); err != nil {
		t.Fatalf("MkdirAll(stray) error = %v", err)
	}
	mustWriteFile(t, filepath.Join(fixture.slotPath("stray"), "notes.txt"), "keep me\n")
	if err := os.Symlink(filepath.Join(fixture.base, "gone"), fixture.slotPath("link")); err != nil {
		t.Fatalf("Symlink(link) error = %v", err)
	}

	_, err := fixture.platform.WorkspaceSourcePull(ctx, "feature-a", "stray")
	assertMissingSlotError(t, err, "feature-a", `source slot "stray" at `+fixture.slotPath("stray")+" is not a git checkout; move it aside")
	_, err = fixture.platform.WorkspaceSyncBase(ctx, "feature-a", "merge")
	assertMissingSlotError(t, err, "feature-a", `source slot "empty" is missing at`, `source slot "link" at `+fixture.slotPath("link")+" is a link whose target is missing; remove it", "materialize them")

	result, err := fixture.platform.WorkspaceRepair(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceRepair() error = %v", err)
	}
	assertRepairActions(t, result, map[string]string{
		"app":   api.WorkspaceRepairOK,
		"empty": api.WorkspaceRepairCreated,
		"link":  api.WorkspaceRepairNeedsAttention,
		"stray": api.WorkspaceRepairNeedsAttention,
	})
	emptyPath, err := filepath.EvalSymlinks(fixture.slotPath("empty"))
	if err != nil {
		t.Fatalf("EvalSymlinks(empty) error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, emptyPath, "rev-parse", "--show-toplevel")); got != emptyPath {
		t.Fatalf("empty slot toplevel = %q, want its own checkout %q", got, emptyPath)
	}
	if got := result.Slots[2].Reason; !strings.Contains(got, "is a link whose target is missing") {
		t.Fatalf("link reason = %q, want the dangling link reported", got)
	}
	if got := result.Slots[3]; !strings.Contains(got.Reason, "exists but is not a git checkout") || got.Status != nil {
		t.Fatalf("stray outcome = %+v, want a non-checkout reported without the enclosing repository's status", got)
	}
	if data, err := os.ReadFile(filepath.Join(fixture.slotPath("stray"), "notes.txt")); err != nil || string(data) != "keep me\n" {
		t.Fatalf("stray contents after repair = %q, %v; want them untouched", data, err)
	}
}

// Every verb that works on a slot names a missing one and points at repair,
// before touching any slot; status reports it as missing.
func TestMissingWorkspaceSlotErrorsPointToRepair(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, slotPath, cache := setupGitWorkspace(t)
	stack, err := manifest.LoadFile(manifest.Path(platform.Root()))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	workspace := stack.Workspaces[workspaceName]
	workspace.Sources["lib"] = manifest.WorkspaceSource{Source: "app", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a-lib", Ref: "main", Subpath: "lib"}
	workspace.Sources["docs"] = manifest.WorkspaceSource{Source: "app", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a-docs", Ref: "main", Subpath: "docs"}
	stack.Workspaces[workspaceName] = workspace
	if err := manifest.SaveFile(manifest.Path(platform.Root()), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	commitFile(t, cache, "base.txt")

	_, err = platform.WorkspaceSyncBase(ctx, workspaceName, "merge")
	assertMissingSlotError(t, err, workspaceName, `source slot "docs" is missing at`, `source slot "lib" is missing at`, "materialize them")
	if _, statErr := os.Stat(filepath.Join(slotPath, "base.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("app after a refused sync-base: Stat(base.txt) err = %v, want app left unsynced", statErr)
	}
	_, err = platform.WorkspacePush(ctx, workspaceName, "")
	assertMissingSlotError(t, err, workspaceName, `source slot "docs" is missing at`, `source slot "lib" is missing at`, "materialize them")

	slotVerbs := map[string]func() error{
		"fetch":    func() error { _, err := platform.WorkspaceSourceFetch(ctx, workspaceName, "lib"); return err },
		"pull":     func() error { _, err := platform.WorkspaceSourcePull(ctx, workspaceName, "lib"); return err },
		"push":     func() error { _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "lib", ""); return err },
		"diff":     func() error { _, err := platform.WorkspaceSourceDiff(ctx, workspaceName, "lib", ""); return err },
		"merge":    func() error { _, err := platform.WorkspaceSourceMerge(ctx, workspaceName, "lib", "main"); return err },
		"rebase":   func() error { _, err := platform.WorkspaceSourceRebase(ctx, workspaceName, "lib", "main"); return err },
		"publish":  func() error { _, err := platform.WorkspaceSourcePublish(ctx, workspaceName, "lib", "", ""); return err },
		"abort":    func() error { _, err := platform.WorkspaceSourceMergeAbort(ctx, workspaceName, "lib"); return err },
		"continue": func() error { _, err := platform.WorkspaceSourceRebaseContinue(ctx, workspaceName, "lib"); return err },
		"rb-abort": func() error { _, err := platform.WorkspaceSourceRebaseAbort(ctx, workspaceName, "lib"); return err },
	}
	for name, verb := range slotVerbs {
		t.Run(name, func(t *testing.T) {
			assertMissingSlotError(t, verb(), workspaceName, `source slot "lib" is missing at `+filepath.Join(platform.Root(), "workspaces", workspaceName, "lib")+"; run")
		})
	}

	states, err := platform.WorkspaceGitStatus(ctx, workspaceName)
	if err != nil {
		t.Fatalf("WorkspaceGitStatus() error = %v, want missing slots reported", err)
	}
	for _, state := range states {
		if state.Slot != "app" && (state.State != "missing" || state.Exists) {
			t.Fatalf("WorkspaceGitStatus() %s = %+v, want state missing", state.Slot, state)
		}
	}
}

// A slot's reason is one line, and a remote URL's credentials, which git
// errors quote, are redacted.
func TestRepairReasonIsOneRedactedLine(t *testing.T) {
	err := errors.New("git [clone https://user:s3cret@example.invalid/app.git /tmp/x]: exit status 128:\nfatal: repository not found\n")
	got := repairReason(err)
	if strings.Contains(got, "\n") || strings.Contains(got, "s3cret") || !strings.Contains(got, "fatal: repository not found") {
		t.Fatalf("repairReason() = %q, want one line without the credential", got)
	}
}

// repairFixture is a workspace "feature-a" created from a template with one
// git worktree slot, "app", at workspaces/feature-a/app on branch feature-a.
type repairFixture struct {
	base     string
	root     string
	platform *Platform
}

func newRepairFixture(t *testing.T) *repairFixture {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	root := filepath.Join(base, ".angee")
	templateRoot := filepath.Join(base, ".templates", "workspaces", "repair")
	if err := os.MkdirAll(filepath.Join(templateRoot, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(repair workspace template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: repair
  sources:
    app:
      kind: git
      repo: ` + remote + `
      default_ref: main
      cache_path: .cache/app
      mode: worktree
      branch: feature-a
      ref: main
      subpath: app
`
	mustWriteFile(t, filepath.Join(templateRoot, "copier.yml"), copierYAML)
	seedWorktreeRemote(t, base, remote)
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: templateRoot, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	return &repairFixture{base: base, root: root, platform: platform}
}

// seedRemote creates another bare remote with a committed main branch.
func (f *repairFixture) seedRemote(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(f.base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", dir, err)
	}
	remote := filepath.Join(dir, "remote.git")
	seedWorktreeRemote(t, dir, remote)
	return remote
}

// declareSlot adds a slot to feature-a in angee.yaml, and its source when one
// is given, the way a person extends an existing workspace.
func (f *repairFixture) declareSlot(t *testing.T, slot string, source *manifest.Source, wsSource manifest.WorkspaceSource) {
	t.Helper()
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if source != nil {
		stack.Sources[wsSource.Source] = *source
	}
	workspace := stack.Workspaces["feature-a"]
	workspace.Sources[slot] = wsSource
	stack.Workspaces["feature-a"] = workspace
	if err := manifest.SaveFile(manifest.Path(f.root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
}

func (f *repairFixture) workspacePath() string {
	return filepath.Join(f.root, "workspaces", "feature-a")
}

func (f *repairFixture) slotPath(subpath string) string {
	return filepath.Join(f.workspacePath(), subpath)
}

// assertRepairActions checks a repair reported exactly the given slots, in slot
// order, with the given actions.
func assertRepairActions(t *testing.T, result api.WorkspaceRepairResult, want map[string]string) {
	t.Helper()
	slots := sortedKeys(want)
	if len(result.Slots) != len(slots) {
		t.Fatalf("repair slots = %+v, want %v", result.Slots, want)
	}
	for i, slot := range slots {
		if got := result.Slots[i]; got.Slot != slot || got.Action != want[slot] {
			t.Fatalf("repair slot %d = %+v, want %s %s (all: %+v)", i, got, slot, want[slot], result.Slots)
		}
	}
}

// assertMissingSlotError checks err is the missing-slot refusal: a conflict
// that points at workspace repair and contains each of parts, not a git error.
func assertMissingSlotError(t *testing.T, err error, workspaceName string, parts ...string) {
	t.Helper()
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v (%T), want a missing-slot ConflictError", err, err)
	}
	message := err.Error()
	if !strings.Contains(message, "; run `angee workspace repair "+workspaceName+"` to materialize ") || strings.Contains(message, "chdir") {
		t.Fatalf("error = %q, want it to point at workspace repair", message)
	}
	for _, part := range parts {
		if !strings.Contains(message, part) {
			t.Fatalf("error = %q, want it to contain %q", message, part)
		}
	}
}

// slotSnapshot captures a checkout's HEAD, branch and working tree state.
func slotSnapshot(t *testing.T, dir string) string {
	t.Helper()
	return strings.Join([]string{
		strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD")),
		strings.TrimSpace(runGitOutputAllowFailure(t, dir, "branch", "--show-current")),
		strings.TrimSpace(runGitOutput(t, dir, "status", "--porcelain", "--untracked-files=all")),
	}, " | ")
}
