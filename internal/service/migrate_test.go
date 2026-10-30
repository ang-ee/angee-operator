package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// directory (src/debug.log), an empty directory, and, in a worktree, an
// untracked file and an untracked link, which a jj slot would snapshot. For
// repositories nested in the slot, see TestMigrateCarriesRepositoriesItMovesWhole
// and TestMigrateRefusesASlotThatIsNotReady.
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
	return f
}

// files maps every file, link and directory under the slot, but its own .git
// and .jj at its top, to its content, link target or "dir", which a migrate,
// and a migrate rolled back, must keep as they are. A .git or .jj further
// down is listed, and walked, as any directory is.
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
		case rel == ".git" || rel == ".jj":
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
// workspace, or the cache registers a worktree that is gone or one staged
// for a worktree slot (gitdriver.StageRoot).
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
		if strings.Contains(reg, asidePrefix) || strings.Contains(reg, "prunable") || strings.Contains(reg, "angee-worktree-stage-") {
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
	for _, want := range []string{`Migrated source slot "app" to a jj slot: jj workspace "dev--ws"`, `on bookmark "ws"`, "Carried 6 untracked or ignored entries across", "changes in the jj slot's working-copy commit: empty/, link, notes.txt"} {
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
	for _, want := range []string{`Migrated source slot "app" to a git worktree`, `on branch "ws", which now points at`, `jj workspace "dev--ws" is forgotten`, "Carried 4 untracked or ignored entries across"} {
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
	if got := f.slotStatus("ws", "app"); got.Mode != manifest.WorkspaceSourceModeWorktree || got.State != "ahead" || got.CurrentRef != "ws" || got.Error != "" {
		t.Errorf("status = %+v, want a worktree on ws, ahead of its base", got)
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
// worktree off its branch, one on no branch with commits beyond its base,
// files git is told not to look at, a submodule, a repository nested in a
// tracked directory or, to jj, in one git does not ignore, a jj name another
// checkout has; a jj slot's repository nested in a tracked directory, its
// uncommitted changes, files jj does not track (untracked_paths), a
// described empty working copy, a conflicted commit under it, a bookmark
// off its history (branch_moved), and a branch another worktree has checked
// out.
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
		{name: "skip-worktree file", mode: manifest.WorkspaceSourceModeWorktree, code: CodePreconditionFailed, says: "files git is told not to look at (skip-worktree or assume-unchanged), whose local versions would be checked out again as committed: g.txt",
			prepare: func(f *migrateFixture) {
				runGit(f.t, f.slot, "update-index", "--skip-worktree", "g.txt")
				mustWriteFile(f.t, filepath.Join(f.slot, "g.txt"), "local override\n")
			}},
		{name: "submodule", mode: manifest.WorkspaceSourceModeWorktree, code: CodePreconditionFailed, says: "it has submodules",
			prepare: func(f *migrateFixture) {
				// Not checked out: an empty directory, which git status reads
				// as clean. A checked-out one is a nested repository.
				base := strings.TrimSpace(runGitOutput(f.t, f.cache(), "rev-parse", "origin/main"))
				mustMkdirAll(f.t, filepath.Join(f.slot, "vendor", "lib"))
				runGit(f.t, f.slot, "update-index", "--add", "--cacheinfo", "160000,"+base+",vendor/lib")
				runGit(f.t, f.slot, "commit", "-q", "-m", "a submodule")
			}},
		{name: "repository in a tracked directory", mode: manifest.WorkspaceSourceModeWorktree, code: CodePreconditionFailed, cause: CauseNestedRepositories, says: "holds repositories outside the directories git ignores: src/.git. A migrate carries a repository across only inside a directory git ignores",
			prepare: func(f *migrateFixture) {
				runGit(f.t, filepath.Join(f.slot, "src"), "init", "-q", "-b", "gh-pages")
				mustWriteFile(f.t, filepath.Join(f.slot, "src", "page.html"), "page\n")
				runGit(f.t, filepath.Join(f.slot, "src"), "add", "page.html")
				runGit(f.t, filepath.Join(f.slot, "src"), "commit", "-q", "-m", "unpushed")
			}},
		{name: "repository in a directory git does not ignore", mode: manifest.WorkspaceSourceModeWorktree, code: CodePreconditionFailed, cause: CauseNestedRepositories, says: "outside the directories git ignores: vendor/lib/.git",
			prepare: func(f *migrateFixture) { runGit(f.t, f.slot, "init", "-q", "vendor/lib") }},
		{name: "nested jj repository", mode: manifest.WorkspaceSourceModeWorktree, code: CodePreconditionFailed, cause: CauseNestedRepositories, says: "outside the directories git ignores: src/.jj",
			prepare: func(f *migrateFixture) { mustMkdirAll(f.t, filepath.Join(f.slot, "src", ".jj")) }},
		{name: "jj repository in a tracked directory", mode: manifest.WorkspaceSourceModeJJ, code: CodePreconditionFailed, cause: CauseNestedRepositories, says: "holds repositories in directories that hold tracked files: src/.git. A migrate carries a repository across only inside a directory that holds no tracked file",
			prepare: func(f *migrateFixture) {
				runGit(f.t, filepath.Join(f.slot, "src"), "init", "-q", "-b", "gh-pages")
				mustWriteFile(f.t, filepath.Join(f.slot, "src", ".git", "info", "exclude"), "*\n")
			}},
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
		{name: "branch checked out elsewhere", mode: manifest.WorkspaceSourceModeJJ, code: CodePreconditionFailed, cause: CauseBranchCheckedOut, says: "git checks a branch out in one worktree only",
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

// A repository nested in a directory a migrate renames whole moves with it,
// either way, its commits and files with it: to jj, one in a directory git
// ignores (a package's clone under node_modules); to a worktree, one in a
// directory that holds no tracked file, at the slot's top or below a
// tracked directory, which jj reads as empty. The worktree then holds the
// latter two outside what git ignores, which a migrate to jj refuses,
// naming them alone.
func TestMigrateCarriesRepositoriesItMovesWhole(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeWorktree)
	ctx := context.Background()
	nested := func(rel string) string {
		dir := filepath.Join(f.slot, filepath.FromSlash(rel))
		runGit(t, f.slot, "init", "-q", "-b", "main", rel)
		mustWriteFile(t, filepath.Join(dir, "own.txt"), rel+"\n")
		runGit(t, dir, "add", "own.txt")
		runGit(t, dir, "commit", "-q", "-m", "unpushed")
		return strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	}
	commits := map[string]string{"node_modules/pkg": nested("node_modules/pkg")}
	files := f.files()
	if result, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", manifest.WorkspaceSourceModeJJ); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourceMigrate(jj) = %+v, %v", result, err)
	}
	if got := f.files(); !maps.Equal(got, files) {
		t.Errorf("files after the migrate to jj = %v, want %v", got, files)
	}
	f.noAside()

	f.jj(f.slot, "commit", "-m", "untracked files")
	commits["vendor/lib"], commits["src/lib"] = nested("vendor/lib"), nested("src/lib")
	files = f.files()
	if result, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", manifest.WorkspaceSourceModeWorktree); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourceMigrate(worktree) = %+v, %v", result, err)
	}
	if got := f.files(); !maps.Equal(got, files) {
		t.Errorf("files after the migrate to a worktree = %v, want %v", got, files)
	}
	for rel, commit := range commits {
		if got := strings.TrimSpace(runGitOutput(t, filepath.Join(f.slot, filepath.FromSlash(rel)), "rev-parse", "HEAD")); got != commit {
			t.Errorf("the repository in %s is at %s, want %s", rel, got, commit)
		}
	}
	f.noAside()

	_, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", manifest.WorkspaceSourceModeJJ)
	if opErr := AsOperationError(err); opErr == nil || opErr.Cause != CauseNestedRepositories || !slices.Equal(opErr.Paths, []string{filepath.Join("src", "lib", ".git"), filepath.Join("vendor", "lib", ".git")}) {
		t.Fatalf("WorkspaceSourceMigrate(jj) again error = %v, want nested_repositories naming src/lib/.git and vendor/lib/.git", err)
	}
}

