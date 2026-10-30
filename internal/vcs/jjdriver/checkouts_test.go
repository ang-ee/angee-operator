package jjdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// TestStoreWithoutGit makes, reads and removes a slot of a jj store that is
// not colocated with git, as a `kind: local` store can be: the slot gets no
// .git, Status reads it without asking git for a push remote, and Remove
// runs no git prune. The store sits inside a git repository with a prunable
// worktree, which git run in the store or the slot would find and prune.
func TestStoreWithoutGit(t *testing.T) {
	isolate(t)
	requireJJ(t)
	ctx := context.Background()
	root := t.TempDir()
	f := &fixture{t: t, root: root}
	f.git(root, "init", "-q", "-b", "main")
	f.write(root, "outer.txt", "outer\n")
	f.git(root, "add", ".")
	f.git(root, "commit", "-q", "-m", "outer")
	f.git(root, "worktree", "add", "-q", "--detach", filepath.Join(root, "gone"), "main")
	if err := os.RemoveAll(filepath.Join(root, "gone")); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	enclosing := f.gitOut(root, "worktree", "list", "--porcelain")

	f.primary = filepath.Join(root, "store")
	f.jj(root, "git", "init", "--no-colocate", f.primary)
	f.write(f.primary, "f.txt", "one\n")
	f.jj(f.primary, "commit", "-m", "base")
	f.jj(f.primary, "bookmark", "create", "main", "-r", "@-")

	log := filepath.Join(t.TempDir(), "git.log")
	d := New(recordingGit(t, log))
	s := vcs.Slot{Path: filepath.Join(root, "workspaces", "ws", "store"), Name: "stack--ws", Primary: f.primary, Branch: "topic", BaseRef: "main"}
	if err := d.Add(ctx, s, f.dest(s), nil); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if !exists(t, filepath.Join(s.Path, ".jj")) || exists(t, filepath.Join(s.Path, ".git")) {
		t.Fatalf("Add() made a slot with .jj %v and .git %v, want a jj workspace without git", exists(t, filepath.Join(s.Path, ".jj")), exists(t, filepath.Join(s.Path, ".git")))
	}
	status, err := d.Status(ctx, s)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Dirty || !status.Pushed || status.Upstream != "" || status.CurrentRef != f.shortID(f.primary, localBookmark("main")) {
		t.Errorf("Status() = %+v, want clean and pushed at main, with no upstream", status)
	}
	f.write(s.Path, "new.txt", "unsaved\n")
	if status, err := d.Status(ctx, s); err != nil || !status.Dirty {
		t.Errorf("Status() after an edit = %+v, %v; want dirty", status, err)
	}

	if err := d.Remove(ctx, s, f.dest(s)); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if exists(t, s.Path) {
		t.Errorf("Remove() left %s", s.Path)
	}
	if ok, err := d.Exists(ctx, s); ok || err != nil {
		t.Errorf("Exists() after Remove = %v, %v; want false", ok, err)
	}
	if got := f.gitOut(root, "worktree", "list", "--porcelain"); got != enclosing {
		t.Errorf("the enclosing repository's worktrees changed:\n%s\nwant:\n%s", got, enclosing)
	}
	if ran, err := os.ReadFile(log); err == nil && len(ran) > 0 {
		t.Errorf("the driver ran git for a store without git:\n%s", ran)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile() error = %v", err)
	}
}

