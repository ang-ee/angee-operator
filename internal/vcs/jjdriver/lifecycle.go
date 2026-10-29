package jjdriver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	_, err = d.jj.recording(ctx, os.TempDir(), primary, []string{"git", "init", "--git-repo", primary, "--", primary})
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
	_, ok, err := d.workspace(ctx, s)
	return ok, err
}

// workspace returns the primary's workspace named Slot.Name, and whether it
// has one.
func (d Driver) workspace(ctx context.Context, s vcs.Slot) (workspace, bool, error) {
	out, err := d.jj.store(ctx, s.Primary, "workspace", "list", "-T", workspacesTemplate)
	if err != nil {
		return workspace{}, false, err
	}
	workspaces, err := decodeLines[workspace](out)
	if err != nil {
		return workspace{}, false, err
	}
	i := slices.IndexFunc(workspaces, func(w workspace) bool { return w.Name == s.Name })
	if i < 0 {
		return workspace{}, false, nil
	}
	return workspaces[i], true, nil
}

// Holds reports whether dir is the checkout of the primary's workspace
// Slot.Name (HeldChange).
func (d Driver) Holds(ctx context.Context, s vcs.Slot, dir string) (bool, error) {
	change, err := d.HeldChange(ctx, s, dir)
	return change != "", err
}

// HeldChange returns the change id of the working-copy commit of the
// primary's workspace Slot.Name when dir is that workspace's checkout, and
// "" when it is not: dir must be a real directory, never a link to one, and
// jj, run in it, must load a workspace whose working-copy
// commit is that workspace's, by name and by change id (a snapshot keeps the
// change id, and another repository's ids never match). It touches no
// working copy and reads no path jj recorded. A directory with no .jj, or one
// jj cannot load as a workspace, holds none. Known limit: a workspace that
// has `jj edit`ed another's working-copy commit shares it, and reads as held.
//
// The service asks it before it keeps what --sync finds at a slot's path
// (plan phase 6 step 4), and before it forgets the name of a slot it
// destroys, since a taken name alone does not say whose the checkout is.
func (d Driver) HeldChange(ctx context.Context, s vcs.Slot, dir string) (string, error) {
	if err := lifecycleFacts(s); err != nil {
		return "", err
	}
	if dir == "" {
		return "", errNoSlot
	}
	for _, path := range []string{dir, filepath.Join(dir, ".jj")} {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", nil
		}
	}
	out, err := d.jj.store(ctx, dir, "log", "--no-graph", "-r", workingCopy.String(), "-T", commitsTemplate)
	if exited(ctx, err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	entries, err := decodeLines[commitEntry](out)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !slices.Contains(entries[0].WorkingCopies, s.Name) {
		return "", nil
	}
	named, ok, err := d.workspace(ctx, s)
	if err != nil || !ok || named.Target.ChangeID != entries[0].Commit.ChangeID {
		return "", err
	}
	return named.Target.ChangeID, nil
}

// ErrRootUnresolved is wrapped by Elsewhere when nothing is at the path jj
// recorded for the workspace, as far as this process can see, and that path
// is not the slot's: a checkout that is gone, or one this process cannot see,
// such as another stack's from inside a container that does not mount it.
var ErrRootUnresolved = errors.New("nothing is at the path jj recorded for the workspace")

// cannotResolve starts jj's error for a workspace whose recorded path does
// not resolve; the path follows it, up to the end of the line.
const cannotResolve = "Error: Cannot resolve absolute workspace path: "

// Recorded returns the path jj recorded for the primary's workspace
// Slot.Name, and whether something is at it (`jj workspace root --name`
// resolves only an existing path). For a path that does not resolve it is
// the path jj names in its error, cleaned. The path is "" when the primary
// has no such workspace. jj's record goes stale once a workspace moves (plan
// section 2), so the service reads it only to refuse forgetting a name, or
// to recognise a slot's own name at the slot's path.
func (d Driver) Recorded(ctx context.Context, s vcs.Slot) (string, bool, error) {
	if err := lifecycleFacts(s); err != nil {
		return "", false, err
	}
	out, err := d.jj.store(ctx, s.Primary, "workspace", "root", "--name="+s.Name)
	var command *CommandError
	switch {
	case err == nil:
	case exited(ctx, err) && errors.As(err, &command) && strings.HasPrefix(command.Stderr, "Error: No such workspace"):
		return "", false, nil
	case exited(ctx, err) && errors.As(err, &command) && strings.HasPrefix(command.Stderr, cannotResolve):
		recorded, _, _ := strings.Cut(strings.TrimPrefix(command.Stderr, cannotResolve), "\n")
		if recorded = strings.TrimSpace(recorded); recorded == "" {
			return "", false, fmt.Errorf("jj workspace %q of %s: %w: %s", s.Name, s.Primary, ErrRootUnresolved, command.Stderr)
		}
		return filepath.Clean(recorded), false, nil
	default:
		return "", false, err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", false, fmt.Errorf("jj workspace %q of %s: jj printed no path: %w", s.Name, s.Primary, ErrRootUnresolved)
	}
	return root, true, nil
}