// A repository in a directory git ignores whole beside one it re-includes
// (`/vendor/*` with `!/vendor/keep/`) migrates to jj inside vendor/, which
// the migrate renames whole, its commit with it: git lists vendor/other/ as
// ignored whole (`ls-files --others --ignored --directory`), though vendor/
// holds what git does not ignore. One in the re-included directory is still
// refused, naming it alone, since jj records nothing of it in its working
// copy.
func TestMigrateCarriesARepositoryGitIgnoresBesideAReincludedOne(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeWorktree)
	ctx := context.Background()
	mustWriteFile(t, filepath.Join(f.slot, ".gitignore"), "node_modules/\n.env\n*.log\n/vendor/*\n!/vendor/keep/\n")
	runGit(t, f.slot, "commit", "-q", "-am", "vendor")
	mustWriteFileAll(t, filepath.Join(f.slot, "vendor", "keep", "k.txt"), "kept\n")
	other := filepath.Join(f.slot, "vendor", "other")
	runGit(t, f.slot, "init", "-q", "-b", "main", "vendor/other")
	mustWriteFile(t, filepath.Join(other, "own.txt"), "own\n")
	runGit(t, other, "add", "own.txt")
	runGit(t, other, "commit", "-q", "-m", "unpushed")
	commit := strings.TrimSpace(runGitOutput(t, other, "rev-parse", "HEAD"))
	runGit(t, f.slot, "init", "-q", "vendor/keep/lib")
	_, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", manifest.WorkspaceSourceModeJJ)
	if opErr := AsOperationError(err); opErr == nil || opErr.Cause != CauseNestedRepositories || !slices.Equal(opErr.Paths, []string{filepath.Join("vendor", "keep", "lib", ".git")}) {
		t.Fatalf("WorkspaceSourceMigrate(jj) error = %v, want nested_repositories naming vendor/keep/lib/.git alone", err)
	}
	if err := os.RemoveAll(filepath.Join(f.slot, "vendor", "keep", "lib")); err != nil {
		t.Fatal(err)
	}
	files := f.files()
	if result, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", manifest.WorkspaceSourceModeJJ); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourceMigrate(jj) = %+v, %v", result, err)
	}
	if got := f.files(); !maps.Equal(got, files) {
		t.Errorf("files after the migrate to jj = %v, want %v", got, files)
	}
	if got := strings.TrimSpace(runGitOutput(t, other, "rev-parse", "HEAD")); got != commit {
		t.Errorf("the repository in vendor/other is at %s, want %s", got, commit)
	}
	f.noAside()
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

