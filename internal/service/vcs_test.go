package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// fakeDriver answers for a slot without running anything, so a test can tell
// that the service asked the driver rather than git.
type fakeDriver struct {
	status vcs.Status
	err    error
	diff   []api.DiffFile
	slots  []vcs.Slot
}

func (f *fakeDriver) Status(_ context.Context, s vcs.Slot) (vcs.Status, error) {
	f.slots = append(f.slots, s)
	return f.status, f.err
}

func (f *fakeDriver) Diff(_ context.Context, s vcs.Slot, _ string) ([]api.DiffFile, error) {
	f.slots = append(f.slots, s)
	return f.diff, f.err
}

// Slot status, the destroy guard and slot diffs read the slot through its
// driver, which is handed the manifest's facts about the slot.
func TestWorkspaceSlotReadsDispatchToTheSlotDriver(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, _, _ := setupGitWorkspace(t)
	fake := &fakeDriver{
		status: vcs.Status{CurrentRef: workspaceName, Ahead: 2, UnpushedReason: "held by the fake"},
		diff:   []api.DiffFile{{NewPath: "fake.txt"}},
	}
	platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }

	status, err := platform.WorkspaceStatus(ctx, workspaceName)
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if len(status.Sources) != 1 {
		t.Fatalf("WorkspaceStatus() sources = %#v, want one slot", status.Sources)
	}
	got := status.Sources[0]
	if got.State != "ahead" || got.Pushed || got.UnpushedReason != "held by the fake" || got.CurrentRef != workspaceName || got.Ahead != 2 {
		t.Fatalf("slot status = %#v, want the fake driver's ahead, unpushed status", got)
	}
	files, err := platform.WorkspaceSourceDiff(ctx, workspaceName, "app", "")
	if err != nil || len(files) != 1 || files[0].NewPath != "fake.txt" {
		t.Fatalf("WorkspaceSourceDiff() = %#v, %v, want the fake driver's diff", files, err)
	}
	if err := platform.WorkspaceDestroy(ctx, workspaceName, false); err == nil || !strings.Contains(err.Error(), "app (held by the fake)") {
		t.Fatalf("WorkspaceDestroy() error = %v, want the fake driver's unpushed reason", err)
	}

	want := vcs.Slot{Path: filepath.Join(platform.Root(), "workspaces", workspaceName, "app"), Branch: workspaceName, BaseRef: "main"}
	if len(fake.slots) != 3 {
		t.Fatalf("driver was asked about %d slots, want 3 (status, diff, destroy guard)", len(fake.slots))
	}
	for _, slot := range fake.slots {
		if slot != want {
			t.Fatalf("driver slot = %#v, want %#v", slot, want)
		}
	}
}

// A driver that fails shows the slot as an error with what it read, and
// blocks destroy, unless it failed only to count: then its pushed verdict
// decides, so a slot whose base was deleted can still be let go.
func TestWorkspaceSlotDriverFailures(t *testing.T) {
	failure := errors.New("driver failed")
	for _, tc := range []struct {
		name        string
		status      vcs.Status
		err         error
		wantDestroy string // "" when destroy succeeds
	}{
		{
			name:        "read error",
			status:      vcs.Status{CurrentRef: "feature-a", Upstream: "origin/gone"},
			err:         failure,
			wantDestroy: "driver failed",
		},
		{
			name:   "count error, nothing held",
			status: vcs.Status{CurrentRef: "feature-a", Upstream: "origin/gone", Pushed: true},
			err:    &vcs.CountError{Err: failure},
		},
		{
			name:        "count error, commits held",
			status:      vcs.Status{CurrentRef: "feature-a", Upstream: "origin/gone", UnpushedReason: "held by the fake"},
			err:         &vcs.CountError{Err: failure},
			wantDestroy: "app (held by the fake)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			platform, workspaceName, _, _ := setupGitWorkspace(t)
			fake := &fakeDriver{status: tc.status, err: tc.err}
			platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }

			status, err := platform.WorkspaceStatus(ctx, workspaceName)
			if err != nil {
				t.Fatalf("WorkspaceStatus() error = %v", err)
			}
			got := status.Sources[0]
			if got.State != "error" || got.Pushed || got.Error != "driver failed" || got.CurrentRef != "feature-a" || got.Upstream != "origin/gone" {
				t.Fatalf("slot status = %#v, want an error state with what the driver read", got)
			}
			if _, err := platform.WorkspaceSourceDiff(ctx, workspaceName, "app", ""); !errors.Is(err, failure) {
				t.Fatalf("WorkspaceSourceDiff() error = %v, want the driver's failure", err)
			}
			err = platform.WorkspaceDestroy(ctx, workspaceName, false)
			switch {
			case tc.wantDestroy == "" && err != nil:
				t.Fatalf("WorkspaceDestroy() error = %v, want the workspace let go", err)
			case tc.wantDestroy != "" && (err == nil || !strings.Contains(err.Error(), tc.wantDestroy)):
				t.Fatalf("WorkspaceDestroy() error = %v, want it refused with %q", err, tc.wantDestroy)
			}
		})
	}
}
