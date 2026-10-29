package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/fslock"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/gitdriver"
	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
)

// isolateJJ gives the test a HOME of its own, as the jj driver's tests do: jj
// keeps its user config there, and a record of every repository it loads, so
// nothing the test runs reaches the developer's config. The test's own jj and
// git commands get an identity; the operator's jj gets the same through
// JJ_USER and JJ_EMAIL, which it passes on.
func isolateJJ(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	config, gitConfig := filepath.Join(home, "jj-config.toml"), filepath.Join(home, ".gitconfig")
	for _, path := range []string{config, gitConfig} {
		mustWriteFile(t, path, "")
	}
	for key, value := range map[string]string{
		"HOME":                home,
		"XDG_CONFIG_HOME":     filepath.Join(home, ".config"),
		"JJ_CONFIG":           config,
		"JJ_USER":             "test",
		"JJ_EMAIL":            "test@example.invalid",
		"GIT_CONFIG_GLOBAL":   gitConfig,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "test",
		"GIT_AUTHOR_EMAIL":    "test@example.invalid",
		"GIT_COMMITTER_NAME":  "test",
		"GIT_COMMITTER_EMAIL": "test@example.invalid",
	} {
		t.Setenv(key, value)
	}
}

// requireJJSlotsHere skips the test when this machine cannot run jj slots,
// as the service decides it, or fails it when ANGEE_REQUIRE_JJ=1 (CI's jj
// job), so a missing jj cannot pass for a green run there.
func requireJJSlotsHere(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	err := (&Platform{}).requireJJSlots(context.Background())
	if err == nil {
		return
	}
	if os.Getenv("ANGEE_REQUIRE_JJ") == "1" {
		t.Fatalf("ANGEE_REQUIRE_JJ=1, but %v", err)
	}
	t.Skipf("%v", err)
}

// supportedJJ is a probe that reports a machine able to run jj slots, for a
// test whose refusal comes before jj would run.
func supportedJJ(context.Context) jjdriver.Capabilities {
	return jjdriver.Capabilities{JJ: "jj 0.45.1", Git: "git version 2.50.1"}
}

// jjFixture is a stack named dev, optionally with `vcs: jj`, and a git
// source, app: a bare remote holding main, which workspace templates cut
// slots of from its cache at .cache/app.
type jjFixture struct {
	t                        *testing.T
	base, root, remote, seed string
	platform                 *Platform
}

func newJJFixture(t *testing.T, vcsName string) *jjFixture {
	t.Helper()
	isolateJJ(t)
	requireJJSlotsHere(t)
	f := newJJStack(t, vcsName)
	runGit(t, f.base, "init", "-q", "--bare", "-b", "main", f.remote)
	runGit(t, f.base, "clone", "-q", f.remote, f.seed)
	mustWriteFile(t, filepath.Join(f.seed, "f.txt"), "one\n")
	runGit(t, f.seed, "add", ".")
	runGit(t, f.seed, "commit", "-q", "-m", "base")
	runGit(t, f.seed, "push", "-q", "origin", "HEAD:main")
	return f
}

// newJJStack is the fixture's stack alone, with no remote and no jj: for a
// refusal that comes before anything runs.
func newJJStack(t *testing.T, vcsName string) *jjFixture {
	t.Helper()
	base := t.TempDir()
	f := &jjFixture{t: t, base: base, root: filepath.Join(base, ".angee"), remote: filepath.Join(base, "app.git"), seed: filepath.Join(base, "seed")}
	f.writeStack(f.root, vcsName)
	platform, err := New(f.root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	f.platform = platform
	return f
}

// writeStack saves an empty stack named dev at root.
func (f *jjFixture) writeStack(root, vcsName string) {
	f.t.Helper()
	mustMkdirAll(f.t, root)
	stack := &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "dev", VCS: vcsName}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		f.t.Fatalf("SaveFile() error = %v", err)
	}
}

// app is the template source entry of a slot of app, with the slot's own
// fields after it.
func (f *jjFixture) app(fields string) string {
	return "source: app\nkind: git\nrepo: " + f.remote + "\ndefault_ref: main\ncache_path: .cache/app\n" + fields
}

// missing is a clone slot of a remote that does not exist, which fails a
// create after the slots before it.
func (f *jjFixture) missing() string {
	return "kind: git\nrepo: " + filepath.Join(f.base, "missing.git") + "\ndefault_ref: main\nmode: clone\nref: main\nsubpath: zz-missing\n"
}

// template writes workspace template name whose sources are slots, slot name
// to the fields of its `_angee.sources` entry, and returns its path. Its
// branch input defaults to the workspace's name.
func (f *jjFixture) template(name string, slots map[string]string) string {
	f.t.Helper()
	var sources strings.Builder
	for _, slot := range sortedKeys(slots) {
		sources.WriteString("    " + slot + ":\n")
		for _, line := range strings.Split(strings.TrimSpace(slots[slot]), "\n") {
			sources.WriteString("      " + line + "\n")
		}
	}
	root := filepath.Join(f.base, ".templates", "workspaces", name)
	mustMkdirAll(f.t, filepath.Join(root, "template"))
	mustWriteFile(f.t, filepath.Join(root, "copier.yml"), `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: `+name+`
  inputs:
    branch:
      type: str
      default: ""
  sources:
`+sources.String()+`branch:
  type: str
  default: ""
`)
	mustWriteFile(f.t, filepath.Join(root, "template", "README.md.jinja"), "workspace {{ workspace_name }}\n")
	return root
}

func (f *jjFixture) workspace(name string) string { return filepath.Join(f.root, "workspaces", name) }

func (f *jjFixture) cache() string { return filepath.Join(f.root, ".cache", "app") }

// create creates workspace name from template, its branch input set to name.
func (f *jjFixture) create(template, name string, sync bool) error {
	f.t.Helper()
	_, err := f.platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: name, Inputs: map[string]string{"branch": name}, Sync: sync})
	return err
}

// jj runs jj in dir and returns its standard output.
func (f *jjFixture) jj(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("jj", append([]string{"--no-pager", "--color", "never"}, args...)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		f.t.Fatalf("jj %q in %s: %v\n%s", args, dir, err, stderr.String())
	}
	return stdout.String()
}

// jjWorkspaces maps each jj workspace of the repository at primary to the
// change id of its working-copy commit.
func (f *jjFixture) jjWorkspaces(primary string) map[string]string {
	f.t.Helper()
	workspaces := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(f.jj(primary, "--ignore-working-copy", "workspace", "list", "-T", `json(self) ++ "\n"`)), "\n") {
		var w struct {
			Name   string `json:"name"`
			Target struct {
				ChangeID string `json:"change_id"`
			} `json:"target"`
		}
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			f.t.Fatalf("decode %q: %v", line, err)
		}
		workspaces[w.Name] = w.Target.ChangeID
	}
	return workspaces
}

// registrations lists the git worktrees primary keeps besides itself, as
// `git worktree list --porcelain` prints them, prunable ones included.
func (f *jjFixture) registrations(primary string) []string {
	f.t.Helper()
	blocks := strings.Split(strings.TrimSpace(runGitOutput(f.t, primary, "worktree", "list", "--porcelain")), "\n\n")
	return blocks[1:]
}

// slotStatus reads one slot of workspace through the service.
func (f *jjFixture) slotStatus(workspace, slot string) api.WorkspaceSourceStatus {
	f.t.Helper()
	status, err := f.platform.WorkspaceStatus(context.Background(), workspace)
	if err != nil {
		f.t.Fatalf("WorkspaceStatus(%s) error = %v", workspace, err)
	}
	for _, source := range status.Sources {
		if source.Slot == slot {
			return source
		}
	}
	f.t.Fatalf("WorkspaceStatus(%s) has no slot %s: %+v", workspace, slot, status.Sources)
	return api.WorkspaceSourceStatus{}
}

// forget drops a workspace's record and leaves its files, as a create that
// died before saving the manifest would.
func (f *jjFixture) forget(name string) {
	f.t.Helper()
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		f.t.Fatalf("LoadFile() error = %v", err)
	}
	delete(stack.Workspaces, name)
	if err := manifest.SaveFile(manifest.Path(f.root), stack); err != nil {
		f.t.Fatalf("SaveFile() error = %v", err)
	}
}

func pathExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("Lstat(%s) error = %v", path, err)
	}
	return err == nil
}

