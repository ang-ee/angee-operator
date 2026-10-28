package jjdriver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// rollbackTimeout bounds a failed Add's rollback, which runs on a fresh
// context so that it still runs when the caller's context is what failed.
const rollbackTimeout = 30 * time.Second

// errNoName refuses a lifecycle call for a slot without its workspace name,
// which jj keeps its workspaces by.
var errNoName = errors.New("a jj slot needs its workspace name")

// EnsurePrimary makes primary, a git clone (a source's cache), a jj
// repository colocated with it, unless it already has a .jj. It is safe to
// call again: with a .jj in place it runs nothing.
//
// It runs `jj git init --git-repo <primary> <primary>`, which makes the same
// colocated repository as `--colocate` but refuses a directory that is not a
// git repository, where `--colocate` would create a new, empty one. jj leaves
// git on its branch and ignores .jj itself (plan section 2), so git keeps
// working in the primary and in its git worktree slots. jj runs in the system
// temporary directory: run inside the primary, it would also load, and record
// in the user's config directory, any jj repository above it.
//
// It sets no repository config. The one setting the driver relies on,
// snapshot.auto-update-stale, is passed on every command (plan D8, Q9), since
// jj keeps a repository's config in the user's config directory, which an
// operator with another HOME does not share. (jj's init records a trunk()
// alias there by itself; nothing here reads it.)
//
// It is a method of the jj driver and not of vcs.Driver: a git primary needs
// nothing, and the service must call it before it asks the primary about its
// workspaces (Exists), so it cannot be a step of Add. The caller holds the
// primary's lock (plan D9), as for a clone of the primary.
func (d Driver) EnsurePrimary(ctx context.Context, primary string) error {
	if primary == "" {
		return errNoPrimary
	}
	primary, err := filepath.Abs(primary)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(primary, ".jj")); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("look for a jj repository in %s: %w", primary, err)
	}
	_, err = d.jj.run(ctx, os.TempDir(), []string{"git", "init", "--git-repo", primary, "--", primary})
	return err
}

// Exists reports whether the slot's primary has a workspace named Slot.Name.
// It reads the primary's workspace list by name, never jj's record of a
// workspace's path, which goes stale once a workspace moves (plan section 2),
// and touches no working copy.
func (d Driver) Exists(ctx context.Context, s vcs.Slot) (bool, error) {
	if err := lifecycleFacts(s); err != nil {
		return false, err
	}
	out, err := d.jj.primaryStore(ctx, s.Primary, "workspace", "list", "-T", workspacesTemplate)
	if err != nil {
		return false, err
	}
	workspaces, err := decodeLines[workspace](out)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(workspaces, func(w workspace) bool { return w.Name == s.Name }), nil
}

// Add makes the slot in place (plan D4): jj writes a secondary workspace of
// Slot.Primary named Slot.Name, colocated with git, straight to Slot.Path.
// Its working-copy commit starts on the local bookmark Slot.Branch when there
// is one, else on the base Status counts against (baseCandidates); jj
// resolves that choice from one revset. No bookmark is created, so git sees a
// detached HEAD and no new branch.
//
// The guard, all through dest:
//   - Before anything is written: Slot.Path holds nothing or an empty
//     directory, and the primary has no workspace named Slot.Name (D13). jj
//     refuses an existing name too, but the rollback below forgets
//     Slot.Name, so Add must know the name was free: otherwise a failed Add
//     would forget another slot's workspace.
//   - Add makes the directory, and any missing parent, and checks that
//     Slot.Path, looked up by name, leads to it.
//   - jj creates the workspace in it (`jj workspace add`).
//   - After: Slot.Path still leads to that directory. A parent swapped for a
//     symlink in between shows here: jj then wrote the checkout where the
//     symlink leads, and recorded that path. Add itself removes nothing
//     there, since removing through the swapped path is what the guard is
//     there to prevent, but the rollback's forget does write through it, as
//     jj: it deletes the .git jj wrote at the recorded path and runs its own
//     `git worktree prune`. The rest of what jj wrote there stays.
//
// On a mismatch, or any error once the directory is made, Add rolls back as
// Remove does: forget, remove through dest, prune. jj can create the
// workspace and still exit 1 (a revision that resolves to nothing), so every
// error rolls back.
//
// configure must be nil. A jj slot has no configuration of its own: a linked
// git worktree shares its primary's git config, and the slot shares the
// primary's jj repository, so a clone slot's remotes, which configure writes,
// are the primary's here, reconciled by the service on the source's cache.
// Nor could it run before the checkout reaches Slot.Path, as vcs.Driver
// promises, since jj writes the checkout there. So it is refused before
// anything is written, not ignored.
//
// `jj workspace add` runs in the primary as any jj command there runs,
// snapshotting the primary's working copy and importing git's refs first: jj
// refuses to add a workspace with --ignore-working-copy (after creating it),
// and the import is what shows jj the base the service has just fetched into
// the primary with git. The caller holds the primary's lock (plan D9).
func (d Driver) Add(ctx context.Context, s vcs.Slot, dest vcs.Dest, configure func(context.Context, string) error) error {
	if configure != nil {
		return fmt.Errorf("configuring a jj slot, which shares its primary's git and jj configuration: %w", vcs.ErrUnsupported)
	}
	if err := lifecycleFacts(s); err != nil {
		return err
	}
	if !filepath.IsAbs(s.Path) {
		return fmt.Errorf("jj slot path %q is not absolute", s.Path)
	}
	if s.BaseRef == "" {
		return fmt.Errorf("jj slot %s has no base ref to start from: %w", s.Path, vcs.ErrBaseNotFound)
	}
	if err := emptyDestination(s.Path, dest); err != nil {
		return err
	}
	taken, err := d.Exists(ctx, s)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("jj workspace %q already exists in %s: %w", s.Name, s.Primary, vcs.ErrNameTaken)
	}
	if err := dest.MkdirAll(0o755); err != nil {
		return fmt.Errorf("make jj slot directory %s: %w", s.Path, err)
	}
	if err := d.create(ctx, s, dest); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if rollbackErr := d.Remove(cleanupCtx, s, dest); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("roll back jj slot %s: %w", s.Path, rollbackErr))
		}
		return err
	}
	return nil
}