// TestLifecycleCommandsWithoutGit pins, through a fake jj and git, that a
// slot of a primary with no .git is added without --colocate and removed
// without a git prune.
func TestLifecycleCommandsWithoutGit(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n"})
	ctx := context.Background()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	if err := os.Mkdir(primary, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	s := vcs.Slot{Path: filepath.Join(root, "ws", "src"), Name: "stack--ws", Primary: primary, BaseRef: "main"}
	dest, err := copierx.OpenGuardedPath(root, root, "ws/src", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath() error = %v", err)
	}
	defer func() { _ = dest.Close() }()
	if err := driver().Add(ctx, s, dest, nil); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := driver().Remove(ctx, s, dest); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	lines := commandLines(t, home)
	if add := lineWith(lines, "workspace add"); add < 0 || strings.Contains(lines[add], "--colocate") {
		t.Errorf("ran %q; want `jj workspace add` without --colocate", lines)
	}
	if lineWith(lines, "git ") >= 0 {
		t.Errorf("ran %q; want no git", lines)
	}
}

// TestHolds tells a slot's own checkout from everything else at a path: the
// primary, another workspace's checkout, a directory that is not a jj
// workspace, and another primary's checkout of a workspace with the same
// name.
func TestHolds(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	d := driver()
	a := f.slotSpec("workspaces/a/src", "stack--a", "")
	b := f.slotSpec("workspaces/b/src", "stack--b", "")
	for _, s := range []vcs.Slot{a, b} {
		if err := f.add(s); err != nil {
			t.Fatalf("Add(%s) error = %v", s.Name, err)
		}
	}
	plain, empty, broken := filepath.Join(f.root, "plain"), filepath.Join(f.root, "empty"), filepath.Join(f.root, "broken")
	for _, dir := range []string{plain, empty, filepath.Join(broken, ".jj")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	f.write(plain, "f.txt", "plain\n")

	other := newPrimary(t)
	twin := other.slotSpec("workspaces/a/src", "stack--a", "")
	if err := other.add(twin); err != nil {
		t.Fatalf("Add(twin) error = %v", err)
	}

	for _, tc := range []struct {
		name string
		s    vcs.Slot
		dir  string
		want bool
	}{
		{"its own checkout", a, a.Path, true},
		{"another workspace's checkout", a, b.Path, false},
		{"the primary", a, f.primary, false},
		{"a plain directory", a, plain, false},
		{"an empty directory", a, empty, false},
		{"a .jj jj cannot load", a, broken, false},
		{"a missing path", a, filepath.Join(f.root, "missing"), false},
		{"another primary's checkout of the same name", a, twin.Path, false},
	} {
		if got, err := d.Holds(ctx, tc.s, tc.dir); got != tc.want || err != nil {
			t.Errorf("Holds(%s, %s) = %v, %v; want %v", tc.s.Name, tc.name, got, err, tc.want)
		}
	}
}

// TestCheckoutAt reads the working-copy commit jj loads at a directory: a
// slot's, and the primary's own, each with the workspaces whose commit it
// is, two once one workspace has `jj edit`ed the other's; and nothing for a
// directory that is no workspace, a .jj jj cannot load, a missing path or a
// link to a checkout.
func TestCheckoutAt(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	d := driver()
	a := f.slotSpec("workspaces/a/src", "stack--a", "")
	b := f.slotSpec("workspaces/b/src", "stack--b", "")
	for _, s := range []vcs.Slot{a, b} {
		if err := f.add(s); err != nil {
			t.Fatalf("Add(%s) error = %v", s.Name, err)
		}
	}
	plain, broken, link := filepath.Join(f.root, "plain"), filepath.Join(f.root, "broken"), filepath.Join(f.root, "link")
	for _, dir := range []string{plain, filepath.Join(broken, ".jj")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	if err := os.Symlink(a.Path, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	check := func(name, dir string, want Checkout) {
		t.Helper()
		got, err := d.CheckoutAt(ctx, dir)
		if err != nil || got.ChangeID != want.ChangeID || !slices.Equal(got.Workspaces, want.Workspaces) {
			t.Errorf("CheckoutAt(%s) = %+v, %v; want %+v", name, got, err, want)
		}
	}
	check("a slot", a.Path, Checkout{ChangeID: f.changeID(a.Path, workingCopy), Workspaces: []string{a.Name}})
	check("the primary", f.primary, Checkout{ChangeID: f.changeID(f.primary, workingCopy), Workspaces: []string{"default"}})
	for name, dir := range map[string]string{"a plain directory": plain, "a .jj jj cannot load": broken, "a missing path": filepath.Join(f.root, "missing"), "a link to a slot": link} {
		check(name, dir, Checkout{})
	}
	shared := f.changeID(a.Path, workingCopy)
	f.jj(b.Path, "edit", shared)
	got, err := d.CheckoutAt(ctx, a.Path)
	if want := []string{a.Name, b.Name}; err != nil || got.ChangeID != shared || !slices.Equal(slices.Sorted(slices.Values(got.Workspaces)), want) {
		t.Errorf("CheckoutAt(a slot whose commit b edits) = %+v, %v; want %s held by %q", got, err, shared, want)
	}
	if _, err := d.CheckoutAt(ctx, ""); !errors.Is(err, errNoSlot) {
		t.Errorf("CheckoutAt(\"\") error = %v, want a refusal", err)
	}
}

// TestElsewhere fails closed: it finds a name's checkout at another path,
// even one jj cannot load there, and nothing when the name is free or its
// checkout is the slot's own path. A record jj cannot resolve, because the
// checkout is gone or out of sight, is ErrRootUnresolved, never "free".
func TestElsewhere(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	d := driver()
	live := f.slotSpec("workspaces/a/src", "stack--ws", "")
	unreadable := f.slotSpec("workspaces/u/src", "stack--u", "")
	for _, s := range []vcs.Slot{live, unreadable} {
		if err := f.add(s); err != nil {
			t.Fatalf("Add(%s) error = %v", s.Name, err)
		}
	}
	moved := f.slotSpec("workspaces/b/src", "stack--ws", "")
	free := f.slotSpec("workspaces/c/src", "stack--free", "")

	if got, err := d.Elsewhere(ctx, moved); err != nil || !samePath(got, live.Path) {
		t.Errorf("Elsewhere(another path) = %q, %v; want %s", got, err, live.Path)
	}
	for name, s := range map[string]vcs.Slot{"its own path": live, "a free name": free} {
		if got, err := d.Elsewhere(ctx, s); got != "" || err != nil {
			t.Errorf("Elsewhere(%s) = %q, %v; want none", name, got, err)
		}
	}
	jjDir := filepath.Join(unreadable.Path, ".jj")
	if err := os.Chmod(jjDir, 0); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(jjDir, 0o755) })
	elsewhere := unreadable
	elsewhere.Path = filepath.Join(f.root, "workspaces", "v", "src")
	if got, err := d.Elsewhere(ctx, elsewhere); err != nil || !samePath(got, unreadable.Path) {
		t.Errorf("Elsewhere() of a checkout jj cannot load = %q, %v; want %s", got, err, unreadable.Path)
	}
	if err := os.RemoveAll(live.Path); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	if got, err := d.Elsewhere(ctx, moved); got != "" || !errors.Is(err, ErrRootUnresolved) {
		t.Errorf("Elsewhere() once the checkout is gone = %q, %v; want ErrRootUnresolved", got, err)
	}
}

// TestForgetName forgets a name whose checkout is gone and changes nothing
// git keeps: the slot's own registration and an unrelated prunable one both
// stay for the next prune, which is why it needs no lock. While the
// checkout exists it refuses and runs nothing.
func TestForgetName(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	d := driver()
	s := f.slotSpec("workspaces/a/src", "stack--a", "")
	unrelated := f.slotSpec("workspaces/b/src", "stack--b", "")
	for _, slot := range []vcs.Slot{s, unrelated} {
		if err := f.add(slot); err != nil {
			t.Fatalf("Add(%s) error = %v", slot.Name, err)
		}
	}
	change, err := d.HeldChange(ctx, s, s.Path)
	if err != nil || change == "" {
		t.Fatalf("HeldChange() = %q, %v; want the slot's change", change, err)
	}
	op := f.opHead()
	if err := d.ForgetName(ctx, s, change); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Errorf("ForgetName() with the checkout in place error = %v, want a refusal", err)
	}
	if got := f.opHead(); got != op {
		t.Errorf("a refused ForgetName recorded operation %s", got)
	}

	for _, slot := range []vcs.Slot{s, unrelated} {
		if err := os.RemoveAll(slot.Path); err != nil {
			t.Fatalf("RemoveAll() error = %v", err)
		}
	}
	regs := f.registrations()
	if len(regs) != 2 {
		t.Fatalf("git worktrees besides the primary = %q, want both slots', prunable", regs)
	}
	if err := d.ForgetName(ctx, s, change); err != nil {
		t.Fatalf("ForgetName() error = %v", err)
	}
	if names := f.workspaceNames(); slices.Contains(names, s.Name) || !slices.Contains(names, unrelated.Name) {
		t.Errorf("workspaces = %q, want %s forgotten and %s kept", names, s.Name, unrelated.Name)
	}
	if got := f.registrations(); !slices.Equal(got, regs) {
		t.Errorf("git worktrees = %q, want them as they were: %q", got, regs)
	}
	if err := d.ForgetName(ctx, s, change); err != nil {
		t.Errorf("ForgetName() of a name already forgotten error = %v, want nothing to do", err)
	}

	// A workspace of the same name added meanwhile, as the lock's holder
	// could, has another change, and is left alone.
	again := f.slotSpec("workspaces/c/src", s.Name, "")
	if err := f.add(again); err != nil {
		t.Fatalf("Add(again) error = %v", err)
	}
	if err := d.ForgetName(ctx, s, change); err == nil || !strings.Contains(err.Error(), "no longer the slot") {
		t.Errorf("ForgetName() of a name re-added elsewhere error = %v, want a refusal", err)
	}
	if names := f.workspaceNames(); !slices.Contains(names, s.Name) {
		t.Errorf("workspaces = %q, want the re-added %s kept", names, s.Name)
	}
}

// TestAddOverAStaleRegistration adds a slot again at a path git still
// registers a worktree for, as a slot removed without the primary's lock and
// then forgotten leaves it: git refuses such an add, so Add prunes first.
func TestAddOverAStaleRegistration(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	s := f.slotSpec("workspaces/ws/src", "stack--ws", "")
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	change, err := driver().HeldChange(ctx, s, s.Path)
	if err != nil {
		t.Fatalf("HeldChange() error = %v", err)
	}
	if err := os.RemoveAll(s.Path); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	if err := driver().ForgetName(ctx, s, change); err != nil {
		t.Fatalf("ForgetName() error = %v", err)
	}
	if regs := f.registrations(); len(regs) != 1 || !strings.Contains(regs[0], "prunable") {
		t.Fatalf("git worktrees besides the primary = %q, want the removed slot's, prunable", regs)
	}
	if err := f.add(s); err != nil {
		t.Fatalf("Add() again at the same path error = %v", err)
	}
	if regs := f.registrations(); len(regs) != 1 || strings.Contains(regs[0], "prunable") {
		t.Errorf("git worktrees besides the primary = %q, want the new slot's alone", regs)
	}
	if got := f.gitOut(s.Path, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Errorf("git rev-parse --is-inside-work-tree in the new slot = %q, want true", got)
	}
}

// TestAddLeavesOtherStaleRegistrations prunes nothing when git registers no
// worktree at the new slot's path: a prune would also drop registrations of
// checkouts this process cannot see, such as another stack's from inside a
// container.
func TestAddLeavesOtherStaleRegistrations(t *testing.T) {
	f := newPrimary(t)
	gone := f.slotSpec("workspaces/a/src", "stack--a", "")
	if err := f.add(gone); err != nil {
		t.Fatalf("Add(a) error = %v", err)
	}
	if err := os.RemoveAll(gone.Path); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	if err := f.add(f.slotSpec("workspaces/b/src", "stack--b", "")); err != nil {
		t.Fatalf("Add(b) error = %v", err)
	}
	if regs := f.registrations(); len(regs) != 2 || !slices.ContainsFunc(regs, func(r string) bool { return strings.Contains(r, "prunable") }) {
		t.Errorf("git worktrees = %q, want a's, still prunable, and b's", regs)
	}
}

// TestHeldChangeFollowsNoLink: a link to a slot's checkout is not that
// checkout, so a destroy never forgets a workspace through one.
func TestHeldChangeFollowsNoLink(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	s := f.slotSpec("workspaces/a/src", "stack--a", "")
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(s.Path, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if change, err := driver().HeldChange(ctx, s, s.Path); change == "" || err != nil {
		t.Fatalf("HeldChange(the checkout) = %q, %v; want its change", change, err)
	}
	if change, err := driver().HeldChange(ctx, s, link); change != "" || err != nil {
		t.Errorf("HeldChange(a link to it) = %q, %v; want none", change, err)
	}
}

// TestRecorded reads the path jj recorded for a workspace, also once nothing
// is at it, from jj's own error.
func TestRecorded(t *testing.T) {
	f := newPrimary(t)
	ctx := context.Background()
	s := f.slotSpec("workspaces/a/src", "stack--a", "")
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if path, resolved, err := driver().Recorded(ctx, s); err != nil || !resolved || !samePath(path, s.Path) {
		t.Errorf("Recorded() = %q, %v, %v; want %s, resolved", path, resolved, err, s.Path)
	}
	if err := os.RemoveAll(s.Path); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	if path, resolved, err := driver().Recorded(ctx, s); err != nil || resolved || !samePath(path, s.Path) {
		t.Errorf("Recorded() once gone = %q, %v, %v; want %s, not resolved", path, resolved, err, s.Path)
	}
	free := f.slotSpec("workspaces/b/src", "stack--free", "")
	if path, resolved, err := driver().Recorded(ctx, free); path != "" || resolved || err != nil {
		t.Errorf("Recorded(a free name) = %q, %v, %v; want none", path, resolved, err)
	}
	if got, err := driver().Elsewhere(ctx, s); got != "" || err != nil {
		t.Errorf("Elsewhere() of a name recorded at the slot's own missing path = %q, %v; want none", got, err)
	}
}
