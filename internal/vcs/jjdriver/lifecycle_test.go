package jjdriver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// newPrimary is a source's cache as phase 6 will hand it to Add: a git clone
// that EnsurePrimary has colocated with jj, with no slot yet.
func newPrimary(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, primaryOnly)
	if err := driver().EnsurePrimary(context.Background(), f.primary); err != nil {
		t.Fatalf("EnsurePrimary() error = %v", err)
	}
	return f
}

// slotSpec is the jj slot at rel under the fixture's root, named name, on
// branch, based on main on the primary's remote.
func (f *fixture) slotSpec(rel, name, branch string) vcs.Slot {
	return vcs.Slot{
		Path:       filepath.Join(f.root, filepath.FromSlash(rel)),
		Name:       name,
		Primary:    f.primary,
		Branch:     branch,
		BaseRef:    "main",
		BaseRemote: f.remoteName,
	}
}

// dest opens the guard the service opens on s.Path, rooted at the fixture's
// root as the service's is at the stack root.
func (f *fixture) dest(s vcs.Slot) *copierx.GuardedPath {
	f.t.Helper()
	rel, err := filepath.Rel(f.root, s.Path)
	if err != nil {
		f.t.Fatalf("Rel() error = %v", err)
	}
	guard, err := copierx.OpenGuardedPath(f.root, f.root, filepath.ToSlash(rel), nil)
	if err != nil {
		f.t.Fatalf("OpenGuardedPath(%s) error = %v", rel, err)
	}
	f.t.Cleanup(func() { _ = guard.Close() })
	return guard
}

// add makes s through a fresh guard.
func (f *fixture) add(s vcs.Slot) error {
	return driver().Add(context.Background(), s, f.dest(s), nil)
}

// workspaceNames lists the primary's workspaces as the test's own jj reads
// them.
func (f *fixture) workspaceNames() []string {
	f.t.Helper()
	out := f.jj(f.primary, "--ignore-working-copy", "workspace", "list", "-T", workspacesTemplate)
	workspaces, err := decodeLines[workspace]([]byte(out))
	if err != nil {
		f.t.Fatalf("decode workspaces: %v", err)
	}
	names := make([]string, len(workspaces))
	for i, w := range workspaces {
		names[i] = w.Name
	}
	return names
}

// registrations lists the git worktrees the primary keeps besides itself, as
// `git worktree list --porcelain` prints them, prunable ones included.
func (f *fixture) registrations() []string {
	f.t.Helper()
	out := strings.TrimSpace(f.gitOut(f.primary, "worktree", "list", "--porcelain"))
	return strings.Split(out, "\n\n")[1:]
}

// opHead is the primary's latest operation.
func (f *fixture) opHead() string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(f.primary, "--ignore-working-copy", "op", "log", "--no-graph", "-n", "1", "-T", operationTemplate))
}

// gitOut runs git in dir and returns its standard output, trimmed.
func (f *fixture) gitOut(dir string, args ...string) string {
	f.t.Helper()
	return strings.TrimSpace(runTool(f.t, dir, nil, "git", args...))
}

// exists reports whether there is an entry at path, a symlink included.
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("Lstat(%s) error = %v", path, err)
	}
	return err == nil
}

