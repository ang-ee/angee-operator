package jjdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

var _ vcs.SnapshotLayer = Driver{}

// jjRefs is the namespace jj keeps refs of its own under in a colocated Git
// store: the keep refs that stop Git collecting its commits, and its record
// of remote tags.
const jjRefs = "refs/jj/"

// SnapshotIdentity pins the native history and operation required by cold recovery.
type SnapshotIdentity struct {
	CommitID    string
	ChangeID    string
	OperationID string
	Workspaces  []string
	// Published are the commits a remote-only verdict counted as on the
	// remotes: the targets of every remote bookmark and remote tag. Only that
	// verdict reads them.
	Published []string
}

// SnapshotEvidence reads immutable JJ identity for an Angee stack snapshot
// without asking jj to snapshot or update the working copy. All reads are
// pinned to the first observed operation.
// With remoteOnly it refuses any visible commit a snapshot from the remotes
// alone would lose (the unpublished revset), the working-copy commits of
// every workspace included unless jj would discard them itself.
func (d Driver) SnapshotEvidence(ctx context.Context, slot string, remoteOnly bool) (SnapshotIdentity, error) {
	operation, err := d.jj.run(ctx, slot, []string{"-R", ".", "--ignore-working-copy", "op", "log", "--limit", "1", "--no-graph", "-T", operationTemplate})
	if err != nil {
		return SnapshotIdentity{}, err
	}
	op := strings.TrimSpace(string(operation))
	entries, err := d.commits(ctx, slot, op, workingCopy)
	if err != nil {
		return SnapshotIdentity{}, err
	}
	if len(entries) != 1 {
		return SnapshotIdentity{}, errors.New("a stack snapshot of a jj checkout requires one unambiguous working-copy commit")
	}
	identity := SnapshotIdentity{CommitID: entries[0].Commit.CommitID, ChangeID: entries[0].Commit.ChangeID, OperationID: op, Workspaces: entries[0].WorkingCopies}
	if remoteOnly {
		local, err := d.jj.read(ctx, slot, op, "log", "--no-graph", "--limit", "1", "-r", unpublished.String(), "-T", countTemplate)
		if err != nil {
			return SnapshotIdentity{}, err
		}
		if len(local) > 0 {
			return SnapshotIdentity{}, &git.SnapshotPublicationError{Reason: "a remote-only stack snapshot refuses unpublished JJ commits; use the bundle source policy"}
		}
		anchors, err := d.commits(ctx, slot, op, published)
		if err != nil {
			return SnapshotIdentity{}, err
		}
		for _, anchor := range anchors {
			identity.Published = append(identity.Published, anchor.Commit.CommitID)
		}
	}
	return identity, nil
}

// SnapshotLayerPlan checks JJ workspace and shared-store pointers against the
// declared source and gitStore, the verified Git plan's store, before reading
// any evidence. For an Angee stack snapshot, reads remain pinned and never ask
// jj to snapshot the working copy. Only a workspace verified to keep its
// commits in gitStore names jj's metadata in it (the worktree-root .jj and
// refs/jj/) as Colocated, so the Git verdict leaves exactly that to
// SnapshotEvidence's.
func (d Driver) SnapshotLayerPlan(ctx context.Context, slot vcs.Slot, gitStore string, remoteOnly bool) (vcs.SnapshotPlan, error) {
	root := slot.Path
	resolvedStore, err := filepath.EvalSymlinks(slot.Primary)
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	plan := vcs.SnapshotPlan{WorktreeExclude: []string{filepath.Join(root, ".jj")}}
	// A shared JJ workspace points at the declared source store's .jj/repo.
	// Snapshot that store into a separate, relocatable component. The manifest
	// records the reconstruction layout; pointer files are never followed blindly.
	jj := filepath.Join(root, ".jj")
	info, err := os.Lstat(jj)
	if os.IsNotExist(err) {
		return plan, nil
	}
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	if !info.IsDir() {
		return vcs.SnapshotPlan{}, vcs.SnapshotRefusal("JJ workspace metadata must be a real directory")
	}
	repo := filepath.Join(jj, "repo")
	if info, err := os.Lstat(repo); err != nil {
		return vcs.SnapshotPlan{}, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return vcs.SnapshotPlan{}, vcs.SnapshotRefusal("JJ repo must be a real directory or a pointer file")
	} else if info.Mode().IsRegular() {
		data, err := os.ReadFile(repo)
		if err != nil {
			return vcs.SnapshotPlan{}, err
		}
		target := strings.TrimSpace(string(data))
		if !filepath.IsAbs(target) {
			target = filepath.Join(jj, target)
		}
		repo, err = filepath.EvalSymlinks(target)
		if err != nil {
			return vcs.SnapshotPlan{}, err
		}
		expected, err := filepath.EvalSymlinks(filepath.Join(resolvedStore, ".jj/repo"))
		if err != nil || repo != expected {
			return vcs.SnapshotPlan{}, vcs.SnapshotRefusal("JJ repo pointer does not match the declared source store")
		}
	}
	if !vcs.SnapshotWithin(root, repo) && !vcs.SnapshotWithin(resolvedStore, repo) {
		return vcs.SnapshotPlan{}, vcs.SnapshotRefusal("JJ repository escapes declared source store")
	}
	backed := false
	gitTarget := filepath.Join(repo, "store/git_target")
	if data, err := os.ReadFile(gitTarget); err == nil {
		target := strings.TrimSpace(string(data))
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(gitTarget), target)
		}
		target, err = filepath.EvalSymlinks(target)
		if err != nil || target != gitStore {
			return vcs.SnapshotPlan{}, vcs.SnapshotRefusal("JJ backing Git target differs from the verified source object store")
		}
		backed = true
	}
	evidence, err := d.SnapshotEvidence(ctx, root, remoteOnly)
	if err != nil {
		return vcs.SnapshotPlan{}, err
	}
	plan.Components = []vcs.SnapshotComponent{{Name: "jj-workspace", Root: jj, Exclude: []string{filepath.Join(jj, "repo")}}, {Name: "jj-repo", Root: repo}}
	if backed {
		plan.Colocated = &vcs.ColocatedMetadata{Dirs: []string{jj}, GitRefs: []string{jjRefs}, Published: evidence.Published}
	}
	plan.Evidence = api.SnapshotSourceEvidence{JJRevision: evidence.CommitID, JJChangeID: evidence.ChangeID, JJOperation: evidence.OperationID, JJWorkspaces: evidence.Workspaces}
	return plan, nil
}