// Elsewhere returns where the primary's workspace Slot.Name is checked out,
// when jj's record of its path (Recorded) leads to an entry other than
// Slot.Path; else "": the name is free, or recorded at Slot.Path, so nothing
// else can be checked out under it. It fails closed: whatever is at that
// other path counts, whether or not this process can confirm it is the
// workspace's checkout, since forgetting the name would delete that
// checkout's .git. A record of another path with nothing at it is an error
// wrapping ErrRootUnresolved, for the caller to decide; any other failure of
// jj is returned as it is.
func (d Driver) Elsewhere(ctx context.Context, s vcs.Slot) (string, error) {
	recorded, resolved, err := d.Recorded(ctx, s)
	switch {
	case err != nil:
		return "", err
	case recorded == "" || samePath(recorded, s.Path):
		return "", nil
	case !resolved:
		return "", fmt.Errorf("jj workspace %q of %s, recorded at %s: %w", s.Name, s.Primary, recorded, ErrRootUnresolved)
	}
	if _, err := os.Lstat(recorded); errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("jj workspace %q of %s, recorded at %s: %w", s.Name, s.Primary, recorded, ErrRootUnresolved)
	} else if err != nil {
		return "", err
	}
	return recorded, nil
}

// samePath reports whether a and b name the same path, once the symbolic
// links of the part of each that exists are resolved (vcs.CanonicalPath).
func samePath(a, b string) bool {
	return vcs.CanonicalPath(a) == vcs.CanonicalPath(b)
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
// Remove does: remove through dest, forget, drop the slot's own git
// registration. jj can create the workspace and still exit 1 (a revision
// that resolves to nothing), so every error rolls back.
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
//
// The slot is colocated with git (`--colocate`) when the primary is: when it
// has a .git, as a source's cache always has. A local store jj keeps without
// git's working tree (`jj git init` without colocation) gets a slot without
// one too (plan phase 6 step 7). A git worktree a colocated primary still
// registers at Slot.Path is dropped first (dropStaleRegistration).
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
	colocated, err := colocated(s.Primary)
	if err != nil {
		return err
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
	if colocated {
		if err := d.dropStaleRegistration(ctx, s, dest); err != nil {
			return err
		}
	}
	if err := dest.MkdirAll(0o755); err != nil {
		return fmt.Errorf("make jj slot directory %s: %w", s.Path, err)
	}
	if err := d.create(ctx, s, dest, colocated); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if rollbackErr := d.Remove(cleanupCtx, s, dest); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("roll back jj slot %s: %w", s.Path, rollbackErr))
		}
		return err
	}
	return nil
}

// dropStaleRegistration drops the git worktree registration git still keeps
// at Slot.Path when the slot's checkout is gone, as a slot removed without
// the primary's lock leaves it: git refuses to add a worktree there (probed:
// "is a missing but already registered worktree"). The caller holds the
// primary's lock, and has checked that Slot.Path holds nothing or an empty
// directory; an empty one goes first, through dest, since git deregisters a
// worktree by its path only once nothing is there (deregister), and Add makes
// the directory again.
func (d Driver) dropStaleRegistration(ctx context.Context, s vcs.Slot, dest vcs.Dest) error {
	registered, err := d.git.WorktreeRegistered(ctx, s.Primary, s.Path)
	if err != nil || !registered {
		return err
	}
	if err := dest.RemoveAll(); err != nil {
		return err
	}
	return d.deregister(ctx, s)
}