// create runs `jj workspace add` into the directory dest holds, checking
// before and after that Slot.Path leads to it.
func (d Driver) create(ctx context.Context, s vcs.Slot, dest vcs.Dest) error {
	if err := dest.VerifyPathIdentity(s.Path); err != nil {
		return fmt.Errorf("jj slot %s: %w", s.Path, err)
	}
	if _, err := d.jj.inPrimary(ctx, s.Primary, "workspace", "add", "--colocate", "--name="+s.Name, "-r", startRevset(s).String(), "--", s.Path); err != nil {
		return err
	}
	if err := dest.VerifyPathIdentity(s.Path); err != nil {
		return fmt.Errorf("jj slot %s changed while jj created it, so jj may have written the checkout elsewhere, and its files stay there: %w", s.Path, err)
	}
	return nil
}

// Remove removes the slot in the order the probes require (plan section 2).
// It forgets the workspace by name (`jj workspace forget`), which also drops
// its git worktree registration while its directory exists; forgetting a
// workspace the primary does not have changes nothing. It then removes the
// directory through dest, and runs `git worktree prune` in the primary, which
// drops the registration of a slot whose directory was gone before the forget,
// and of any other worktree of the primary whose checkout is missing. Each
// step runs even when an earlier one failed, so whatever part of the slot did
// go is not left registered; the errors are joined.
//
// The forget touches no working copy. The caller holds the primary's lock
// (plan D9).
func (d Driver) Remove(ctx context.Context, s vcs.Slot, dest vcs.Dest) error {
	if err := lifecycleFacts(s); err != nil {
		return err
	}
	_, err := d.jj.primaryStore(ctx, s.Primary, "workspace", "forget", "--", s.Name)
	err = errors.Join(err, dest.RemoveAll())
	return errors.Join(err, d.git.WorktreePrune(ctx, s.Primary))
}

// lifecycleFacts refuses a slot the lifecycle cannot name in its primary.
func lifecycleFacts(s vcs.Slot) error {
	switch {
	case s.Primary == "":
		return errNoPrimary
	case s.Name == "":
		return errNoName
	}
	return nil
}

// emptyDestination refuses a slot path that holds anything but an empty real
// directory. jj refuses a non-empty one too, but only once it runs, after the
// name check.
func emptyDestination(path string, dest vcs.Dest) error {
	info, exists, err := dest.Lstat()
	if err != nil {
		return fmt.Errorf("jj slot %s: %w", path, err)
	}
	if !exists {
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("jj slot %s already exists and is not a directory", path)
	}
	empty, err := dest.IsEmptyDirectory()
	if err != nil {
		return fmt.Errorf("jj slot %s: %w", path, err)
	}
	if !empty {
		return fmt.Errorf("jj slot %s already exists and is not empty", path)
	}
	return nil
}

// startRevset is where a new slot's working-copy commit starts: the local
// bookmark Slot.Branch when there is one, else the slot's base, the first of
// baseCandidates that exists. When none exists, jj creates the workspace and
// then fails, and Add rolls it back.
func startRevset(s vcs.Slot) revset {
	var candidates []revset
	if s.Branch != "" {
		candidates = append(candidates, localBookmark(s.Branch))
	}
	for _, c := range baseCandidates(s.BaseRef, s.BaseRemote) {
		candidates = append(candidates, c.revset)
	}
	return coalesce(candidates...)
}
