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

// sourceSlotFixture is a stack named dev whose git sources are bare
// repositories beside it, each holding main, and workspace templates under
// its base.
type sourceSlotFixture struct {
	t          *testing.T
	base, root string
	stack      *manifest.Stack
	platform   *Platform
}

func newSourceSlotFixture(t *testing.T) *sourceSlotFixture {
	t.Helper()
	base := t.TempDir()
	f := &sourceSlotFixture{t: t, base: base, root: filepath.Join(base, ".angee")}
	f.stack = &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "dev", Sources: map[string]manifest.Source{}}
	mustMkdirAll(t, f.root)
	platform, err := New(f.root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	f.platform = platform
	return f
}

// source declares git source name, a bare repository of its own holding
// main, marked for a slot or not.
func (f *sourceSlotFixture) source(name string, slot bool) {
	f.t.Helper()
	remote, seed := filepath.Join(f.base, "remotes", name+".git"), filepath.Join(f.base, "seeds", name)
	mustMkdirAll(f.t, filepath.Dir(remote))
	mustMkdirAll(f.t, filepath.Dir(seed))
	runGit(f.t, f.base, "init", "-q", "--bare", "-b", "main", remote)
	runGit(f.t, f.base, "clone", "-q", remote, seed)
	runGit(f.t, seed, "config", "user.email", "test@example.invalid")
	runGit(f.t, seed, "config", "user.name", "test")
	mustWriteFile(f.t, filepath.Join(seed, name+".txt"), name+"\n")
	runGit(f.t, seed, "add", ".")
	runGit(f.t, seed, "commit", "-q", "-m", "base")
	runGit(f.t, seed, "push", "-q", "origin", "HEAD:main")
	f.stack.Sources[name] = manifest.Source{Kind: "git", Repo: remote, DefaultRef: "main", Slot: slot}
}

// save writes the stack's manifest.
func (f *sourceSlotFixture) save() {
	f.t.Helper()
	if err := manifest.SaveFile(manifest.Path(f.root), f.stack); err != nil {
		f.t.Fatalf("SaveFile() error = %v", err)
	}
}

// template writes workspace template name with the given `_angee` lines
// after its inputs (sources, source_slot), and returns its path. Its
// slot_mode input defaults to worktree.
func (f *sourceSlotFixture) template(name, angee string) string {
	f.t.Helper()
	root := filepath.Join(f.base, ".templates", "workspaces", name)
	mustMkdirAll(f.t, filepath.Join(root, "template"))
	mustWriteFile(f.t, filepath.Join(root, "copier.yml"), `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: `+name+`
  inputs:
    slot_mode:
      type: str
      default: worktree
    branch_prefix:
      type: str
      default: dev
`+angee+`slot_mode:
  type: str
  default: worktree
branch_prefix:
  type: str
  default: dev
`)
	mustWriteFile(f.t, filepath.Join(root, "template", "README.md.jinja"), "workspace {{ workspace_name }}\n")
	return root
}

// slots are the slots the saved manifest records for workspace name.
func (f *sourceSlotFixture) slots(name string) map[string]manifest.WorkspaceSource {
	f.t.Helper()
	stack, err := f.platform.LoadStack()
	if err != nil {
		f.t.Fatalf("LoadStack() error = %v", err)
	}
	return stack.Workspaces[name].Sources
}

// The prototype every test's template declares: the slots of marked sources
// are worktrees on the workspace's branch.
const sourceSlotPrototype = `  source_slot:
    mode: "${inputs.slot_mode}"
    branch: "${inputs.branch_prefix}/${name | slug}"
`

