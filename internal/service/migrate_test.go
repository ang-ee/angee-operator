package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

// migrateFixture is a jjFixture with workspace ws whose slot app, of the
// app source, is a git worktree on branch ws (worktree) or a jj slot on
// bookmark ws (jj), holding a commit of its own and files neither tracks.
type migrateFixture struct {
	*jjFixture
	slot string
}

// newMigrateFixture creates ws with slot app in mode, a commit of its own on
// it (a .gitignore, g.txt and src/a.txt), and what the slot holds beyond its
// commits: ignored files at its top (.env, node_modules/) and below a tracked
// directory (src/debug.log), an empty directory, a repository nested in it,
// and, in a worktree, an untracked file and an untracked link, which a jj
// slot would snapshot.
func newMigrateFixture(t *testing.T, mode string) *migrateFixture {
	t.Helper()
	f := &migrateFixture{jjFixture: newJJFixture(t, "")}
	template := f.template("one", map[string]string{"app": f.app("mode: " + mode + "\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: app")})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	f.slot = filepath.Join(f.workspace("ws"), "app")
	mustWriteFile(t, filepath.Join(f.slot, ".gitignore"), "node_modules/\n.env\n*.log\n")
	mustWriteFile(t, filepath.Join(f.slot, "g.txt"), "mine\n")
	mustMkdirAll(t, filepath.Join(f.slot, "src"))
	mustWriteFile(t, filepath.Join(f.slot, "src", "a.txt"), "a\n")
	if mode == manifest.WorkspaceSourceModeJJ {
		f.jj(f.slot, "commit", "-m", "slot change")
	} else {
		runGit(t, f.slot, "add", ".")
		runGit(t, f.slot, "commit", "-q", "-m", "slot change")
		mustWriteFile(t, filepath.Join(f.slot, "notes.txt"), "notes\n")
		if err := os.Symlink("g.txt", filepath.Join(f.slot, "link")); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}
	}
	mustWriteFile(t, filepath.Join(f.slot, ".env"), "SECRET=1\n")
	mustMkdirAll(t, filepath.Join(f.slot, "node_modules", "pkg"))
	mustWriteFile(t, filepath.Join(f.slot, "node_modules", "pkg", "index.js"), "module.exports = 1\n")
	mustWriteFile(t, filepath.Join(f.slot, "src", "debug.log"), "log\n")
	mustMkdirAll(t, filepath.Join(f.slot, "empty"))
	runGit(t, f.slot, "init", "-q", "nested")
	mustWriteFile(t, filepath.Join(f.slot, "nested", "n.txt"), "nested\n")
	return f
}

// files maps every file, link and directory under the slot, but its own .git
// and .jj and a nested repository's .git, to its content, link target or
// "dir", which a migrate, and a migrate rolled back, must keep as they are.
func (f *migrateFixture) files() map[string]string {
	f.t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(f.slot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.slot, path)
		switch {
		case rel == ".":
			return nil
		case entry.Name() == ".git" || rel == ".jj":
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			files[rel] = "-> " + target
			return err
		case entry.IsDir():
			files[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		files[rel] = string(data)
		return err
	})
	if err != nil {
		f.t.Fatalf("WalkDir(%s) error = %v", f.slot, err)
	}
	return files
}

// recorded is the slot's record in the saved manifest.
func (f *migrateFixture) recorded() manifest.WorkspaceSource {
	f.t.Helper()
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		f.t.Fatalf("LoadFile() error = %v", err)
	}
	return stack.Workspaces["ws"].Sources["app"]
}

// head is the commit git has checked out in the slot.
func (f *migrateFixture) head() string {
	f.t.Helper()
	return strings.TrimSpace(runGitOutput(f.t, f.slot, "rev-parse", "HEAD"))
}

// tip is the commit under the jj slot's working copy.
func (f *migrateFixture) tip() string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", "@-", "-T", "commit_id"))
}

