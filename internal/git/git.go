// Package git provides a hybrid git client.
//
// Read-only queries (status, refs) are implemented with go-git where possible
// so they avoid spawning a process per call. Config, ahead/behind, and write or
// network operations (clone, fetch, pull, push, merge, rebase, worktree add)
// shell out to the git CLI so they inherit the user's credential helpers, SSH
// config, config includes, and upstream git's precedence and graph semantics.
package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

type Client struct {
	Bin string
}

func New() Client {
	return Client{Bin: "git"}
}

// Run executes an arbitrary git command. Kept as an escape hatch for callers
// that need git operations not exposed by the typed API (e.g. checkout in
// templates.go). Prefer adding a typed method over using Run.
func (c Client) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return out, nil
}

func (c Client) runText(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := c.Run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// open returns the go-git repository at dir. Discovers the repo via parent
// walk so worktrees and bare-checked-out directories work.
func openRepo(dir string) (*gogit.Repository, error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true, EnableDotGitCommonDir: true})
	if err != nil {
		return nil, fmt.Errorf("open git repo at %s: %w", dir, err)
	}
	return repo, nil
}

// --- Network / write operations: shell out to git CLI for auth correctness ---

func (c Client) Clone(ctx context.Context, repo, dest string, args ...string) error {
	cmdArgs := append([]string{"clone"}, args...)
	cmdArgs = append(cmdArgs, repo, dest)
	_, err := c.Run(ctx, "", cmdArgs...)
	return err
}

func (c Client) CloneRef(ctx context.Context, repo, dest, ref string) error {
	args := []string{}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	return c.Clone(ctx, repo, dest, args...)
}

func (c Client) Fetch(ctx context.Context, dir string) error {
	_, err := c.Run(ctx, dir, "fetch", "--all", "--prune")
	return err
}

func (c Client) Merge(ctx context.Context, dir, ref string) error {
	_, err := c.Run(ctx, dir, "merge", "--no-edit", ref)
	return err
}

func (c Client) Rebase(ctx context.Context, dir, ref string) error {
	_, err := c.Run(ctx, dir, "rebase", ref)
	return err
}

func (c Client) WorktreeAdd(ctx context.Context, repoDir, dest, ref string) error {
	args := []string{"worktree", "add", dest}
	if ref != "" {
		args = append(args, ref)
	}
	_, err := c.Run(ctx, repoDir, args...)
	return err
}

func (c Client) WorktreeAddBranch(ctx context.Context, repoDir, dest, branch, ref string) error {
	args := []string{"worktree", "add"}
	if branch != "" {
		args = append(args, "-b", branch)
	}
	args = append(args, dest)
	if ref != "" {
		args = append(args, ref)
	}
	_, err := c.Run(ctx, repoDir, args...)
	return err
}

// WorktreeRemove deregisters the worktree at dest and deletes its working
// tree. It targets only dest, so sibling worktrees of repoDir are left intact;
// --force lets git drop the worktree even when it holds uncommitted changes. It
// is used to reclaim a populated leftover worktree from a failed create before
// re-adding the path, and to roll back worktrees a failed create materialized.
// The doubled --force drops the worktree even when it is locked, and on current
// git even when its working tree has already gone missing, since reclaim and
// rollback always mean to discard the leftover. The missing-registration
// recovery in materializeWorkspaceSource still routes through WorktreePrune, the
// portable recovery for older git that refuses to remove a missing working tree.
func (c Client) WorktreeRemove(ctx context.Context, repoDir, dest string) error {
	_, err := c.Run(ctx, repoDir, "worktree", "remove", "--force", "--force", dest)
	return err
}

// WorktreePrune clears administrative entries for worktrees whose working tree
// has gone missing, so one can be re-added at a path git still reports as
// "missing but already registered". It is git's canonical recovery for a
// prunable registration (WorktreeRemove rejects a missing working tree). It
// acts on every prunable entry in repoDir, so callers invoke it only when an
// add actually conflicts, not preemptively.
func (c Client) WorktreePrune(ctx context.Context, repoDir string) error {
	_, err := c.Run(ctx, repoDir, "worktree", "prune")
	return err
}