// A rollback that stops is reported as such, not as the step's failure: the
// error is INTERNAL, cause migrate_incomplete, and names where the slot's
// checkout is. Its record stays, so status, repair, destroy and the next
// migrate of the slot refuse with the same cause until the slot is put back
// and the record deleted.
func TestMigrateReportsARollbackThatStops(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeWorktree)
	ctx := context.Background()
	f.platform.migrateFault = func(at string) error {
		if at != migrateStepAside {
			return nil
		}
		// Something makes the slot's path again, which the rollback cannot
		// rename the checkout back over.
		mustMkdirAll(t, filepath.Join(f.slot, "intruder"))
		return uncommittedChanges([]string{"g.txt"}, "", `source slot "app"`, "app", "app")
	}
	_, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", "")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeInternal || opErr.Cause != CauseMigrateIncomplete || len(opErr.Paths) != 2 || !strings.HasPrefix(filepath.Base(opErr.Paths[1]), asidePrefix) ||
		!strings.Contains(err.Error(), "rolling it back stopped") || !strings.Contains(err.Error(), opErr.Paths[1]) {
		t.Fatalf("WorkspaceSourceMigrate() error = %v (%+v), want a stopped rollback naming the aside", err, opErr)
	}
	aside := opErr.Paths[1]
	if !pathExists(t, filepath.Join(aside, ".env")) {
		t.Fatalf("the checkout set aside at %s lost its files", aside)
	}
	f.platform.migrateFault = nil
	if got := f.slotStatus("ws", "app"); got.State != "error" || !strings.Contains(got.Error, "did not finish") || !strings.Contains(got.Error, aside) {
		t.Errorf("status = %+v, want the unfinished migrate named", got)
	}
	if _, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", ""); AsOperationError(err) == nil || AsOperationError(err).Cause != CauseMigrateIncomplete {
		t.Errorf("WorkspaceSourceMigrate() again error = %v, want migrate_incomplete", err)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("WorkspaceDestroy() error = %v, want it refused", err)
	}
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "ws", "app"); AsOperationError(err) == nil || AsOperationError(err).Cause != CauseMigrateIncomplete {
		t.Errorf("WorkspaceSourceFetch() error = %v, want migrate_incomplete", err)
	}
	if _, err := f.platform.WorkspaceSyncBase(ctx, "ws", ""); AsOperationError(err) == nil || AsOperationError(err).Cause != CauseMigrateIncomplete {
		t.Errorf("WorkspaceSyncBase() error = %v, want migrate_incomplete", err)
	}
	if !pathExists(t, aside) {
		t.Fatalf("a refused destroy removed the checkout set aside")
	}
	result, _ := f.platform.WorkspaceRepair(ctx, "ws")
	if len(result.Slots) != 1 || result.Slots[0].Action != "failed" || !strings.Contains(result.Slots[0].Reason, "did not finish") {
		t.Errorf("WorkspaceRepair() = %+v, want the slot refused", result)
	}

	// Put back by hand, as the hint says, the slot migrates.
	if err := os.RemoveAll(f.slot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aside, f.slot); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.cache(), "worktree", "repair", f.slot)
	if err := os.Remove(f.platform.migrateMarkerPath("ws", "app")); err != nil {
		t.Fatal(err)
	}
	if result, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", ""); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourceMigrate() once put back = %+v, %v", result, err)
	}
}