// TestEnsurePrimary colocates a git clone with jj, leaving git on its branch,
// and does nothing once it is: the second call runs no jj at all. A directory
// that is not a git repository is refused, not turned into a new one.
func TestEnsurePrimary(t *testing.T) {
	f := newFixture(t, primaryOnly)
	ctx := context.Background()
	d := driver()

	plain, missing := filepath.Join(f.root, "plain"), filepath.Join(f.root, "missing")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	for _, path := range []string{plain, missing} {
		if err := d.EnsurePrimary(ctx, path); err == nil {
			t.Errorf("EnsurePrimary(%s) error = nil, want jj's refusal of a directory that is not a git repository", filepath.Base(path))
		}
		for _, made := range []string{".jj", ".git"} {
			if exists(t, filepath.Join(path, made)) {
				t.Errorf("EnsurePrimary(%s) made %s", filepath.Base(path), made)
			}
		}
	}

	if err := d.EnsurePrimary(ctx, f.primary); err != nil {
		t.Fatalf("EnsurePrimary() error = %v", err)
	}
	if status := f.jj(f.primary, "git", "colocation", "status"); !strings.Contains(status, "is currently colocated with Git") {
		t.Errorf("jj git colocation status = %q, want colocated", status)
	}
	if branch := f.gitOut(f.primary, "symbolic-ref", "--short", "HEAD"); branch != "main" {
		t.Errorf("the primary's git HEAD is %q, want main", branch)
	}

	op, path := f.opHead(), os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	if err := d.EnsurePrimary(ctx, f.primary); err != nil {
		t.Fatalf("EnsurePrimary() again, with no jj on PATH, error = %v; want nothing run", err)
	}
	t.Setenv("PATH", path)
	if got := f.opHead(); got != op {
		t.Errorf("EnsurePrimary() again recorded operation %s", got)
	}
}

// TestAddMakesAColocatedSlot makes a slot with no bookmark of its branch:
// colocated, so git works in it, on a detached HEAD at the base the cache's
// last git fetch brought, with no branch or bookmark created, and listed by
// the primary's git and jj alike.
func TestAddMakesAColocatedSlot(t *testing.T) {
	f := newPrimary(t)
	// The service refreshes the cache with git, not jj, after EnsurePrimary.
	f.write(f.seed, "g.txt", "two\n")
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-q", "-m", "two")
	f.git(f.seed, "push", "-q", "origin", "HEAD:main")
	f.git(f.primary, "fetch", "-q", f.remoteName)

	s := f.slotSpec("workspaces/ws/src", "stack--ws", "workspace/ws")
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if got := f.gitOut(s.Path, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Errorf("git rev-parse --is-inside-work-tree = %q, want true", got)
	}
	if got := f.gitOut(s.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Errorf("the slot's git HEAD is on %q, want detached", got)
	}
	if got, want := f.gitOut(s.Path, "rev-parse", "HEAD"), f.gitOut(f.primary, "rev-parse", f.remoteName+"/main"); got != want {
		t.Errorf("the slot is at %s, want the fetched base %s", got, want)
	}
	if got := f.gitOut(f.primary, "for-each-ref", "--format=%(refname)", "refs/heads"); got != "refs/heads/main" {
		t.Errorf("git branches = %q, want only main", got)
	}
	marks, err := decodeLines[bookmark]([]byte(f.jj(s.Path, "--ignore-working-copy", "bookmark", "list", "--all-remotes", "-T", bookmarksTemplate)))
	if err != nil {
		t.Fatalf("decode bookmarks: %v", err)
	}
	for _, m := range marks {
		if m.Name == s.Branch {
			t.Errorf("Add() created bookmark %+v", m)
		}
	}
	if names := f.workspaceNames(); !slices.Contains(names, s.Name) {
		t.Errorf("workspaces = %q, want %q among them", names, s.Name)
	}
	if regs := f.registrations(); len(regs) != 1 || strings.Contains(regs[0], "prunable") {
		t.Errorf("git worktrees besides the primary = %q, want the slot", regs)
	}
}

