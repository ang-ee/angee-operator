package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The git slot verbs ask the git driver whether they may run (vcs.Driver.Ready)
// instead of the service judging the branch and the uncommitted changes, and
// keep their refusals: pull, push, workspace push and destroy refuse a slot
// off its branch with branch_mismatch, naming git's own command that puts it
// back, and pull, push and workspace push refuse uncommitted changes with
// uncommitted_changes, listing them. Codes, causes, paths, hints and
// messages are what they were before the move.
func TestGitSlotVerbsKeepTheirRefusals(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, slotPath, cache := setupGitWorkspace(t)
	runGit(t, slotPath, "switch", "-q", "-c", "codex/feature-a")

	fix := fmt.Sprintf("git -C %s switch %s", slotPath, workspaceName)
	hint := fmt.Sprintf("Switch it back with `%s`, then retry.", fix)
	mismatch := fmt.Sprintf(`workspace "feature-a" source "app" has branch mismatch: current branch/ref "codex/feature-a", expected workspace branch "feature-a" at %s. %s`, slotPath, hint)
	for _, tc := range []struct {
		verb, title string
		call        func() error
	}{
		{"pull", `Pull for workspace "feature-a" slot "app": `, func() error { _, err := platform.WorkspaceSourcePull(ctx, workspaceName, "app"); return err }},
		{"push", `Push for workspace "feature-a" slot "app": `, func() error { _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", ""); return err }},
		{"workspace push", `Push workspace "feature-a": `, func() error { _, err := platform.WorkspacePush(ctx, workspaceName, ""); return err }},
		{"destroy", "", func() error { return platform.WorkspaceDestroy(ctx, workspaceName, false) }},
	} {
		err := tc.call()
		opErr := AsOperationError(err)
		if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseBranchMismatch || opErr.Slot != "app" || opErr.Workspace != workspaceName ||
			opErr.Hint != hint || !slices.Equal(opErr.Paths, []string{slotPath}) || err.Error() != tc.title+mismatch {
			t.Errorf("%s off its branch: error = %q (%+v), want branch_mismatch %q", tc.verb, err, opErr, tc.title+mismatch)
		}
	}

	runGit(t, slotPath, "switch", "-q", workspaceName)
	defer func() { pinRepairRefusal(t, platform, workspaceName, slotPath, cache, fix, hint) }()
	mustWriteFile(t, filepath.Join(slotPath, "scratch.txt"), "wip\n")
	const uncommitted = `source slot "app" has uncommitted changes in scratch.txt. Commit or restore the changes, then retry.`
	for _, tc := range []struct {
		verb, title string
		call        func() error
	}{
		{"pull", `Pull for workspace "feature-a" slot "app": `, func() error { _, err := platform.WorkspaceSourcePull(ctx, workspaceName, "app"); return err }},
		{"push", `Push for workspace "feature-a" slot "app": `, func() error { _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", ""); return err }},
		{"workspace push", `Push workspace "feature-a": `, func() error { _, err := platform.WorkspacePush(ctx, workspaceName, ""); return err }},
	} {
		err := tc.call()
		opErr := AsOperationError(err)
		if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseUncommittedChanges || opErr.Slot != "app" ||
			opErr.Hint != "Commit or restore the changes, then retry." || !slices.Equal(opErr.Paths, []string{"scratch.txt"}) || err.Error() != tc.title+uncommitted {
			t.Errorf("%s with a change: error = %q (%+v), want uncommitted_changes %q", tc.verb, err, opErr, tc.title+uncommitted)
		}
	}
}

// pinRepairRefusal has repair cut the slot at slotPath again, off its branch
// (a post-checkout hook switches it), and pins repair's refusal: the branch
// mismatch, worded as the other verbs word it, with git's fix.
func pinRepairRefusal(t *testing.T, platform *Platform, workspaceName, slotPath, cache, fix, hint string) {
	t.Helper()
	if t.Failed() {
		return
	}
	runGit(t, cache, "worktree", "remove", "--force", slotPath)
	hooks := filepath.Join(t.TempDir(), "hooks")
	mustMkdirAll(t, hooks)
	mustWriteFile(t, filepath.Join(hooks, "post-checkout"), "#!/bin/sh\n[ \"$(git rev-parse --abbrev-ref HEAD)\" = hijacked ] || git switch -q -c hijacked\n")
	if err := os.Chmod(filepath.Join(hooks, "post-checkout"), 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	runGit(t, cache, "config", "core.hooksPath", hooks)
	_, err := platform.WorkspaceRepair(context.Background(), workspaceName)
	want := fmt.Sprintf(`workspace "feature-a" source "app" has branch mismatch: current branch/ref "hijacked", expected workspace branch "feature-a" at %s. %s`, slotPath, hint)
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), fix) {
		t.Errorf("repair off its branch: error = %q, want it to hold %q", err, want)
	}
}