// asideOf is the one directory a migrate set aside in workspace ws.
func (f *migrateFixture) asideOf() string {
	f.t.Helper()
	matches, err := filepath.Glob(filepath.Join(f.workspace("ws"), asidePrefix+"*"))
	if err != nil || len(matches) != 1 {
		f.t.Fatalf("set-aside directories = %q, %v, want one", matches, err)
	}
	return matches[0]
}

// What is written into the old checkout after its files were carried across,
// as by a process whose working directory was in it, keeps the old checkout:
// the migrate succeeds, and warns, with its path, instead of deleting it; a
// jj slot's workspace is kept with it.
func TestMigrateKeepsAnOldCheckoutWrittenMeanwhile(t *testing.T) {
	for _, mode := range []string{manifest.WorkspaceSourceModeWorktree, manifest.WorkspaceSourceModeJJ} {
		t.Run(mode, func(t *testing.T) {
			f := newMigrateFixture(t, mode)
			var aside string
			f.platform.migrateFault = func(at string) error {
				if at == migrateStepSave {
					aside = f.asideOf()
					mustWriteFile(t, filepath.Join(aside, "late.log"), "written late\n")
				}
				return nil
			}
			result, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
			if err != nil || !result.OK || !strings.Contains(result.Message, "Warning: the old") || !strings.Contains(result.Message, "was kept at "+aside) || !strings.Contains(result.Message, "late.log") {
				t.Fatalf("WorkspaceSourceMigrate() = %+v, %v, want it migrated, warning that the old checkout was kept", result, err)
			}
			if data, err := os.ReadFile(filepath.Join(aside, "late.log")); err != nil || string(data) != "written late\n" {
				t.Fatalf("the late file = %q, %v, want it kept", data, err)
			}
			if mode == manifest.WorkspaceSourceModeJJ && !slices.Contains(f.jjNames(), "dev--ws") {
				t.Errorf("jj workspaces = %q, want dev--ws kept with its checkout", f.jjNames())
			}
			if pathExists(t, f.platform.migrateMarkerPath("ws", "app")) {
				t.Errorf("the migrate's record was left after it finished")
			}
			// The checkout kept is in the workspace: destroy refuses to purge it.
			if err := f.platform.WorkspaceDestroy(context.Background(), "ws", true); err == nil || !strings.Contains(err.Error(), aside) {
				t.Errorf("WorkspaceDestroy() error = %v, want it refused, naming %s", err, aside)
			}
			if !pathExists(t, filepath.Join(aside, "late.log")) {
				t.Errorf("a refused destroy removed the checkout kept")
			}
		})
	}
}

// A new worktree that cannot be moved back after the jj workspace is
// forgotten (something made the slot's path meanwhile) keeps the migrate's
// record: the slot's status names where the worktree is, and destroy
// refuses, rather than purging it with the workspace.
func TestMigrateKeepsItsRecordWhenTheNewWorktreeIsLeftAside(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeJJ)
	ctx := context.Background()
	f.platform.migrateFault = func(at string) error {
		if at == migrateStepForgotten {
			mustMkdirAll(t, filepath.Join(f.slot, "intruder"))
		}
		return nil
	}
	result, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", "")
	if err != nil || !strings.Contains(result.Message, "the new worktree was left at") || !strings.Contains(result.Message, "record") {
		t.Fatalf("WorkspaceSourceMigrate() = %+v, %v, want it migrated, warning that the new worktree was left aside", result, err)
	}
	aside := f.asideOf()
	if !pathExists(t, filepath.Join(aside, ".env")) {
		t.Fatalf("the new worktree left at %s lost its carried files", aside)
	}
	if !pathExists(t, f.platform.migrateMarkerPath("ws", "app")) {
		t.Fatalf("the migrate's record was removed though the new worktree is left aside")
	}
	f.platform.migrateFault = nil
	if got := f.slotStatus("ws", "app"); got.State != "error" || !strings.Contains(got.Error, "did not finish cleaning up") || !strings.Contains(got.Error, "mv "+shellQuote(aside)+" "+shellQuote(f.slot)) {
		t.Errorf("status = %+v, want the unfinished clean-up named, with the move back of %s", got, aside)
	}
	err = f.platform.WorkspaceDestroy(ctx, "ws", true)
	if err == nil || !strings.Contains(err.Error(), aside) {
		t.Errorf("WorkspaceDestroy() error = %v, want it refused, naming %s", err, aside)
	}
	if err != nil && strings.Contains(err.Error(), "worktree remove --force --force "+shellQuote(aside)) {
		t.Errorf("WorkspaceDestroy() error = %v, says to remove the new worktree, which is to be moved back", err)
	}
	if !pathExists(t, aside) {
		t.Fatalf("a refused destroy removed the worktree left aside")
	}
	// Once what made the slot's path is gone, following the record's steps
	// puts the slot right.
	if err := os.RemoveAll(f.slot); err != nil {
		t.Fatal(err)
	}
	f.followRecord()
	if got := f.slotStatus("ws", "app"); got.Mode != manifest.WorkspaceSourceModeWorktree || got.Error != "" || !pathExists(t, filepath.Join(f.slot, ".env")) {
		t.Errorf("status once the record is followed = %+v, want a usable worktree with its files", got)
	}
	f.noAside()
}