// A workspace with a git worktree slot and a jj slot of one source's cache:
// the jj slot is a colocated jj workspace named dev--<workspace>, detached at
// the base, with no branch made for it, while the worktree slot of the same
// cache keeps working after jj's add has made the cache use relative worktree
// paths. Status reads the jj slot through jj: clean, then dirty for an edit
// no jj command has recorded, ahead once committed, and behind or diverged
// once the base moves; neither slot is ever branch-mismatch. Destroy refuses
// while the jj slot holds work, and otherwise forgets its workspace and
// leaves no git worktree registration.
func TestJJSlotBesideAWorktreeSlotOfOneCache(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	template := f.template("mixed", map[string]string{
		"tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
		"work": f.app("mode: jj\nref: main\nsubpath: work"),
	})
	for _, name := range []string{"feature-a", "feature-b"} {
		if err := f.create(template, name, false); err != nil {
			t.Fatalf("WorkspaceCreate(%s) error = %v", name, err)
		}
	}
	work := filepath.Join(f.workspace("feature-a"), "work")
	if got := strings.TrimSpace(runGitOutput(t, work, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Fatalf("git rev-parse --is-inside-work-tree in the jj slot = %q, want true", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, work, "rev-parse", "--abbrev-ref", "HEAD")); got != "HEAD" {
		t.Errorf("the jj slot's git HEAD is on %q, want detached", got)
	}
	if got, want := strings.TrimSpace(runGitOutput(t, work, "rev-parse", "HEAD")), strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main")); got != want {
		t.Errorf("the jj slot is at %s, want the base %s", got, want)
	}
	if got := strings.Fields(runGitOutput(t, f.cache(), "for-each-ref", "--format=%(refname:short)", "refs/heads")); !slices.Equal(got, []string{"feature-a", "feature-b", "main"}) {
		t.Errorf("cache branches = %q, want the worktree slots' and main only", got)
	}
	if names := f.jjWorkspaces(f.cache()); !slices.Equal(sortedKeys(names), []string{"default", "dev--feature-a", "dev--feature-b"}) {
		t.Errorf("jj workspaces of the cache = %q, want default and one per workspace", sortedKeys(names))
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache(), "config", "--get", "extensions.relativeworktrees")); got != "true" {
		t.Errorf("cache extensions.relativeworktrees = %q, want jj's add to have set it", got)
	}
	if out, err := exec.Command("git", "-C", filepath.Join(f.workspace("feature-a"), "tree"), "status", "--short").CombinedOutput(); err != nil {
		t.Fatalf("git status in the worktree slot after the jj add: %v: %s", err, out)
	}

	status, err := f.platform.WorkspaceStatus(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if status.State != "ready" {
		t.Errorf("workspace state = %q, want ready", status.State)
	}
	got := f.slotStatus("feature-a", "work")
	if got.Mode != manifest.WorkspaceSourceModeJJ || got.State != "clean" || got.Dirty || !got.Pushed || got.Ahead != 0 || got.Behind != 0 || got.Error != "" {
		t.Errorf("jj slot status = %+v, want a clean, pushed jj slot", got)
	}

	mustWriteFile(t, filepath.Join(work, "new.txt"), "unrecorded\n")
	got = f.slotStatus("feature-a", "work")
	if got.State != "dirty" || !got.Dirty || got.Pushed || got.UnpushedReason != "uncommitted changes" {
		t.Errorf("jj slot status after an edit = %+v, want dirty and not pushed", got)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "feature-a", true); err == nil || !strings.Contains(err.Error(), "work (uncommitted changes)") {
		t.Fatalf("WorkspaceDestroy() of a slot with an unrecorded edit error = %v, want a refusal naming it", err)
	}

	f.jj(work, "commit", "-m", "work")
	got = f.slotStatus("feature-a", "work")
	if got.State != "ahead" || got.Ahead != 1 || got.Pushed || !strings.Contains(got.UnpushedReason, "ahead of base ref main@origin") {
		t.Errorf("jj slot status after a commit = %+v, want ahead of main@origin and not pushed", got)
	}

	mustWriteFile(t, filepath.Join(f.seed, "g.txt"), "two\n")
	runGit(t, f.seed, "add", ".")
	runGit(t, f.seed, "commit", "-q", "-m", "upstream")
	runGit(t, f.seed, "push", "-q", "origin", "HEAD:main")
	if _, err := f.platform.SourceFetch(ctx, "app"); err != nil {
		t.Fatalf("SourceFetch() error = %v", err)
	}
	if got := f.slotStatus("feature-b", "work"); got.State != "behind" || got.Behind != 1 || !got.Pushed {
		t.Errorf("untouched jj slot after the base moved = %+v, want behind and pushed", got)
	}
	if got := f.slotStatus("feature-a", "work"); got.State != "diverged" || got.Ahead != 1 || got.Behind != 1 {
		t.Errorf("jj slot with a commit after the base moved = %+v, want diverged", got)
	}
	for _, name := range []string{"feature-a", "feature-b"} {
		status, err := f.platform.WorkspaceStatus(ctx, name)
		if err != nil {
			t.Fatalf("WorkspaceStatus(%s) error = %v", name, err)
		}
		for _, source := range status.Sources {
			if source.State == workspaceSourceStateBranchMismatch || status.State == "discrepancy" {
				t.Errorf("workspace %s slot %s state %q, workspace %q; want no branch mismatch", name, source.Slot, source.State, status.State)
			}
		}
	}

	if err := f.platform.WorkspaceDestroy(ctx, "feature-b", true); err != nil {
		t.Fatalf("WorkspaceDestroy(feature-b) error = %v", err)
	}
	if pathExists(t, f.workspace("feature-b")) {
		t.Errorf("WorkspaceDestroy(feature-b) left its directory")
	}
	if names := f.jjWorkspaces(f.cache()); slices.Contains(sortedKeys(names), "dev--feature-b") {
		t.Errorf("jj workspaces after the destroy = %q, want dev--feature-b forgotten", sortedKeys(names))
	}
	for _, reg := range f.registrations(f.cache()) {
		if strings.Contains(reg, "feature-b") || strings.Contains(reg, "prunable") {
			t.Errorf("git worktree registration %q after the destroy, want none of feature-b's", reg)
		}
	}
	if len(f.registrations(f.cache())) != 2 {
		t.Errorf("git worktrees of the cache = %q, want feature-a's two slots", f.registrations(f.cache()))
	}
	if err := f.platform.WorkspaceDestroy(ctx, "feature-a", true); err == nil || !strings.Contains(err.Error(), "ahead of base ref main@origin") {
		t.Fatalf("WorkspaceDestroy(feature-a) of a slot with a commit of its own error = %v, want a refusal", err)
	}
	if status, err := f.platform.WorkspaceSourceFetch(ctx, "feature-a", "work"); err != nil || status.Mode != manifest.WorkspaceSourceModeJJ {
		t.Errorf("WorkspaceSourceFetch() of the jj slot = %+v, %v, want it fetched and read as a jj slot", status, err)
	}
}

