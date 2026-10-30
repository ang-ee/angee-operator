package copierx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAsideFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

// A guarded directory set aside keeps its name's parent and depth, a new
// directory takes its path, and entries move between the two by rename,
// never over an entry, and back, or back under another name; the directory
// set aside goes back when the path is free, and is removed through its own
// name.
func TestGuardedPathSetsADirectoryAside(t *testing.T) {
	root := t.TempDir()
	slot := filepath.Join(root, "ws", "app")
	writeAsideFile(t, filepath.Join(slot, ".env"), "secret\n")
	writeAsideFile(t, filepath.Join(slot, "src", "debug.log"), "log\n")
	writeAsideFile(t, filepath.Join(slot, "src", "a.txt"), "old\n")
	if err := os.Symlink("a.txt", filepath.Join(slot, "src", "link")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	guard, err := OpenGuardedPath(root, root, "ws/app", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath() error = %v", err)
	}
	defer func() { _ = guard.Close() }()

	aside, err := guard.SetAside(".aside-", slot)
	if err != nil {
		t.Fatalf("SetAside() error = %v", err)
	}
	if filepath.Dir(aside.Path()) != filepath.Dir(slot) || !strings.HasPrefix(filepath.Base(aside.Path()), ".aside-") {
		t.Fatalf("set aside at %s, want a private name beside %s", aside.Path(), slot)
	}
	if _, err := os.Lstat(slot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(slot) after SetAside error = %v, want nothing there", err)
	}
	if err := guard.RestoreAside(aside); err != nil {
		t.Fatalf("RestoreAside() error = %v", err)
	}
	if aside, err = guard.SetAside(".aside-", slot); err != nil {
		t.Fatalf("SetAside() again error = %v", err)
	}

	if err := guard.MkdirAll(0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeAsideFile(t, filepath.Join(slot, "src", "a.txt"), "new\n")
	for _, rel := range []string{".env", "src/debug.log", "src/link"} {
		if err := guard.MoveFromAside(aside, rel); err != nil {
			t.Fatalf("MoveFromAside(%s) error = %v", rel, err)
		}
	}
	if err := guard.MoveFromAside(aside, "src/a.txt"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("MoveFromAside(src/a.txt) over an entry error = %v, want fs.ErrExist", err)
	}
	if err := guard.MoveFromAside(aside, "../escape"); err == nil {
		t.Fatalf("MoveFromAside(../escape) succeeded, want it refused")
	}
	if target, err := os.Readlink(filepath.Join(slot, "src", "link")); err != nil || target != "a.txt" {
		t.Fatalf("the link moved as %q, %v, want the link itself", target, err)
	}
	if data, err := os.ReadFile(filepath.Join(slot, ".env")); err != nil || string(data) != "secret\n" {
		t.Fatalf(".env moved as %q, %v", data, err)
	}

	// What the directory set aside holds again is kept beside, under
	// another name, never overwritten.
	writeAsideFile(t, filepath.Join(aside.Path(), ".env"), "rewritten\n")
	if err := guard.MoveToAside(aside, ".env"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("MoveToAside(.env) over an entry error = %v, want fs.ErrExist", err)
	}
	if err := guard.MoveToAsideAs(aside, ".env", "../escape"); err == nil {
		t.Fatalf("MoveToAsideAs(.env, ../escape) succeeded, want it refused")
	}
	if err := guard.MoveToAsideAs(aside, ".env", "src/a.txt"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("MoveToAsideAs(.env, src/a.txt) over an entry error = %v, want fs.ErrExist", err)
	}
	if err := guard.MoveToAsideAs(aside, ".env", ".env.kept"); err != nil {
		t.Fatalf("MoveToAsideAs(.env, .env.kept) error = %v", err)
	}
	for _, rel := range []string{"src/debug.log", "src/link"} {
		if err := guard.MoveToAside(aside, rel); err != nil {
			t.Fatalf("MoveToAside(%s) error = %v", rel, err)
		}
	}
	if err := guard.RestoreAside(aside); err == nil {
		t.Fatalf("RestoreAside() over the new directory succeeded, want it refused")
	}
	if err := guard.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	if err := guard.RestoreAside(aside); err != nil {
		t.Fatalf("RestoreAside() error = %v", err)
	}
	for rel, want := range map[string]string{".env": "rewritten\n", ".env.kept": "secret\n", "src/debug.log": "log\n", "src/a.txt": "old\n"} {
		if data, err := os.ReadFile(filepath.Join(slot, rel)); err != nil || string(data) != want {
			t.Errorf("%s after the restore = %q, %v, want %q", rel, data, err, want)
		}
	}
}

// A directory set aside whose private name was swapped meanwhile is never
// acted on, and an intermediate link is never followed by a move.
func TestAsideRefusesWhatChanged(t *testing.T) {
	root := t.TempDir()
	slot := filepath.Join(root, "app")
	writeAsideFile(t, filepath.Join(slot, "dir", "x"), "x\n")
	guard, err := OpenGuardedPath(root, root, "app", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath() error = %v", err)
	}
	defer func() { _ = guard.Close() }()
	aside, err := guard.SetAside(".aside-", slot)
	if err != nil {
		t.Fatalf("SetAside() error = %v", err)
	}
	if err := guard.MkdirAll(0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(slot, "dir")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if err := guard.MoveFromAside(aside, "dir/x"); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("MoveFromAside() under a link error = %v, want it refused", err)
	}

	moved := aside.Path() + ".moved"
	if err := os.Rename(aside.Path(), moved); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	writeAsideFile(t, filepath.Join(aside.Path(), "other"), "other\n")
	if err := aside.Verify(); err == nil {
		t.Fatalf("Verify() of a swapped directory succeeded")
	}
	if err := aside.RemoveAll(); err == nil {
		t.Fatalf("RemoveAll() of a swapped directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(aside.Path(), "other")); err != nil {
		t.Fatalf("the swapped-in directory was touched: %v", err)
	}
}