// noAside fails the test when anything a migrate set aside is left in the
// workspace, or the cache registers a worktree that is gone.
func (f *migrateFixture) noAside() {
	f.t.Helper()
	entries, err := os.ReadDir(f.workspace("ws"))
	if err != nil {
		f.t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), asidePrefix) {
			f.t.Errorf("a migrate left %s in the workspace", entry.Name())
		}
	}
	for _, reg := range f.registrations(f.cache()) {
		if strings.Contains(reg, asidePrefix) || strings.Contains(reg, "prunable") {
			f.t.Errorf("the cache keeps registration %q", reg)
		}
	}
}

// registeredAt reports the cache's registrations of a worktree at the slot.
func (f *migrateFixture) registeredAt() []string {
	f.t.Helper()
	var at []string
	for _, reg := range f.registrations(f.cache()) {
		path, _, _ := strings.Cut(strings.TrimPrefix(reg, "worktree "), "\n")
		if canonical(f.t, path) == canonical(f.t, f.slot) {
			at = append(at, reg)
		}
	}
	return at
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

// jjNames lists the cache's jj workspaces but its own.
func (f *migrateFixture) jjNames() []string {
	f.t.Helper()
	if !pathExists(f.t, filepath.Join(f.cache(), ".jj")) {
		return nil
	}
	return slices.DeleteFunc(sortedKeys(f.jjWorkspaces(f.cache())), func(name string) bool { return name == "default" })
}

// A worktree slot migrated to a jj slot and back keeps its commits and every
// file it holds beyond them, ignored or untracked, and the manifest records
// each mode. To jj: the slot is jj workspace dev--ws on bookmark ws, whose
// commit is the worktree's, and the untracked files git did not ignore are
// changes in its working-copy commit, which the message says. Back: the
// worktree is on branch ws at the commit under the jj slot's working copy,
// once that is committed, and the jj workspace is forgotten. Neither leaves
// a git registration of the slot's old checkout, a set-aside directory or a
// jj name behind.
func TestMigrateAWorktreeSlotToJJAndBack(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeWorktree)
	ctx := context.Background()
	files, head := f.files(), f.head()

	result, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", "")
	if err != nil || !result.OK {
		t.Fatalf("WorkspaceSourceMigrate(to jj) = %+v, %v", result, err)
	}
	for _, want := range []string{`Migrated source slot "app" to a jj slot: jj workspace "dev--ws"`, `on bookmark "ws"`, "Carried 7 untracked or ignored entries across", "changes in the jj slot's working-copy commit: empty/, link, nested/, notes.txt"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q does not say %q", result.Message, want)
		}
	}
	if got := f.recorded(); got.Mode != manifest.WorkspaceSourceModeJJ || got.VCS != "" {
		t.Errorf("recorded slot = %+v, want mode jj", got)
	}
	if !pathExists(t, filepath.Join(f.slot, ".jj")) {
		t.Fatalf("the slot is not a jj workspace")
	}
	if got := f.tip(); got != head {
		t.Errorf("the jj slot's @- is %s, want the worktree's HEAD %s", got, head)
	}
	if got := strings.TrimSpace(f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", "@-", "-T", "bookmarks")); got != "ws" {
		t.Errorf("bookmarks on @- = %q, want ws", got)
	}
	if got := f.files(); !maps.Equal(got, files) {
		t.Errorf("files after the migrate to jj = %v, want %v", got, files)
	}
	if got := f.jjNames(); !slices.Equal(got, []string{"dev--ws"}) {
		t.Errorf("jj workspaces = %q, want dev--ws", got)
	}
	if regs := f.registeredAt(); len(regs) != 1 || !strings.Contains(regs[0], "detached") {
		t.Errorf("registrations at the slot = %q, want the jj slot's alone", regs)
	}
	f.noAside()
	status := result.Source
	if status == nil || status.Mode != manifest.WorkspaceSourceModeJJ || status.State != "dirty" {
		t.Errorf("result's status = %+v, want a dirty jj slot", status)
	}
	if got := f.slotStatus("ws", "app"); got.Mode != manifest.WorkspaceSourceModeJJ || got.Error != "" {
		t.Errorf("status = %+v, want a jj slot", got)
	}

	f.jj(f.slot, "commit", "-m", "notes")
	tip := f.tip()
	result, err = f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", manifest.WorkspaceSourceModeWorktree)
	if err != nil || !result.OK {
		t.Fatalf("WorkspaceSourceMigrate(to worktree) = %+v, %v", result, err)
	}
	for _, want := range []string{`Migrated source slot "app" to a git worktree`, `on branch "ws", which now points at`, `jj workspace "dev--ws" is forgotten`, "Carried 5 untracked or ignored entries across"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q does not say %q", result.Message, want)
		}
	}
	if strings.Contains(result.Message, "Warning") {
		t.Errorf("message %q warns", result.Message)
	}
	if got := f.recorded(); got.Mode != manifest.WorkspaceSourceModeWorktree {
		t.Errorf("recorded slot = %+v, want mode worktree", got)
	}
	if pathExists(t, filepath.Join(f.slot, ".jj")) {
		t.Errorf("the worktree still has a .jj")
	}
	if got := strings.TrimSpace(runGitOutput(t, f.slot, "rev-parse", "--abbrev-ref", "HEAD")); got != "ws" {
		t.Errorf("the worktree is on %q, want ws", got)
	}
	if got := f.head(); got != tip {
		t.Errorf("the worktree is at %s, want the jj slot's %s", got, tip)
	}
	if got := f.files(); !maps.Equal(got, files) {
		t.Errorf("files after the migrate back = %v, want %v", got, files)
	}
	if got := f.jjNames(); len(got) != 0 {
		t.Errorf("jj workspaces after the migrate back = %q, want dev--ws forgotten", got)
	}
	if regs := f.registeredAt(); len(regs) != 1 || !strings.Contains(regs[0], "branch refs/heads/ws") {
		t.Errorf("registrations at the slot = %q, want the worktree's on ws alone", regs)
	}
	f.noAside()
	// git counts the untracked nested repository, as it did before the first
	// migrate.
	if got := f.slotStatus("ws", "app"); got.Mode != manifest.WorkspaceSourceModeWorktree || got.State != "dirty" || got.CurrentRef != "ws" || got.Error != "" {
		t.Errorf("status = %+v, want a worktree on ws, dirty with its untracked files", got)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err == nil || !strings.Contains(err.Error(), "app (") {
		t.Errorf("WorkspaceDestroy() error = %v, want the guard to read the worktree as unpushed", err)
	}
}