// Other worktrees of the cache are left as they are by a migrate, either way
// and when it rolls back: their links keep their form, and a registration
// whose checkout is gone is not pruned.
func TestMigrateLeavesOtherWorktreesAlone(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeWorktree)
	ctx := context.Background()
	other, gone := filepath.Join(f.base, "other"), filepath.Join(f.base, "gone")
	runGit(t, f.cache(), "-c", "worktree.useRelativePaths=false", "worktree", "add", "-q", "--detach", other, "main")
	runGit(t, f.cache(), "worktree", "add", "-q", "--detach", gone, "main")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	link := func() string {
		data, err := os.ReadFile(filepath.Join(other, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	before := link()
	check := func(when string) {
		t.Helper()
		if got := link(); got != before {
			t.Errorf("%s: the other worktree's .git = %q, want %q", when, got, before)
		}
		if regs := strings.Join(f.registrations(f.cache()), "\n"); !strings.Contains(regs, "worktree "+canonical(t, f.base)+"/gone") && !strings.Contains(regs, gone) {
			t.Errorf("%s: the gone worktree's registration was pruned: %s", when, regs)
		}
	}
	failTool(t, "jj", "workspace", "add")
	if _, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", ""); err == nil {
		t.Fatalf("WorkspaceSourceMigrate() with jj failing succeeded")
	}
	check("after a rollback")
	t.Setenv("PATH", strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[1])
	if _, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", manifest.WorkspaceSourceModeJJ); err != nil {
		t.Fatalf("WorkspaceSourceMigrate(jj) error = %v", err)
	}
	check("after a migrate to jj")
	f.jj(f.slot, "commit", "-m", "notes")
	failTool(t, "git", "worktree", "add")
	if _, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", ""); err == nil {
		t.Fatalf("WorkspaceSourceMigrate() with git's worktree add failing succeeded")
	}
	check("after a git rollback")
	t.Setenv("PATH", strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[1])
	if _, err := f.platform.WorkspaceSourceMigrate(ctx, "ws", "app", manifest.WorkspaceSourceModeWorktree); err != nil {
		t.Fatalf("WorkspaceSourceMigrate(worktree) error = %v", err)
	}
	check("after a migrate to a worktree")
}

// killedMigrate is the panic killAt's fault raises, as a process killed
// after a step stops: nothing after it runs, no rollback included.
type killedMigrate struct{}

// killAt runs a migrate of slot app to to that stops dead after step, once
// before, when given, has run there: what the next action had done of its
// work when the kill came.
func (f *migrateFixture) killAt(step, to string, before func()) {
	f.t.Helper()
	f.platform.migrateFault = func(at string) error {
		if at == step {
			if before != nil {
				before()
			}
			panic(killedMigrate{})
		}
		return nil
	}
	func() {
		defer func() {
			if _, killed := recover().(killedMigrate); !killed {
				f.t.Fatalf("the migrate was not killed at %s", step)
			}
		}()
		_, _ = f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", to)
	}()
	f.platform.migrateFault = nil
}

// stage is where git makes the worktree of a migrate to one, killed after
// its stage was made: in the one stage root in TMPDIR.
func (f *migrateFixture) stage() string {
	f.t.Helper()
	roots, err := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "angee-worktree-stage-*"))
	if err != nil || len(roots) != 1 {
		f.t.Fatalf("stage roots = %q, %v, want one", roots, err)
	}
	return filepath.Join(roots[0], "source")
}

// followRecord recovers by hand from a migrate that stopped, as its record
// says: it runs every command the record's steps quote, in order, then
// deletes the record, and returns the record's phase.
func (f *migrateFixture) followRecord() string {
	f.t.Helper()
	phase, step, out, err := f.runRecord()
	if err != nil {
		f.t.Fatalf("step %q: %v\n%s", step, err, out)
	}
	if err := os.Remove(f.platform.migrateMarkerPath("ws", "app")); err != nil {
		f.t.Fatal(err)
	}
	return phase
}

