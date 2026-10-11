package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"syscall"

	"github.com/ang-ee/angee-operator/internal/fslock"
)

// errPrimaryLockNotHeld marks a failure to take a primary's lock, as opposed
// to a failure of the work done under it, so a caller can still do what needs
// no lock: a rollback removes its own slot through the slot's guard and skips
// the prune.
var errPrimaryLockNotHeld = errors.New("primary lock not held")

// primaryLockError is a failure to take a primary's lock: err, the reason,
// marked errPrimaryLockNotHeld.
type primaryLockError struct{ err error }

func (e primaryLockError) Error() string { return e.err.Error() }

func (e primaryLockError) Unwrap() []error { return []error{e.err, errPrimaryLockNotHeld} }

// withPrimaryLock runs fn holding the cross-process lock of the primary at
// path (a git source's cache), so two stacks, or a stack and the CLI, that
// share it take turns on what could interleave badly: its worktree
// registrations (adding and pruning worktrees) and its remotes (clone, fetch,
// pull, push). The verbs on a slot take no lock: git's ref transactions keep
// each ref update whole, and they never prune registrations.
//
// Lock order: the stack mutation lease, then the root lock, then a primary's
// lock, which is always the last one taken. fslock is not reentrant, so fn
// must not take the same primary's lock again: code that runs under it calls
// materializeSourceLocked, never materializeSource.
//
// A failure to take the lock matches errPrimaryLockNotHeld. A wait that
// ANGEE_LOCK_TIMEOUT ends is a CONFLICT, and a lock file that cannot be
// created says to make the cache's directory writable.
func withPrimaryLock(ctx context.Context, primary string, fn func() error) error {
	lockPath, err := primaryLockPath(primary)
	if err != nil {
		return primaryLockError{fmt.Errorf("lock source cache %s: %w", primary, err)}
	}
	lock := fslock.New(lockPath).ReportWait(slog.LevelWarn, fmt.Sprintf("waiting for source cache %s: another angee process is changing it", primary))
	ran := false
	err = lock.With(ctx, func() error {
		ran = true
		return fn()
	})
	if err == nil || ran {
		return err
	}
	return primaryLockError{primaryLockFailure(primary, lockPath, err)}
}

// primaryLockFailure says why the lock of the primary at path, lockPath, was
// not taken, and what to do about it.
func primaryLockFailure(primary, lockPath string, err error) error {
	switch {
	case errors.Is(err, fslock.ErrTimeout):
		hint := "Retry when it finishes."
		return &OperationError{
			Code:    CodeConflict,
			Paths:   []string{primary},
			Hint:    hint,
			Summary: fmt.Sprintf("another angee process is changing source cache %s (%v). %s", primary, err, hint),
			Err:     err,
		}
	case errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS):
		hint := fmt.Sprintf("Make %s writable; to bind-mount the cache into a container, mount that directory rather than the cache alone.", filepath.Dir(lockPath))
		return &OperationError{
			Code:    CodePreconditionFailed,
			Paths:   []string{lockPath},
			Hint:    hint,
			Summary: fmt.Sprintf("source cache %s cannot be locked: its lock file goes beside it, and %v. %s", primary, err, hint),
			Err:     err,
		}
	}
	return err
}

// primaryLockPath is the lock file of the primary at path, beside it as
// <parent>/.<name>.angee.lock: outside the primary, which does not exist
// before its first clone. A primary reached through a symlink is locked
// beside its target, so every path to one primary takes the same lock. A
// primary that does not exist yet is locked beside its path; any other
// failure to resolve it is returned, rather than locking another file.
func primaryLockPath(primary string) (string, error) {
	resolved, err := filepath.EvalSymlinks(primary)
	switch {
	case err == nil:
		primary = resolved
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	return filepath.Join(filepath.Dir(primary), "."+filepath.Base(primary)+".angee.lock"), nil
}