// TestAddOnAnExistingBookmark starts a slot whose branch the primary has as
// a local bookmark on it, still detached; a branch only a remote has does
// not count, and the slot starts at the base, as a git worktree slot would.
func TestAddOnAnExistingBookmark(t *testing.T) {
	f := newPrimary(t)
	f.git(f.seed, "checkout", "-q", "-b", "topic")
	f.write(f.seed, "t.txt", "topic\n")
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-q", "-m", "topic")
	f.git(f.seed, "push", "-q", "origin", "topic", "topic:remote-only")
	f.git(f.primary, "fetch", "-q", f.remoteName)
	f.git(f.primary, "branch", "-q", "topic", f.remoteName+"/topic")
	topic := f.gitOut(f.primary, "rev-parse", "topic")
	base := f.gitOut(f.primary, "rev-parse", f.remoteName+"/main")

	for branch, want := range map[string]string{"topic": topic, "remote-only": base} {
		s := f.slotSpec("workspaces/"+branch+"/src", "stack--"+branch, branch)
		if err := f.add(s); err != nil {
			t.Fatalf("Add(%s) error = %v", branch, err)
		}
		if got := f.gitOut(s.Path, "rev-parse", "HEAD"); got != want {
			t.Errorf("Add(%s) started at %s, want %s", branch, got, want)
		}
		if got := f.gitOut(s.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
			t.Errorf("Add(%s): git HEAD is on %q, want detached", branch, got)
		}
	}
	status, err := driver().Status(context.Background(), f.slotSpec("workspaces/topic/src", "stack--topic", "topic"))
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.CurrentRef != "topic" || !status.Pushed {
		t.Errorf("Status() = %+v, want on topic and pushed", status)
	}
}

// TestStatusReadsASlotAddMade reads a slot Add made with the existing Status:
// clean and pushed at the base, then dirty once a file is written.
func TestStatusReadsASlotAddMade(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	s := f.slotSpec("workspaces/ws/src", "stack--ws", "")
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	got, err := driver().Status(ctx, s)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	want := vcs.Status{CurrentRef: f.shortID(f.primary, f.base()), Pushed: true, ChangeID: f.changeID(s.Path, workingCopy)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Status() = %+v, want %+v", got, want)
	}

	f.write(s.Path, "new.txt", "unsaved\n")
	got, err = driver().Status(ctx, s)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !got.Dirty || got.Pushed || got.UnpushedReason != "uncommitted changes" {
		t.Errorf("Status() after an edit = %+v, want dirty and not pushed", got)
	}
}

// TestAddRefusesATakenName refuses a name the primary already has before
// anything is written: no directory, no operation, and the slot that has the
// name keeps it. jj would refuse the name too, but only after Add's rollback
// had forgotten the other slot's workspace.
func TestAddRefusesATakenName(t *testing.T) {
	f := newPrimary(t)
	first := f.slotSpec("workspaces/a/src", "stack--ws", "")
	if err := f.add(first); err != nil {
		t.Fatalf("Add(first) error = %v", err)
	}
	op, regs := f.opHead(), f.registrations()

	second := f.slotSpec("workspaces/b/src", "stack--ws", "")
	err := f.add(second)
	if !errors.Is(err, vcs.ErrNameTaken) || !strings.Contains(err.Error(), `"stack--ws"`) {
		t.Errorf("Add(second) error = %v, want vcs.ErrNameTaken naming the workspace", err)
	}
	if exists(t, filepath.Dir(second.Path)) {
		t.Errorf("Add(second) wrote %s", filepath.Dir(second.Path))
	}
	if got := f.opHead(); got != op {
		t.Errorf("Add(second) recorded operation %s", got)
	}
	if names := f.workspaceNames(); !slices.Contains(names, first.Name) {
		t.Errorf("workspaces = %q, want the first slot's kept", names)
	}
	if got := f.registrations(); !slices.Equal(got, regs) {
		t.Errorf("git worktrees = %q, want %q", got, regs)
	}
	if got := f.gitOut(first.Path, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Errorf("the first slot no longer works with git: %q", got)
	}
}