// WorktreeRegistered reports whether repoDir has a worktree registered at dest.
func (c Client) WorktreeRegistered(ctx context.Context, repoDir, dest string) (bool, error) {
	out, err := c.runText(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want, err := canonicalWorktreePath(dest)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path, err := canonicalWorktreePath(strings.TrimPrefix(line, "worktree "))
		if err == nil && path == want {
			return true, nil
		}
	}
	return false, nil
}

func canonicalWorktreePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return filepath.Clean(abs), nil
	}
	return filepath.Clean(filepath.Join(parent, filepath.Base(abs))), nil
}

func (c Client) Pull(ctx context.Context, dir string) error {
	_, err := c.Run(ctx, dir, "pull", "--ff-only")
	return err
}

func (c Client) Push(ctx context.Context, dir string, ref string) error {
	args := []string{"push"}
	if ref != "" {
		remote, err := c.PushRemote(ctx, dir)
		if err != nil {
			return err
		}
		args = append(args, remote, ref)
	}
	_, err := c.Run(ctx, dir, args...)
	return err
}

func (c Client) PushSetUpstream(ctx context.Context, dir string, ref string) error {
	if ref == "" {
		return c.Push(ctx, dir, ref)
	}
	remote, err := c.PushRemote(ctx, dir)
	if err != nil {
		return err
	}
	_, err = c.Run(ctx, dir, "push", "-u", remote, ref)
	return err
}

// --- Read-only queries: go-git ---

func (c Client) RefExists(ctx context.Context, dir, ref string) bool {
	if strings.TrimSpace(ref) == "" {
		return false
	}
	repo, err := openRepo(dir)
	if err != nil {
		_, err := c.Run(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		return err == nil
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil || hash == nil {
		return false
	}
	if _, err := repo.CommitObject(*hash); err != nil {
		return false
	}
	return true
}

func (c Client) SyncBaseRef(ctx context.Context, dir, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("base ref is empty")
	}
	remotes, err := c.Remotes(ctx, dir)
	if err != nil {
		return "", err
	}
	candidates := []string{}
	if strings.HasPrefix(ref, "refs/") || remoteQualifiedRef(ref, remotes) {
		candidates = append(candidates, ref)
	} else {
		for _, remote := range remotes {
			if remote == "origin" {
				candidates = append([]string{remote + "/" + ref}, candidates...)
			} else {
				candidates = append(candidates, remote+"/"+ref)
			}
		}
		candidates = append(candidates, ref)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if c.RefExists(ctx, dir, candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("base ref %q was not found after fetch", ref)
}

func remoteQualifiedRef(ref string, remotes []string) bool {
	for _, remote := range remotes {
		if strings.HasPrefix(ref, remote+"/") {
			return true
		}
	}
	return false
}

func (c Client) CurrentRef(ctx context.Context, dir string) (string, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return c.currentRefCLI(ctx, dir)
	}
	head, err := repo.Head()
	if err != nil {
		return c.currentRefCLI(ctx, dir)
	}
	if head.Name().IsBranch() {
		return head.Name().Short(), nil
	}
	return shortHash(head.Hash().String()), nil
}

func (c Client) CurrentBranch(ctx context.Context, dir string) (string, bool, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return c.currentBranchCLI(ctx, dir)
	}
	head, err := repo.Head()
	if err != nil {
		// Detached or no commits yet: not an error for this query.
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return "", false, nil
		}
		return c.currentBranchCLI(ctx, dir)
	}
	if !head.Name().IsBranch() {
		return "", false, nil
	}
	return head.Name().Short(), true, nil
}

func (c Client) Upstream(ctx context.Context, dir string) (string, bool, error) {
	branch, ok, err := c.CurrentBranch(ctx, dir)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	repo, err := openRepo(dir)
	if err != nil {
		return c.upstreamCLI(ctx, dir)
	}
	cfg, err := repo.Config()
	if err != nil {
		return c.upstreamCLI(ctx, dir)
	}
	br, ok := cfg.Branches[branch]
	if !ok || br.Remote == "" || br.Merge == "" {
		return "", false, nil
	}
	mergeShort := strings.TrimPrefix(string(br.Merge), "refs/heads/")
	return br.Remote + "/" + mergeShort, true, nil
}

func (c Client) AheadCount(ctx context.Context, dir, base string) (int, error) {
	ahead, _, err := c.AheadBehind(ctx, dir, base)
	return ahead, err
}

