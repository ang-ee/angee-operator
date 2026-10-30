package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/fslock"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/gitdriver"
)

// lockWaitHandler is a logger that closes waiting when fslock warns that it is
// waiting for the lock at path, and keeps the message.
type lockWaitHandler struct {
	path    string
	once    sync.Once
	waiting chan struct{}
	message string
}

func (h *lockWaitHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *lockWaitHandler) Handle(_ context.Context, record slog.Record) error {
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "path" && attr.Value.String() == h.path {
			h.once.Do(func() {
				h.message = record.Message
				close(h.waiting)
			})
			return false
		}
		return true
	})
	return nil
}

func (h *lockWaitHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *lockWaitHandler) WithGroup(string) slog.Handler { return h }

// warnings is a logger that keeps the warnings it is given.
type warnings struct {
	mu       sync.Mutex
	messages []string
}

func (w *warnings) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelWarn }

func (w *warnings) Handle(_ context.Context, record slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages = append(w.messages, record.Message)
	return nil
}

func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }

func (w *warnings) WithGroup(string) slog.Handler { return w }

// cacheLockPath is the lock file beside app's cache, worked out from the
// cache's directory rather than by primaryLockPath.
func (f *lifecycleFixture) cacheLockPath(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(filepath.Dir(f.cache()))
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	return filepath.Join(parent, ".app.angee.lock")
}

// Every change to a source cache waits while another holder, another stack or
// the CLI in a separate process, has the cache's lock, warns at the default
// verbosity that it is waiting for that cache, and goes on once the lock is
// released: adding a worktree, removing one (prune), and fetching, pulling or
// pushing the cache. The lock is .<name>.angee.lock beside the cache.
func TestSourceCacheChangesWaitForThePrimaryLock(t *testing.T) {
	f := newLifecycleFixture(t)
	template := f.template(t, "worktree", "app")
	if _, err := f.platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "first"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	lockPath := f.cacheLockPath(t)

	for _, op := range []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"workspace create", func(ctx context.Context) error {
			_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "second", Inputs: map[string]string{"branch": "second"}})
			return err
		}},
		{"workspace destroy", func(ctx context.Context) error { return f.platform.WorkspaceDestroy(ctx, "second", true) }},
		{"source fetch", func(ctx context.Context) error {
			_, err := f.platform.SourceFetch(ctx, "app")
			return err
		}},
		{"source pull", func(ctx context.Context) error {
			_, err := f.platform.SourcePull(ctx, "app")
			return err
		}},
		{"source push", func(ctx context.Context) error {
			_, err := f.platform.SourcePush(ctx, "app", "")
			return err
		}},
	} {
		holder := fslock.New(lockPath)
		if err := holder.Lock(context.Background()); err != nil {
			t.Fatalf("Lock(%s) error = %v", lockPath, err)
		}
		handler := &lockWaitHandler{path: lockPath, waiting: make(chan struct{})}
		ctx := logctx.With(context.Background(), slog.New(handler))
		done := make(chan error, 1)
		go func() { done <- op.run(ctx) }()
		select {
		case err := <-done:
			_ = holder.Unlock()
			t.Fatalf("%s finished (error %v) while another holder had the cache's lock", op.name, err)
		case <-handler.waiting:
		case <-time.After(time.Minute):
			_ = holder.Unlock()
			t.Fatalf("%s neither warned that it waits for the cache's lock nor finished", op.name)
		}
		if err := holder.Unlock(); err != nil {
			t.Fatalf("Unlock() error = %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("%s once the lock was released error = %v", op.name, err)
		}
		if want := "waiting for source cache " + f.cache() + ": another angee process is changing it"; handler.message != want {
			t.Fatalf("%s wait warning = %q, want %q", op.name, handler.message, want)
		}
	}
	if _, err := filepath.EvalSymlinks(filepath.Join(f.workspace("second"), "app")); err == nil {
		t.Fatal("the destroyed workspace's slot is still there")
	}
}