// TestAddRefusesAnOccupiedDestination refuses a non-empty directory and a
// file before anything is written, and takes an empty directory.
func TestAddRefusesAnOccupiedDestination(t *testing.T) {
	f := newPrimary(t)
	full := f.slotSpec("workspaces/full/src", "stack--full", "")
	file := f.slotSpec("workspaces/file/src", "stack--file", "")
	empty := f.slotSpec("workspaces/empty/src", "stack--empty", "")
	for _, dir := range []string{full.Path, filepath.Dir(file.Path), empty.Path} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	f.write(full.Path, "keep.txt", "keep\n")
	f.write(filepath.Dir(file.Path), "src", "a file\n")
	op := f.opHead()

	for s, want := range map[*vcs.Slot]string{&full: "not empty", &file: "not a directory"} {
		if err := f.add(*s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Add(%s) error = %v, want %q", s.Name, err, want)
		}
	}
	if got, err := os.ReadFile(filepath.Join(full.Path, "keep.txt")); err != nil || string(got) != "keep\n" {
		t.Errorf("keep.txt = %q, %v; want it untouched", got, err)
	}
	if got, err := os.ReadFile(file.Path); err != nil || string(got) != "a file\n" {
		t.Errorf("the file at the slot path = %q, %v; want it untouched", got, err)
	}
	if got := f.opHead(); got != op {
		t.Errorf("a refused Add recorded operation %s", got)
	}
	if err := f.add(empty); err != nil {
		t.Fatalf("Add(empty directory) error = %v", err)
	}
	if names := f.workspaceNames(); len(names) != 2 || !slices.Contains(names, empty.Name) {
		t.Errorf("workspaces = %q, want default and %s", names, empty.Name)
	}
}

// TestAddRollsBackWhenTheBaseResolvesToNothing: jj creates the workspace and
// then fails on a revision that resolves to nothing, and Add leaves no
// workspace, no directory and no git worktree registration behind.
func TestAddRollsBackWhenTheBaseResolvesToNothing(t *testing.T) {
	f := newPrimary(t)
	s := f.slotSpec("workspaces/ws/src", "stack--ws", "")
	s.BaseRef = "no-such-base"
	err := f.add(s)
	var command *CommandError
	if !errors.As(err, &command) {
		t.Fatalf("Add() error = %v, want jj's failure", err)
	}
	if names := f.workspaceNames(); slices.Contains(names, s.Name) {
		t.Errorf("workspaces = %q, want %s forgotten", names, s.Name)
	}
	if exists(t, s.Path) {
		t.Errorf("Add() left %s", s.Path)
	}
	if regs := f.registrations(); len(regs) != 0 {
		t.Errorf("git worktrees besides the primary = %q, want none", regs)
	}
}

// swapParent is the guard the service opened, swapping the slot's parent for
// a symlink to outside as soon as Add's first identity check has passed, so
// jj runs through the symlink (plan D4).
type swapParent struct {
	*copierx.GuardedPath
	t               *testing.T
	parent, outside string
	swapped         bool
}

func (d *swapParent) VerifyPathIdentity(path string) error {
	if err := d.GuardedPath.VerifyPathIdentity(path); err != nil || d.swapped {
		return err
	}
	d.swapped = true
	if err := os.Rename(d.parent, d.parent+"-moved"); err != nil {
		d.t.Fatalf("Rename() error = %v", err)
	}
	if err := os.Symlink(d.outside, d.parent); err != nil {
		d.t.Fatalf("Symlink() error = %v", err)
	}
	return nil
}

// TestAddDetectsAParentSwappedForASymlink: the parent swapped between the
// checks before and after jj runs is detected, the workspace is forgotten and
// its registration dropped, and the directory Add made is removed where it
// is, never through the symlink. Where the symlink leads, jj's forget deletes
// the .git jj wrote, and the rest of what jj wrote is left there.
func TestAddDetectsAParentSwappedForASymlink(t *testing.T) {
	f := newPrimary(t)
	parent := filepath.Join(f.root, "workspaces", "ws")
	outside := filepath.Join(f.root, "outside")
	for _, dir := range []string{parent, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	s := f.slotSpec("workspaces/ws/src", "stack--ws", "")
	dest := &swapParent{GuardedPath: f.dest(s), t: t, parent: parent, outside: outside}
	err := driver().Add(context.Background(), s, dest, nil)
	if !dest.swapped {
		t.Fatalf("the parent was never swapped")
	}
	if err == nil || !strings.Contains(err.Error(), "changed while jj created it") {
		t.Fatalf("Add() error = %v, want the swap detected", err)
	}
	if names := f.workspaceNames(); slices.Contains(names, s.Name) {
		t.Errorf("workspaces = %q, want %s forgotten", names, s.Name)
	}
	if regs := f.registrations(); len(regs) != 0 {
		t.Errorf("git worktrees besides the primary = %q, want none", regs)
	}
	if exists(t, filepath.Join(parent+"-moved", "src")) {
		t.Errorf("the directory Add made is still there")
	}
	if !exists(t, filepath.Join(outside, "src", "f.txt")) {
		t.Errorf("Add() removed what jj wrote through the symlink")
	}
	if exists(t, filepath.Join(outside, "src", ".git")) {
		t.Errorf("the forget left the .git jj wrote through the symlink")
	}
}

// recordingGit is a git client whose git appends each command to log, then
// runs the git on PATH.
func recordingGit(t *testing.T, log string) git.Client {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git) error = %v", err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	script := filepath.Join(t.TempDir(), "git")
	body := "#!/bin/sh\nprintf 'git %s\\n' \"$*\" >> " + quote(log) + "\nexec " + quote(real) + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return git.Client{Bin: script}
}

