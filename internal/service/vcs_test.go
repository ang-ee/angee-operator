package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// fakeDriver answers for a slot without running anything, so a test can tell
// that the service asked the driver rather than git. Status and Diff record
// their slots in slots; every other method records a call.
type fakeDriver struct {
	status vcs.Status
	err    error
	diff   []api.DiffFile
	slots  []vcs.Slot

	calls      []fakeCall
	detached   bool             // the checkout is on no branch
	upstream   string           // the branch it tracks; "" for none
	syncResult *api.GitOpResult // SyncBase's result; nil for OK
	resolveErr error            // returned by ResolveBase
	fail       map[string]error // an error for a method by name
}

// fakeCall is one driver method the service called, with its arguments.
type fakeCall struct {
	method string
	slot   vcs.Slot
	args   []string
}

func (f *fakeDriver) call(method string, s vcs.Slot, args ...string) {
	f.calls = append(f.calls, fakeCall{method: method, slot: s, args: args})
}

func (f *fakeDriver) result(method string) api.GitOpResult {
	return api.GitOpResult{OK: true, ConflictFiles: []string{}, Message: "fake " + method}
}

func (f *fakeDriver) Status(_ context.Context, s vcs.Slot) (vcs.Status, error) {
	f.slots = append(f.slots, s)
	return f.status, f.err
}

func (f *fakeDriver) Diff(_ context.Context, s vcs.Slot, _ string) ([]api.DiffFile, error) {
	f.slots = append(f.slots, s)
	return f.diff, f.err
}

func (f *fakeDriver) BranchMismatch(_ context.Context, s vcs.Slot) (string, error) {
	f.call("BranchMismatch", s)
	return "", nil
}

func (f *fakeDriver) DirtyPaths(_ context.Context, s vcs.Slot) ([]string, error) {
	f.call("DirtyPaths", s)
	return nil, f.fail["DirtyPaths"]
}

func (f *fakeDriver) FetchURL(_ context.Context, s vcs.Slot) string {
	f.call("FetchURL", s)
	return "fake://fetch"
}

func (f *fakeDriver) PushURL(_ context.Context, s vcs.Slot, remote string) string {
	f.call("PushURL", s, remote)
	return "fake://push"
}

func (f *fakeDriver) ResolveBase(_ context.Context, s vcs.Slot) (string, error) {
	f.call("ResolveBase", s)
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	return "fake/" + s.BaseRef, nil
}

func (f *fakeDriver) Fetch(_ context.Context, s vcs.Slot) error {
	f.call("Fetch", s)
	return nil
}

func (f *fakeDriver) Pull(_ context.Context, s vcs.Slot) error {
	f.call("Pull", s)
	return nil
}

// PlanPush plans as the git driver does for a checkout with commits of its
// own: the ref, else the upstream, else refused when detached, else the
// branch to track.
func (f *fakeDriver) PlanPush(_ context.Context, s vcs.Slot, ref, branch string) (vcs.PushPlan, error) {
	f.call("PlanPush", s, ref, branch)
	switch {
	case ref != "":
		return vcs.PushPlan{Ref: ref}, nil
	case f.upstream != "":
		return vcs.PushPlan{}, nil
	case f.detached:
		return vcs.PushPlan{}, vcs.ErrDetachedWithWork
	}
	return vcs.PushPlan{Track: branch}, nil
}

func (f *fakeDriver) Push(_ context.Context, s vcs.Slot, plan vcs.PushPlan) error {
	if plan.Track != "" {
		f.call("Push", s, "-u "+plan.Track)
	} else {
		f.call("Push", s, plan.Ref)
	}
	return nil
}

func (f *fakeDriver) SyncBase(_ context.Context, s vcs.Slot, onto, method string) (api.GitOpResult, error) {
	f.call("SyncBase", s, onto, method)
	if f.syncResult != nil {
		return *f.syncResult, nil
	}
	return f.result("SyncBase"), nil
}