// runRecord runs every command the steps of the migrate's record quote, in
// order, until one fails, and returns the record's phase and, when one
// failed, its step, its output and the error.
func (f *migrateFixture) runRecord() (phase, step, out string, err error) {
	f.t.Helper()
	marker := f.platform.migrateMarkerPath("ws", "app")
	data, err := os.ReadFile(marker)
	if err != nil {
		f.t.Fatalf("ReadFile(%s) error = %v", marker, err)
	}
	var record migrateMarker
	if err := json.Unmarshal(data, &record); err != nil {
		f.t.Fatalf("decode the record: %v", err)
	}
	command := regexp.MustCompile("`([^`]+)`")
	for _, step := range record.Steps {
		for _, quoted := range command.FindAllStringSubmatch(step, -1) {
			cmd := exec.Command("sh", "-c", quoted[1])
			cmd.Dir = f.base
			if out, err := cmd.CombinedOutput(); err != nil {
				return record.Phase, step, string(out), fmt.Errorf("%s: %w", quoted[1], err)
			}
		}
	}
	return record.Phase, "", "", nil
}

// A migrate killed after a step leaves a record whose steps fit how far it
// got, and following them by hand recovers: before the manifest is written,
// the entries carried into the new checkout go back, the new checkout is
// removed, and the slot is moved back, as it was, so a migrate then runs;
// once it is written, the slot is migrated, and the steps clean up the old
// checkout (forgetting the jj workspace the slot was, with nothing at its
// path, when the new worktree was set aside for that), leaving nothing
// behind. Until then, the slot's status names the steps.
//
// The kills before the write are: before the new checkout exists; while it
// is being made (adding, with what the tool had made by then: a whole jj
// slot, jj's name and git's registration included, or a worktree, made and
// registered by git in its stage, with its copy at the slot's path, which
// git had not registered there yet, so the record's steps remove the stage
// by its path, leaving nothing registered or on disk); and right after the
// first entry is renamed into it (carried), before anything else, when the
// record holds every kind of undo step (the bookmark, the slot set aside,
// the new checkout, the entries) and names every entry, renamed or not.
// Each phase after the write is killed once. Killing it after every step
// was trimmed: the suite runs under -race in CI's jj job, within go test's
// ten minutes.
func TestMigrateKilledAtEachStepRecovers(t *testing.T) {
	for _, tc := range []struct {
		mode, step, phase string
		// before is what the action after step had done when it was killed.
		before func(f *migrateFixture)
	}{
		{mode: manifest.WorkspaceSourceModeWorktree, step: migrateStepAside, phase: migratePhaseMigrating},
		{mode: manifest.WorkspaceSourceModeWorktree, step: migrateStepAdding, phase: migratePhaseMigrating, before: func(f *migrateFixture) {
			f.jj(f.cache(), "workspace", "add", "--colocate", "--name", "dev--ws", "-r", "main", f.slot)
		}},
		{mode: manifest.WorkspaceSourceModeWorktree, step: migrateStepCarried, phase: migratePhaseMigrating},
		{mode: manifest.WorkspaceSourceModeWorktree, step: migrateStepSaved, phase: migratePhaseSaved},
		{mode: manifest.WorkspaceSourceModeJJ, step: migrateStepAdding, phase: migratePhaseMigrating, before: func(f *migrateFixture) {
			runGit(f.t, f.cache(), "worktree", "add", "-q", f.stage(), "ws")
			mustWriteFileAll(f.t, filepath.Join(f.slot, "src", "a.txt"), "a\n")
		}},
		{mode: manifest.WorkspaceSourceModeJJ, step: migrateStepCarried, phase: migratePhaseMigrating},
		{mode: manifest.WorkspaceSourceModeJJ, step: migrateStepSaved, phase: migratePhaseSaved},
		{mode: manifest.WorkspaceSourceModeJJ, step: migrateStepForgotten, phase: migratePhaseForgetting},
	} {
		t.Run(tc.mode+"/"+tc.step, func(t *testing.T) {
			f := newMigrateFixture(t, tc.mode)
			// The stage of a worktree's add (gitdriver.StageRoot), where
			// nothing else stages one.
			t.Setenv("TMPDIR", t.TempDir())
			to := manifest.WorkspaceSourceModeJJ
			files, commit, names := f.files(), f.head(), f.jjNames()
			if tc.mode == manifest.WorkspaceSourceModeJJ {
				to, commit = manifest.WorkspaceSourceModeWorktree, f.tip()
				f.jj(f.slot, "bookmark", "set", "ws", "--allow-backwards", "-r", "@--")
			}
			var before func()
			if tc.before != nil {
				before = func() { tc.before(f) }
			}
			f.killAt(tc.step, to, before)
			if got := f.slotStatus("ws", "app"); got.State != "error" || !strings.Contains(got.Error, "Recover by hand, in this order: 1. ") {
				t.Fatalf("status after the kill = %+v, want the steps to recover", got)
			}
			if tc.step == migrateStepCarried {
				// .env, the first entry either way, is renamed, and the
				// record names node_modules/, which is not yet.
				aside := f.asideOf()
				if !pathExists(t, filepath.Join(f.slot, ".env")) || pathExists(t, filepath.Join(aside, ".env")) || !pathExists(t, filepath.Join(aside, "node_modules")) {
					t.Fatalf("the kill did not come right after .env was renamed into the new checkout")
				}
				if data, err := os.ReadFile(f.platform.migrateMarkerPath("ws", "app")); err != nil || !strings.Contains(string(data), "if node_modules/ is in the new checkout") {
					t.Fatalf("the record = %s, %v, want it to name every entry", data, err)
				}
			}
			if phase := f.followRecord(); phase != tc.phase {
				t.Fatalf("the record's phase = %q, want %q", phase, tc.phase)
			}
			if staged, _ := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "angee-worktree-stage-*")); len(staged) != 0 {
				t.Errorf("following the record left the staging directories %q", staged)
			}
			if tc.phase == migratePhaseMigrating {
				f.intact(tc.mode, files, commit, names)
				if result, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", to); err != nil || !result.OK {
					t.Fatalf("WorkspaceSourceMigrate() once recovered = %+v, %v", result, err)
				}
				return
			}
			if got := f.recorded(); got.Mode != to {
				t.Errorf("recorded mode = %q, want %q", got.Mode, to)
			}
			if got := f.files(); !maps.Equal(got, files) {
				t.Errorf("files = %v, want %v", got, files)
			}
			f.noAside()
			want := []string{"dev--ws"}
			if to == manifest.WorkspaceSourceModeWorktree {
				want = nil
			}
			if got := f.jjNames(); !slices.Equal(got, want) {
				t.Errorf("jj workspaces = %q, want %q", got, want)
			}
			if got := f.slotStatus("ws", "app"); got.Mode != to || got.Error != "" {
				t.Errorf("status = %+v, want a usable %s slot", got, to)
			}
		})
	}
}