// A workspace cut from a template that declares `_angee.source_slot` gets the
// template's own slots and one per stack source marked `slot: true`: named
// after the source, at that subpath, in the prototype's mode and on its
// branch. A source that is not marked gets none.
func TestWorkspaceCreateCutsASlotPerMarkedSource(t *testing.T) {
	ctx := context.Background()
	f := newSourceSlotFixture(t)
	f.source("framework", false)
	f.source("angee-arp", true)
	f.source("angee-omelas", true)
	f.source("unmarked", false)
	f.save()
	template := f.template("src", `  sources:
    framework:
      mode: "${inputs.slot_mode}"
      branch: "${inputs.branch_prefix}/${name | slug}"
      subpath: angee
`+sourceSlotPrototype)

	created, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "feature-a"})
	if err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	want := map[string]manifest.WorkspaceSource{
		"framework":    {Source: "framework", Mode: "worktree", Branch: "dev/feature-a", Subpath: "angee"},
		"angee-arp":    {Source: "angee-arp", Mode: "worktree", Branch: "dev/feature-a", Subpath: "angee-arp"},
		"angee-omelas": {Source: "angee-omelas", Mode: "worktree", Branch: "dev/feature-a", Subpath: "angee-omelas"},
	}
	got := f.slots("feature-a")
	if len(got) != len(want) {
		t.Fatalf("recorded slots = %v, want %v", sortedKeys(got), sortedKeys(want))
	}
	for slot, source := range want {
		if got[slot] != source {
			t.Fatalf("recorded slot %s = %+v, want %+v", slot, got[slot], source)
		}
		checkout := filepath.Join(created.Path, source.Subpath)
		if branch := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "--abbrev-ref", "HEAD")); branch != "dev/feature-a" {
			t.Fatalf("slot %s is on %q, want dev/feature-a", slot, branch)
		}
		if _, err := os.Stat(filepath.Join(checkout, source.Source+".txt")); err != nil {
			t.Fatalf("slot %s does not hold its source's files: %v", slot, err)
		}
	}
	if _, err := os.Stat(filepath.Join(created.Path, "unmarked")); !os.IsNotExist(err) {
		t.Fatalf("an unmarked source was checked out: %v", err)
	}
	status, err := f.platform.WorkspaceStatus(ctx, "feature-a")
	if err != nil || len(status.Sources) != 3 {
		t.Fatalf("WorkspaceStatus() = %+v, %v; want three slots", status.Sources, err)
	}
	for _, slot := range status.Sources {
		if slot.State != "clean" {
			t.Fatalf("slot %s state = %q (%s), want clean", slot.Slot, slot.State, slot.Error)
		}
	}
}

// A stack-declared workspace, cut when the stack is first prepared, gets its
// marked sources' slots the same way.
func TestDeclaredWorkspaceGetsASlotPerMarkedSource(t *testing.T) {
	ctx := context.Background()
	f := newSourceSlotFixture(t)
	f.source("angee-arp", true)
	f.source("angee-omelas", true)
	template := f.template("src", sourceSlotPrototype)
	f.stack.Workspaces = map[string]manifest.Workspace{"src": {Template: template, Inputs: map[string]string{"branch_prefix": "omelas"}}}
	f.save()

	stack, err := f.platform.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}
	cut, err := f.platform.materializeDeclaredWorkspaces(ctx, stack)
	if err != nil || !cut {
		t.Fatalf("materializeDeclaredWorkspaces() = %v, %v; want the workspace cut", cut, err)
	}
	got := f.slots("src")
	for _, name := range []string{"angee-arp", "angee-omelas"} {
		want := manifest.WorkspaceSource{Source: name, Mode: "worktree", Branch: "omelas/src", Subpath: name}
		if got[name] != want {
			t.Fatalf("recorded slot %s = %+v, want %+v", name, got[name], want)
		}
		if _, err := os.Stat(filepath.Join(f.root, "workspaces", "src", name, name+".txt")); err != nil {
			t.Fatalf("slot %s is not checked out: %v", name, err)
		}
	}
	if len(got) != 2 {
		t.Fatalf("recorded slots = %v, want the two marked sources'", sortedKeys(got))
	}
}