// A slot with no mode under `vcs: jj` is a jj slot, and the manifest records
// it with no mode, as the template wrote it.
func TestVCSJJMakesASlotWithNoModeAJJSlot(t *testing.T) {
	f := newJJFixture(t, manifest.VCSJJ)
	if err := f.create(f.template("plain", map[string]string{"work": f.app("ref: main\nsubpath: work")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	if !pathExists(t, filepath.Join(f.workspace("ws"), "work", ".jj")) {
		t.Fatalf("the slot is not a jj workspace")
	}
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if mode := stack.Workspaces["ws"].Sources["work"].Mode; mode != "" {
		t.Errorf("recorded mode = %q, want none, as the template wrote it", mode)
	}
	if got := f.slotStatus("ws", "work"); got.Mode != manifest.WorkspaceSourceModeJJ || got.State != "clean" {
		t.Errorf("status = %+v, want a clean jj slot", got)
	}
}

// A create that fails rolls its jj slot back: the workspace is forgotten, its
// directory removed and no git worktree registration is left, whether a
// later slot failed or the jj slot itself did. An empty directory that was at
// the slot's path before is put back, empty.
func TestJJSlotCreateRollsBack(t *testing.T) {
	f := newJJFixture(t, "")
	partial := f.template("partial", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work"), "zz-missing": f.missing()})
	broken := f.template("broken", map[string]string{"work": f.app("mode: jj\nref: no-such-base\nsubpath: work")})
	for _, tc := range []struct {
		name, template, want string
		emptyDir             bool
	}{
		{name: "later-slot", template: partial, want: "missing.git"},
		{name: "later-slot-empty-dir", template: partial, want: "missing.git", emptyDir: true},
		{name: "jj-slot-empty-dir", template: broken, want: "no-such-base", emptyDir: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := filepath.Join(f.workspace(tc.name), "work")
			if tc.emptyDir {
				mustMkdirAll(t, work)
			}
			if err := f.create(tc.template, tc.name, false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("WorkspaceCreate() error = %v, want the failure naming %s", err, tc.want)
			}
			if names := f.jjWorkspaces(f.cache()); !slices.Equal(sortedKeys(names), []string{"default"}) {
				t.Errorf("jj workspaces after the rollback = %q, want only the cache's own", sortedKeys(names))
			}
			if regs := f.registrations(f.cache()); len(regs) != 0 {
				t.Errorf("git worktrees after the rollback = %q, want none", regs)
			}
			switch {
			case tc.emptyDir:
				assertEmptyDir(t, work)
			case pathExists(t, f.workspace(tc.name)):
				t.Errorf("the rollback left %s", f.workspace(tc.name))
			}
		})
	}
}

// --sync over a jj slot that is still its workspace's checkout, as a create
// that died before saving the manifest leaves it, keeps it as it is: an edit
// no jj command has recorded stays, and so does its working-copy commit. A
// create without --sync refuses the path, and a --sync create that fails
// later leaves the kept slot alone.
func TestJJSlotSyncKeepsAnExistingCheckout(t *testing.T) {
	f := newJJFixture(t, "")
	template := f.template("jj", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")})
	partial := f.template("partial", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work"), "zz-missing": f.missing()})
	if err := f.create(template, "feature-a", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	work := filepath.Join(f.workspace("feature-a"), "work")
	unsaved := filepath.Join(work, "unsaved.txt")
	mustWriteFile(t, unsaved, "work in progress\n")
	change := f.jjWorkspaces(f.cache())["dev--feature-a"]
	f.forget("feature-a")

	if err := f.create(template, "feature-a", false); err == nil || !strings.Contains(err.Error(), "already exists and is not empty") {
		t.Fatalf("WorkspaceCreate() without --sync error = %v, want the occupied path refused", err)
	}
	if err := f.create(partial, "feature-a", true); err == nil || !strings.Contains(err.Error(), "missing.git") {
		t.Fatalf("WorkspaceCreate(--sync) with a failing slot error = %v, want that slot's failure", err)
	}
	if err := f.create(template, "feature-a", true); err != nil {
		t.Fatalf("WorkspaceCreate(--sync) error = %v", err)
	}
	if got, err := os.ReadFile(unsaved); err != nil || string(got) != "work in progress\n" {
		t.Fatalf("the unrecorded edit after --sync = %q, %v; want it kept", got, err)
	}
	if got := f.jjWorkspaces(f.cache())["dev--feature-a"]; got != change {
		t.Errorf("working-copy change after --sync = %s, want %s, the slot as it was", got, change)
	}
	if got := f.slotStatus("feature-a", "work"); got.State != "dirty" {
		t.Errorf("status after --sync = %+v, want dirty with the kept edit", got)
	}

	// A path that holds something other than the workspace's checkout is
	// refused even with --sync, whether the name is taken or not.
	if err := f.create(template, "feature-b", false); err != nil {
		t.Fatalf("WorkspaceCreate(feature-b) error = %v", err)
	}
	other := filepath.Join(f.workspace("feature-b"), "work")
	if err := os.RemoveAll(other); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	mustMkdirAll(t, other)
	mustWriteFile(t, filepath.Join(other, "not-a-slot.txt"), "mine\n")
	f.forget("feature-b")
	if err := f.create(template, "feature-b", true); err == nil || !strings.Contains(err.Error(), `is not the checkout of jj workspace "dev--feature-b"`) {
		t.Fatalf("WorkspaceCreate(--sync) over another directory error = %v, want it refused", err)
	}
	mustMkdirAll(t, filepath.Join(f.workspace("feature-c"), "work"))
	mustWriteFile(t, filepath.Join(f.workspace("feature-c"), "work", "mine.txt"), "mine\n")
	if err := f.create(template, "feature-c", true); err == nil || !strings.Contains(err.Error(), "already exists and is not empty") {
		t.Fatalf("WorkspaceCreate(--sync) over a directory with no jj workspace error = %v, want it refused", err)
	}
	for _, path := range []string{filepath.Join(other, "not-a-slot.txt"), filepath.Join(f.workspace("feature-c"), "work", "mine.txt")} {
		if !pathExists(t, path) {
			t.Errorf("a refused --sync removed %s", path)
		}
	}
}

// Repair cuts a jj slot whose directory is gone again: its name, which the
// cache still has, is forgotten and the slot added anew.
func TestJJSlotRepairReAddsAMissingCheckout(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("jj", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")}), "feature-a", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	work := filepath.Join(f.workspace("feature-a"), "work")
	before := f.jjWorkspaces(f.cache())["dev--feature-a"]
	if err := os.RemoveAll(work); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	result, err := f.platform.WorkspaceRepair(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceRepair() error = %v (%+v)", err, result)
	}
	if len(result.Slots) != 1 || result.Slots[0].Action != api.WorkspaceRepairCreated || !strings.Contains(result.Slots[0].Reason, "jj workspace") {
		t.Errorf("repair = %+v, want the jj slot created", result.Slots)
	}
	after, ok := f.jjWorkspaces(f.cache())["dev--feature-a"]
	if !ok || after == before {
		t.Errorf("jj workspace after the repair: change %q (listed %v), want a new one", after, ok)
	}
	if regs := f.registrations(f.cache()); len(regs) != 1 || strings.Contains(regs[0], "prunable") {
		t.Errorf("git worktrees after the repair = %q, want the new slot's alone", regs)
	}
	if got := f.slotStatus("feature-a", "work"); got.State != "clean" {
		t.Errorf("status after the repair = %+v, want clean", got)
	}
}

// Two stacks named alike share a cache outside both roots, so the second
// one's workspace of the same name would take the first's jj workspace name.
// It is refused as a CONFLICT naming the name, with or without --sync, before
// jj adds anything, and the first stack's slot keeps working.
func TestJJSlotNameTakenByAnotherStack(t *testing.T) {
	f := newJJFixture(t, "")
	shared := filepath.Join(f.base, "shared", "app")
	runGit(t, f.base, "clone", "-q", f.remote, shared)
	template := f.template("shared", map[string]string{"work": "source: app\nkind: git\nrepo: " + f.remote + "\ndefault_ref: main\ncache_path: " + shared + "\nmode: jj\nref: main\nsubpath: work"})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() in the first stack error = %v", err)
	}
	first := filepath.Join(f.workspace("ws"), "work")

	otherRoot := filepath.Join(f.base, "other", ".angee")
	f.writeStack(otherRoot, "")
	other, err := New(otherRoot)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, sync := range []bool{false, true} {
		_, err := other.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "ws", Sync: sync})
		if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeConflict || !strings.Contains(err.Error(), `jj workspace "dev--ws"`) || !strings.Contains(err.Error(), first) {
			t.Fatalf("WorkspaceCreate(sync %v) in the second stack error = %v, want a CONFLICT naming dev--ws and where it lives", sync, err)
		}
	}
	if pathExists(t, filepath.Join(otherRoot, "workspaces", "ws", "work")) {
		t.Errorf("the refused create left its slot")
	}
	if names := f.jjWorkspaces(shared); !slices.Contains(sortedKeys(names), "dev--ws") {
		t.Errorf("jj workspaces = %q, want the first stack's kept", sortedKeys(names))
	}
	if got := strings.TrimSpace(runGitOutput(t, first, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Errorf("the first stack's slot no longer works with git: %q", got)
	}
}

// A jj workspace name is <stack>--<workspace>, plus --<slot> for each of
// several jj slots of one primary, and a part that could make two names meet
// is refused.
func TestJJWorkspaceNames(t *testing.T) {
	platform, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stack := &manifest.Stack{Name: "dev", Sources: map[string]manifest.Source{
		"app":   {Kind: "git", CachePath: "sources/app"},
		"alias": {Kind: "git", CachePath: "sources/app"},
		"docs":  {Kind: "git"},
		"notes": {Kind: "local", Path: "/srv/notes"},
	}}
	jjSlot := func(source string) manifest.WorkspaceSource {
		return manifest.WorkspaceSource{Source: source, Mode: manifest.WorkspaceSourceModeJJ}
	}
	for _, tc := range []struct {
		name    string
		stack   string
		ws      string
		sources map[string]manifest.WorkspaceSource
		want    map[string]string
		wantErr string
	}{
		{name: "one per primary", stack: "dev", ws: "ws", sources: map[string]manifest.WorkspaceSource{
			"app": jjSlot("app"), "docs": jjSlot("docs"), "notes": jjSlot("notes"), "tree": {Source: "app", Mode: manifest.WorkspaceSourceModeWorktree},
		}, want: map[string]string{"app": "dev--ws", "docs": "dev--ws", "notes": "dev--ws"}},
		{name: "two of one primary", stack: "dev", ws: "ws", sources: map[string]manifest.WorkspaceSource{
			"a": jjSlot("app"), "b": jjSlot("alias"), "docs": jjSlot("docs"),
		}, want: map[string]string{"a": "dev--ws--a", "b": "dev--ws--b", "docs": "dev--ws"}},
		{name: "no jj slot", stack: "a--b", ws: "x--y", sources: map[string]manifest.WorkspaceSource{"app": {Source: "app"}}},
		{name: "stack with --", stack: "a--b", ws: "ws", sources: map[string]manifest.WorkspaceSource{"app": jjSlot("app")}, wantErr: `stack name "a--b"`},
		{name: "workspace with --", stack: "dev", ws: "x--y", sources: map[string]manifest.WorkspaceSource{"app": jjSlot("app")}, wantErr: `workspace name "x--y"`},
		{name: "workspace ending in -", stack: "dev", ws: "x-", sources: map[string]manifest.WorkspaceSource{"app": jjSlot("app")}, wantErr: `workspace name "x-"`},
		{name: "unnamed stack", stack: "", ws: "ws", sources: map[string]manifest.WorkspaceSource{"app": jjSlot("app")}, wantErr: `stack name ""`},
		{name: "slot starting with -", stack: "dev", ws: "ws", sources: map[string]manifest.WorkspaceSource{"-a": jjSlot("app"), "b": jjSlot("alias")}, wantErr: `slot name "-a"`},
	} {
		stack.Name = tc.stack
		got, err := platform.jjWorkspaceNames(stack, tc.ws, tc.sources)
		if tc.wantErr != "" {
			var invalid *InvalidInputError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: error = %v, want an invalid name %s", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || len(got) != len(tc.want) {
			t.Errorf("%s: names = %v, %v; want %v", tc.name, got, err, tc.want)
			continue
		}
		for slot, name := range tc.want {
			if got[slot] != name {
				t.Errorf("%s: names = %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

// A workspace name jj workspace names cannot take is refused before any slot
// is cut, the worktree slot before it included.
func TestJJSlotCreateRefusesANameThatCouldCollide(t *testing.T) {
	f := newJJStack(t, "")
	f.platform.jjProbe = supportedJJ
	template := f.template("mixed", map[string]string{
		"a-tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
		"work":   f.app("mode: jj\nref: main\nsubpath: work"),
	})
	err := f.create(template, "x--y", false)
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeInvalidInput || !strings.Contains(err.Error(), `workspace name "x--y"`) {
		t.Fatalf("WorkspaceCreate() error = %v, want INVALID_INPUT naming the workspace name", err)
	}
	for _, path := range []string{f.workspace("x--y"), f.cache()} {
		if pathExists(t, path) {
			t.Errorf("the refused create made %s", path)
		}
	}
}

// A machine that cannot run jj slots refuses a create with one, before any
// slot is cut or any cache cloned, saying what to install: no jj, a jj
// without `workspace add --colocate`, or a git older than 2.48.
func TestJJSlotCreateWithoutTheCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps jjdriver.Capabilities
		hint string
	}{
		{name: "no jj", caps: jjdriver.Capabilities{JJErr: errors.New("jj is not on PATH"), Git: "git version 2.50.1"}, hint: jjdriver.InstallHint},
		{name: "old git", caps: jjdriver.Capabilities{JJ: "jj 0.45.1", Git: "git version 2.47.2"}, hint: "Upgrade git to 2.48 or later."},
		{name: "git below the driver's floor", caps: jjdriver.Capabilities{JJ: "jj 0.45.1", Git: "git version 2.40.1", GitErr: errors.New("git version 2.40.1 is older than 2.42")}, hint: "Upgrade git to 2.48 or later."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newJJStack(t, "")
			f.platform.jjProbe = func(context.Context) jjdriver.Capabilities { return tc.caps }
			template := f.template("mixed", map[string]string{
				"a-tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
				"work":   f.app("mode: jj\nref: main\nsubpath: work"),
			})
			err := f.create(template, "feature-a", false)
			opErr := AsOperationError(err)
			if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseJJUnavailable || !strings.Contains(opErr.Hint, tc.hint) || strings.Contains(opErr.Hint, "2.42") || !errors.Is(err, vcs.ErrUnsupported) {
				t.Fatalf("WorkspaceCreate() error = %v (%+v), want PRECONDITION_FAILED jj_unavailable hinting %q", err, opErr, tc.hint)
			}
			for _, path := range []string{f.workspace("feature-a"), f.cache()} {
				if pathExists(t, path) {
					t.Errorf("the refused create made %s", path)
				}
			}
		})
	}
}

// A jj slot's primary must be where the manifest says (plan phase 6 step 8):
// a cache outside the stack root that is not there, as in a container that
// does not mount it, and a cache_path that is a dangling symlink, are refused
// before anything is cut, naming the path to mount. A local source must
// already be a jj repository.
func TestJJSlotPrimaryNotWhereTheManifestSays(t *testing.T) {
	f := newJJStack(t, "")
	f.platform.jjProbe = supportedJJ
	outside := filepath.Join(f.base, "elsewhere", "app")
	mustMkdirAll(t, filepath.Join(f.root, ".cache"))
	dangling := filepath.Join(f.root, ".cache", "dangling")
	if err := os.Symlink(filepath.Join(f.base, "gone"), dangling); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	plain := filepath.Join(f.base, "plain")
	mustMkdirAll(t, plain)
	for _, tc := range []struct {
		name, slot, want string
	}{
		{name: "outside the root", slot: "source: app\nkind: git\nrepo: " + f.remote + "\ndefault_ref: main\ncache_path: " + outside + "\nmode: jj\nsubpath: work", want: "mount " + outside},
		{name: "dangling symlink", slot: "source: app\nkind: git\nrepo: " + f.remote + "\ndefault_ref: main\ncache_path: .cache/dangling\nmode: jj\nsubpath: work", want: "mount " + dangling},
		{name: "local store without jj", slot: "source: notes\nkind: local\npath: " + plain + "\ndefault_ref: main\nmode: jj\nsubpath: work", want: "is not a jj repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := strings.ReplaceAll(tc.name, " ", "-")
			err := f.create(f.template(name, map[string]string{"work": tc.slot}), name, false)
			if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodePreconditionFailed || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("WorkspaceCreate() error = %v, want PRECONDITION_FAILED saying %q", err, tc.want)
			}
			for _, path := range []string{f.workspace(name), outside} {
				if pathExists(t, path) {
					t.Errorf("the refused create made %s", path)
				}
			}
		})
	}
}

// A local source that is a jj repository gets a jj workspace of its store
// rather than a link (plan phase 6 step 7): colocated with git when the store
// is, and without a .git when it is not. Destroy forgets both.
func TestJJSlotOfALocalStore(t *testing.T) {
	f := newJJFixture(t, manifest.VCSJJ)
	ctx := context.Background()
	colocated := filepath.Join(f.base, "colocated")
	runGit(t, f.base, "clone", "-q", f.remote, colocated)
	f.jj(colocated, "git", "init", "--colocate")
	plain := filepath.Join(f.base, "plain")
	f.jj(f.base, "git", "init", "--no-colocate", plain)
	mustWriteFile(t, filepath.Join(plain, "f.txt"), "one\n")
	f.jj(plain, "commit", "-m", "base")
	f.jj(plain, "bookmark", "create", "main", "-r", "@-")
	template := f.template("stores", map[string]string{
		"col":   "source: col\nkind: local\npath: " + colocated + "\ndefault_ref: main\nsubpath: col",
		"nocol": "source: nocol\nkind: local\npath: " + plain + "\ndefault_ref: main\nsubpath: nocol",
	})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	for store, wantGit := range map[string]bool{"col": true, "nocol": false} {
		slot := filepath.Join(f.workspace("ws"), store)
		if info, err := os.Lstat(slot); err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("slot %s: %v, %v; want a directory, not a link", store, info, err)
		}
		if !pathExists(t, filepath.Join(slot, ".jj")) || pathExists(t, filepath.Join(slot, ".git")) != wantGit {
			t.Errorf("slot %s: .jj %v, .git %v; want a jj workspace, with .git %v", store, pathExists(t, filepath.Join(slot, ".jj")), pathExists(t, filepath.Join(slot, ".git")), wantGit)
		}
		if got := f.slotStatus("ws", store); got.Kind != "local" || got.Mode != manifest.WorkspaceSourceModeJJ || got.State != "clean" {
			t.Errorf("slot %s status = %+v, want a clean jj slot of a local source", store, got)
		}
	}
	for _, store := range []string{colocated, plain} {
		if names := f.jjWorkspaces(store); !slices.Contains(sortedKeys(names), "dev--ws") {
			t.Errorf("jj workspaces of %s = %q, want dev--ws", store, sortedKeys(names))
		}
	}
	edit := filepath.Join(f.workspace("ws"), "nocol", "new.txt")
	mustWriteFile(t, edit, "unrecorded\n")
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err == nil || !strings.Contains(err.Error(), "nocol (uncommitted changes)") {
		t.Fatalf("WorkspaceDestroy() with an edit in a local store's jj slot error = %v, want a refusal", err)
	}
	if err := os.Remove(edit); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err != nil {
		t.Fatalf("WorkspaceDestroy() error = %v", err)
	}
	for _, store := range []string{colocated, plain} {
		if names := f.jjWorkspaces(store); slices.Contains(sortedKeys(names), "dev--ws") {
			t.Errorf("jj workspaces of %s after the destroy = %q, want dev--ws forgotten", store, sortedKeys(names))
		}
	}
	if regs := f.registrations(colocated); len(regs) != 0 {
		t.Errorf("git worktrees of the colocated store after the destroy = %q, want none", regs)
	}
}

// A rollback that cannot take the cache's lock still removes its jj slot,
// and forgets the slot's jj workspace too, which no prune would: the next
// create of the slot is not refused for a name that is still taken. git keeps
// the slot's registration until the cache is next pruned, which that create
// does before it adds.
func TestJJSlotRollbackWhileTheCacheLockIsHeld(t *testing.T) {
	f := newJJFixture(t, "")
	t.Setenv("ANGEE_LOCK_TIMEOUT", "100ms")
	partial := f.template("partial", map[string]string{"a-work": f.app("mode: jj\nref: main\nsubpath: work"), "docs": f.app("mode: clone\nref: main\nsubpath: docs")})
	template := f.template("jj", map[string]string{"a-work": f.app("mode: jj\nref: main\nsubpath: work")})
	var holder *fslock.Lock
	git := gitdriver.New(f.platform.gitClient())
	f.platform.slotDrivers = func(s vcs.Slot) vcs.Driver {
		switch s.Mode {
		case vcs.ModeJJ:
			return jjdriver.New(f.platform.gitClient())
		case vcs.ModeClone:
			return holdLockAndFail{Driver: git, hold: func() error {
				lockPath, err := primaryLockPath(f.cache())
				if err != nil {
					return err
				}
				holder = fslock.New(lockPath)
				return holder.Lock(context.Background())
			}}
		}
		return git
	}
	mustMkdirAll(t, f.workspace("feature-a"))
	logged := &warnings{}
	ctx := logctx.With(context.Background(), slog.New(logged))

	_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: partial, Name: "feature-a"})
	if holder != nil {
		_ = holder.Unlock()
	}
	if err == nil || !strings.Contains(err.Error(), "clone failed on purpose") {
		t.Fatalf("WorkspaceCreate() error = %v, want the clone slot's failure", err)
	}
	if pathExists(t, filepath.Join(f.workspace("feature-a"), "work")) {
		t.Errorf("the rollback left the jj slot")
	}
	if names := f.jjWorkspaces(f.cache()); slices.Contains(sortedKeys(names), "dev--feature-a") {
		t.Errorf("jj workspaces after the rollback = %q, want dev--feature-a forgotten", sortedKeys(names))
	}
	if regs := f.registrations(f.cache()); len(regs) != 1 || !strings.Contains(regs[0], "prunable") {
		t.Errorf("git worktrees after the rollback = %q, want the slot's, prunable", regs)
	}
	if !slices.ContainsFunc(logged.messages, func(m string) bool { return strings.Contains(m, "without deregistering it from source cache") }) {
		t.Errorf("warnings = %q, want one saying the slot stays registered", logged.messages)
	}

	if err := f.create(template, "feature-a", false); err != nil {
		t.Fatalf("WorkspaceCreate() after the rollback error = %v", err)
	}
	if regs := f.registrations(f.cache()); len(regs) != 1 || strings.Contains(regs[0], "prunable") {
		t.Errorf("git worktrees after the next create = %q, want the new slot's alone", regs)
	}
}

// nestedRepositories finds every .git and .jj below a slot's own, whatever
// they are, links included, and nothing a link leads to: a link to a
// repository, and the metadata of a nested repository, are not looked into.
func TestNestedRepositories(t *testing.T) {
	slot, outside := filepath.Join(t.TempDir(), "slot"), t.TempDir()
	for _, dir := range []string{".jj/working_copy", "a/b/.git/objects", "a/b/.git/x/.jj", "c/.jj", "node_modules/pkg/.git"} {
		mustMkdirAll(t, filepath.Join(slot, filepath.FromSlash(dir)))
	}
	mustMkdirAll(t, filepath.Join(outside, ".git"))
	mustWriteFile(t, filepath.Join(slot, ".git"), "gitdir: ../primary/.git/worktrees/slot\n")
	mustMkdirAll(t, filepath.Join(slot, "d"))
	mustWriteFile(t, filepath.Join(slot, "d", ".git"), "gitdir: /elsewhere\n")
	mustMkdirAll(t, filepath.Join(slot, "e"))
	for link, target := range map[string]string{"link": outside, "e/.git": filepath.Join(outside, ".git")} {
		if err := os.Symlink(target, filepath.Join(slot, filepath.FromSlash(link))); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}
	}
	mustMkdirAll(t, filepath.Join(slot, "slots", "inner", ".git"))
	got, err := nestedRepositories(slot, []string{"slots/inner"}, true)
	if err != nil {
		t.Fatalf("nestedRepositories() error = %v", err)
	}
	want := []string{filepath.FromSlash("a/b/.git"), filepath.FromSlash("c/.jj"), filepath.FromSlash("d/.git"), filepath.FromSlash("e/.git"), filepath.FromSlash("node_modules/pkg/.git")}
	if !slices.Equal(got, want) {
		t.Errorf("nestedRepositories() = %q, want %q: a link named .git counts, and the other slot at slots/inner is left to its own status", got, want)
	}
	// A slot that is not colocated has no .git of its own at its top.
	if got, err := nestedRepositories(slot, []string{"slots/inner"}, false); err != nil || !slices.Equal(got, append([]string{".git"}, want...)) {
		t.Errorf("nestedRepositories() of a slot not colocated = %q, %v; want its top .git counted too", got, err)
	}
	sources := map[string]manifest.WorkspaceSource{
		"root": {Source: "app", Subpath: "."}, "inner": {Source: "app", Subpath: "slots/inner"},
		"work": {Source: "app", Subpath: "work"}, "deeper": {Source: "app", Subpath: "work/sub"}, "named": {Source: "app"},
	}
	for subpath, want := range map[string][]string{".": {"named", "slots/inner", "work", "work/sub"}, "work": {"sub"}, "slots/inner": nil} {
		self := map[string]string{".": "root", "work": "work", "slots/inner": "inner"}[subpath]
		got := nestedSlots(subpath, sources, self)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("nestedSlots(%q) = %q, want %q", subpath, got, want)
		}
	}
	if reason := nestedRepositoriesReason(slices.Repeat([]string{"x/.git"}, 12)); !strings.HasSuffix(reason, "x/.git and 2 more") {
		t.Errorf("nestedRepositoriesReason() = %q, want ten named and the rest counted", reason)
	}
}

