package gitdriver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

var _ vcs.SnapshotPlanner = Driver{}

// SnapshotPlan checks native Git metadata and publication without changing the repository.
// The caller must confine each returned component to its declared filesystem boundary.
// The publication verdict runs last, once Git and the layer have verified their metadata.
func (d Driver) SnapshotPlan(ctx context.Context, slot vcs.Slot, remoteOnly bool, layer vcs.SnapshotLayer) (vcs.SnapshotPlan, error) {
	client := d.git
	root, store := slot.Path, slot.Primary
	policy := "bundle"
	if remoteOnly {
		policy = "fetch_published"
	}
	head, _, err := client.RunIsolated(ctx, root, nil, "--no-pager", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return vcs.SnapshotPlan{}, fmt.Errorf("source needs a readable Git-backed repository: %w", err)
	}
	revision := api.SnapshotSourceEvidence{GitRevision: strings.TrimSpace(string(head)), Policy: policy, Remotes: map[string]string{}}
	common, _, err := client.RunIsolated(ctx, root, nil, "--no-pager", "rev-parse", "--git-common-dir")
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	commonPath := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(root, commonPath)
	}
	commonPath, err = filepath.EvalSymlinks(commonPath)
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	resolvedStore, err := filepath.EvalSymlinks(store)
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	if !vcs.SnapshotWithin(root, commonPath) && !vcs.SnapshotWithin(resolvedStore, commonPath) {
		return vcs.SnapshotPlan{}, vcs.SnapshotRefusal("git metadata points outside this source and its declared store")
	}
	if data, err := os.ReadFile(filepath.Join(commonPath, "objects/info/alternates")); err == nil && strings.TrimSpace(string(data)) != "" {
		return vcs.SnapshotPlan{}, vcs.SnapshotRefusal("git alternates require a self-contained object store")
	}
	plan := vcs.SnapshotPlan{RepositoryRoot: commonPath, WorktreeExclude: []string{filepath.Join(root, ".git")}, Components: []vcs.SnapshotComponent{{Name: "git", Root: commonPath, Exclude: []string{filepath.Join(commonPath, "worktrees")}}}}
	worktreeGit, _, err := client.RunIsolated(ctx, root, nil, "--no-pager", "rev-parse", "--git-dir")
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	gitDir := strings.TrimSpace(string(worktreeGit))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	if !vcs.SnapshotWithin(commonPath, gitDir) {
		return vcs.SnapshotPlan{}, vcs.SnapshotRefusal("worktree Git directory escapes verified common store")
	}
	if gitDir != commonPath {
		plan.Components = append(plan.Components, vcs.SnapshotComponent{Name: "git-worktree", Root: gitDir})
	}
	names, err := client.Remotes(ctx, root)
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	for _, remote := range names {
		raw, ok, err := client.RemoteURL(ctx, root, remote)
		if err != nil {
			return vcs.SnapshotPlan{}, err
		}
		if ok {
			revision.Remotes[remote] = logctx.RedactURL(raw)
		}
	}
	plan.Evidence = revision
	var layered vcs.SnapshotPlan
	if layer != nil {
		if layered, err = layer.SnapshotLayerPlan(ctx, slot, commonPath, remoteOnly); err != nil {
			return vcs.SnapshotPlan{}, err
		}
	}
	if remoteOnly {
		colocated, err := colocatedMetadata(root, layered.Colocated)
		if err != nil {
			return vcs.SnapshotPlan{}, err
		}
		if _, err := client.PublishedSnapshot(ctx, root, colocated); err != nil {
			return vcs.SnapshotPlan{}, err
		}
	}
	return plan.Layered(layered), nil
}

// colocatedMetadata is what the Git publication verdict leaves to a verified
// colocated layer: its directories, by name, which must lie directly in the
// worktree root, its ref namespaces, and the commits its verdict counts as
// published. A layer that verified nothing leaves the verdict strict.
func colocatedMetadata(root string, own *vcs.ColocatedMetadata) (git.Colocated, error) {
	if own == nil {
		return git.Colocated{}, nil
	}
	colocated := git.Colocated{Refs: own.GitRefs, Revisions: own.Published}
	for _, dir := range own.Dirs {
		dir = filepath.Clean(dir)
		if filepath.Dir(dir) != filepath.Clean(root) {
			return git.Colocated{}, vcs.SnapshotRefusal("colocated metadata lies outside the source worktree root")
		}
		colocated.Dirs = append(colocated.Dirs, filepath.Base(dir))
	}
	return colocated, nil
}