// orderDest is the service's guard, recording in log when the slot is
// removed through it, and whether the primary still listed the workspace
// then.
type orderDest struct {
	*copierx.GuardedPath
	f    *fixture
	name string
	log  string
}

func (d *orderDest) RemoveAll() error {
	line := "remove: forgotten\n"
	if slices.Contains(d.f.workspaceNames(), d.name) {
		line = "remove: still listed\n"
	}
	file, err := os.OpenFile(d.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		d.f.t.Fatalf("OpenFile() error = %v", err)
	}
	_, err = file.WriteString(line)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		d.f.t.Fatalf("write %s: %v", d.log, err)
	}
	return d.GuardedPath.RemoveAll()
}

// TestRemove forgets the workspace, then removes the directory, then prunes
// the primary's git worktrees (plan section 2), leaving no workspace and no
// registration.
func TestRemove(t *testing.T) {
	f := newPrimary(t)
	s := f.slotSpec("workspaces/ws/src", "stack--ws", "")
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	log := filepath.Join(t.TempDir(), "order.log")
	dest := &orderDest{GuardedPath: f.dest(s), f: f, name: s.Name, log: log}
	if err := New(recordingGit(t, log)).Remove(context.Background(), s, dest); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if names := f.workspaceNames(); slices.Contains(names, s.Name) {
		t.Errorf("workspaces = %q, want %s forgotten", names, s.Name)
	}
	if exists(t, s.Path) {
		t.Errorf("Remove() left %s", s.Path)
	}
	if regs := f.registrations(); len(regs) != 0 {
		t.Errorf("git worktrees besides the primary = %q, want none", regs)
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	removed := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "remove: ") })
	pruned := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "worktree prune") })
	if removed < 0 || lines[removed] != "remove: forgotten" || pruned < removed {
		t.Errorf("Remove() ran %q; want the forget before the removal and the prune after it", lines)
	}
}

// TestRemoveAfterTheDirectoryIsGone forgets a slot whose directory someone
// already deleted, and the prune drops the registration the forget could not.
// A second Remove changes nothing.
func TestRemoveAfterTheDirectoryIsGone(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	s := f.slotSpec("workspaces/ws/src", "stack--ws", "")
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := os.RemoveAll(s.Path); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	if regs := f.registrations(); len(regs) != 1 || !strings.Contains(regs[0], "prunable") {
		t.Fatalf("git worktrees besides the primary = %q, want the slot's, prunable", regs)
	}
	for range 2 {
		if err := driver().Remove(ctx, s, f.dest(s)); err != nil {
			t.Fatalf("Remove() error = %v", err)
		}
		if names := f.workspaceNames(); slices.Contains(names, s.Name) {
			t.Errorf("workspaces = %q, want %s forgotten", names, s.Name)
		}
		if regs := f.registrations(); len(regs) != 0 {
			t.Errorf("git worktrees besides the primary = %q, want none", regs)
		}
	}
}