func (f *fakeDriver) Merge(_ context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	f.call("Merge", s, ref)
	return f.result("Merge"), nil
}

func (f *fakeDriver) Rebase(_ context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	f.call("Rebase", s, ref)
	return f.result("Rebase"), nil
}

func (f *fakeDriver) MergeAbort(_ context.Context, s vcs.Slot) (api.GitOpResult, error) {
	f.call("MergeAbort", s)
	if err := f.fail["MergeAbort"]; err != nil {
		return api.GitOpResult{}, err
	}
	return f.result("MergeAbort"), nil
}

func (f *fakeDriver) RebaseAbort(_ context.Context, s vcs.Slot) (api.GitOpResult, error) {
	f.call("RebaseAbort", s)
	return f.result("RebaseAbort"), nil
}

func (f *fakeDriver) RebaseContinue(_ context.Context, s vcs.Slot) (api.GitOpResult, error) {
	f.call("RebaseContinue", s)
	return f.result("RebaseContinue"), nil
}

// PlanPublish publishes to the intent's remote, else origin, the intent's
// branch, else the one the checkout is on (the slot's), creating it when
// detached.
func (f *fakeDriver) PlanPublish(_ context.Context, s vcs.Slot, intent vcs.PublishIntent) (vcs.PublishPlan, error) {
	f.call("PlanPublish", s, intent.Remote, intent.Branch, strconv.FormatBool(intent.Explicit))
	remote := intent.Remote
	if remote == "" {
		remote = "origin"
	}
	branch := intent.Branch
	if branch == "" && !f.detached {
		branch = s.Branch
	}
	return vcs.PublishPlan{Remote: remote, URL: "fake://" + remote, Branch: branch, Create: f.detached}, nil
}

func (f *fakeDriver) PreparePublish(_ context.Context, s vcs.Slot, plan vcs.PublishPlan) error {
	f.call("PreparePublish", s, plan.Branch, strconv.FormatBool(plan.Create))
	return nil
}