// A clone or a jj repository made inside a jj slot is invisible to jj, so the
// slot's status stays clean and pushed (owner, 2026-10-29), but destroy
// refuses to delete it, naming each, until they are gone.
func TestJJSlotDestroyRefusesNestedRepositories(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("jj", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	work := filepath.Join(f.workspace("ws"), "work")
	clone, inner := filepath.Join(work, "vendor", "lib"), filepath.Join(work, "notes", "inner")
	runGit(t, f.base, "clone", "-q", f.remote, clone)
	mustMkdirAll(t, filepath.Dir(inner))
	f.jj(f.base, "git", "init", "--no-colocate", inner)
	if got := f.slotStatus("ws", "work"); got.State != "clean" || !got.Pushed {
		t.Errorf("status with nested repositories = %+v, want clean and pushed, as jj sees it", got)
	}
	err := f.platform.WorkspaceDestroy(ctx, "ws", true)
	for _, want := range []string{"work (repositories inside it that jj does not record:", filepath.Join("vendor", "lib", ".git"), filepath.Join("notes", "inner", ".jj")} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("WorkspaceDestroy() error = %v, want a refusal naming %q", err, want)
		}
	}
	if !pathExists(t, filepath.Join(clone, "f.txt")) {
		t.Fatalf("the refused destroy removed the nested clone")
	}
	for _, dir := range []string{filepath.Dir(clone), filepath.Dir(inner)} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("RemoveAll() error = %v", err)
		}
	}
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err != nil {
		t.Fatalf("WorkspaceDestroy() once they are gone error = %v", err)
	}
}