// TestExists reports a workspace of the slot's name, by name, and nothing
// for another name or once the slot is removed.
func TestExists(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	d := driver()
	s := f.slotSpec("workspaces/ws/src", "stack--ws", "")
	other := s
	other.Name = "stack--other"
	check := func(when string, slot vcs.Slot, want bool) {
		t.Helper()
		if got, err := d.Exists(ctx, slot); got != want || err != nil {
			t.Errorf("Exists(%s) %s = %v, %v; want %v", slot.Name, when, got, err, want)
		}
	}
	check("before Add", s, false)
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	check("after Add", s, true)
	check("after Add", other, false)
	if err := d.Remove(ctx, s, f.dest(s)); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	check("after Remove", s, false)
}

// TestLifecycleHostileNames makes, finds and removes slots whose workspace
// names a revset, a flag parser or a shell would misread: each reaches jj
// as one argument and comes back as it went in.
func TestLifecycleHostileNames(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	d := driver()
	for i, name := range []string{`we"ird \..@ws`, "-stack--ws", "with space", "ünï--wörk", `x" | all() | "`} {
		s := f.slotSpec(filepath.Join("workspaces", string(rune('a'+i)), "src"), name, "")
		if err := f.add(s); err != nil {
			t.Fatalf("Add(%q) error = %v", name, err)
		}
		if names := f.workspaceNames(); !slices.Contains(names, name) {
			t.Errorf("workspaces = %q, want %q among them", names, name)
		}
		if ok, err := d.Exists(ctx, s); !ok || err != nil {
			t.Errorf("Exists(%q) = %v, %v; want true", name, ok, err)
		}
		if err := d.Remove(ctx, s, f.dest(s)); err != nil {
			t.Fatalf("Remove(%q) error = %v", name, err)
		}
		if ok, err := d.Exists(ctx, s); ok || err != nil {
			t.Errorf("Exists(%q) after Remove = %v, %v; want false", name, ok, err)
		}
	}
	if regs := f.registrations(); len(regs) != 0 {
		t.Errorf("git worktrees besides the primary = %q, want none", regs)
	}
}

// TestLifecycleRefusesMissingFacts refuses, before running anything, a slot
// with no primary or no name, a relative path or no base for Add, and a
// configure step, which a jj slot has no use for.
func TestLifecycleRefusesMissingFacts(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n"})
	ctx := context.Background()
	d := driver()
	full := vcs.Slot{Path: filepath.Join(t.TempDir(), "slot"), Name: "stack--ws", Primary: t.TempDir(), BaseRef: "main"}
	without := func(edit func(*vcs.Slot)) vcs.Slot {
		s := full
		edit(&s)
		return s
	}
	noName := without(func(s *vcs.Slot) { s.Name = "" })
	noPrimary := without(func(s *vcs.Slot) { s.Primary = "" })
	configure := func(context.Context, string) error { return nil }
	for name, tc := range map[string]struct {
		call func() error
		is   error
	}{
		"EnsurePrimary, no primary": {call: func() error { return d.EnsurePrimary(ctx, "") }},
		"Exists, no name":           {call: func() error { _, err := d.Exists(ctx, noName); return err }},
		"Exists, no primary":        {call: func() error { _, err := d.Exists(ctx, noPrimary); return err }},
		"Add, no name":              {call: func() error { return d.Add(ctx, noName, nil, nil) }},
		"Add, no primary":           {call: func() error { return d.Add(ctx, noPrimary, nil, nil) }},
		"Add, relative path": {call: func() error {
			return d.Add(ctx, without(func(s *vcs.Slot) { s.Path = "slot" }), nil, nil)
		}},
		"Add, no base": {call: func() error {
			return d.Add(ctx, without(func(s *vcs.Slot) { s.BaseRef = "" }), nil, nil)
		}, is: vcs.ErrBaseNotFound},
		"Add, configure":     {call: func() error { return d.Add(ctx, full, nil, configure) }, is: vcs.ErrUnsupported},
		"Remove, no name":    {call: func() error { return d.Remove(ctx, noName, nil) }},
		"Remove, no primary": {call: func() error { return d.Remove(ctx, noPrimary, nil) }},
	} {
		err := tc.call()
		if err == nil || (tc.is != nil && !errors.Is(err, tc.is)) {
			t.Errorf("%s: error = %v, want a refusal (%v)", name, err, tc.is)
		}
	}
	if runs := fakeRuns(t, home); len(runs) != 0 {
		t.Fatalf("jj or git ran %d times, want none: %v", len(runs), runs)
	}
}