// keptBeside is files, a slot's, as a migrate undone leaves them when a
// process wrote node_modules/pkg/index.js, as rebuilt, into the old checkout
// after node_modules/ was carried across: the copy carried across beside it,
// as node_modules.migrated.
func keptBeside(files map[string]string, rebuilt string) map[string]string {
	kept := maps.Clone(files)
	for rel, content := range files {
		if rest, ok := strings.CutPrefix(rel, "node_modules"); ok {
			kept["node_modules"+keptSuffix+rest] = content
		}
	}
	kept[filepath.Join("node_modules", "pkg", "index.js")] = rebuilt
	return kept
}

// A process still writing into the old checkout (a watcher rebuilding
// node_modules) can make an entry there again once it is carried across.
// Undoing the migrate deletes neither copy: a migrate killed and recovered
// by its record's steps, either way, and one rolled back in process, move
// the copy carried across beside the rebuilt one, as node_modules.migrated,
// before the new checkout is removed, and the rollback's error says so.
func TestMigrateUndoneKeepsWhatTheOldCheckoutHoldsAgain(t *testing.T) {
	injected := errors.New("injected failure")
	for _, tc := range []struct {
		mode   string
		killed bool
	}{
		{manifest.WorkspaceSourceModeWorktree, true},
		{manifest.WorkspaceSourceModeJJ, true},
		{manifest.WorkspaceSourceModeWorktree, false},
	} {
		name := tc.mode + "/rolled back"
		if tc.killed {
			name = tc.mode + "/killed"
		}
		t.Run(name, func(t *testing.T) {
			f := newMigrateFixture(t, tc.mode)
			to, commit := manifest.WorkspaceSourceModeJJ, f.head()
			if tc.mode == manifest.WorkspaceSourceModeJJ {
				to, commit = manifest.WorkspaceSourceModeWorktree, f.tip()
			}
			files, names := f.files(), f.jjNames()
			rebuild := func() {
				mustWriteFileAll(t, filepath.Join(f.asideOf(), "node_modules", "pkg", "index.js"), "rebuilt\n")
			}
			if tc.killed {
				f.killAt(migrateStepSave, to, rebuild)
				if phase := f.followRecord(); phase != migratePhaseMigrating {
					t.Fatalf("the record's phase = %q, want %q", phase, migratePhaseMigrating)
				}
			} else {
				f.platform.migrateFault = func(at string) error {
					if at == migrateStepSave {
						rebuild()
						return injected
					}
					return nil
				}
				_, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", to)
				f.platform.migrateFault = nil
				opErr := AsOperationError(err)
				if !errors.Is(err, injected) || opErr == nil || !strings.Contains(err.Error(), "kept beside them in it: node_modules.migrated.") || !slices.Contains(opErr.Paths, filepath.Join(f.slot, "node_modules.migrated")) {
					t.Fatalf("WorkspaceSourceMigrate() error = %v (%+v), want the injected failure, saying node_modules.migrated is kept", err, opErr)
				}
			}
			f.intact(tc.mode, keptBeside(files, "rebuilt\n"), commit, names)
		})
	}
}

