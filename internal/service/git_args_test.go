package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Refs, branches and remotes reach the service from API requests. Each verb
// hands them to git as arguments only, so one shaped like an option is refused
// instead of run: --exec runs a command on rebase, --mirror rewrites every
// branch on the remote, and --output writes a file on the operator's host.
func TestWorkspaceSourceVerbsReadRequestValuesAsArgumentsOnly(t *testing.T) {
	t.Setenv("LC_ALL", "C") // git's messages are matched below
	ctx := context.Background()
	platform, workspaceName, slotPath, cache := setupGitWorkspace(t)
	remote := filepath.Join(filepath.Dir(cache), "remote.git")
	base := t.TempDir()
	// A tracked branch with a commit of its own is what a rebase replays, so
	// an injected --exec would run.
	runGit(t, slotPath, "branch", "--set-upstream-to=fork/main")
	commitFile(t, slotPath, "change.txt")

	executed := filepath.Join(base, "executed")
	if result, err := platform.WorkspaceSourceRebase(ctx, workspaceName, "app", "--exec=touch "+executed); err == nil && result.OK {
		t.Errorf("WorkspaceSourceRebase() with an option as ref = %#v, want it refused", result)
	}
	if _, err := os.Stat(executed); !os.IsNotExist(err) {
		t.Errorf("WorkspaceSourceRebase() ran --exec (stat error = %v)", err)
	}
	if result, err := platform.WorkspaceSourceMerge(ctx, workspaceName, "app", "--exec=x"); err == nil || !strings.Contains(err.Error(), "not something we can merge") {
		t.Errorf("WorkspaceSourceMerge() with an option as ref = %#v, %v, want git to refuse it as a revision", result, err)
	}

	if _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", "--mirror"); err == nil {
		t.Error("WorkspaceSourcePush() accepted an option as its ref")
	}
	if _, err := platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "fork", "--mirror"); err == nil {
		t.Error("WorkspaceSourcePublish() accepted an option as its branch")
	}
	// Publish takes a branch name, so refspec syntax is refused too: ":keep"
	// would delete the remote's keep branch.
	runGit(t, cache, "push", "fork", "main:keep")
	if _, err := platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "fork", ":keep"); err == nil {
		t.Error("WorkspaceSourcePublish() accepted a refspec as its branch")
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{"keep", "main"}) {
		t.Errorf("remote heads = %v, want keep and main (nothing mirrored or deleted)", heads)
	}

	written := filepath.Join(base, "written.diff")
	if _, err := platform.WorkspaceSourceDiff(ctx, workspaceName, "app", "--output="+written); err == nil {
		t.Error("WorkspaceSourceDiff() accepted an option as its ref")
	}
	if _, err := platform.SourceDiff(ctx, "app", "--output="+written); err == nil {
		t.Error("SourceDiff() accepted an option as its ref")
	}
	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Errorf("a diff wrote %s (stat error = %v)", written, err)
	}
}