func (f *fakeDriver) Publish(_ context.Context, s vcs.Slot, plan vcs.PublishPlan) (api.GitOpResult, error) {
	f.call("Publish", s, plan.Remote, plan.Branch)
	return f.result("Publish"), nil
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
	// No git to run: every read must go through the driver.
	t.Setenv("PATH", t.TempDir())

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

	want := vcs.Slot{Path: filepath.Join(platform.Root(), "workspaces", workspaceName, "app"), Branch: workspaceName, BaseRef: "main", BaseRemote: "origin"}
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

// Every slot verb runs through the slot's driver: its preconditions read the
// slot there, and the driver carries the verb out, handed the manifest's facts
// about the slot. The calls are listed in the order the service makes them.
func TestWorkspaceSlotVerbsDispatchToTheSlotDriver(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		detached  bool
		upstream  string
		run       func(p *Platform, ws string) (string, error) // returns the result's message, if any
		want      []string
		wantMsg   string
		wantError string
	}{
		{name: "fetch", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSourceFetch(ctx, ws, "app")
			return "", err
		}, want: []string{"FetchURL", "Fetch"}},
		{name: "pull", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSourcePull(ctx, ws, "app")
			return "", err
		}, want: []string{"BranchMismatch", "DirtyPaths", "FetchURL", "Pull"}},
		{name: "push to the upstream", upstream: "origin/feature-a", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSourcePush(ctx, ws, "app", "")
			return "", err
		}, want: []string{"BranchMismatch", "DirtyPaths", "PushURL()", "PlanPush(,feature-a)", "Push()"}},
		{name: "push a new branch", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSourcePush(ctx, ws, "app", "")
			return "", err
		}, want: []string{"BranchMismatch", "DirtyPaths", "PushURL()", "PlanPush(,feature-a)", "Push(-u feature-a)"}},
		{name: "push a ref", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSourcePush(ctx, ws, "app", "v1")
			return "", err
		}, want: []string{"BranchMismatch", "DirtyPaths", "PushURL()", "PlanPush(v1,feature-a)", "Push(v1)"}},
		{name: "workspace push", upstream: "origin/feature-a", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspacePush(ctx, ws, "")
			return "", err
		}, want: []string{"BranchMismatch", "DirtyPaths", "PushURL()", "PlanPush(,feature-a)", "Push()"}},
		{name: "sync-base by default", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSyncBase(ctx, ws, "")
			return "", err
		}, want: []string{"BranchMismatch", "DirtyPaths", "FetchURL", "Fetch", "ResolveBase", "SyncBase(fake/main,)"}},
		{name: "sync-base by merge", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSyncBase(ctx, ws, "merge")
			return "", err
		}, want: []string{"BranchMismatch", "DirtyPaths", "FetchURL", "Fetch", "ResolveBase", "SyncBase(fake/main,merge)"}},
		{name: "sync-base by rebase", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSyncBase(ctx, ws, "rebase")
			return "", err
		}, want: []string{"BranchMismatch", "DirtyPaths", "FetchURL", "Fetch", "ResolveBase", "SyncBase(fake/main,rebase)"}},
		{name: "merge", run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourceMerge(ctx, ws, "app", "main")
			return result.Message, err
		}, want: []string{"Merge(main)"}, wantMsg: "fake Merge"},
		{name: "rebase", run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourceRebase(ctx, ws, "app", "main")
			return result.Message, err
		}, want: []string{"Rebase(main)"}, wantMsg: "fake Rebase"},
		{name: "merge abort", run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourceMergeAbort(ctx, ws, "app")
			return result.Message, err
		}, want: []string{"MergeAbort"}, wantMsg: "fake MergeAbort"},
		{name: "rebase abort", run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourceRebaseAbort(ctx, ws, "app")
			return result.Message, err
		}, want: []string{"RebaseAbort"}, wantMsg: "fake RebaseAbort"},
		{name: "rebase continue", run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourceRebaseContinue(ctx, ws, "app")
			return result.Message, err
		}, want: []string{"RebaseContinue"}, wantMsg: "fake RebaseContinue"},
		{name: "publish", upstream: "origin/feature-a", run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourcePublish(ctx, ws, "app", "", "")
			return result.Message, err
		}, want: []string{"PlanPublish(,feature-a,false)", "PreparePublish(feature-a,false)", "Publish(origin,feature-a)"}, wantMsg: "fake Publish"},
		{name: "publish from a detached HEAD", detached: true, run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourcePublish(ctx, ws, "app", "fork", "new")
			return result.Message, err
		}, want: []string{"PlanPublish(fork,new,true)", "PreparePublish(new,true)", "Publish(fork,new)"}, wantMsg: "fake Publish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform, workspaceName, _, _ := setupGitWorkspace(t)
			fake := &fakeDriver{detached: tc.detached, upstream: tc.upstream}
			platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
			// No git to run: every verb must go through the driver.
			t.Setenv("PATH", t.TempDir())

			msg, err := tc.run(platform, workspaceName)
			if err != nil {
				t.Fatalf("verb error = %v", err)
			}
			if msg != tc.wantMsg {
				t.Fatalf("result message = %q, want %q", msg, tc.wantMsg)
			}
			want := vcs.Slot{Path: filepath.Join(platform.Root(), "workspaces", workspaceName, "app"), Branch: workspaceName, BaseRef: "main", BaseRemote: "origin"}
			var got []string
			for _, call := range fake.calls {
				if call.slot != want {
					t.Fatalf("%s slot = %#v, want %#v", call.method, call.slot, want)
				}
				name := call.method
				if len(call.args) > 0 {
					name += "(" + strings.Join(call.args, ",") + ")"
				}
				got = append(got, name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("driver calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// A sync that stops on conflicts is merge_conflict on the files its driver
// reports, with the output it stopped with as the detail.
func TestWorkspaceSyncBaseReportsTheDriversConflict(t *testing.T) {
	platform, workspaceName, _, _ := setupGitWorkspace(t)
	fake := &fakeDriver{syncResult: &api.GitOpResult{Conflicted: true, ConflictFiles: []string{"fake.txt"}, Message: "CONFLICT (content): fake.txt\n"}}
	platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
	t.Setenv("PATH", t.TempDir())

	_, err := platform.WorkspaceSyncBase(context.Background(), workspaceName, "merge")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeGitFailed || opErr.Cause != CauseMergeConflict || !slices.Equal(opErr.Paths, []string{"fake.txt"}) ||
		opErr.Slot != "app" || opErr.Source != "app" || opErr.Detail != "CONFLICT (content): fake.txt" || opErr.GitLine != "" {
		t.Fatalf("WorkspaceSyncBase() error = %+v, want merge_conflict on the driver's files", opErr)
	}
	if want := `Sync base for workspace "feature-a": merging fake/main into source slot "app" stopped on conflicts in fake.txt. Resolve the conflicts`; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("message = %q, want it to start %q", err.Error(), want)
	}
}

// A base the driver cannot find refuses the sync as a precondition naming
// the base; any other failure to resolve it is a git failure of that step.
// Either way, no slot is synced.
func TestWorkspaceSyncBaseReportsTheDriversBaseFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		code      string
		wantStart string
	}{
		{"missing base", fmt.Errorf("base ref %q was not found after fetch: %w", "main", vcs.ErrBaseNotFound), CodePreconditionFailed,
			`Sync base for workspace "feature-a": source slot "app": base ref "main" was not found after fetch: base not found. Check the base ref "main" of source slot "app".`},
		{"other failure", errors.New("cannot read the remotes"), CodeGitFailed,
			`Sync base for workspace "feature-a": resolving the base of source slot "app" failed.`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform, workspaceName, _, _ := setupGitWorkspace(t)
			fake := &fakeDriver{resolveErr: tc.err}
			platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
			t.Setenv("PATH", t.TempDir())

			_, err := platform.WorkspaceSyncBase(context.Background(), workspaceName, "merge")
			if opErr := AsOperationError(err); opErr == nil || opErr.Code != tc.code || opErr.Slot != "app" || !strings.HasPrefix(err.Error(), tc.wantStart) {
				t.Fatalf("WorkspaceSyncBase() error = %v, want it to start %q", err, tc.wantStart)
			}
			for _, call := range fake.calls {
				if call.method == "SyncBase" {
					t.Fatal("SyncBase was called after the base failed to resolve")
				}
			}
		})
	}
}

