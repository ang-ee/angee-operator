package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// lifecycleFixture is a stack with three sources a workspace template can cut
// slots from: app (a git remote with main and develop, cut as a worktree of
// its cache at .cache/app), docs (another git remote, cut as a clone) and
// notes (a local directory, linked). TMPDIR points into the fixture, so the
// staging directories a create leaves behind can be seen.
type lifecycleFixture struct {
	base, root, tmp string
	appRemote       string
	docsRemote      string
	notes           string
	platform        *Platform
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	f := &lifecycleFixture{base: base, root: filepath.Join(base, ".angee"), tmp: filepath.Join(base, "tmp"), notes: filepath.Join(base, "notes")}
	for _, dir := range []string{f.tmp, f.notes, filepath.Join(base, "app"), filepath.Join(base, "docs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}
	t.Setenv("TMPDIR", f.tmp)
	f.appRemote = filepath.Join(base, "app", "remote.git")
	seedWorktreeRemote(t, filepath.Join(base, "app"), f.appRemote)
	seed := filepath.Join(base, "app", "seed")
	runGit(t, seed, "switch", "-c", "develop")
	commitFile(t, seed, "develop.txt")
	runGit(t, seed, "push", "origin", "develop")
	f.docsRemote = filepath.Join(base, "docs", "remote.git")
	seedWorktreeRemote(t, filepath.Join(base, "docs"), f.docsRemote)
	mustWriteFile(t, filepath.Join(f.notes, "notes.txt"), "notes\n")
	platform, err := New(f.root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	f.platform = platform
	return f
}

// template writes a workspace template named name whose sources are the
// given slots: "app", "docs", "notes", or "missing" (a clone of a remote
// that does not exist). app's branch and ref come from the branch and ref
// inputs.
func (f *lifecycleFixture) template(t *testing.T, name string, slots ...string) string {
	t.Helper()
	snippets := map[string]string{
		"app": `    app:
      kind: git
      repo: ` + f.appRemote + `
      default_ref: main
      cache_path: .cache/app
      mode: worktree
      branch: "${inputs.branch}"
      ref: "${inputs.ref}"
      subpath: app
`,
		"docs": `    docs:
      kind: git
      repo: ` + f.docsRemote + `
      default_ref: main
      cache_path: .cache/docs
      mode: clone
      ref: main
      subpath: docs
`,
		"notes": `    notes:
      kind: local
      path: ` + f.notes + `
      subpath: notes
`,
		"missing": `    zz-missing:
      kind: git
      repo: ` + filepath.Join(f.base, "missing.git") + `
      default_ref: main
      mode: clone
      ref: main
      subpath: zz-missing
`,
	}
	var sources strings.Builder
	for _, slot := range slots {
		snippet, ok := snippets[slot]
		if !ok {
			t.Fatalf("unknown fixture slot %q", slot)
		}
		sources.WriteString(snippet)
	}
	templateRoot := filepath.Join(f.base, ".templates", "workspaces", name)
	if err := os.MkdirAll(filepath.Join(templateRoot, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template) error = %v", err)
	}
	mustWriteFile(t, filepath.Join(templateRoot, "copier.yml"), `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: `+name+`
  inputs:
    branch:
      type: str
      default: feature-a
    ref:
      type: str
      default: main
  sources:
`+sources.String()+`branch:
  type: str
  default: feature-a
ref:
  type: str
  default: main
`)
	mustWriteFile(t, filepath.Join(templateRoot, "template", "README.md.jinja"), "workspace {{ workspace_name }}\n")
	return templateRoot
}

func (f *lifecycleFixture) workspace(name string) string {
	return filepath.Join(f.root, "workspaces", name)
}

func (f *lifecycleFixture) cache() string {
	return filepath.Join(f.root, ".cache", "app")
}

// registeredWorktrees counts the worktrees git lists for app's cache, the
// cache's own checkout included.
func (f *lifecycleFixture) registeredWorktrees(t *testing.T) int {
	t.Helper()
	return strings.Count(runGitOutput(t, f.cache(), "worktree", "list", "--porcelain"), "worktree ")
}

// forget drops a workspace's record from angee.yaml and leaves its files, as
// a create that died before saving the manifest would.
func (f *lifecycleFixture) forget(t *testing.T, name string) {
	t.Helper()
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	delete(stack.Workspaces, name)
	if err := manifest.SaveFile(manifest.Path(f.root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
}

// assertNoStaging fails when a create left a staging directory in TMPDIR.
func (f *lifecycleFixture) assertNoStaging(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.tmp)
	if err != nil {
		t.Fatalf("ReadDir(TMPDIR) error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "angee-") && strings.Contains(entry.Name(), "-stage-") {
			t.Fatalf("staging directory %s left in TMPDIR", entry.Name())
		}
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("%s holds %v, want it empty", dir, names)
	}
}

// A create that fails at a later slot undoes every slot it cut before: the
// worktree is removed and deregistered from the cache, the clone and the link
// are removed (the linked directory is left alone), the workspace directory
// goes, and no staging directory is left.
func TestWorkspaceCreateRollsBackEarlierSlotsWhenALaterOneFails(t *testing.T) {
	f := newLifecycleFixture(t)
	template := f.template(t, "partial", "app", "docs", "notes", "missing")
	recorder := recordGit(t, f.base)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	ran := recorder.run(t, func() {
		_, err = f.platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "feature-a"})
	})
	if err == nil || !strings.Contains(err.Error(), "missing.git") {
		t.Fatalf("WorkspaceCreate() error = %v, want the missing remote's clone failure", err)
	}
	for i, command := range ran {
		ran[i] = strings.ReplaceAll(cleanStages(command), cwd, "<cwd>")
	}
	// The slots are cut in order (app, docs, notes, zz-missing); the rollback
	// then deregisters the worktree it removed.
	want := []string{
		"<cwd> [] clone --origin origin --branch main --end-of-options <base>/app/remote.git <base>/.angee/.cache/.app.clone-N/clone",
		"<base>/.angee/.cache/.app.clone-N/clone [] config --get-all --end-of-options remote.origin.url",
		"<base>/.angee/.cache/app [] worktree list --porcelain",
		"<base>/.angee/.cache/app [] -c worktree.useRelativePaths=false worktree add -b feature-a --end-of-options <base>/tmp/angee-worktree-stage-N/source main",
		"<cwd> [] clone --origin origin --branch main --end-of-options <base>/docs/remote.git <base>/tmp/angee-clone-stage-N/source",
		"<base>/tmp/angee-clone-stage-N/source [] config --get-all --end-of-options remote.origin.url",
		"<cwd> [] clone --origin origin --branch main --end-of-options <base>/missing.git <base>/tmp/angee-clone-stage-N/source",
		"<base>/.angee/.cache/app [] worktree prune",
	}
	if os.Getenv("ANGEE_RECORD_GIT") != "" {
		t.Logf("rollback:\n%q", ran)
	} else if strings.Join(ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("failed create ran git:\n%s\nwant:\n%s", strings.Join(ran, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Lstat(f.workspace("feature-a")); !os.IsNotExist(err) {
		t.Fatalf("workspace directory after the failed create: Lstat error = %v, want it gone", err)
	}
	if got := f.registeredWorktrees(t); got != 1 {
		t.Fatalf("worktrees registered in the cache after rollback = %d, want 1 (the cache's own)", got)
	}
	if _, err := os.Stat(filepath.Join(f.notes, "notes.txt")); err != nil {
		t.Fatalf("linked local source after rollback: Stat error = %v, want it intact", err)
	}
	f.assertNoStaging(t)
}

// A worktree slot works when the user's git is set to write worktree links as
// relative paths (worktree.useRelativePaths, git 2.48+), however it is added:
// on a new branch, on a branch that exists, or detached. A link relative to
// the staging directory would break once the checkout is installed at the
// slot path, so the worktree is added with absolute links, and the cache's
// repository format is left as it was, without extensions.relativeWorktrees.
func TestWorktreeSlotWorksWhenGitWritesRelativePaths(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	if usage, _ := exec.Command("git", "worktree", "add", "-h").CombinedOutput(); !strings.Contains(string(usage), "relative-paths") {
		t.Skip("git before 2.48 has no worktree.useRelativePaths")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	useGlobalGitConfig(t, f.base, "[worktree]\n\tuseRelativePaths = true\n")
	template := f.template(t, "worktree", "app")

	for _, tc := range []struct {
		name, branch string
		setup        func()
	}{
		{name: "new-branch", branch: "new-branch"},
		{name: "existing-branch", branch: "existing", setup: func() { runGit(t, f.cache(), "branch", "existing", "main") }},
		{name: "detached", branch: ""},
	} {
		if tc.setup != nil {
			tc.setup()
		}
		if _, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: tc.name, Inputs: map[string]string{"branch": tc.branch}}); err != nil {
			t.Fatalf("WorkspaceCreate(%s) error = %v", tc.name, err)
		}
		slot := filepath.Join(f.workspace(tc.name), "app")
		if out, err := exec.Command("git", "-C", slot, "status", "--short").CombinedOutput(); err != nil {
			t.Fatalf("git status in the %s slot error = %v: %s", tc.name, err, out)
		}
	}
	if list := runGitOutput(t, f.cache(), "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 4 || strings.Contains(list, "prunable") {
		t.Fatalf("cache worktrees:\n%s\nwant the cache's own and the three slots', none prunable", list)
	}
	out, err := exec.Command("git", "-C", f.cache(), "config", "--get", "extensions.relativeWorktrees").CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("cache extensions.relativeWorktrees = %q (%v), want it unset", out, err)
	}
}

// Creating a worktree slot touches no other slot of its cache. A slot whose
// .git is broken (a directory where git wrote a file) neither fails the
// create nor is changed by it.
func TestWorktreeSlotCreateLeavesTheCachesOtherSlotsAlone(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	template := f.template(t, "worktree", "app")
	create := func(name string) error {
		_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: name, Inputs: map[string]string{"branch": name}})
		return err
	}
	if err := create("first"); err != nil {
		t.Fatalf("WorkspaceCreate(first) error = %v", err)
	}
	dotGit := filepath.Join(f.workspace("first"), "app", ".git")
	if err := os.Remove(dotGit); err != nil {
		t.Fatalf("Remove(.git) error = %v", err)
	}
	mustMkdirAll(t, dotGit)
	marker := filepath.Join(dotGit, "marker")
	mustWriteFile(t, marker, "left as it was\n")

	if err := create("second"); err != nil {
		t.Fatalf("WorkspaceCreate(second) beside a broken slot error = %v", err)
	}
	if out, err := exec.Command("git", "-C", filepath.Join(f.workspace("second"), "app"), "status", "--short").CombinedOutput(); err != nil {
		t.Fatalf("git status in the new slot error = %v: %s", err, out)
	}
	info, err := os.Lstat(dotGit)
	if err != nil || !info.IsDir() {
		t.Fatalf("broken slot's .git after the create: Lstat = %v, %v, want the directory it was", info, err)
	}
	entries, err := os.ReadDir(dotGit)
	if err != nil || len(entries) != 1 || entries[0].Name() != "marker" {
		t.Fatalf("broken slot's .git holds %v (%v), want only marker", entries, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "left as it was\n" {
		t.Fatalf("broken slot's marker = %q, %v, want it unchanged", got, err)
	}
}

// --sync reclaims a leftover slot only when the cache has it registered as a
// worktree; a populated directory git does not know is refused with or
// without it and left as it is. A registered leftover is refused without
// --sync, and a registration whose directory is gone is reclaimed by any
// create. The reclaim deregisters the leftover before adding the slot again.
func TestWorkspaceCreateSyncReclaimsOnlyARegisteredLeftover(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	template := f.template(t, "worktree", "app")
	create := func(name string, sync bool) error {
		_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: name, Sync: sync})
		return err
	}
	if err := create("feature-a", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	recorder := recordGit(t, f.base)
	// worktreeGit returns the worktree commands run while create runs.
	worktreeGit := func(name string, sync bool) ([]string, error) {
		var err error
		var commands []string
		for _, command := range recorder.run(t, func() { err = create(name, sync) }) {
			if strings.Contains(command, " worktree ") {
				commands = append(commands, cleanStages(command))
			}
		}
		return commands, err
	}
	const cache = "<base>/.angee/.cache/app []"
	leftover := filepath.Join(f.workspace("feature-a"), "app", "leftover.txt")

	f.forget(t, "feature-a")
	mustWriteFile(t, leftover, "uncommitted\n")
	ran, err := worktreeGit("feature-a", false)
	if err == nil || !strings.Contains(err.Error(), "already exists and is not empty") {
		t.Fatalf("WorkspaceCreate() over a registered leftover without --sync error = %v, want it refused", err)
	}
	if _, statErr := os.Stat(leftover); statErr != nil {
		t.Fatalf("leftover after the refused create: Stat error = %v, want it kept", statErr)
	}
	if want := []string(nil); strings.Join(ran, "\n") != strings.Join(want, "\n") {
		t.Fatalf("refused create ran worktree commands:\n%s\nwant none", strings.Join(ran, "\n"))
	}

	ran, err = worktreeGit("feature-a", true)
	if err != nil {
		t.Fatalf("WorkspaceCreate(--sync) over a registered leftover error = %v", err)
	}
	if _, statErr := os.Stat(leftover); !os.IsNotExist(statErr) {
		t.Fatalf("leftover after the reclaim: Stat error = %v, want it gone", statErr)
	}
	slot := filepath.Join(f.workspace("feature-a"), "app")
	if got := strings.TrimSpace(runGitOutput(t, slot, "branch", "--show-current")); got != "feature-a" {
		t.Fatalf("reclaimed slot branch = %q, want feature-a", got)
	}
	want := []string{
		cache + " worktree list --porcelain",
		cache + " worktree prune",
		cache + " -c worktree.useRelativePaths=false worktree add --end-of-options <base>/tmp/angee-worktree-stage-N/source feature-a",
	}
	if strings.Join(ran, "\n") != strings.Join(want, "\n") {
		t.Fatalf("reclaim ran:\n%s\nwant:\n%s", strings.Join(ran, "\n"), strings.Join(want, "\n"))
	}

	stray := filepath.Join(f.workspace("feature-b"), "app", "precious.txt")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	mustWriteFile(t, stray, "not a worktree\n")
	ran, err = worktreeGit("feature-b", true)
	if err == nil || !strings.Contains(err.Error(), "already exists and is not empty") {
		t.Fatalf("WorkspaceCreate(--sync) over an unregistered directory error = %v, want it refused", err)
	}
	if got, readErr := os.ReadFile(stray); readErr != nil || string(got) != "not a worktree\n" {
		t.Fatalf("unregistered directory after the refused create: %q, %v, want it untouched", got, readErr)
	}
	if want := []string{cache + " worktree list --porcelain"}; strings.Join(ran, "\n") != strings.Join(want, "\n") {
		t.Fatalf("refused --sync create ran:\n%s\nwant:\n%s", strings.Join(ran, "\n"), strings.Join(want, "\n"))
	}

	f.forget(t, "feature-a")
	if err := os.RemoveAll(f.workspace("feature-a")); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	ran, err = worktreeGit("feature-a", false)
	if err != nil {
		t.Fatalf("WorkspaceCreate() over a registration whose directory is gone error = %v", err)
	}
	if strings.Join(ran, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stale registration reclaim ran:\n%s\nwant:\n%s", strings.Join(ran, "\n"), strings.Join(want, "\n"))
	}
	if got := f.registeredWorktrees(t); got != 2 {
		t.Fatalf("worktrees registered after the reclaims = %d, want 2 (the cache's and feature-a's)", got)
	}
	f.assertNoStaging(t)
}

// swapOnStage puts a git on PATH that, the first time it is handed a slot's
// staging directory (a worktree add or a clone), replaces the workspace
// directory with a symlink to attacker before running the real git: the
// slot's parent is swapped while the create runs.
func swapOnStage(t *testing.T, workspace, attacker string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "swapped")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *angee-worktree-stage-*|*angee-clone-stage-*)
    if [ ! -e %[1]q ]; then
      : > %[1]q
      mv %[2]q %[2]q-moved && ln -s %[3]q %[2]q
    fi
    ;;
esac
exec %[4]q "$@"
`, marker, workspace, attacker, real)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(git) error = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A git slot is staged outside the stack and installed through the
// destination capability opened before git ran, so swapping the slot's parent
// for a symlink while git runs never puts the slot where the symlink points.
// The create fails, and what it installed under the original parent is
// removed, and for a worktree deregistered.
func TestWorkspaceCreateKeepsAGitSlotOutOfASwappedParent(t *testing.T) {
	for _, tc := range []struct {
		name, slot string
	}{
		{name: "worktree", slot: "app"},
		{name: "clone", slot: "docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			template := f.template(t, tc.name, tc.slot)
			attacker := filepath.Join(f.base, "attacker")
			if err := os.MkdirAll(attacker, 0o755); err != nil {
				t.Fatalf("MkdirAll(attacker) error = %v", err)
			}
			workspace := f.workspace("feature-a")
			swapOnStage(t, workspace, attacker)

			if _, err := f.platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "feature-a"}); err == nil {
				t.Fatal("WorkspaceCreate() with the slot's parent swapped for a symlink error = nil, want a failure")
			}
			if target, err := os.Readlink(workspace); err != nil || target != attacker {
				t.Fatalf("workspace path = %q, %v, want the swapped-in symlink (the swap must have run)", target, err)
			}
			assertEmptyDir(t, attacker)
			if _, err := os.Lstat(filepath.Join(workspace+"-moved", tc.slot)); !os.IsNotExist(err) {
				t.Fatalf("slot under the original parent: Lstat error = %v, want it removed", err)
			}
			if tc.slot == "app" {
				if got := f.registeredWorktrees(t); got != 1 {
					t.Fatalf("worktrees registered in the cache = %d, want 1 (the cache's own)", got)
				}
			}
			f.assertNoStaging(t)
		})
	}
}

var (
	stagingDir = regexp.MustCompile(`angee-(worktree|clone)-stage-[0-9]+`)
	cacheStage = regexp.MustCompile(`\.app\.clone-[0-9]+`)
	renderDir  = regexp.MustCompile(`angee-reconcile-[0-9]+`)
)

// cleanStages writes the random part of a staging directory's name as N.
func cleanStages(command string) string {
	command = stagingDir.ReplaceAllString(command, "angee-$1-stage-N")
	command = renderDir.ReplaceAllString(command, "angee-reconcile-N")
	return cacheStage.ReplaceAllString(command, ".app.clone-N")
}

// Creating a workspace with a worktree, a clone and a local slot, and
// destroying it, runs exactly this git, in this order: for a worktree on a
// new branch, on a branch that exists, detached, and on a base only the
// remote has.
func TestWorkspaceCreateAndDestroyRunTheirGit(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	template := f.template(t, "lifecycle", "app", "docs", "notes")
	recorder := recordGit(t, f.base)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	record := func(verb func() error) ([]string, error) {
		var err error
		ran := recorder.run(t, func() { err = verb() })
		for i, command := range ran {
			ran[i] = strings.ReplaceAll(cleanStages(command), cwd, "<cwd>")
		}
		return ran, err
	}
	create := func(name string, inputs map[string]string) func() error {
		return func() error {
			_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: name, Inputs: inputs})
			return err
		}
	}
	destroy := func(name string) func() error {
		return func() error { return f.platform.WorkspaceDestroy(ctx, name, true) }
	}

	const (
		cache = "<base>/.angee/.cache/app []"
		stage = "<base>/tmp/angee-worktree-stage-N/source"
		// A worktree is added with absolute links.
		add = cache + " -c worktree.useRelativePaths=false worktree add"
	)
	// The clone slot is cloned into a staging directory and given its
	// remotes there; then copier renders the template.
	cloneAndRender := []string{
		"<cwd> [] clone --origin origin --branch main --end-of-options <base>/docs/remote.git <base>/tmp/angee-clone-stage-N/source",
		"<base>/tmp/angee-clone-stage-N/source [] config --get-all --end-of-options remote.origin.url",
		"<base>/.templates/workspaces/lifecycle [Copier] rev-parse --show-toplevel",
		"<base>/tmp/angee-reconcile-N [Copier] config --type=bool --get core.fileMode",
	}
	// An existing cache is checked against the manifest's remotes and
	// fetched before a worktree is cut from it.
	refresh := []string{
		cache + " config --get-all --end-of-options remote.origin.url",
		cache + " fetch --all --prune",
	}
	// The destroy guard reads each git slot; reads names the app slot's.
	guard := func(workspace string, reads ...string) []string {
		app := "<base>/.angee/workspaces/" + workspace + "/app []"
		docs := "<base>/.angee/workspaces/" + workspace + "/docs []"
		commands := []string{app + " status --porcelain=v1 -z --untracked-files=all"}
		for _, read := range reads {
			commands = append(commands, app+" "+read)
		}
		return append(commands,
			docs+" status --porcelain=v1 -z --untracked-files=all",
			docs+" rev-list --left-right --count --end-of-options origin/main...HEAD",
		)
	}
	onMain := []string{
		"rev-parse --verify --quiet --symbolic-full-name --end-of-options main@{upstream}",
		"rev-parse --git-dir --git-common-dir",
		"rev-list --count HEAD --not --end-of-options main origin/main",
		"rev-list --left-right --count --end-of-options main...HEAD",
	}
	prune := []string{cache + " worktree prune"}

	for _, tc := range []struct {
		name string
		verb func() error
		git  []string
	}{
		{
			name: "create on a new branch",
			verb: create("feature-a", map[string]string{"branch": "feature-a"}),
			git: concat([]string{
				"<cwd> [] clone --origin origin --branch main --end-of-options <base>/app/remote.git <base>/.angee/.cache/.app.clone-N/clone",
				"<base>/.angee/.cache/.app.clone-N/clone [] config --get-all --end-of-options remote.origin.url",
				cache + " worktree list --porcelain",
				add + " -b feature-a --end-of-options " + stage + " main",
			}, cloneAndRender),
		},
		{
			name: "destroy",
			verb: destroy("feature-a"),
			git:  concat(guard("feature-a", onMain...), prune),
		},
		{
			name: "create on a branch that exists",
			verb: create("feature-a", map[string]string{"branch": "feature-a"}),
			git: concat(refresh, []string{
				cache + " worktree list --porcelain",
				add + " --end-of-options " + stage + " feature-a",
			}, cloneAndRender),
		},
		{
			name: "destroy again",
			verb: destroy("feature-a"),
			git:  concat(guard("feature-a", onMain...), prune),
		},
		{
			name: "create detached",
			verb: create("feature-b", map[string]string{"branch": ""}),
			git: concat(refresh, []string{
				cache + " worktree list --porcelain",
				add + " --detach --end-of-options " + stage + " main",
			}, cloneAndRender),
		},
		{
			name: "destroy detached",
			verb: destroy("feature-b"),
			git: concat(guard("feature-b",
				"rev-parse --verify --quiet --symbolic-full-name --end-of-options main@{upstream}",
				"rev-parse --git-dir --git-common-dir",
				"rev-list --count HEAD --not --branches --remotes --tags",
				"rev-list --left-right --count --end-of-options main...HEAD",
			), prune),
		},
		{
			// The local base is looked for first; only its absence resolves
			// the remote counterpart.
			name: "create on a remote-only base",
			verb: create("feature-c", map[string]string{"branch": "feature-c", "ref": "develop"}),
			git: concat(refresh, []string{
				cache + " worktree list --porcelain",
				cache + " rev-parse --verify --quiet --symbolic-full-name --end-of-options develop@{upstream}",
				add + " --no-track -b feature-c --end-of-options " + stage + " origin/develop",
			}, cloneAndRender),
		},
		{
			name: "destroy the remote-only base",
			verb: destroy("feature-c"),
			git: concat(guard("feature-c",
				"rev-parse --verify --quiet --symbolic-full-name --end-of-options develop@{upstream}",
				"rev-parse --git-dir --git-common-dir",
				"rev-list --count HEAD --not --end-of-options origin/develop",
				"rev-list --left-right --count --end-of-options origin/develop...HEAD",
			), prune),
		},
	} {
		ran, err := record(tc.verb)
		if err != nil {
			t.Fatalf("%s error = %v", tc.name, err)
		}
		if os.Getenv("ANGEE_RECORD_GIT") != "" {
			t.Logf("%s:\n%q", tc.name, ran)
			continue
		}
		if strings.Join(ran, "\n") != strings.Join(tc.git, "\n") {
			t.Errorf("%s ran git:\n%s\nwant:\n%s", tc.name, strings.Join(ran, "\n"), strings.Join(tc.git, "\n"))
		}
	}
	f.assertNoStaging(t)
}