// A template without the prototype cuts its own slots only, whatever the
// stack marks.
func TestWorkspaceWithoutSourceSlotCutsNoMarkedSource(t *testing.T) {
	f := newSourceSlotFixture(t)
	f.source("framework", false)
	f.source("angee-arp", true)
	f.save()
	template := f.template("plain", `  sources:
    framework:
      mode: worktree
      branch: "plain/${name | slug}"
`)
	if _, err := f.platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	if got := f.slots("a"); len(got) != 1 || got["framework"].Source != "framework" {
		t.Fatalf("recorded slots = %+v, want the template's slot alone", got)
	}
}

// A marked source the template already checks out, in a slot of the
// source's name or of another, keeps that slot: it is cut once, by the
// template's own rule. A slot of the source's name that checks out another
// source is refused, and so is another slot at the path the marked source's
// slot would take.
func TestMarkedSourceAndADeclaredSlotOfItsName(t *testing.T) {
	ctx := context.Background()

	t.Run("the same source keeps the declared slot", func(t *testing.T) {
		f := newSourceSlotFixture(t)
		f.source("angee-arp", true)
		f.source("angee-omelas", true)
		f.save()
		// The declared slot is optional and cut as a detached worktree,
		// which the prototype would not do.
		template := f.template("src", `  sources:
    angee-arp:
      source: angee-arp
      mode: worktree
      ref: main
      subpath: angee-arp
      optional: true
`+sourceSlotPrototype)
		created, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "a"})
		if err != nil {
			t.Fatalf("WorkspaceCreate() error = %v", err)
		}
		got := f.slots("a")
		if len(got) != 2 {
			t.Fatalf("recorded slots = %v, want one per source", sortedKeys(got))
		}
		if want := (manifest.WorkspaceSource{Source: "angee-arp", Mode: "worktree", Ref: "main", Subpath: "angee-arp"}); got["angee-arp"] != want {
			t.Fatalf("slot angee-arp = %+v, want the template's own %+v", got["angee-arp"], want)
		}
		if want := (manifest.WorkspaceSource{Source: "angee-omelas", Mode: "worktree", Branch: "dev/a", Subpath: "angee-omelas"}); got["angee-omelas"] != want {
			t.Fatalf("slot angee-omelas = %+v, want the prototype's %+v", got["angee-omelas"], want)
		}
		assertDetached(t, filepath.Join(created.Path, "angee-arp"))
	})

	t.Run("a declared slot that names its source by an input", func(t *testing.T) {
		f := newSourceSlotFixture(t)
		f.source("work", true)
		f.save()
		// `source: ""` falls back to the slot's name, the marked source's.
		template := f.template("src", `  sources:
    work:
      source: ""
      mode: worktree
      ref: main
      subpath: .work
`+sourceSlotPrototype)
		if _, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "a"}); err != nil {
			t.Fatalf("WorkspaceCreate() error = %v", err)
		}
		if got := f.slots("a"); len(got) != 1 || got["work"].Subpath != ".work" {
			t.Fatalf("recorded slots = %+v, want the declared slot alone", got)
		}
	})

	t.Run("a declared slot of another name keeps the source too", func(t *testing.T) {
		f := newSourceSlotFixture(t)
		f.source("angee", true)
		f.source("angee-arp", true)
		f.save()
		// Cut by the prototype as well, the source would be two worktrees on
		// one branch, which git refuses.
		template := f.template("src", `  sources:
    framework:
      source: angee
      mode: worktree
      branch: "${inputs.branch_prefix}/${name | slug}"
      subpath: fw
`+sourceSlotPrototype)
		if _, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "a"}); err != nil {
			t.Fatalf("WorkspaceCreate() error = %v", err)
		}
		got := f.slots("a")
		if len(got) != 2 || got["framework"].Subpath != "fw" || got["angee-arp"].Subpath != "angee-arp" {
			t.Fatalf("recorded slots = %+v, want the declared slot of angee and the marked angee-arp", got)
		}
	})

	t.Run("another source is refused", func(t *testing.T) {
		f := newSourceSlotFixture(t)
		f.source("x", true)
		f.source("y", false)
		f.save()
		template := f.template("src", `  sources:
    x:
      source: y
      mode: worktree
      ref: main
`+sourceSlotPrototype)
		_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "a"})
		want := `workspace "a": source "x" is marked slot: true, but the template's slot "x" checks out source "y"; rename one of them, or remove the mark`
		if err == nil || err.Error() != want {
			t.Fatalf("WorkspaceCreate() error = %v, want %q", err, want)
		}
		f.assertNothingCut("a")
	})

	t.Run("another source that is not declared is refused too", func(t *testing.T) {
		f := newSourceSlotFixture(t)
		f.source("x", true)
		f.save()
		// The declared slot is optional and its source undeclared, so it is
		// skipped; its name is still its own.
		template := f.template("src", `  sources:
    x:
      source: elsewhere
      mode: worktree
      ref: main
      optional: true
`+sourceSlotPrototype)
		_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "a"})
		if err == nil || !strings.Contains(err.Error(), `the template's slot "x" checks out source "elsewhere"`) {
			t.Fatalf("WorkspaceCreate() error = %v", err)
		}
		f.assertNothingCut("a")
	})

	t.Run("another slot at its path is refused", func(t *testing.T) {
		f := newSourceSlotFixture(t)
		f.source("framework", false)
		f.source("lib", true)
		f.save()
		template := f.template("src", `  sources:
    framework:
      mode: worktree
      ref: main
      subpath: lib
`+sourceSlotPrototype)
		_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "a"})
		want := `workspace "a": source "lib" is marked slot: true, but the template's slot "framework" is checked out at lib, where its slot would be; rename the source, or remove the mark`
		if err == nil || err.Error() != want {
			t.Fatalf("WorkspaceCreate() error = %v, want %q", err, want)
		}
		f.assertNothingCut("a")
	})
}