// A verb the slot's driver does not support is invalid input, whether the
// driver says so carrying the verb out or answering a precondition.
func TestUnsupportedSlotVerbsAreInvalidInput(t *testing.T) {
	ctx := context.Background()
	unsupported := fmt.Errorf("%w: this slot never stops mid-merge", vcs.ErrUnsupported)
	for _, tc := range []struct {
		name   string
		method string
		run    func(p *Platform, ws string) error
	}{
		{"verb", "MergeAbort", func(p *Platform, ws string) error { _, err := p.WorkspaceSourceMergeAbort(ctx, ws, "app"); return err }},
		{"precondition", "DirtyPaths", func(p *Platform, ws string) error { _, err := p.WorkspaceSourcePull(ctx, ws, "app"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform, workspaceName, _, _ := setupGitWorkspace(t)
			fake := &fakeDriver{fail: map[string]error{tc.method: unsupported}}
			platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }

			err := tc.run(platform, workspaceName)
			var invalid *InvalidInputError
			if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeInvalidInput || !errors.As(err, &invalid) ||
				err.Error() != "slot: not supported for this slot: this slot never stops mid-merge" {
				t.Fatalf("error = %v (%+v), want invalid input on the slot", err, opErr)
			}
			// Reported as invalid input, it is still the driver's refusal.
			if !errors.Is(err, vcs.ErrUnsupported) || !errors.Is(err, unsupported) {
				t.Fatalf("error = %v, want it to still match vcs.ErrUnsupported and the driver's error", err)
			}
		})
	}
}