// A new checkout that still holds an entry the migrate carries across once
// the ones it carried are back (written there meanwhile, by a process
// writing the slot by its path) is not removed: the rollback stops, naming
// it, and the record's step refuses too, until it is moved out of the way.
func TestMigrateKeepsANewCheckoutThatHoldsAnEntry(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeWorktree)
	files, commit, names := f.files(), f.head(), f.jjNames()
	written := filepath.Join(f.slot, "node_modules", "written.js")
	f.platform.migrateFault = func(at string) error {
		if at == migrateStepCarried {
			mustWriteFileAll(t, written, "written\n")
			return errors.New("injected failure")
		}
		return nil
	}
	_, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
	f.platform.migrateFault = nil
	if opErr := AsOperationError(err); opErr == nil || opErr.Cause != CauseMigrateIncomplete || !strings.Contains(err.Error(), "holds node_modules/, which the migrate carries across") {
		t.Fatalf("WorkspaceSourceMigrate() error = %v (%+v), want a stopped rollback naming node_modules/", err, opErr)
	}
	aside := f.asideOf()
	if !pathExists(t, written) || !pathExists(t, filepath.Join(aside, "node_modules", "pkg", "index.js")) || !pathExists(t, filepath.Join(aside, ".env")) {
		t.Fatalf("the stopped rollback lost an entry")
	}
	if _, step, out, err := f.runRecord(); err == nil || !strings.HasPrefix(step, "remove what the migrate made") || !strings.Contains(out, "still holds node_modules, which the migrate carries across") {
		t.Fatalf("following the record = step %q, %q, %v, want its removal of the new checkout refused", step, out, err)
	}
	if !pathExists(t, written) {
		t.Fatalf("the record's refused removal deleted node_modules")
	}
	if err := os.Rename(filepath.Join(f.slot, "node_modules"), filepath.Join(aside, "node_modules"+keptSuffix)); err != nil {
		t.Fatal(err)
	}
	f.followRecord()
	want := maps.Clone(files)
	want["node_modules"+keptSuffix], want[filepath.Join("node_modules"+keptSuffix, "written.js")] = "dir", "written\n"
	f.intact(manifest.WorkspaceSourceModeWorktree, want, commit, names)
}

// The checks before a migrate are made again on the checkout set aside: a
// repository nested in it there refuses the migrate, which rolls back.
func TestMigrateChecksTheCheckoutSetAsideForRepositories(t *testing.T) {
	f := newMigrateFixture(t, manifest.WorkspaceSourceModeWorktree)
	f.platform.migrateFault = func(at string) error {
		if at == migrateStepAside {
			runGit(t, filepath.Join(f.asideOf(), "src"), "init", "-q")
		}
		return nil
	}
	_, err := f.platform.WorkspaceSourceMigrate(context.Background(), "ws", "app", "")
	if opErr := AsOperationError(err); opErr == nil || opErr.Cause != CauseNestedRepositories || !slices.Equal(opErr.Paths, []string{"src/.git"}) {
		t.Fatalf("WorkspaceSourceMigrate() error = %v, want nested_repositories naming src/.git", err)
	}
	if got := f.recorded(); got.Mode != manifest.WorkspaceSourceModeWorktree {
		t.Errorf("recorded mode = %q, want worktree", got.Mode)
	}
	if !pathExists(t, filepath.Join(f.slot, "src", ".git")) {
		t.Errorf("the nested repository is gone")
	}
	f.noAside()
}

// A migrate stopped while writing the manifest leaves both ways in its
// record, and the hint chooses by what the manifest records.
func TestUnfinishedMigrateWhileSaving(t *testing.T) {
	p := &Platform{root: t.TempDir()}
	m := &migration{p: p, workspaceName: "ws", slot: "app", path: filepath.Join(p.root, "workspaces", "ws", "app"), to: manifest.WorkspaceSourceModeJJ}
	if err := m.write(migrateMarker{Phase: migratePhaseSaving, Steps: []string{"undo it"}, Cleanup: []string{"clean up"}}); err != nil {
		t.Fatal(err)
	}
	reason, hint, unfinished := p.unfinishedMigrate("ws", "app", m.path)
	if !unfinished || !strings.Contains(reason, "while writing the manifest") ||
		!strings.Contains(hint, `If angee.yaml records mode "jj" for source slot "app", the migrate is done: Recover by hand, in this order: 1. clean up;`) ||
		!strings.Contains(hint, "Otherwise: Recover by hand, in this order: 1. undo it;") {
		t.Fatalf("unfinishedMigrate() = %q, %q, %v", reason, hint, unfinished)
	}
}
