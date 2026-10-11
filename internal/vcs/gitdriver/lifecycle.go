package gitdriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// Exists reports whether the slot's cache lists a worktree at its path. A
// clone slot is a repository of its own, which no cache lists.
func (d Driver) Exists(ctx context.Context, s vcs.Slot) (bool, error) {
	if s.Mode != vcs.ModeWorktree {
		return false, nil
	}
	return d.git.WorktreeRegistered(ctx, s.Primary, s.Path)
}

// Add makes the slot's checkout in a staging directory outside the stack and
// installs it at the slot path through dest, so a parent of the path swapped
// while git runs cannot redirect it.
func (d Driver) Add(ctx context.Context, s vcs.Slot, dest vcs.Dest, configure func(ctx context.Context, checkout string) error) error {
	switch s.Mode {
	case vcs.ModeWorktree:
		return d.addWorktree(ctx, s, dest, configure)
	case vcs.ModeClone:
		return d.addClone(ctx, s, dest, configure)
	}
	return fmt.Errorf("%w: slot mode %q", vcs.ErrUnsupported, s.Mode)
}

// Remove removes the checkout through dest and, for a worktree, runs `git
// worktree prune` in the cache, which drops every registration whose
// checkout is missing, not only this slot's. The prune runs even when the
// removal failed, so whatever part of the checkout did go is not left
// registered.
func (d Driver) Remove(ctx context.Context, s vcs.Slot, dest vcs.Dest) error {
	err := dest.RemoveAll()
	if s.Mode == vcs.ModeWorktree {
		err = errors.Join(err, d.git.WorktreePrune(ctx, s.Primary))
	}
	return err
}

// addWorktree adds the worktree in a staging directory, installs it through
// dest, and points the cache's registration at the slot path. Until that
// repair, git takes the registration for one whose checkout is gone, so the
// caller holds the cache's lock throughout. On failure the installed
// checkout and the stage are removed and the registration pruned.
func (d Driver) addWorktree(ctx context.Context, s vcs.Slot, dest vcs.Dest, configure func(context.Context, string) error) error {
	stageRoot, err := os.MkdirTemp("", "angee-worktree-stage-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	stage := filepath.Join(stageRoot, "source")
	installed := false
	defer func() {
		if installed {
			return
		}
		_ = os.RemoveAll(stage)
		// A fresh context, so the prune runs even when ctx is what failed.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = d.git.WorktreePrune(cleanupCtx, s.Primary)
		cancel()
	}()
	useExisting := s.Branch != "" && d.git.RefExists(ctx, s.Primary, "refs/heads/"+s.Branch)
	if useExisting {
		logctx.From(ctx).Warn(fmt.Sprintf("branch %q already exists in %s; checking it out into worktree without creating a new branch", s.Branch, s.Primary))
	}
	// The cache is cloned on its default ref only, so another base may exist
	// only as <clone remote>/<ref>. Start from that explicitly: given the bare
	// name, git creates and checks out a local tracking branch of that name
	// (even with -b), and --detach refuses it.
	start, startIsRemote := startRef(ctx, d.git, s.Primary, s.BaseRef, s.BaseRemote)
	switch {
	case s.Branch == "":
		// A slot without a branch starts detached at its base, so an untouched
		// slot never leaves a branch behind; publish creates the branch.
		// Without --detach git would check out the base itself, failing when
		// the cache has it checked out.
		err = d.git.WorktreeAddDetached(ctx, s.Primary, stage, start)
	case useExisting:
		err = d.git.WorktreeAdd(ctx, s.Primary, stage, s.Branch)
	default:
		// A branch cut from a remote-tracking ref would track it, and a push
		// would then aim at the base branch.
		err = d.git.WorktreeAddBranch(ctx, s.Primary, stage, s.Branch, start, !startIsRemote)
	}
	if err != nil {
		return err
	}
	if configure != nil {
		if err := configure(ctx, stage); err != nil {
			return err
		}
	}
	if _, exists, err := dest.Lstat(); err != nil {
		return err
	} else if exists {
		if err := dest.RemoveAll(); err != nil {
			return err
		}
	}
	if err := dest.ReplaceFrom(ctx, stage); err != nil {
		return err
	}
	if err := finishWorktree(s, dest, stage); err != nil {
		_ = dest.RemoveAll()
		return err
	}
	installed = true
	return nil
}

// finishWorktree checks that the slot path still leads to what dest
// installed, repairs the registration, and drops the stage.
func finishWorktree(s vcs.Slot, dest vcs.Dest, stage string) error {
	if err := dest.VerifyPathIdentity(s.Path); err != nil {
		return err
	}
	if err := repairRegistration(stage, s.Path, s.Primary); err != nil {
		return err
	}
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	return dest.VerifyPathIdentity(s.Path)
}

// repairRegistration points the cache's administrative entry for the
// worktree staged at stage at its installed path, dest. The entry is the one
// stage's .git names, and it must lie inside the cache's worktree
// administration directory.
func repairRegistration(stage, dest, cachePath string) error {
	pointer, err := os.ReadFile(filepath.Join(stage, ".git"))
	if err != nil {
		return fmt.Errorf("read staged worktree gitdir: %w", err)
	}
	line := strings.TrimSpace(string(pointer))
	if !strings.HasPrefix(line, "gitdir: ") {
		return fmt.Errorf("staged worktree .git has invalid gitdir pointer")
	}
	adminPath := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
	if !filepath.IsAbs(adminPath) {
		adminPath = filepath.Join(stage, adminPath)
	}
	adminPath, err = filepath.EvalSymlinks(adminPath)
	if err != nil {
		return fmt.Errorf("resolve staged worktree gitdir: %w", err)
	}
	worktreesRoot, err := filepath.EvalSymlinks(filepath.Join(cachePath, ".git", "worktrees"))
	if err != nil {
		return fmt.Errorf("resolve worktree administration root: %w", err)
	}
	if !strictlyInside(worktreesRoot, adminPath) {
		return fmt.Errorf("staged worktree gitdir %q escapes administration root %q", adminPath, worktreesRoot)
	}
	controlPath := filepath.Join(adminPath, "gitdir")
	rel, err := filepath.Rel(worktreesRoot, controlPath)
	if err != nil {
		return err
	}
	control, err := copierx.OpenGuardedPath(worktreesRoot, worktreesRoot, filepath.ToSlash(rel), nil)
	if err != nil {
		return fmt.Errorf("open worktree administration link: %w", err)
	}
	defer func() { _ = control.Close() }()
	return control.WriteFile([]byte(filepath.Join(dest, ".git")+"\n"), 0o644)
}

// strictlyInside reports whether child lies below parent, not at it.
func strictlyInside(parent, child string) bool {
	parent, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	child, err = filepath.Abs(child)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// addClone clones the slot's base from Slot.CloneURL into a staging
// directory, under the remote name Slot.BaseRemote, and installs it through
// dest once configure has run on it.
func (d Driver) addClone(ctx context.Context, s vcs.Slot, dest vcs.Dest, configure func(context.Context, string) error) error {
	stageRoot, err := os.MkdirTemp("", "angee-clone-stage-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	stage := filepath.Join(stageRoot, "source")
	if err := d.git.CloneRef(ctx, s.CloneURL, stage, s.BaseRef, s.BaseRemote); err != nil {
		return err
	}
	if configure != nil {
		if err := configure(ctx, stage); err != nil {
			return err
		}
	}
	return dest.ReplaceFrom(ctx, stage)
}