// probeCacheLock puts a git on PATH that, for every command it runs on app's
// cache (in the cache, or cloning it or configuring the clone in the staging
// directory beside it) that changes it or lists its worktrees, records in the
// returned log whether the cache's lock is held, by trying to take it without
// waiting. Reads of the cache's state are not recorded, nor is a clone slot,
// cloned into TMPDIR.
func probeCacheLock(t *testing.T, f *lifecycleFixture) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl not installed")
	}
	base, err := filepath.EvalSymlinks(f.base)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	cache := filepath.Join(base, ".angee", ".cache", "app")
	lock := filepath.Join(base, ".angee", ".cache", ".app.angee.lock")
	bin := t.TempDir()
	log := filepath.Join(bin, "lock.log")
	script := fmt.Sprintf(`#!/bin/sh
probe=
case "$(pwd -P)" in
  %[1]q|%[1]q/*|*/.app.clone-*) probe=1 ;;
esac
case "$*" in
  *.app.clone-*) probe=1 ;;
esac
subcommand() {
  while [ "$1" = -c ]; do shift 2; done
  printf '%%s' "$1"
}
case "$(subcommand "$@")" in
  worktree|fetch|pull|push|clone|remote|branch) ;;
  config) case "$*" in *--get*) probe= ;; esac ;;
  *) probe= ;;
esac
if [ -n "$probe" ]; then
  if perl -MFcntl=:flock -e 'open(F,"<",$ARGV[0]) or exit 1; exit(flock(F,LOCK_EX|LOCK_NB)?1:0)' %[2]q; then
    printf 'locked %%s\n' "$*" >> %[3]q
  else
    printf 'UNLOCKED %%s\n' "$*" >> %[3]q
  fi
fi
exec %[4]q "$@"
`, cache, lock, log, real)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(git) error = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// The cache's lock covers every git command that changes the cache or reads
// what a change decides on: the clone and fetch that refresh it, the listing
// that finds a leftover (Exists), worktree add and prune, and pull and push,
// in create, a --sync reclaim, destroy, a failed create's rollback, and the
// source verbs.
func TestCacheGitRunsUnderThePrimaryLock(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	template := f.template(t, "worktree", "app")
	failing := f.template(t, "partial", "app", "missing")
	log := probeCacheLock(t, f)
	create := func(name, template string, sync bool) error {
		_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: name, Sync: sync, Inputs: map[string]string{"branch": name}})
		return err
	}
	if err := create("a", template, false); err != nil {
		t.Fatalf("WorkspaceCreate(a, a fresh cache) error = %v", err)
	}
	if err := create("b", template, false); err != nil {
		t.Fatalf("WorkspaceCreate(b) error = %v", err)
	}
	f.forget(t, "b")
	if err := create("b", template, true); err != nil {
		t.Fatalf("WorkspaceCreate(b, --sync over its leftover) error = %v", err)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "b", true); err != nil {
		t.Fatalf("WorkspaceDestroy(b) error = %v", err)
	}
	if err := create("c", failing, false); err == nil {
		t.Fatal("WorkspaceCreate(c) with a missing remote error = nil, want a failure")
	}
	if _, err := f.platform.SourceFetch(ctx, "app"); err != nil {
		t.Fatalf("SourceFetch() error = %v", err)
	}
	if _, err := f.platform.SourcePull(ctx, "app"); err != nil {
		t.Fatalf("SourcePull() error = %v", err)
	}
	if _, err := f.platform.SourcePush(ctx, "app", ""); err != nil {
		t.Fatalf("SourcePush() error = %v", err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile(lock log) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "UNLOCKED ") {
			t.Errorf("git ran on the cache without its lock: %s", strings.TrimPrefix(line, "UNLOCKED "))
		}
	}
	for _, command := range []string{"clone ", "worktree list ", "worktree add ", "worktree prune", "fetch --all --prune", "pull --ff-only", "push"} {
		seen := false
		for _, line := range lines {
			if strings.HasPrefix(line, "locked ") && strings.Contains(line, " "+command) {
				seen = true
				break
			}
		}
		if !seen {
			t.Errorf("no %q ran on the cache, so the probe proves nothing for it; ran:\n%s", command, strings.Join(lines, "\n"))
		}
	}
}

// holdLockAndFail is a slot driver whose Add takes the cache's lock as
// another process would, keeps it, and fails, so the create rolls back while
// the lock is held.
type holdLockAndFail struct {
	vcs.Driver
	hold func() error
}

func (d holdLockAndFail) Add(context.Context, vcs.Slot, vcs.Dest, func(context.Context, string) error) error {
	if err := d.hold(); err != nil {
		return err
	}
	return errors.New("clone failed on purpose")
}

