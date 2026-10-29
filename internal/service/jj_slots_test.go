package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
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
// vcs.ErrUnsupported. So does workspace push over a workspace that holds one
// whose manifest names mode jj, which would otherwise skip it. A slot with no
// mode under `vcs: jj` is a jj slot too for the per-slot verbs; workspace
// push walks only the slots that name a mode (plan Q7), so it skips that one
// and reaches the worktree slot, which is not on disk.
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
			{"diff", "", func() error { _, err := platform.WorkspaceSourceDiff(ctx, "ws", "work", ""); return err }},
			{"workspace push", "workspace.push", func() error { _, err := platform.WorkspacePush(ctx, "ws", ""); return err }},
		} {
			err := tc.call()
			opErr := AsOperationError(err)
			if vcsJJ && tc.verb == "workspace push" {
				if opErr == nil || opErr.Cause != CauseSlotMissing || opErr.Slot != "app" {
					t.Errorf("vcs jj, workspace push: error = %v (%+v), want the slot with no mode skipped and app's slot_missing", err, opErr)
				}
				continue
			}
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

// onDisk makes the slots of jjVerbStack's workspace look materialized, with
// no git or jj behind them: each verb below refuses before it would run one.
func onDisk(t *testing.T, platform *Platform) {
	t.Helper()
	ws := filepath.Join(platform.Root(), "workspaces", "ws")
	mustMkdirAll(t, filepath.Join(ws, "app", ".git"))
	mustMkdirAll(t, filepath.Join(ws, "work", ".jj"))
}

// merge-abort, rebase-abort and rebase-continue on a jj slot, with or without
// a mode of its own, are the caller's invalid input: a jj slot never has a
// merge or rebase in progress. The jj driver says so, before it runs jj.
func TestJJSlotHasNoOperationInProgress(t *testing.T) {
	for _, vcsJJ := range []bool{false, true} {
		platform := jjVerbStack(t, vcsJJ)
		onDisk(t, platform)
		ctx := context.Background()
		for verb, call := range map[string]func() error{
			"merge-abort":     func() error { _, err := platform.WorkspaceSourceMergeAbort(ctx, "ws", "work"); return err },
			"rebase-abort":    func() error { _, err := platform.WorkspaceSourceRebaseAbort(ctx, "ws", "work"); return err },
			"rebase-continue": func() error { _, err := platform.WorkspaceSourceRebaseContinue(ctx, "ws", "work"); return err },
		} {
			err := call()
			var invalid *InvalidInputError
			opErr := AsOperationError(err)
			if opErr == nil || opErr.Code != CodeInvalidInput || opErr.Cause != "" || !errors.As(err, &invalid) || !errors.Is(err, vcs.ErrUnsupported) ||
				!strings.Contains(err.Error(), verb+": a jj slot never has a merge or rebase in progress") || opErr.Operation != "workspace.source-"+verb {
				t.Errorf("vcs jj %v, %s: error = %v (%+v), want INVALID_INPUT saying a jj slot never has an operation in progress", vcsJJ, verb, err, opErr)
			}
		}
	}
}

// An explicit merge of a workspace that has a slot naming mode jj is refused
// before any slot is read (plan D11): rebasing when a merge was asked for
// would be worse than refusing. With no method, or by rebase, the jj slot
// is not refused; here the git slot's check then fails, with no git to run.
func TestSyncBaseByMergeRefusesAJJSlot(t *testing.T) {
	platform := jjVerbStack(t, false)
	platform.jjProbe = supportedJJ
	onDisk(t, platform)
	ctx := context.Background()
	_, err := platform.WorkspaceSyncBase(ctx, "ws", "merge")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeInvalidInput || opErr.Slot != "work" || opErr.Workspace != "ws" || opErr.Operation != "workspace.sync-base" ||
		!errors.Is(err, vcs.ErrUnsupported) || !strings.HasPrefix(err.Error(), `Sync base for workspace "ws": source slot "work" cannot be synced by merge (a jj slot is synced by rebasing`) {
		t.Fatalf("WorkspaceSyncBase(merge) error = %v (%+v), want INVALID_INPUT naming the jj slot", err, opErr)
	}
	for _, method := range []string{"", "rebase"} {
		_, err := platform.WorkspaceSyncBase(ctx, "ws", method)
		if opErr := AsOperationError(err); opErr == nil || opErr.Code == CodeInvalidInput || errors.Is(err, vcs.ErrUnsupported) || !strings.Contains(err.Error(), "git is not installed") {
			t.Errorf("WorkspaceSyncBase(%q) error = %v (%+v), want the jj slot let through to the git slot's check", method, err, opErr)
		}
	}
}

// A machine that cannot run jj slots refuses sync-base and rebase of one as
// jj_unavailable, before it fetches anything, rather than fail a fetch with
// no jj to run.
func TestJJSlotVerbsNeedJJ(t *testing.T) {
	platform := jjVerbStack(t, false)
	onDisk(t, platform)
	platform.jjProbe = func(context.Context) jjdriver.Capabilities {
		return jjdriver.Capabilities{JJErr: errors.New("jj not found"), Git: "git version 2.50.1"}
	}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"sync-base": func() error { _, err := platform.WorkspaceSyncBase(ctx, "ws", ""); return err },
		"rebase":    func() error { _, err := platform.WorkspaceSourceRebase(ctx, "ws", "work", "main@origin"); return err },
	} {
		if opErr := AsOperationError(call()); opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseJJUnavailable {
			t.Errorf("%s error = %+v, want PRECONDITION_FAILED jj_unavailable", name, opErr)
		}
	}
}
