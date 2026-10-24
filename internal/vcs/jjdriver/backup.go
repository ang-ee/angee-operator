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

// BackupIdentity pins the native history and operation required by cold recovery.
type BackupIdentity struct {
	CommitID    string
	ChangeID    string
	OperationID string
	Workspaces  []string
}

// BackupEvidence reads immutable JJ identity without snapshotting or updating
// the working copy. All reads are pinned to the first observed operation.
func (d Driver) BackupEvidence(ctx context.Context, slot string, remoteOnly bool) (BackupIdentity, error) {
	operation, err := d.jj.run(ctx, slot, []string{"-R", ".", "--ignore-working-copy", "op", "log", "--limit", "1", "--no-graph", "-T", operationTemplate})
	if err != nil {
		return BackupIdentity{}, err
	}
	op := strings.TrimSpace(string(operation))
	commit, err := d.jj.read(ctx, slot, op, []string{"log", "--no-graph", "-r", "@", "-T", commitsTemplate}...)
	if err != nil {
		return BackupIdentity{}, err
	}
	entries, err := decodeLines[commitEntry](commit)
	if err != nil {
		return BackupIdentity{}, err
	}
	if len(entries) != 1 {
		return BackupIdentity{}, errors.New("JJ capture requires one unambiguous working-copy commit")
	}
	if remoteOnly {
		local, err := d.jj.read(ctx, slot, op, "log", "--no-graph", "-r", "all() ~ ::remote_bookmarks()", "-T", countTemplate)
		if err != nil {
			return BackupIdentity{}, err
		}
		if len(local) > 0 {
			return BackupIdentity{}, &git.BackupPublicationError{Reason: "remote-only capture refuses unpublished JJ commits; use portable capture"}
		}
	}
	return BackupIdentity{CommitID: entries[0].Commit.CommitID, ChangeID: entries[0].Commit.ChangeID, OperationID: op, Workspaces: entries[0].WorkingCopies}, nil
}

// BackupPlan checks JJ workspace and shared-store pointers against the declared
// source and Git plan. Reads remain pinned and never snapshot the working copy.
func (d Driver) BackupPlan(ctx context.Context, slot vcs.Slot, commonPath string, remoteOnly bool) (vcs.BackupPlan, error) {
	root := slot.Path
	resolvedStore, err := filepath.EvalSymlinks(slot.Primary)
	if err != nil {
		return vcs.BackupPlan{}, err
	}
	plan := vcs.BackupPlan{WorktreeExclude: []string{filepath.Join(root, ".jj")}}
	revision := api.BackupSourceEvidence{}
	// A shared JJ workspace points at the declared source store's .jj/repo.
	// Capture that store into a separate, relocatable component. The manifest
	// records the reconstruction layout; pointer files are never followed blindly.
	jj := filepath.Join(root, ".jj")
	if info, err := os.Lstat(jj); err == nil {
		evidence, err := d.BackupEvidence(ctx, root, remoteOnly)
		if err != nil {
			return vcs.BackupPlan{}, err
		}
		revision.JJRevision = evidence.CommitID
		revision.JJChangeID = evidence.ChangeID
		revision.JJOperation = evidence.OperationID
		revision.JJWorkspaces = evidence.Workspaces

		if !info.IsDir() {
			return vcs.BackupPlan{}, vcs.BackupRefusal("JJ workspace metadata must be a real directory")
		}
		repo := filepath.Join(jj, "repo")
		if info, err := os.Lstat(repo); err != nil {
			return vcs.BackupPlan{}, err
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
		gitTarget := filepath.Join(repo, "store/git_target")
		if data, err := os.ReadFile(gitTarget); err == nil {
			target := strings.TrimSpace(string(data))
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(gitTarget), target)
			}
			target, err = filepath.EvalSymlinks(target)
			if err != nil || target != commonPath {
				return vcs.BackupPlan{}, vcs.BackupRefusal("JJ backing Git target differs from the verified source object store")
			}
		}
		plan.Components = []vcs.BackupComponent{{Name: "jj-workspace", Root: jj, Exclude: []string{filepath.Join(jj, "repo")}}, {Name: "jj-repo", Root: repo}}
	} else if !os.IsNotExist(err) {
		return vcs.BackupPlan{}, err
	}
	plan.Evidence = revision
	return plan, nil
}
