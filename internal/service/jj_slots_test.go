package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// jjVerbStack saves a stack whose workspace ws has a git worktree slot, app,
// and a jj slot, work: one that names mode jj, or, with vcsJJ, one that
// names no mode under `vcs: jj`. Nothing is on disk, and PATH holds no git
// or jj, so a verb that ran either fails differently.
func jjVerbStack(t *testing.T, vcsJJ bool) *Platform {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".angee")
	work := manifest.WorkspaceSource{Source: "notes", Mode: manifest.WorkspaceSourceModeJJ, Ref: "main", Subpath: "work"}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "dev",
		Sources: map[string]manifest.Source{
			"app":   {Kind: "git", Repo: "https://example.invalid/app.git", DefaultRef: "main"},
			"notes": {Kind: "git", Repo: "https://example.invalid/notes.git", DefaultRef: "main"},
		},
		Workspaces: map[string]manifest.Workspace{
			"ws": {Template: "workspaces/src", Sources: map[string]manifest.WorkspaceSource{
				"app":  {Source: "app", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "ws", Ref: "main", Subpath: "app"},
				"work": work,
			}},
		},
	}
	if vcsJJ {
		stack.VCS = manifest.VCSJJ
		work.Mode = ""
		stack.Workspaces["ws"].Sources["work"] = work
	}
	mustMkdirAll(t, root)
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return platform
}

// Every slot verb phase 8 has not implemented refuses a jj slot, before it
// runs anything, with one typed error: INVALID_INPUT, cause
// jj_slot_unsupported, the verb and the slot named, matching
// vcs.ErrUnsupported. So do workspace push and sync-base over a workspace
// that holds one, which would otherwise skip it; they name only the jj slot.
// A slot with no mode under `vcs: jj` is a jj slot too.
func TestJJSlotVerbsRefuseUntilPhase8(t *testing.T) {
	for _, vcsJJ := range []bool{false, true} {
		platform := jjVerbStack(t, vcsJJ)
		ctx := context.Background()
		for _, tc := range []struct {
			verb, operation string
			call            func() error
		}{
			{"fetch", "workspace.source-fetch", func() error { _, err := platform.WorkspaceSourceFetch(ctx, "ws", "work"); return err }},
			{"pull", "workspace.source-pull", func() error { _, err := platform.WorkspaceSourcePull(ctx, "ws", "work"); return err }},
			{"push", "workspace.source-push", func() error { _, err := platform.WorkspaceSourcePush(ctx, "ws", "work", ""); return err }},
			{"publish", "workspace.source-publish", func() error {
				_, err := platform.WorkspaceSourcePublish(ctx, "ws", "work", "", "feature")
				return err
			}},
			{"merge", "workspace.source-merge", func() error { _, err := platform.WorkspaceSourceMerge(ctx, "ws", "work", "main"); return err }},
			{"rebase", "workspace.source-rebase", func() error { _, err := platform.WorkspaceSourceRebase(ctx, "ws", "work", "main"); return err }},
			{"merge-abort", "workspace.source-merge-abort", func() error { _, err := platform.WorkspaceSourceMergeAbort(ctx, "ws", "work"); return err }},
			{"rebase-abort", "workspace.source-rebase-abort", func() error { _, err := platform.WorkspaceSourceRebaseAbort(ctx, "ws", "work"); return err }},
			{"rebase-continue", "workspace.source-rebase-continue", func() error {
				_, err := platform.WorkspaceSourceRebaseContinue(ctx, "ws", "work")
				return err
			}},
			{"diff", "", func() error { _, err := platform.WorkspaceSourceDiff(ctx, "ws", "work", ""); return err }},
			{"workspace push", "workspace.push", func() error { _, err := platform.WorkspacePush(ctx, "ws", ""); return err }},
			{"sync-base", "workspace.sync-base", func() error { _, err := platform.WorkspaceSyncBase(ctx, "ws", ""); return err }},
			{"sync-base", "workspace.sync-base", func() error { _, err := platform.WorkspaceSyncBase(ctx, "ws", "merge"); return err }},
		} {
			err := tc.call()
			opErr := AsOperationError(err)
			if opErr == nil || opErr.Code != CodeInvalidInput || opErr.Cause != CauseJJSlotUnsupported || opErr.Slot != "work" || opErr.Workspace != "ws" || opErr.Operation != tc.operation {
				t.Errorf("vcs jj %v, %s: error = %v (%+v), want INVALID_INPUT jj_slot_unsupported for slot work of ws, operation %q", vcsJJ, tc.verb, err, opErr, tc.operation)
				continue
			}
			if !errors.Is(err, vcs.ErrUnsupported) {
				t.Errorf("vcs jj %v, %s: error = %v, want it to match vcs.ErrUnsupported", vcsJJ, tc.verb, err)
			}
			if want := tc.verb + ` is not supported on jj slot "work" yet`; !strings.Contains(err.Error(), want) {
				t.Errorf("vcs jj %v, %s: error = %q, want it to say %q", vcsJJ, tc.verb, err, want)
			}
		}
	}
}