// A slot already in the mode asked for is left as it is.
func TestMigrateToTheModeASlotHas(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeWorktree)
	files := f.files()
	result, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", manifest.WorkspaceSourceModeWorktree)
	if err != nil || !result.OK || !strings.Contains(result.Message, "is a git worktree already") {
		t.Fatalf("WorkspaceSourceMigrate(worktree) of a worktree = %+v, %v, want nothing to migrate", result, err)
	}
	if got := f.files(); !maps.Equal(got, files) || pathExists(t, filepath.Join(f.cache(), ".jj")) {
		t.Errorf("a migrate with nothing to do changed the slot or the cache")
	}
}

// A jj slot that names no mode, made by `vcs: jj`, becomes a worktree that
// names its mode, and the message says the aggregates now walk it.
func TestMigrateAJJSlotWithNoMode(t *testing.T) {
	f := &migrateFixture{jjFixture: newJJFixture(t, manifest.VCSJJ)}
	if err := f.create(f.template("plain", map[string]string{"app": f.app("branch: \"${inputs.branch}\"\nref: main\nsubpath: app")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	f.slot = filepath.Join(f.workspace("ws"), "app")
	if got := f.recorded(); got.Mode != "" || got.VCS != manifest.VCSJJ {
		t.Fatalf("recorded slot = %+v, want no mode and vcs jj", got)
	}
	result, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
	if err != nil || !strings.Contains(result.Message, "now names its mode") || !strings.Contains(result.Message, "detached") && !strings.Contains(result.Message, `on branch "ws"`) {
		t.Fatalf("WorkspaceSourceMigrate() = %+v, %v", result, err)
	}
	if got := f.recorded(); got.Mode != manifest.WorkspaceSourceModeWorktree || got.VCS != "" {
		t.Errorf("recorded slot = %+v, want mode worktree and no vcs", got)
	}
}

// A jj slot on no branch becomes a worktree detached at the commit under its
// working copy, its own commits included, which a worktree on no branch
// holds as a git slot does.
func TestMigrateAJJSlotOnNoBranch(t *testing.T) {
	f := &migrateFixture{jjFixture: newJJFixture(t, "")}
	if err := f.create(f.template("plain", map[string]string{"app": f.app("mode: jj\nref: main\nsubpath: app")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	f.slot = filepath.Join(f.workspace("ws"), "app")
	mustWriteFile(t, filepath.Join(f.slot, "g.txt"), "mine\n")
	f.jj(f.slot, "commit", "-m", "slot change")
	tip := f.tip()
	result, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
	if err != nil || !strings.Contains(result.Message, "detached at "+tip[:12]) {
		t.Fatalf("WorkspaceSourceMigrate() = %+v, %v, want a worktree detached at %s", result, err, tip)
	}
	if got := f.head(); got != tip {
		t.Errorf("the worktree is at %s, want %s", got, tip)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.slot, "rev-parse", "--abbrev-ref", "HEAD")); got != "HEAD" {
		t.Errorf("the worktree is on %q, want detached", got)
	}
	if got := f.slotStatus("ws", "app"); got.Pushed || got.Error != "" {
		t.Errorf("status = %+v, want the commit of its own counted as unpushed", got)
	}
}

// intact fails the test unless the slot is still a mode slot with files,
// on head (a commit id, for git) or tip (for jj), and nothing a migrate set
// aside is left: the state a migrate refused or rolled back leaves.
func (f *migrateFixture) intact(mode string, files map[string]string, commit string, names []string) {
	f.t.Helper()
	if got := f.recorded(); got.Mode != mode {
		f.t.Errorf("recorded mode = %q, want %q", got.Mode, mode)
	}
	if got := f.files(); !maps.Equal(got, files) {
		f.t.Errorf("files = %v, want %v", got, files)
	}
	if mode == manifest.WorkspaceSourceModeJJ {
		if got := f.tip(); got != commit {
			f.t.Errorf("the jj slot's @- is %s, want %s", got, commit)
		}
		f.jj(f.slot, "status")
		if regs := f.registeredAt(); len(regs) != 1 {
			f.t.Errorf("registrations at the slot = %q, want one", regs)
		}
	} else {
		if got := f.head(); got != commit {
			f.t.Errorf("the worktree is at %s, want %s", got, commit)
		}
		if got := strings.TrimSpace(runGitOutput(f.t, f.slot, "rev-parse", "--abbrev-ref", "HEAD")); got != "ws" {
			f.t.Errorf("the worktree is on %q, want ws", got)
		}
		if regs := f.registeredAt(); len(regs) != 1 || !strings.Contains(regs[0], "branch refs/heads/ws") {
			f.t.Errorf("registrations at the slot = %q, want the worktree's on ws", regs)
		}
	}
	if got := f.jjNames(); !slices.Equal(got, names) {
		f.t.Errorf("jj workspaces = %q, want %q", got, names)
	}
	f.noAside()
	if got := f.slotStatus("ws", "app"); got.Mode != mode || got.Error != "" {
		f.t.Errorf("status = %+v, want a usable %s slot", got, mode)
	}
}

// A migrate that fails at any step, either way, rolls back: the slot is the
// checkout it was, with every file, on the same commit and branch, recorded
// in its mode, and the cache keeps no registration or jj name of the new
// checkout; a migrate run again then succeeds. The last case fails the
// manifest's write itself, which another writer changed meanwhile.
func TestMigrateRollsBackAFailureAtEachStep(t *testing.T) {
	injected := errors.New("injected failure")
	for _, mode := range []string{manifest.WorkspaceSourceModeWorktree, manifest.WorkspaceSourceModeJJ} {
		for _, step := range []string{migrateStepAside, migrateStepAdded, migrateStepCarried, migrateStepSave, "manifest changed"} {
			t.Run(mode+"/"+step, func(t *testing.T) {
				f := newMigrateFixture(t, mode)
				ctx := context.Background()
				files, commit, names := f.files(), f.head(), f.jjNames()
				to := manifest.WorkspaceSourceModeJJ
				if mode == manifest.WorkspaceSourceModeJJ {
					commit, to = f.tip(), manifest.WorkspaceSourceModeWorktree
					// Created at the bookmark's commit, which the migrate moves.
					f.jj(f.slot, "bookmark", "set", "ws", "--allow-backwards", "-r", "@--")
				}
				bookmark := ""
				if mode == manifest.WorkspaceSourceModeJJ {
					bookmark = strings.TrimSpace(f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", `bookmarks(exact:"ws")`, "-T", "commit_id"))
				}
				f.platform.migrateFault = func(at string) error {
					switch {
					case step == "manifest changed" && at == migrateStepSave:
						data, err := os.ReadFile(manifest.Path(f.root))
						if err != nil {
							return err
						}
						return os.WriteFile(manifest.Path(f.root), append(data, []byte("# edited\n")...), 0o644)
					case at == step:
						return injected
					}
					return nil
				}
				_, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", to)
				if step == "manifest changed" {
					if err == nil || !strings.Contains(err.Error(), "changed") {
						t.Fatalf("WorkspaceSourceMigrate() with the manifest changed error = %v, want the write refused", err)
					}
				} else if !errors.Is(err, injected) {
					t.Fatalf("WorkspaceSourceMigrate() error = %v, want the injected failure", err)
				}
				if strings.Contains(err.Error(), "could not be rolled back") {
					t.Fatalf("the rollback failed: %v", err)
				}
				f.intact(mode, files, commit, names)
				if mode == manifest.WorkspaceSourceModeJJ {
					if got := strings.TrimSpace(f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", `bookmarks(exact:"ws")`, "-T", "commit_id")); got != bookmark {
						t.Errorf("bookmark ws at %s, want it put back at %s", got, bookmark)
					}
				}

				f.platform.migrateFault = nil
				if step == "manifest changed" {
					// The comment is the other writer's; keep it, and migrate.
					if _, err := manifest.LoadFile(manifest.Path(f.root)); err != nil {
						t.Fatalf("LoadFile() error = %v", err)
					}
				}
				if result, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", to); err != nil || !result.OK {
					t.Fatalf("WorkspaceSourceMigrate() again = %+v, %v", result, err)
				}
				if got := f.files(); !maps.Equal(got, files) {
					t.Errorf("files after the migrate run again = %v, want %v", got, files)
				}
				f.noAside()
			})
		}
	}
}

// A migrate a slot is not ready for is refused before anything changes, in
// the error contract's terms, either way: changes to tracked files, a
// worktree off its branch, one on no branch with commits beyond its base, a
// jj name another checkout has; a jj slot's uncommitted changes, files jj
// does not track (untracked_paths), a described empty working copy, a
// conflicted commit under it, a bookmark off its history (branch_moved),
// and a branch another worktree has checked out.
func TestMigrateRefusesASlotThatIsNotReady(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		prepare    func(f *migrateFixture)
		code       string
		cause      string
		says       string
	}{
		{name: "changed tracked file", mode: manifest.WorkspaceSourceModeWorktree, code: CodePreconditionFailed, cause: CauseUncommittedChanges, says: `has uncommitted changes in g.txt`,
			prepare: func(f *migrateFixture) { mustWriteFile(f.t, filepath.Join(f.slot, "g.txt"), "changed\n") }},
		{name: "off its branch", mode: manifest.WorkspaceSourceModeWorktree, code: CodePreconditionFailed, cause: CauseBranchMismatch, says: "branch mismatch",
			prepare: func(f *migrateFixture) { runGit(f.t, f.slot, "switch", "-q", "-c", "other") }},
		{name: "name taken", mode: manifest.WorkspaceSourceModeWorktree, code: CodeConflict, says: `jj workspace "dev--ws" of`,
			prepare: func(f *migrateFixture) {
				f.jj(f.cache(), "git", "init", "--git-repo", ".")
				f.jj(f.cache(), "workspace", "add", "--colocate", "--name", "dev--ws", "-r", "main", filepath.Join(f.base, "elsewhere"))
			}},
		{name: "jj changes", mode: manifest.WorkspaceSourceModeJJ, code: CodePreconditionFailed, cause: CauseUncommittedChanges, says: "has uncommitted changes in wip.txt",
			prepare: func(f *migrateFixture) { mustWriteFile(f.t, filepath.Join(f.slot, "wip.txt"), "wip\n") }},
		{name: "untracked paths", mode: manifest.WorkspaceSourceModeJJ, code: CodePreconditionFailed, cause: CauseUntrackedPaths, says: "migrate leaves out, since no commit holds them: big.bin",
			prepare: func(f *migrateFixture) {
				f.jj(f.cache(), "config", "set", "--repo", "snapshot.max-new-file-size", "16")
				mustWriteFile(f.t, filepath.Join(f.slot, "big.bin"), strings.Repeat("x", 200))
			}},
		{name: "described working copy", mode: manifest.WorkspaceSourceModeJJ, code: CodePreconditionFailed, says: "which the migrate would leave behind",
			prepare: func(f *migrateFixture) { f.jj(f.slot, "describe", "-m", "next") }},
		{name: "conflicted commit", mode: manifest.WorkspaceSourceModeJJ, code: CodePreconditionFailed, says: "has conflicts, which a git worktree would check out",
			prepare: func(f *migrateFixture) {
				f.jjCommit(f.slot, "f.txt", "slot\n", "slot edit")
				f.moveBase("f.txt", "upstream\n")
				f.jj(f.slot, "git", "fetch")
				f.jj(f.slot, "rebase", "-b", "@", "--onto", "main@origin")
			}},
		{name: "bookmark off its history", mode: manifest.WorkspaceSourceModeJJ, code: CodePreconditionFailed, cause: CauseBranchMoved, says: "merge ws app ws`",
			prepare: func(f *migrateFixture) {
				f.jj(f.cache(), "new", "main@origin", "-m", "elsewhere")
				f.jj(f.cache(), "bookmark", "set", "ws", "--allow-backwards", "-r", "@")
				f.jj(f.cache(), "new")
			}},
		{name: "branch checked out elsewhere", mode: manifest.WorkspaceSourceModeJJ, code: CodePreconditionFailed, says: "git checks a branch out in one worktree only",
			prepare: func(f *migrateFixture) {
				f.jj(f.slot, "bookmark", "set", "ws", "-r", "@-")
				runGit(f.t, f.cache(), "worktree", "add", "-q", filepath.Join(f.base, "other"), "ws")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMigrateFixture(t, tc.mode)
			tc.prepare(f)
			files, names := f.files(), f.jjNames()
			commit := f.head()
			if tc.mode == manifest.WorkspaceSourceModeJJ {
				commit = f.tip()
			}
			_, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
			opErr := AsOperationError(err)
			if opErr == nil || opErr.Code != tc.code || opErr.Cause != tc.cause || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("WorkspaceSourceMigrate() error = %v (%+v), want %s %s saying %q", err, opErr, tc.code, tc.cause, tc.says)
			}
			if got := f.recorded(); got.Mode != tc.mode {
				t.Errorf("recorded mode = %q, want %q", got.Mode, tc.mode)
			}
			if got := f.files(); !maps.Equal(got, files) {
				t.Errorf("files = %v, want %v", got, files)
			}
			if tc.mode == manifest.WorkspaceSourceModeJJ && f.tip() != commit || tc.mode != manifest.WorkspaceSourceModeJJ && f.head() != commit {
				t.Errorf("the slot moved off %s", commit)
			}
			if got := f.jjNames(); !slices.Equal(got, names) {
				t.Errorf("jj workspaces = %q, want %q", got, names)
			}
			f.noAside()
		})
	}
}

// A slot on no branch whose HEAD holds commits beyond its base is refused,
// since a jj slot on no branch starts at the base; one at its base migrates.
func TestMigrateAWorktreeOnNoBranch(t *testing.T) {
	f := &migrateFixture{jjFixture: newJJFixture(t, "")}
	if err := f.create(f.template("plain", map[string]string{"app": f.app("mode: worktree\nref: main\nsubpath: app")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	f.slot = filepath.Join(f.workspace("ws"), "app")
	mustWriteFile(t, filepath.Join(f.slot, "g.txt"), "mine\n")
	runGit(t, f.slot, "add", ".")
	runGit(t, f.slot, "commit", "-q", "-m", "detached work")
	_, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
	if opErr := AsOperationError(err); opErr == nil || opErr.Cause != CauseDetachedHead || opErr.Slot != "app" || !strings.Contains(err.Error(), `source slot "app" names no branch`) {
		t.Fatalf("WorkspaceSourceMigrate() of a detached worktree with work error = %v, want detached_head", err)
	}
	runGit(t, f.slot, "checkout", "-q", "--detach", "HEAD~1")
	result, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
	if err != nil || !result.OK {
		t.Fatalf("WorkspaceSourceMigrate() of a detached worktree at its base = %+v, %v", result, err)
	}
	if got, want := f.tip(), strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main")); got != want {
		t.Errorf("the jj slot starts at %s, want the base %s", got, want)
	}
}

// What migrate cannot move is refused before anything is read from disk: a
// `to` it does not know, a slot of a local source, a clone, the workspace's
// own directory, and a slot others lie inside.
func TestMigratePlanRefusals(t *testing.T) {
	p := &Platform{root: t.TempDir()}
	stack := &manifest.Stack{Name: "dev",
		Sources: map[string]manifest.Source{"app": {Kind: "git", Repo: "https://example.invalid/app.git"}, "notes": {Kind: "local", Path: "/notes"}},
		Workspaces: map[string]manifest.Workspace{"ws": {Sources: map[string]manifest.WorkspaceSource{
			"tree":   {Source: "app", Mode: manifest.WorkspaceSourceModeWorktree, Subpath: "tree"},
			"inner":  {Source: "app", Mode: manifest.WorkspaceSourceModeWorktree, Subpath: "tree/inner"},
			"clone":  {Source: "app", Mode: manifest.WorkspaceSourceModeClone, Subpath: "clone"},
			"plain":  {Source: "app", Subpath: "plain"},
			"notes":  {Source: "notes", Mode: manifest.WorkspaceSourceModeJJ, Subpath: "notes"},
			"root":   {Source: "app", Mode: manifest.WorkspaceSourceModeJJ, Subpath: "."},
			"inside": {Source: "app", Mode: manifest.WorkspaceSourceModeWorktree, Subpath: "inside"},
		}}}}
	for _, tc := range []struct{ slot, to, field, says string }{
		{"inside", "hg", "to", `not "hg"`},
		{"notes", "", "slot", "is of local source"},
		{"clone", "", "slot", "is a clone or link"},
		{"plain", "jj", "slot", "is a clone or link"},
		{"root", "", "slot", "is the workspace's own directory"},
		{"tree", "", "slot", "lie inside source slot \"tree\" (inner)"},
	} {
		_, err := p.planMigration(stack, "ws", tc.slot, tc.to)
		var invalid *InvalidInputError
		if !errors.As(err, &invalid) || invalid.Field != tc.field || !strings.Contains(invalid.Reason, tc.says) {
			t.Errorf("planMigration(%s, %q) error = %v, want %s refused saying %q", tc.slot, tc.to, err, tc.field, tc.says)
		}
	}
	for _, tc := range []struct{ workspace, slot string }{{"none", "tree"}, {"ws", "none"}} {
		var missing *NotFoundError
		if _, err := p.planMigration(stack, tc.workspace, tc.slot, ""); !errors.As(err, &missing) {
			t.Errorf("planMigration(%s, %s) error = %v, want not found", tc.workspace, tc.slot, err)
		}
	}
	m, err := p.planMigration(stack, "ws", "inside", "")
	if err != nil || m.from != manifest.WorkspaceSourceModeWorktree || m.to != manifest.WorkspaceSourceModeJJ {
		t.Fatalf("planMigration(inside) = %+v, %v, want worktree to jj", m, err)
	}
}

// untrackedEntries lists the tops of what a jj slot's commit does not track,
// following no link and leaving out the slot's own .jj and .git.
func TestUntrackedEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".git", "src/a.txt", "src/debug.log", "node_modules/pkg/i.js", ".env", "deep/er/x.txt", "deep/er/y.log", ".jj/repo"} {
		mustWriteFileAll(t, filepath.Join(dir, name), "x")
	}
	mustMkdirAll(t, filepath.Join(dir, "empty"))
	if err := os.Symlink("src", filepath.Join(dir, "link")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	got, err := untrackedEntries(dir, []string{"src/a.txt", "deep/er/x.txt"})
	if err != nil {
		t.Fatalf("untrackedEntries() error = %v", err)
	}
	if want := []string{".env", "deep/er/y.log", "empty/", "link", "node_modules/", "src/debug.log"}; !slices.Equal(got, want) {
		t.Errorf("untrackedEntries() = %q, want %q", got, want)
	}
}

// failTool puts on PATH a name that fails, saying so, when its arguments
// hold every one of words, and otherwise runs the real tool.
func failTool(t *testing.T, name string, words ...string) {
	t.Helper()
	real, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("LookPath(%s) error = %v", name, err)
	}
	bin := t.TempDir()
	var match strings.Builder
	for _, word := range words {
		fmt.Fprintf(&match, "case \" $* \" in *\" %s \"*) ;; *) exec %q \"$@\";; esac\n", word, real)
	}
	script := "#!/bin/sh\n" + match.String() + "echo \"Error: injected " + name + " failure\" >&2\nexit 1\n"
	mustWriteFile(t, filepath.Join(bin, name), script)
	if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A migrate whose new checkout the tool fails to make (jj's workspace add,
// git's worktree add) rolls back as an injected failure does, the tool's
// own rollback of what it made included.
func TestMigrateRollsBackAToolThatFails(t *testing.T) {
	for _, tc := range []struct {
		mode, tool string
		words      []string
	}{
		{manifest.WorkspaceSourceModeWorktree, "jj", []string{"workspace", "add"}},
		{manifest.WorkspaceSourceModeJJ, "git", []string{"worktree", "add"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			f := newMigrateFixture(t, tc.mode)
			files, commit, names := f.files(), f.head(), f.jjNames()
			if tc.mode == manifest.WorkspaceSourceModeJJ {
				commit = f.tip()
			}
			failTool(t, tc.tool, tc.words...)
			_, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
			if err == nil || !strings.Contains(err.Error(), "injected "+tc.tool+" failure") || strings.Contains(err.Error(), "could not be rolled back") {
				t.Fatalf("WorkspaceSourceMigrate() error = %v, want the tool's failure, rolled back", err)
			}
			f.intact(tc.mode, files, commit, names)
		})
	}
}