// A rollback that cannot get the cache's lock still removes the worktree slot
// it created, through the slot's guard, and warns that the cache keeps the
// slot's registration; the cache's next prune drops it, here the next create
// of that slot. The workspace directory already exists, as on a --sync retry,
// so only the slot's own rollback can remove the slot.
func TestRollbackRemovesAWorktreeSlotWhileTheCacheLockIsHeld(t *testing.T) {
	f := newLifecycleFixture(t)
	partial := f.template(t, "partial", "app", "docs")
	template := f.template(t, "worktree", "app")
	t.Setenv("ANGEE_LOCK_TIMEOUT", "100ms")
	var holder *fslock.Lock
	git := gitdriver.New(f.platform.gitClient())
	f.platform.slotDrivers = func(s vcs.Slot) vcs.Driver {
		if s.Mode == vcs.ModeClone {
			return holdLockAndFail{Driver: git, hold: func() error {
				holder = fslock.New(f.cacheLockPath(t))
				return holder.Lock(context.Background())
			}}
		}
		return git
	}
	if err := os.MkdirAll(f.workspace("feature-a"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	logged := &warnings{}
	ctx := logctx.With(context.Background(), slog.New(logged))

	_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: partial, Name: "feature-a"})
	if holder != nil {
		_ = holder.Unlock()
	}
	if err == nil || !strings.Contains(err.Error(), "clone failed on purpose") {
		t.Fatalf("WorkspaceCreate() error = %v, want the clone slot's failure", err)
	}
	slot := filepath.Join(f.workspace("feature-a"), "app")
	if _, err := os.Lstat(slot); !os.IsNotExist(err) {
		t.Fatalf("worktree slot after the rollback: Lstat error = %v, want it removed", err)
	}
	if list := runGitOutput(t, f.cache(), "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 2 || !strings.Contains(list, "prunable") {
		t.Fatalf("cache worktrees after the rollback:\n%s\nwant the cache's own and the slot's registration, prunable", list)
	}
	warned := false
	for _, message := range logged.messages {
		warned = warned || strings.Contains(message, "without deregistering it from source cache "+f.cache())
	}
	if !warned {
		t.Fatalf("warnings = %q, want one saying the slot stays registered in the cache", logged.messages)
	}

	if _, err := f.platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{Template: template, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() after the rollback error = %v", err)
	}
	if list := runGitOutput(t, f.cache(), "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 2 || strings.Contains(list, "prunable") {
		t.Fatalf("cache worktrees after the next create:\n%s\nwant the cache's own and the new slot's", list)
	}
}

// A lock another process keeps past ANGEE_LOCK_TIMEOUT is a CONFLICT to retry,
// and a lock file the operator cannot create beside the cache says to make the
// cache's directory writable, or to mount it rather than the cache alone.
func TestPrimaryLockFailuresSayWhatToDo(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	template := f.template(t, "worktree", "app")
	if _, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "first"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	lockPath := f.cacheLockPath(t)

	t.Setenv("ANGEE_LOCK_TIMEOUT", "50ms")
	holder := fslock.New(lockPath)
	if err := holder.Lock(ctx); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	_, err := f.platform.SourceFetch(ctx, "app")
	_ = holder.Unlock()
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeConflict || !errors.Is(err, fslock.ErrTimeout) ||
		!strings.HasPrefix(err.Error(), `Fetch for source "app": another angee process is changing source cache `+f.cache()) ||
		!strings.HasSuffix(err.Error(), "Retry when it finishes.") {
		t.Fatalf("SourceFetch() with the cache locked error = %v (%+v), want a CONFLICT to retry", err, opErr)
	}

	if os.Geteuid() == 0 {
		t.Skip("root writes any directory")
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("Remove(lock) error = %v", err)
	}
	parent := filepath.Dir(f.cache())
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	_, err = f.platform.SourceFetch(ctx, "app")
	opErr = AsOperationError(err)
	if opErr == nil || opErr.Code != CodePreconditionFailed || !strings.Contains(err.Error(), "mount that directory rather than the cache alone") {
		t.Fatalf("SourceFetch() with an unwritable cache directory error = %v (%+v), want a precondition naming the fix", err, opErr)
	}
}

// The lock file of a cache reached through a symlink sits beside the
// symlink's target, so every path to one cache takes the same lock. A cache
// not cloned yet is locked beside its path; one whose path cannot be resolved
// for any other reason is not locked at all, rather than locked elsewhere.
func TestPrimaryLockPath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	cache := filepath.Join(base, "caches", "app")
	link := filepath.Join(base, "link")
	mustMkdirAll(t, cache)
	if err := os.Symlink(cache, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	want := filepath.Join(base, "caches", ".app.angee.lock")
	for _, path := range []string{cache, link} {
		if got, err := primaryLockPath(path); err != nil || got != want {
			t.Fatalf("primaryLockPath(%s) = %s, %v, want %s", path, got, err, want)
		}
	}
	if got, err := primaryLockPath(filepath.Join(base, "missing")); err != nil || got != filepath.Join(base, ".missing.angee.lock") {
		t.Fatalf("primaryLockPath(a cache not cloned yet) = %s, %v, want it beside the path", got, err)
	}
	loop := filepath.Join(base, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if got, err := primaryLockPath(loop); err == nil {
		t.Fatalf("primaryLockPath(a symlink loop) = %s, want an error", got)
	}
	err = withPrimaryLock(context.Background(), loop, func() error { return nil })
	if !errors.Is(err, errPrimaryLockNotHeld) {
		t.Fatalf("withPrimaryLock(a symlink loop) error = %v, want it marked not held", err)
	}
}
