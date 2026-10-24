package gitdriver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// BackupPlan checks native Git metadata and publication without changing the repository.
// The caller must confine each returned component to its declared filesystem boundary.
func (d Driver) BackupPlan(ctx context.Context, slot vcs.Slot, remoteOnly bool) (vcs.BackupPlan, error) {
	client := d.git
	root, store := slot.Path, slot.Primary
	policy := "bundle"
	if remoteOnly {
		policy = "fetch_published"
	}
	head, _, err := client.RunIsolated(ctx, root, nil, "--no-pager", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return vcs.BackupPlan{}, fmt.Errorf("source needs a readable Git-backed repository: %w", err)
	}
	revision := api.BackupSourceEvidence{GitRevision: strings.TrimSpace(string(head)), Policy: policy, Remotes: map[string]string{}}
	common, _, err := client.RunIsolated(ctx, root, nil, "--no-pager", "rev-parse", "--git-common-dir")
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	commonPath := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(root, commonPath)
	}
	commonPath, err = filepath.EvalSymlinks(commonPath)
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	resolvedStore, err := filepath.EvalSymlinks(store)
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	if !vcs.BackupWithin(root, commonPath) && !vcs.BackupWithin(resolvedStore, commonPath) {
		return vcs.BackupPlan{}, vcs.BackupRefusal("git metadata points outside this source and its declared store")
	}
	if data, err := os.ReadFile(filepath.Join(commonPath, "objects/info/alternates")); err == nil && strings.TrimSpace(string(data)) != "" {
		return vcs.BackupPlan{}, vcs.BackupRefusal("git alternates require a self-contained object store")
	}
	if policy == "fetch_published" {
		remotes, err := client.PublishedBackup(ctx, root)
		if err != nil {
			return vcs.BackupPlan{}, err
		}
		revision.Remotes = remotes
	}
	plan := vcs.BackupPlan{RepositoryRoot: commonPath, WorktreeExclude: []string{filepath.Join(root, ".git")}, Components: []vcs.BackupComponent{{Name: "git", Root: commonPath, Exclude: []string{filepath.Join(commonPath, "worktrees")}}}}
	worktreeGit, _, err := client.RunIsolated(ctx, root, nil, "--no-pager", "rev-parse", "--git-dir")
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	gitDir := strings.TrimSpace(string(worktreeGit))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	if !vcs.BackupWithin(commonPath, gitDir) {
		return vcs.BackupPlan{}, vcs.BackupRefusal("worktree Git directory escapes verified common store")
	}
	if gitDir != commonPath {
		plan.Components = append(plan.Components, vcs.BackupComponent{Name: "git-worktree", Root: gitDir})
	}
	names, err := client.Remotes(ctx, root)
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	for _, remote := range names {
		raw, ok, err := client.RemoteURL(ctx, root, remote)
		if err != nil {
			return vcs.BackupPlan{}, err
		}
		if ok {
			revision.Remotes[remote] = logctx.RedactURL(raw)
		}
	}
	plan.Evidence = revision
	return plan, nil
}