// assertNothingCut checks a refused create left no workspace behind.
func (f *sourceSlotFixture) assertNothingCut(name string) {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(f.root, "workspaces", name)); !os.IsNotExist(err) {
		f.t.Fatalf("workspace directory after a refused create: %v, want none", err)
	}
	if got := f.slots(name); len(got) != 0 {
		f.t.Fatalf("recorded slots after a refused create = %+v", got)
	}
}

// The prototype says how the slots of marked sources are cut, not which
// source a slot checks out or where.
func TestSourceSlotPrototypeTakesNoSourceOrSubpath(t *testing.T) {
	for field, line := range map[string]string{
		"source":  `source: app`,
		"subpath": `subpath: app`,
		"repo":    `repo: https://example.invalid/app.git`,
		"kind":    `kind: git`,
	} {
		t.Run(field, func(t *testing.T) {
			f := newSourceSlotFixture(t)
			f.source("lib", true)
			f.save()
			template := f.template("src", "  source_slot:\n    mode: worktree\n    "+line+"\n")
			_, err := f.platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "a"})
			var invalid *InvalidInputError
			if err == nil || !errors.As(err, &invalid) || !strings.Contains(err.Error(), "_angee.source_slot sets "+field+", but it takes only mode, branch, ref and optional") {
				t.Fatalf("WorkspaceCreate() error = %v, want the prototype refused for %s", err, field)
			}
		})
	}
}

// An optional prototype skips a marked source whose slot cannot be cut.
func TestOptionalSourceSlotSkipsASourceItCannotCut(t *testing.T) {
	f := newSourceSlotFixture(t)
	f.source("angee-arp", true)
	f.stack.Sources["gone"] = manifest.Source{Kind: "git", Repo: filepath.Join(f.base, "remotes", "gone.git"), DefaultRef: "main", Slot: true}
	f.save()
	template := f.template("src", sourceSlotPrototype+"    optional: true\n")
	if _, err := f.platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	if got := f.slots("a"); len(got) != 1 || got["angee-arp"].Source != "angee-arp" {
		t.Fatalf("recorded slots = %+v, want the reachable source's alone", got)
	}
}