// deregister drops the git worktree registration the primary keeps at
// Slot.Path, and no other: `git worktree remove --force --force <path>`,
// which with nothing at the path removes only git's record of it (probed on
// git 2.50; git has removed a missing worktree this way since long before
// the 2.48 a jj slot needs). A prune would drop every registration whose
// checkout this process cannot see, such as other stacks' slots out of a
// container's sight. It refuses while anything is at the path, which git
// would delete outside the slot's guard. A primary that is not colocated, or
// that registers nothing at the path, has nothing to drop.
func (d Driver) deregister(ctx context.Context, s vcs.Slot) error {
	colocated, err := colocated(s.Primary)
	if err != nil || !colocated {
		return err
	}
	registered, err := d.git.WorktreeRegistered(ctx, s.Primary, s.Path)
	if err != nil || !registered {
		return err
	}
	if _, err := os.Lstat(s.Path); err == nil {
		return fmt.Errorf("deregister jj slot %s from %s: it still exists", s.Path, s.Primary)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return d.git.WorktreeRemove(ctx, s.Primary, vcs.CanonicalPath(s.Path))
}

// create runs `jj workspace add` into the directory dest holds, colocated
// when the primary is, checking before and after that Slot.Path leads to it.
func (d Driver) create(ctx context.Context, s vcs.Slot, dest vcs.Dest, colocated bool) error {
	if err := dest.VerifyPathIdentity(s.Path); err != nil {
		return fmt.Errorf("jj slot %s: %w", s.Path, err)
	}
	args := []string{"workspace", "add"}
	if colocated {
		args = append(args, "--colocate")
	}
	args = append(args, "--name="+s.Name, "-r", startRevset(s).String(), "--", s.Path)
	if _, err := d.jj.inPrimary(ctx, s.Primary, args...); err != nil {
		return err
	}
	if err := dest.VerifyPathIdentity(s.Path); err != nil {
		return fmt.Errorf("jj slot %s changed while jj created it, so jj may have written the checkout elsewhere, and its files stay there: %w", s.Path, err)
	}
	return nil
}

// Remove removes the slot, touching nothing git keeps of any other slot:
//   - it removes the directory through dest first, since `jj workspace
//     forget` of a workspace whose directory exists also prunes every git
//     worktree of the primary whose checkout is missing (probed), which drops
//     other stacks' registrations a container cannot see;
//   - then forgets the workspace by name, which with the directory gone
//     changes only jj's record; forgetting a workspace the primary does not
//     have changes nothing;
//   - then drops the slot's own git worktree registration (deregister), and
//     no other.
//
// A removal that fails and leaves the directory stops there, since both
// later steps would then reach into it outside the guard. Otherwise each step
// runs even when an earlier one failed, and the errors are joined. A primary
// that is not colocated with git (no .git) registers no git worktree, so git
// is not run: run there, it would act on whatever repository encloses it.
//
// The forget touches no working copy. The caller holds the primary's lock
// (plan D9).
func (d Driver) Remove(ctx context.Context, s vcs.Slot, dest vcs.Dest) error {
	if err := lifecycleFacts(s); err != nil {
		return err
	}
	err := dest.RemoveAll()
	if err != nil {
		if _, statErr := os.Lstat(s.Path); !errors.Is(statErr, fs.ErrNotExist) {
			return err
		}
	}
	_, forgetErr := d.jj.store(ctx, s.Primary, "workspace", "forget", "--", s.Name)
	return errors.Join(err, forgetErr, d.deregister(ctx, s))
}

// ForgetName forgets the primary's workspace Slot.Name once the slot's
// directory is gone, as a rollback or destroy that cannot take the primary's
// lock must, so the name does not stay taken. With nothing at the
// workspace's path, `jj workspace forget` changes only jj's own record: it
// removes no git worktree registration and prunes none (probed), which is
// what the lock guards, and jj merges concurrent operations on its own. It
// refuses while Slot.Path holds anything, since the forget would then remove
// the slot's git registration and prune the primary's missing ones.
//
// change is the change id the slot's working copy had (HeldChange, read
// before the directory was removed). The name is forgotten only while its
// working copy is still that change: the lock's holder may have added a
// workspace of the same name meanwhile, such as another stack's, which is
// then left alone, with an error saying so. A name the primary no longer has
// is nothing to forget. The check and the forget are two jj commands, so a
// workspace re-added in the moment between them is still forgotten; the
// window is that narrow, and only open while another process holds the lock.
func (d Driver) ForgetName(ctx context.Context, s vcs.Slot, change string) error {
	if err := lifecycleFacts(s); err != nil {
		return err
	}
	if change == "" {
		return fmt.Errorf("forget jj workspace %q: no working-copy change to forget it by", s.Name)
	}
	if _, err := os.Lstat(s.Path); err == nil {
		return fmt.Errorf("forget jj workspace %q: its slot %s still exists", s.Name, s.Path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	named, ok, err := d.workspace(ctx, s)
	switch {
	case err != nil:
		return err
	case !ok:
		return nil
	case named.Target.ChangeID != change:
		return fmt.Errorf("jj workspace %q of %s is no longer the slot removed from %s: its working copy is change %s, not %s", s.Name, s.Primary, s.Path, named.Target.ChangeID, change)
	}
	_, err = d.jj.store(ctx, s.Primary, "workspace", "forget", "--", s.Name)
	return err
}

// colocated reports whether the repository at dir is colocated with git: it
// has a .git, which git's own commands would otherwise look for in the
// directories above it.
func colocated(dir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("look for a git repository in %s: %w", dir, err)
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