// TestLifecycleCommands pins, through a fake jj and git, the commands the
// lifecycle runs and where: `jj git init` outside any repository; the
// workspace list and the forget on the primary's repository without a
// working copy; `jj workspace add` in the primary, with the name, the start
// revset and the path as single arguments; and git's prune in the primary,
// before the add and after the removal. The primary has a .git, as a
// source's cache has, so the slot is colocated.
func TestLifecycleCommands(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n"})
	ctx := context.Background()
	d := driver()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	if err := os.MkdirAll(filepath.Join(primary, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	s := vcs.Slot{
		Path: filepath.Join(root, "ws", "src"), Name: "stack--ws", Primary: primary,
		Branch: "workspace/ws", BaseRef: "main", BaseRemote: "upstream",
	}
	dest, err := copierx.OpenGuardedPath(root, root, "ws/src", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath() error = %v", err)
	}
	defer func() { _ = dest.Close() }()
	if err := d.EnsurePrimary(ctx, primary); err != nil {
		t.Fatalf("EnsurePrimary() error = %v", err)
	}
	if err := d.Add(ctx, s, dest, nil); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := d.Remove(ctx, s, dest); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	jj := func(args ...string) []string { return append(slices.Clone(globalArgs), args...) }
	start := `coalesce(bookmarks(exact:"workspace/ws"), remote_bookmarks(exact:"main", remote=exact:"upstream"), bookmarks(exact:"main"))`
	want := []struct {
		tool, dir string
		args      []string
	}{
		{"jj", os.TempDir(), jj("git", "init", "--git-repo", primary, "--", primary)},
		{"jj", primary, jj("-R", ".", "--ignore-working-copy", "workspace", "list", "-T", workspacesTemplate)},
		{"git", primary, []string{"worktree", "prune"}},
		{"jj", primary, jj("-R", ".", "workspace", "add", "--colocate", "--name=stack--ws", "-r", start, "--", s.Path)},
		{"jj", primary, jj("-R", ".", "--ignore-working-copy", "workspace", "forget", "--", "stack--ws")},
		{"git", primary, []string{"worktree", "prune"}},
	}
	runs := fakeRuns(t, home)
	if len(runs) != len(want) {
		t.Fatalf("ran %d commands, want %d: %v", len(runs), len(want), runs)
	}
	for i, w := range want {
		run := runs[i]
		gotDir, _ := filepath.EvalSymlinks(run.Dir)
		wantDir, _ := filepath.EvalSymlinks(w.dir)
		if run.Tool != w.tool || !slices.Equal(run.Args, w.args) || gotDir != wantDir {
			t.Errorf("run %d = %s %q in %s, want %s %q in %s", i, run.Tool, run.Args, run.Dir, w.tool, w.args, w.dir)
		}
	}
}

// fakeSlot is a jj slot of a primary directory holding only an empty .git,
// for the fake jj and git, and the guard the service opens on its path. The
// path holds nothing, or, with files, a directory holding those, as Remove
// finds a slot.
func fakeSlot(t *testing.T, files ...string) (vcs.Slot, *copierx.GuardedPath) {
	t.Helper()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	if err := os.MkdirAll(filepath.Join(primary, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	s := vcs.Slot{Path: filepath.Join(root, "ws", "src"), Name: "stack--ws", Primary: primary, BaseRef: "main"}
	if len(files) > 0 {
		if err := os.MkdirAll(s.Path, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(s.Path, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	dest, err := copierx.OpenGuardedPath(root, root, "ws/src", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath() error = %v", err)
	}
	t.Cleanup(func() { _ = dest.Close() })
	return s, dest
}

// commandLines is each run of the fake tools in home as one line: the tool
// and its arguments.
func commandLines(t *testing.T, home string) []string {
	t.Helper()
	runs := fakeRuns(t, home)
	lines := make([]string, len(runs))
	for i, run := range runs {
		lines[i] = run.Tool + " " + strings.Join(run.Args, " ")
	}
	return lines
}

// lineWith is the index of the first of lines that contains s, else -1.
func lineWith(lines []string, s string) int {
	return slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, s) })
}

// lastLineWith is the index of the last of lines that contains s, else -1.
func lastLineWith(lines []string, s string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], s) {
			return i
		}
	}
	return -1
}

// failedJJ reports whether err holds a failed jj command with arg among its
// arguments, and failedGit a failed git command.
func failedJJ(err error, arg string) bool {
	var command *CommandError
	return errors.As(err, &command) && slices.Contains(command.Args, arg)
}

func failedGit(err error, arg string) bool {
	var command *git.CommandError
	return errors.As(err, &command) && slices.Contains(command.Args, arg)
}

// TestRemoveGoesOnWhenAStepFails: a failed forget still lets Remove remove
// the directory and prune, and a failed prune after it is reported with it,
// joined.
func TestRemoveGoesOnWhenAStepFails(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n", "fail-on": "forget\nprune\n"})
	s, dest := fakeSlot(t, "f.txt")
	err := driver().Remove(context.Background(), s, dest)
	if !failedJJ(err, "forget") || !failedGit(err, "prune") {
		t.Errorf("Remove() error = %v, want the forget's and the prune's failures joined", err)
	}
	if exists(t, s.Path) {
		t.Errorf("Remove() left %s after the forget failed", s.Path)
	}
	lines := commandLines(t, home)
	if forget, prune := lineWith(lines, "workspace forget"), lineWith(lines, "worktree prune"); forget < 0 || prune < forget {
		t.Errorf("Remove() ran %q; want the forget, then the prune", lines)
	}
}

// cancelDest is the service's guard, cancelling the caller's context once
// Add's first identity check has passed. jj's add is then never started (Go
// refuses to start a command under a cancelled context), so the test pins
// that the rollback runs on a context of its own, not that jj is stopped
// mid-run.
type cancelDest struct {
	*copierx.GuardedPath
	cancel context.CancelFunc
}

func (d *cancelDest) VerifyPathIdentity(path string) error {
	err := d.GuardedPath.VerifyPathIdentity(path)
	d.cancel()
	return err
}

// TestAddRollsBackOnItsOwnContext: when the caller's context is what failed
// the add, the rollback still forgets the workspace, removes the directory
// and prunes, on a context of its own.
func TestAddRollsBackOnItsOwnContext(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n"})
	s, guard := fakeSlot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := driver().Add(ctx, s, &cancelDest{GuardedPath: guard, cancel: cancel}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Add() error = %v, want the caller's cancellation", err)
	}
	if strings.Contains(err.Error(), "roll back") {
		t.Errorf("Add() error = %v, want the rollback to succeed", err)
	}
	lines := commandLines(t, home)
	if forget, prune := lineWith(lines, "workspace forget"), lastLineWith(lines, "worktree prune"); forget < 0 || prune < forget {
		t.Errorf("Add() ran %q; want the rollback's forget, then its prune", lines)
	}
	if exists(t, s.Path) {
		t.Errorf("Add() left %s", s.Path)
	}
}

// TestAddReportsAFailedRollback: a rollback that fails too is reported with
// the failure it rolled back, joined, and still runs every step.
func TestAddReportsAFailedRollback(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n", "fail-on": "add\nprune\n"})
	s, dest := fakeSlot(t)
	err := driver().Add(context.Background(), s, dest, nil)
	if !failedJJ(err, "add") || !failedGit(err, "prune") || !strings.Contains(err.Error(), "roll back jj slot "+s.Path) {
		t.Errorf("Add() error = %v, want the add's failure joined with the rollback's", err)
	}
	if lines := commandLines(t, home); lineWith(lines, "workspace forget") < 0 {
		t.Errorf("Add() ran %q; want the rollback's forget", lines)
	}
	if exists(t, s.Path) {
		t.Errorf("Add() left %s", s.Path)
	}
}
