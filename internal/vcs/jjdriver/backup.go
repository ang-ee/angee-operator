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

var _ vcs.BackupLayer = Driver{}

// jjRefs is the namespace jj keeps refs of its own under in a colocated Git
// store: the keep refs that stop Git collecting its commits, and its record
// of remote tags.
const jjRefs = "refs/jj/"

// BackupIdentity pins the native history and operation required by cold recovery.
type BackupIdentity struct {
	CommitID    string
	ChangeID    string
	OperationID string
	Workspaces  []string
	// Published are the commits a remote-only verdict counted as on the
	// remotes: the targets of every remote bookmark and remote tag. Only that
	// verdict reads them.
	Published []string
}

// BackupEvidence reads immutable JJ identity without snapshotting or updating
// the working copy. All reads are pinned to the first observed operation.
// With remoteOnly it refuses any visible commit a capture from the remotes
// alone would lose (the unpublished revset), the working-copy commits of
// every workspace included unless jj would discard them itself.
func (d Driver) BackupEvidence(ctx context.Context, slot string, remoteOnly bool) (BackupIdentity, error) {
	operation, err := d.jj.run(ctx, slot, []string{"-R", ".", "--ignore-working-copy", "op", "log", "--limit", "1", "--no-graph", "-T", operationTemplate})
	if err != nil {
		return BackupIdentity{}, err
	}
	op := strings.TrimSpace(string(operation))
	entries, err := d.commits(ctx, slot, op, workingCopy)
	if err != nil {
		return BackupIdentity{}, err
	}
	if len(entries) != 1 {
		return BackupIdentity{}, errors.New("JJ capture requires one unambiguous working-copy commit")
	}
	identity := BackupIdentity{CommitID: entries[0].Commit.CommitID, ChangeID: entries[0].Commit.ChangeID, OperationID: op, Workspaces: entries[0].WorkingCopies}
	if remoteOnly {
		local, err := d.jj.read(ctx, slot, op, "log", "--no-graph", "--limit", "1", "-r", unpublished.String(), "-T", countTemplate)
		if err != nil {
			return BackupIdentity{}, err
		}
		if len(local) > 0 {
			return BackupIdentity{}, &git.BackupPublicationError{Reason: "remote-only capture refuses unpublished JJ commits; use portable capture"}
		}
		anchors, err := d.commits(ctx, slot, op, published)
		if err != nil {
			return BackupIdentity{}, err
		}
		for _, anchor := range anchors {
			identity.Published = append(identity.Published, anchor.Commit.CommitID)
		}
	}
	return identity, nil
}

// BackupLayerPlan checks JJ workspace and shared-store pointers against the
// declared source and gitStore, the verified Git plan's store, before reading
// any evidence. Reads remain pinned and never snapshot the working copy. Only
// a workspace verified to keep its commits in gitStore names jj's metadata in
// it (the worktree-root .jj and refs/jj/) as Colocated, so the Git verdict
// leaves exactly that to BackupEvidence's.
func (d Driver) BackupLayerPlan(ctx context.Context, slot vcs.Slot, gitStore string, remoteOnly bool) (vcs.BackupPlan, error) {
	root := slot.Path
	resolvedStore, err := filepath.EvalSymlinks(slot.Primary)
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	plan := vcs.BackupPlan{WorktreeExclude: []string{filepath.Join(root, ".jj")}}
	// A shared JJ workspace points at the declared source store's .jj/repo.
	// Capture that store into a separate, relocatable component. The manifest
	// records the reconstruction layout; pointer files are never followed blindly.
	jj := filepath.Join(root, ".jj")
	info, err := os.Lstat(jj)
	if os.IsNotExist(err) {
		return plan, nil
	}
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	if !info.IsDir() {
		return vcs.BackupPlan{}, vcs.BackupRefusal("JJ workspace metadata must be a real directory")
	}
	repo := filepath.Join(jj, "repo")
	if info, err := os.Lstat(repo); err != nil {
		return vcs.BackupPlan{}, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return vcs.BackupPlan{}, vcs.BackupRefusal("JJ repo must be a real directory or a pointer file")
	} else if info.Mode().IsRegular() {
		data, err := os.ReadFile(repo)
		if err != nil {
			return vcs.BackupPlan{}, err
		}
		target := strings.TrimSpace(string(data))
		if !filepath.IsAbs(target) {
			target = filepath.Join(jj, target)
		}
		repo, err = filepath.EvalSymlinks(target)
		if err != nil {
			return vcs.BackupPlan{}, err
		}
		expected, err := filepath.EvalSymlinks(filepath.Join(resolvedStore, ".jj/repo"))
		if err != nil || repo != expected {
			return vcs.BackupPlan{}, vcs.BackupRefusal("JJ repo pointer does not match the declared source store")
		}
	}
	if !vcs.BackupWithin(root, repo) && !vcs.BackupWithin(resolvedStore, repo) {
		return vcs.BackupPlan{}, vcs.BackupRefusal("JJ repository escapes declared source store")
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
			return vcs.BackupPlan{}, vcs.BackupRefusal("JJ backing Git target differs from the verified source object store")
		}
		backed = true
	}
	evidence, err := d.BackupEvidence(ctx, root, remoteOnly)
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	plan.Components = []vcs.BackupComponent{{Name: "jj-workspace", Root: jj, Exclude: []string{filepath.Join(jj, "repo")}}, {Name: "jj-repo", Root: repo}}
	if backed {
		plan.Colocated = &vcs.ColocatedMetadata{Dirs: []string{jj}, GitRefs: []string{jjRefs}, Published: evidence.Published}
	}
	plan.Evidence = api.BackupSourceEvidence{JJRevision: evidence.CommitID, JJChangeID: evidence.ChangeID, JJOperation: evidence.OperationID, JJWorkspaces: evidence.Workspaces}
	return plan, nil
}