func (c Client) AheadBehind(ctx context.Context, dir, base string) (ahead int, behind int, err error) {
	return c.aheadBehindCLI(ctx, dir, base)
}

func (c Client) Config(ctx context.Context, dir, key string) (string, bool, error) {
	return c.configCLI(ctx, dir, key)
}

func (c Client) configCLI(ctx context.Context, dir, key string) (string, bool, error) {
	value, err := c.runText(ctx, dir, "config", "--get", key)
	if err != nil || value == "" {
		return "", false, nil
	}
	return value, true, nil
}

func (c Client) Remotes(ctx context.Context, dir string) ([]string, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return c.remotesCLI(ctx, dir)
	}
	remotes, err := repo.Remotes()
	if err != nil {
		return c.remotesCLI(ctx, dir)
	}
	names := make([]string, 0, len(remotes))
	for _, r := range remotes {
		names = append(names, r.Config().Name)
	}
	return names, nil
}

func (c Client) PushRemote(ctx context.Context, dir string) (string, error) {
	branch, hasBranch, err := c.CurrentBranch(ctx, dir)
	if err != nil {
		return "", err
	}
	if hasBranch {
		if remote, ok, err := c.Config(ctx, dir, "branch."+branch+".pushRemote"); err != nil {
			return "", err
		} else if ok {
			return remote, nil
		}
	}
	if remote, ok, err := c.Config(ctx, dir, "remote.pushDefault"); err != nil {
		return "", err
	} else if ok {
		return remote, nil
	}
	if hasBranch {
		if remote, ok, err := c.Config(ctx, dir, "branch."+branch+".remote"); err != nil {
			return "", err
		} else if ok {
			return remote, nil
		}
	}
	remotes, err := c.Remotes(ctx, dir)
	if err != nil {
		return "", err
	}
	for _, remote := range remotes {
		if remote == "origin" {
			return remote, nil
		}
	}
	if len(remotes) == 1 {
		return remotes[0], nil
	}
	if len(remotes) == 0 {
		return "", fmt.Errorf("no git remotes configured")
	}
	if hasBranch {
		return "", fmt.Errorf("multiple git remotes configured; set branch.%s.pushRemote or remote.pushDefault", branch)
	}
	return "", fmt.Errorf("multiple git remotes configured; set remote.pushDefault")
}

func (c Client) Dirty(ctx context.Context, dir string) (bool, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return c.dirtyCLI(ctx, dir)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return c.dirtyCLI(ctx, dir)
	}
	st, err := wt.Status()
	if err != nil {
		return c.dirtyCLI(ctx, dir)
	}
	return !st.IsClean(), nil
}

func (c Client) currentRefCLI(ctx context.Context, dir string) (string, error) {
	branch, err := c.runText(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil && branch != "" {
		return branch, nil
	}
	hash, err := c.runText(ctx, dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return hash, nil
}

func (c Client) currentBranchCLI(ctx context.Context, dir string) (string, bool, error) {
	branch, err := c.runText(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil && branch != "" {
		return branch, true, nil
	}
	if _, repoErr := c.runText(ctx, dir, "rev-parse", "--git-dir"); repoErr != nil {
		return "", false, err
	}
	return "", false, nil
}

func (c Client) upstreamCLI(ctx context.Context, dir string) (string, bool, error) {
	_, ok, err := c.CurrentBranch(ctx, dir)
	if err != nil || !ok {
		return "", false, err
	}
	upstream, err := c.runText(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil || upstream == "" {
		return "", false, nil
	}
	return upstream, true, nil
}

func (c Client) aheadBehindCLI(ctx context.Context, dir, base string) (int, int, error) {
	out, err := c.runText(ctx, dir, "rev-list", "--left-right", "--count", base+"...HEAD")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("git rev-list count returned %q", out)
	}
	behind, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse behind count %q: %w", fields[0], err)
	}
	ahead, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse ahead count %q: %w", fields[1], err)
	}
	return ahead, behind, nil
}

func (c Client) remotesCLI(ctx context.Context, dir string) ([]string, error) {
	out, err := c.runText(ctx, dir, "remote")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Fields(out), nil
}

func (c Client) dirtyCLI(ctx context.Context, dir string) (bool, error) {
	out, err := c.runText(ctx, dir, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}