// sharedCacheStacks is two stacks named dev whose workspace templates cut a
// jj slot of one cache outside both roots: the fixture's, and another at
// other/.angee. The first creates workspace ws, whose jj workspace is
// dev--ws.
func sharedCacheStacks(t *testing.T) (f *jjFixture, shared, template, first, otherRoot string) {
	t.Helper()
	f = newJJFixture(t, "")
	shared = filepath.Join(f.base, "shared", "app")
	runGit(t, f.base, "clone", "-q", f.remote, shared)
	template = f.template("shared", map[string]string{"work": "source: app\nkind: git\nrepo: " + f.remote + "\ndefault_ref: main\ncache_path: " + shared + "\nmode: jj\nref: main\nsubpath: work"})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() in the first stack error = %v", err)
	}
	return f, shared, template, filepath.Join(f.workspace("ws"), "work"), filepath.Join(f.base, "other", ".angee")
}

// A second stack named alike that declares the same workspace records the
// slot under the first stack's name, so only Elsewhere stands between it and
// forgetting the first stack's workspace. It refuses whether the first
// stack's checkout can be read there, cannot be (its .jj unreadable, as for
// another user's), or cannot be seen at all (gone, as from a container that
// does not mount it): the cache is shared, so a record jj cannot resolve is
// not this stack's to forget.
func TestJJSlotRecordedNameTakenByAnotherStack(t *testing.T) {
	f, shared, template, first, otherRoot := sharedCacheStacks(t)
	declare := func() *Platform {
		t.Helper()
		mustMkdirAll(t, otherRoot)
		stack := &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "dev",
			Sources: map[string]manifest.Source{"app": {Kind: "git", Repo: f.remote, DefaultRef: "main", CachePath: shared}},
			Workspaces: map[string]manifest.Workspace{"ws": {Template: template, Sources: map[string]manifest.WorkspaceSource{
				"work": {Source: "app", Mode: manifest.WorkspaceSourceModeJJ, Ref: "main", Subpath: "work"},
			}}}}
		if err := manifest.SaveFile(manifest.Path(otherRoot), stack); err != nil {
			t.Fatalf("SaveFile() error = %v", err)
		}
		other, err := New(otherRoot)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		return other
	}
	change := f.jjWorkspaces(shared)["dev--ws"]
	refused := func(when string, other *Platform, wantPath string) {
		t.Helper()
		_, err := other.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "ws"})
		if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeConflict || !strings.Contains(err.Error(), `jj workspace "dev--ws"`) || !strings.Contains(err.Error(), wantPath) {
			t.Fatalf("%s: WorkspaceCreate() error = %v, want a CONFLICT naming dev--ws and %q", when, err, wantPath)
		}
		if got := f.jjWorkspaces(shared)["dev--ws"]; got != change {
			t.Fatalf("%s: dev--ws's working copy = %q, want the first stack's %s kept", when, got, change)
		}
	}
	other := declare()
	refused("readable", other, first)
	if got := strings.TrimSpace(runGitOutput(t, first, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Errorf("the first stack's slot no longer works with git: %q", got)
	}
	// The second stack computes the same name for its slot, which it never
	// made, so the shared cache's dev--ws is the first stack's alone.
	for _, tc := range []struct {
		name     string
		platform *Platform
		want     api.SourceWorkspace
	}{
		{name: "first", platform: f.platform, want: api.SourceWorkspace{Name: "dev--ws", Owned: true, Path: first}},
		{name: "second", platform: other, want: api.SourceWorkspace{Name: "dev--ws"}},
	} {
		state, err := tc.platform.SourceStatus(context.Background(), "app")
		if err != nil {
			t.Fatalf("SourceStatus() in the %s stack error = %v", tc.name, err)
		}
		if want := []api.SourceWorkspace{{Name: "default"}, tc.want}; !reflect.DeepEqual(state.Workspaces, want) {
			t.Errorf("the %s stack's source workspaces = %+v, want %+v", tc.name, state.Workspaces, want)
		}
	}

	jjDir := filepath.Join(first, ".jj")
	if err := os.Chmod(jjDir, 0); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	refused("unreadable", declare(), first)
	if err := os.Chmod(jjDir, 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	if err := os.Rename(first, first+"-hidden"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	refused("out of sight", declare(), "workspace forget dev--ws")
}

// Destroy forgets the jj workspace the slot's checkout is, not the name the
// manifest computes now: here the slot was made as dev--ws--a beside an
// optional sibling of the same cache that was skipped, so the manifest now
// computes dev--ws, which another stack of the same name holds. That one is
// left alone.
func TestJJSlotDestroyForgetsOnlyItsOwnName(t *testing.T) {
	f := newJJFixture(t, "")
	shared := filepath.Join(f.base, "shared", "app")
	runGit(t, f.base, "clone", "-q", f.remote, shared)
	cached := "source: app\nkind: git\nrepo: " + f.remote + "\ndefault_ref: main\ncache_path: " + shared + "\nmode: jj\nsubpath: "
	otherRoot := filepath.Join(f.base, "other", ".angee")
	f.writeStack(otherRoot, "")
	other, err := New(otherRoot)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := other.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: f.template("single", map[string]string{"work": cached + "work\nref: main"}), Name: "ws"}); err != nil {
		t.Fatalf("WorkspaceCreate() in the other stack error = %v", err)
	}
	theirs := filepath.Join(otherRoot, "workspaces", "ws", "work")

	template := f.template("siblings", map[string]string{"a": cached + "a\nref: main", "b": cached + "b\nref: no-such-base\noptional: true"})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	if names := f.jjWorkspaces(shared); !slices.Equal(sortedKeys(names), []string{"default", "dev--ws", "dev--ws--a"}) {
		t.Fatalf("jj workspaces = %q, want the other stack's dev--ws and this one's dev--ws--a", sortedKeys(names))
	}
	if err := f.platform.WorkspaceDestroy(context.Background(), "ws", true); err != nil {
		t.Fatalf("WorkspaceDestroy() error = %v", err)
	}
	if names := f.jjWorkspaces(shared); !slices.Equal(sortedKeys(names), []string{"default", "dev--ws"}) {
		t.Errorf("jj workspaces after the destroy = %q, want dev--ws--a forgotten and the other stack's dev--ws kept", sortedKeys(names))
	}
	if got := strings.TrimSpace(runGitOutput(t, theirs, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Errorf("the other stack's slot no longer works with git: %q", got)
	}
}

// A local source's slot linked before the stack switched to vcs: jj is a
// link at a jj slot's path: status reports it as no jj workspace rather than
// reading the store through it, and destroy removes the link alone.
func TestJJSlotThatIsALink(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	store := filepath.Join(f.base, "store")
	runGit(t, f.base, "clone", "-q", f.remote, store)
	f.jj(store, "git", "init", "--colocate")
	if err := f.create(f.template("linked", map[string]string{"notes": "source: notes\nkind: local\npath: " + store + "\ndefault_ref: main\nsubpath: notes"}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	slot := filepath.Join(f.workspace("ws"), "notes")
	if info, err := os.Lstat(slot); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("slot under vcs: git = %v, %v; want a link", info, err)
	}
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	stack.VCS = manifest.VCSJJ
	if err := manifest.SaveFile(manifest.Path(f.root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	if got := f.slotStatus("ws", "notes"); got.State != "error" || !strings.Contains(got.Error, "is a link, not a jj workspace") {
		t.Errorf("status of a linked jj slot = %+v, want an error saying it is a link", got)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err != nil {
		t.Fatalf("WorkspaceDestroy() error = %v", err)
	}
	if !pathExists(t, filepath.Join(store, "f.txt")) || !slices.Contains(sortedKeys(f.jjWorkspaces(store)), "default") {
		t.Errorf("destroy touched the store behind the link")
	}
}

// With its base gone, a jj slot's status keeps a verdict where it needs none
// (#139: a *vcs.CountError wrapping vcs.ErrBaseNotFound): destroy refuses a
// dirty one for its changes. A clean one with no upstream gets no verdict, a
// bare vcs.ErrBaseNotFound, which destroy also refuses.
func TestJJSlotDestroyWithItsBaseGone(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("jj", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	ws := stack.Workspaces["ws"]
	work := ws.Sources["work"]
	work.Ref = "gone"
	ws.Sources["work"] = work
	stack.Workspaces["ws"] = ws
	if err := manifest.SaveFile(manifest.Path(f.root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err == nil || !errors.Is(err, vcs.ErrBaseNotFound) {
		t.Fatalf("WorkspaceDestroy() of a clean slot with no base error = %v, want the missing base", err)
	}
	mustWriteFile(t, filepath.Join(f.workspace("ws"), "work", "new.txt"), "unrecorded\n")
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err == nil || !strings.Contains(err.Error(), "work (uncommitted changes)") {
		t.Fatalf("WorkspaceDestroy() of a dirty slot with no base error = %v, want its verdict", err)
	}
	if !pathExists(t, filepath.Join(f.workspace("ws"), "work", "new.txt")) {
		t.Errorf("a refused destroy removed the slot's work")
	}
}

// Repair adds a jj slot whose directory was deleted again under the name
// create gave it, which the primary records at the slot's path, rather than
// the name the manifest computes now: create counted an optional sibling of
// the same cache, which was skipped and never recorded. Destroy then forgets
// it, and the workspace can be created again.
func TestJJSlotRepairKeepsTheNameCreateGave(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	template := f.template("siblings", map[string]string{
		"a": f.app("mode: jj\nref: main\nsubpath: a"),
		"b": f.app("mode: jj\nref: no-such-base\nsubpath: b\noptional: true"),
	})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	if names := sortedKeys(f.jjWorkspaces(f.cache())); !slices.Equal(names, []string{"default", "dev--ws--a"}) {
		t.Fatalf("jj workspaces after create = %q, want default and dev--ws--a", names)
	}
	if err := os.RemoveAll(filepath.Join(f.workspace("ws"), "a")); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	if result, err := f.platform.WorkspaceRepair(ctx, "ws"); err != nil {
		t.Fatalf("WorkspaceRepair() error = %v (%+v)", err, result)
	}
	if names := sortedKeys(f.jjWorkspaces(f.cache())); !slices.Equal(names, []string{"default", "dev--ws--a"}) {
		t.Errorf("jj workspaces after repair = %q, want dev--ws--a added again, and no dev--ws", names)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "ws", true); err != nil {
		t.Fatalf("WorkspaceDestroy() error = %v", err)
	}
	if names := sortedKeys(f.jjWorkspaces(f.cache())); !slices.Equal(names, []string{"default"}) {
		t.Errorf("jj workspaces after destroy = %q, want only the cache's own", names)
	}
	if err := f.create(template, "ws", false); err != nil {
		t.Errorf("WorkspaceCreate() again error = %v", err)
	}
}

// A cache whose path is inside the stack root only by its spelling, through
// a symlink to a shared directory, is shared: a record of another path jj
// cannot resolve there is not this stack's to forget, and a missing cache
// there is not cloned through the link.
func TestJJSlotCacheSharedThroughASymlink(t *testing.T) {
	f := newJJFixture(t, "")
	shared := filepath.Join(f.base, "shared")
	mustMkdirAll(t, shared)
	runGit(t, f.base, "clone", "-q", f.remote, filepath.Join(shared, "app"))
	otherRoot := filepath.Join(f.base, "other", ".angee")
	f.writeStack(otherRoot, "")
	other, err := New(otherRoot)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	direct := f.template("direct", map[string]string{"work": "source: app\nkind: git\nrepo: " + f.remote + "\ndefault_ref: main\ncache_path: " + filepath.Join(shared, "app") + "\nmode: jj\nref: main\nsubpath: work"})
	if _, err := other.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: direct, Name: "ws"}); err != nil {
		t.Fatalf("WorkspaceCreate() in the other stack error = %v", err)
	}
	theirs := filepath.Join(otherRoot, "workspaces", "ws", "work")
	if err := os.Rename(theirs, theirs+"-hidden"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	change := f.jjWorkspaces(filepath.Join(shared, "app"))["dev--ws"]

	if err := os.Symlink(shared, filepath.Join(f.root, ".cache")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	linked := f.template("linked", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")})
	stack.Sources = map[string]manifest.Source{"app": {Kind: "git", Repo: f.remote, DefaultRef: "main", CachePath: ".cache/app"}}
	stack.Workspaces = map[string]manifest.Workspace{"ws": {Template: linked, Sources: map[string]manifest.WorkspaceSource{
		"work": {Source: "app", Mode: manifest.WorkspaceSourceModeJJ, Ref: "main", Subpath: "work"},
	}}}
	if err := manifest.SaveFile(manifest.Path(f.root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	err = f.create(linked, "ws", false)
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeConflict || !strings.Contains(err.Error(), `jj workspace "dev--ws"`) {
		t.Fatalf("WorkspaceCreate() over a cache shared through a symlink error = %v, want a CONFLICT", err)
	}
	if got := f.jjWorkspaces(filepath.Join(shared, "app"))["dev--ws"]; got != change {
		t.Errorf("the other stack's dev--ws = %q, want %s kept", got, change)
	}

	f.platform.jjProbe = supportedJJ
	err = f.create(f.template("missing", map[string]string{"work": "source: lib\nkind: git\nrepo: " + f.remote + "\ndefault_ref: main\ncache_path: .cache/lib\nmode: jj\nref: main\nsubpath: work"}), "lib", false)
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodePreconditionFailed || !strings.Contains(err.Error(), "mount") {
		t.Fatalf("WorkspaceCreate() of a missing cache behind the symlink error = %v, want it refused", err)
	}
	if pathExists(t, filepath.Join(shared, "lib")) {
		t.Errorf("the refused create cloned a cache through the symlink")
	}
}

// Destroy lets a jj slot whose directory is empty go, as repair takes it for
// a missing slot, and forgets the workspace jj recorded there.
func TestJJSlotDestroyOverAnEmptyDirectory(t *testing.T) {
	f := newJJFixture(t, "")
	if err := f.create(f.template("jj", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	work := filepath.Join(f.workspace("ws"), "work")
	if err := os.RemoveAll(work); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	mustMkdirAll(t, work)
	if err := f.platform.WorkspaceDestroy(context.Background(), "ws", true); err != nil {
		t.Fatalf("WorkspaceDestroy() error = %v", err)
	}
	if names := sortedKeys(f.jjWorkspaces(f.cache())); !slices.Equal(names, []string{"default"}) {
		t.Errorf("jj workspaces after the destroy = %q, want dev--ws forgotten", names)
	}
	if regs := f.registrations(f.cache()); len(regs) != 0 {
		t.Errorf("git worktrees after the destroy = %q, want none", regs)
	}
}

// Destroy removes its slots on a context the request cannot cancel: with the
// request's context already cancelled, a jj slot's name is still forgotten,
// not left taken by a read that could not run.
func TestJJSlotDestroyCleanupOutlivesTheRequest(t *testing.T) {
	f := newJJFixture(t, "")
	if err := f.create(f.template("jj", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.platform.removeWorkspaceSources(ctx, stack, "ws", f.workspace("ws"), stack.Workspaces["ws"].Sources)
	if pathExists(t, filepath.Join(f.workspace("ws"), "work")) {
		t.Errorf("the slot is still there")
	}
	if names := sortedKeys(f.jjWorkspaces(f.cache())); !slices.Equal(names, []string{"default"}) {
		t.Errorf("jj workspaces = %q, want dev--ws forgotten", names)
	}
}

// A jj slot whose rebase conflicted says so beside its state, with its
// conflicted files, wherever a slot is reported: workspace status, workspace
// git, and the topology's link and summary. Its state keeps the vocabulary
// (ahead here, not a state of its own), the worktree slot beside it reports
// no jj fields, and the flag clears once the conflict is resolved. Before the
// rebase, the moved base shows in base_behind. A file jj will not snapshot
// shows in untracked_paths.
func TestJJSlotReportsItsConflicts(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	template := f.template("mixed", map[string]string{
		"tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
		"work": f.app("mode: jj\nref: main\nsubpath: work"),
	})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	work := filepath.Join(f.workspace("ws"), "work")
	got := f.slotStatus("ws", "work")
	if want := f.jj(work, "log", "--no-graph", "-r", "@", "-T", "change_id"); got.ChangeID != want || got.Conflicted || got.ConflictPaths != nil || got.BaseBehind != 0 {
		t.Errorf("new jj slot = %+v, want change id %s and no conflict", got.JJSlotState, want)
	}

	mustWriteFile(t, filepath.Join(work, "f.txt"), "slot\n")
	f.jj(work, "commit", "-m", "slot edit")
	mustWriteFile(t, filepath.Join(f.seed, "f.txt"), "base\n")
	runGit(t, f.seed, "commit", "-q", "-am", "base edit")
	runGit(t, f.seed, "push", "-q", "origin", "HEAD:main")
	if _, err := f.platform.SourceFetch(ctx, "app"); err != nil {
		t.Fatalf("SourceFetch() error = %v", err)
	}
	if got := f.slotStatus("ws", "work"); got.State != "diverged" || got.BaseBehind != 1 || got.Conflicted {
		t.Errorf("jj slot after the base moved = %+v, want diverged, one behind its base, no conflict", got)
	}

	f.jj(work, "rebase", "-b", "@", "--onto", "main@origin")
	conflicted := func(where, state string, jj api.JJSlotState) {
		t.Helper()
		if state != "ahead" || !jj.Conflicted || !slices.Equal(jj.ConflictPaths, []string{"f.txt"}) || jj.BaseBehind != 0 || jj.ChangeID == "" {
			t.Errorf("%s: conflicted jj slot = %s %+v, want ahead, conflicted in f.txt", where, state, jj)
		}
	}
	none := func(where, slot string, jj api.JJSlotState) {
		t.Helper()
		if !reflect.DeepEqual(jj, api.JJSlotState{}) {
			t.Errorf("%s: worktree slot %s jj fields = %+v, want none", where, slot, jj)
		}
	}
	status, err := f.platform.WorkspaceStatus(ctx, "ws")
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	for _, source := range status.Sources {
		if source.Slot == "work" {
			conflicted("workspace status", source.State, source.JJSlotState)
		} else {
			none("workspace status", source.Slot, source.JJSlotState)
		}
	}
	states, err := f.platform.WorkspaceGitStatus(ctx, "ws")
	if err != nil {
		t.Fatalf("WorkspaceGitStatus() error = %v", err)
	}
	for _, state := range states {
		if state.Slot == "work" {
			conflicted("workspace git", state.State, state.JJSlotState)
		} else {
			none("workspace git", state.Slot, state.JJSlotState)
		}
	}
	topology, err := f.platform.GitOpsTopology(ctx)
	if err != nil {
		t.Fatalf("GitOpsTopology() error = %v", err)
	}
	for _, link := range topology.Links {
		if link.Slot == "work" {
			conflicted("topology link", link.State, link.JJSlotState)
		} else {
			none("topology link", link.Slot, link.JJSlotState)
		}
	}
	if topology.Summary.Conflicted != 1 || topology.Summary.Worktrees != 1 {
		t.Errorf("topology summary = %+v, want one conflicted slot and one worktree, the jj slot not counted as one", topology.Summary)
	}

	mustWriteFile(t, filepath.Join(work, "f.txt"), "resolved\n")
	f.jj(work, "squash")
	if got := f.slotStatus("ws", "work"); got.Conflicted || got.ConflictPaths != nil || got.State != "ahead" {
		t.Errorf("jj slot after resolving = %+v, want ahead and no conflict", got)
	}

	// A file over jj's size limit stays out of the working-copy commit: it is
	// listed, and the slot is dirty.
	f.jj(f.cache(), "config", "set", "--repo", "snapshot.max-new-file-size", "16")
	mustWriteFile(t, filepath.Join(work, "big.bin"), strings.Repeat("x", 200))
	if got := f.slotStatus("ws", "work"); got.State != "dirty" || !slices.Equal(got.UntrackedPaths, []string{"big.bin"}) {
		t.Errorf("jj slot holding a file jj does not snapshot = %+v, want dirty with big.bin untracked", got)
	}
	states, err = f.platform.WorkspaceGitStatus(ctx, "ws")
	if err != nil {
		t.Fatalf("WorkspaceGitStatus() error = %v", err)
	}
	for _, state := range states {
		if state.Slot == "work" && !slices.Equal(state.UntrackedPaths, []string{"big.bin"}) {
			t.Errorf("workspace git untracked paths = %q, want big.bin", state.UntrackedPaths)
		}
	}
}

// The topology lists the jj workspaces of a source whose cache or store is a
// jj repository, and source status does too. One whose checkout is at the
// path this stack's manifest gives one of its jj slots is owned, with that
// path: the longer name create gave a slot whose optional sibling was
// skipped included, and each of two primaries' workspaces of one name with
// its own slot's path. The cache's own, another stack's, and this stack's
// whose checkout is gone are not owned and have no path. A local store not
// colocated with git lists its workspaces too. A cache no jj slot has
// colocated yet lists none and runs no jj, and its worktree slot counts as
// the only worktree. A jj repository jj cannot read is warned about, and the
// source is reported from git as before, with no workspaces; an operator
// without jj lists none and warns of nothing.
func TestGitOpsTopologyListsTheJJWorkspacesOfASource(t *testing.T) {
	f := newJJFixture(t, "")
	var warnings bytes.Buffer
	ctx := logctx.With(context.Background(), slog.New(logctx.NewCLIHandler(&warnings, slog.LevelWarn)))
	if err := f.create(f.template("plain", map[string]string{"tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree")}), "plain", false); err != nil {
		t.Fatalf("WorkspaceCreate(plain) error = %v", err)
	}
	// A local source can be a file, which holds no .jj to look for.
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	notes := filepath.Join(f.base, "notes.txt")
	mustWriteFile(t, notes, "notes\n")
	stack.Sources["notes"] = manifest.Source{Kind: "local", Path: notes}
	if err := manifest.SaveFile(manifest.Path(f.root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	topology, err := f.platform.GitOpsTopology(ctx)
	if err != nil {
		t.Fatalf("GitOpsTopology() error = %v", err)
	}
	if len(topology.Sources) != 2 || topology.Sources[0].Workspaces != nil || topology.Sources[1].Workspaces != nil || topology.Summary.Worktrees != 1 || warnings.Len() != 0 {
		t.Fatalf("topology before any jj slot = %+v, %+v, warnings %q; want sources with no jj workspaces, one worktree, and no jj run", topology.Sources, topology.Summary, warnings.String())
	}
	mustMkdirAll(t, filepath.Join(f.cache(), ".jj"))
	topology, err = f.platform.GitOpsTopology(ctx)
	if err != nil {
		t.Fatalf("GitOpsTopology() over a broken .jj error = %v", err)
	}
	if source := topology.Sources[0]; source.Workspaces != nil || source.State != "clean" || source.Error != "" || !strings.Contains(warnings.String(), "list the jj workspaces of source app") {
		t.Fatalf("topology over a broken .jj = %+v, warnings %q; want the source read from git, no workspaces, and a warning", source, warnings.String())
	}
	if err := os.Remove(filepath.Join(f.cache(), ".jj")); err != nil {
		t.Fatalf("Remove(.jj) error = %v", err)
	}

	if err := f.create(f.template("mixed", map[string]string{
		"tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
		"work": f.app("mode: jj\nref: main\nsubpath: work"),
	}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate(ws) error = %v", err)
	}
	if err := f.create(f.template("siblings", map[string]string{
		"a": f.app("mode: jj\nref: main\nsubpath: a"),
		"b": f.app("mode: jj\nref: no-such-base\nsubpath: b\noptional: true"),
	}), "solo", false); err != nil {
		t.Fatalf("WorkspaceCreate(solo) error = %v", err)
	}
	mustMkdirAll(t, filepath.Join(f.base, "other"))
	f.jj(f.cache(), "workspace", "add", "--colocate", "--name", "other--ws", "-r", "main@origin", filepath.Join(f.base, "other", "ws"))

	want := []api.SourceWorkspace{
		{Name: "default"},
		{Name: "dev--solo--a", Owned: true, Path: filepath.Join(f.workspace("solo"), "a")},
		{Name: "dev--ws", Owned: true, Path: filepath.Join(f.workspace("ws"), "work")},
		{Name: "other--ws"},
	}
	topology, err = f.platform.GitOpsTopology(ctx)
	if err != nil {
		t.Fatalf("GitOpsTopology() error = %v", err)
	}
	if len(topology.Sources) != 2 || topology.Sources[0].Name != "app" || !reflect.DeepEqual(topology.Sources[0].Workspaces, want) {
		t.Errorf("topology source workspaces = %+v, want app's %+v", topology.Sources, want)
	}
	if topology.Summary.Worktrees != 2 {
		t.Errorf("topology worktrees = %d, want the two worktree slots, not the jj slots", topology.Summary.Worktrees)
	}
	for _, link := range topology.Links {
		if link.Mode == manifest.WorkspaceSourceModeJJ && link.Path != filepath.Join(f.workspace("ws"), "work") && link.Path != filepath.Join(f.workspace("solo"), "a") {
			t.Errorf("jj link %s at %s, want the path an owned workspace names", link.ID, link.Path)
		}
	}
	state, err := f.platform.SourceStatus(ctx, "app")
	if err != nil {
		t.Fatalf("SourceStatus() error = %v", err)
	}
	if !reflect.DeepEqual(state.Workspaces, want) {
		t.Errorf("source status workspaces = %+v, want %+v", state.Workspaces, want)
	}

	store := filepath.Join(f.base, "store")
	f.jj(f.base, "git", "init", "--no-colocate", store)
	mustWriteFile(t, filepath.Join(store, "f.txt"), "one\n")
	f.jj(store, "commit", "-m", "base")
	f.jj(store, "bookmark", "create", "main", "-r", "@-")
	if err := f.create(f.template("two", map[string]string{
		"work":  f.app("mode: jj\nref: main\nsubpath: work"),
		"store": "source: store\nkind: local\npath: " + store + "\ndefault_ref: main\nmode: jj\nsubpath: store",
	}), "two", false); err != nil {
		t.Fatalf("WorkspaceCreate(two) error = %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.workspace("solo"), "a")); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	listed := func() map[string][]api.SourceWorkspace {
		t.Helper()
		topology, err := f.platform.GitOpsTopology(ctx)
		if err != nil {
			t.Fatalf("GitOpsTopology() error = %v", err)
		}
		bySource := map[string][]api.SourceWorkspace{}
		for _, source := range topology.Sources {
			bySource[source.Name] = source.Workspaces
		}
		return bySource
	}
	wantBySource := map[string][]api.SourceWorkspace{
		"notes": nil,
		"app": {
			{Name: "default"},
			{Name: "dev--solo--a"},
			{Name: "dev--two", Owned: true, Path: filepath.Join(f.workspace("two"), "work")},
			{Name: "dev--ws", Owned: true, Path: filepath.Join(f.workspace("ws"), "work")},
			{Name: "other--ws"},
		},
		"store": {
			{Name: "default"},
			{Name: "dev--two", Owned: true, Path: filepath.Join(f.workspace("two"), "store")},
		},
	}
	if got := listed(); !reflect.DeepEqual(got, wantBySource) {
		t.Errorf("topology workspaces by source = %+v, want %+v", got, wantBySource)
	}

	// A slot that cannot be read is not owned, and the rest is listed.
	unreadable := filepath.Join(f.workspace("ws"), "work")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
	wantBySource["app"][3] = api.SourceWorkspace{Name: "dev--ws"}
	if got := listed(); !reflect.DeepEqual(got, wantBySource) {
		t.Errorf("topology workspaces with an unreadable slot = %+v, want %+v", got, wantBySource)
	}
	if err := os.Chmod(unreadable, 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	f.platform.jjProbe = func(context.Context) jjdriver.Capabilities {
		return jjdriver.Capabilities{JJErr: errors.New("jj is not on PATH"), Git: "git version 2.50.1"}
	}
	warnings.Reset()
	if got := listed(); got["app"] != nil || got["store"] != nil || warnings.Len() != 0 {
		t.Errorf("topology workspaces without jj = %+v, warnings %q; want none, and no warning", got, warnings.String())
	}
}

// A slot made as its workspace's only jj slot of a cache keeps the name
// <stack>--<workspace> after a second jj slot of the cache is added to the
// manifest by hand and repaired in, as <stack>--<workspace>--<slot>, while
// the manifest now computes the longer name for both: each reads as owned,
// with its own path, from its checkout.
func TestJJWorkspacesOwnASlotUnderTheNameCreateGaveIt(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("jj", map[string]string{"a": f.app("mode: jj\nref: main\nsubpath: a")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	stack, err := manifest.LoadFile(manifest.Path(f.root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	b := stack.Workspaces["ws"].Sources["a"]
	b.Subpath = "b"
	stack.Workspaces["ws"].Sources["b"] = b
	if err := manifest.SaveFile(manifest.Path(f.root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	if result, err := f.platform.WorkspaceRepair(ctx, "ws"); err != nil {
		t.Fatalf("WorkspaceRepair() error = %v (%+v)", err, result)
	}
	state, err := f.platform.SourceStatus(ctx, "app")
	if err != nil {
		t.Fatalf("SourceStatus() error = %v", err)
	}
	want := []api.SourceWorkspace{
		{Name: "default"},
		{Name: "dev--ws", Owned: true, Path: filepath.Join(f.workspace("ws"), "a")},
		{Name: "dev--ws--b", Owned: true, Path: filepath.Join(f.workspace("ws"), "b")},
	}
	if !reflect.DeepEqual(state.Workspaces, want) {
		t.Errorf("source workspaces = %+v, want %+v", state.Workspaces, want)
	}
}

// A name is owned only when one checkout of this stack holds it, and that
// checkout no other name: once a workspace elsewhere named as this stack
// could have named the slot (`<stack>--<workspace>--<slot>`) has `jj edit`ed
// the slot's working-copy commit, the slot's checkout holds both names, and
// neither is owned. Before that, the one elsewhere is not owned.
func TestJJWorkspacesOwnNoneOfTwoNamesOneCheckoutHolds(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("jj", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	elsewhere := filepath.Join(f.base, "elsewhere")
	f.jj(f.cache(), "workspace", "add", "--colocate", "--name", "dev--ws--work", "-r", "main@origin", elsewhere)
	work := filepath.Join(f.workspace("ws"), "work")
	check := func(when string, want []api.SourceWorkspace) {
		t.Helper()
		state, err := f.platform.SourceStatus(ctx, "app")
		if err != nil {
			t.Fatalf("SourceStatus() error = %v", err)
		}
		if !reflect.DeepEqual(state.Workspaces, want) {
			t.Errorf("source workspaces %s = %+v, want %+v", when, state.Workspaces, want)
		}
	}
	check("with one elsewhere", []api.SourceWorkspace{{Name: "default"}, {Name: "dev--ws", Owned: true, Path: work}, {Name: "dev--ws--work"}})
	f.jj(elsewhere, "edit", strings.TrimSpace(f.jj(work, "log", "--no-graph", "-r", "@", "-T", "change_id")))
	check("once it edits the slot's commit", []api.SourceWorkspace{{Name: "default"}, {Name: "dev--ws"}, {Name: "dev--ws--work"}})
}

// A checkout of another repository at the slot's path, under the slot's own
// name, is not the cache's workspace of that name: its working-copy change
// is not the one the cache lists for the name, so the cache's is not owned.
func TestJJWorkspacesOwnNoCheckoutOfAnotherRepository(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("jj", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	work := filepath.Join(f.workspace("ws"), "work")
	if err := os.RemoveAll(work); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	other := filepath.Join(f.base, "other-repo")
	f.jj(f.base, "git", "init", "--no-colocate", other)
	mustWriteFile(t, filepath.Join(other, "f.txt"), "other\n")
	f.jj(other, "commit", "-m", "other")
	f.jj(other, "workspace", "add", "--name", "dev--ws", work)
	state, err := f.platform.SourceStatus(ctx, "app")
	if err != nil {
		t.Fatalf("SourceStatus() error = %v", err)
	}
	if want := []api.SourceWorkspace{{Name: "default"}, {Name: "dev--ws"}}; !reflect.DeepEqual(state.Workspaces, want) {
		t.Errorf("source workspaces = %+v, want %+v", state.Workspaces, want)
	}
}
