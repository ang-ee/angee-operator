package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/manifest"
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
	publishes  bool             // a push publishes, as a jj slot's does
	upstream   string           // the branch it tracks; "" for none
	syncResult *api.GitOpResult // SyncBase's result; nil for OK
	resolveErr error            // returned by ResolveBase
	fail       map[string]error // an error for a method by name
	emptyDir   string           // what Add installs
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

// Exists records the call and reports the slot unlisted.
func (f *fakeDriver) Exists(_ context.Context, s vcs.Slot) (bool, error) {
	f.call("Exists", s)
	return false, f.fail["Exists"]
}

// Add records the call, whether it was handed a configure step, and installs
// an empty directory through d, as a checkout would arrive.
func (f *fakeDriver) Add(ctx context.Context, s vcs.Slot, d vcs.Dest, configure func(context.Context, string) error) error {
	f.call("Add", s, "configure="+strconv.FormatBool(configure != nil))
	if err := f.fail["Add"]; err != nil {
		return err
	}
	return d.ReplaceFrom(ctx, f.emptyDir)
}

// Remove records the call and removes the slot through d.
func (f *fakeDriver) Remove(_ context.Context, s vcs.Slot, d vcs.Dest) error {
	f.call("Remove", s)
	return d.RemoveAll()
}

func (f *fakeDriver) Status(_ context.Context, s vcs.Slot) (vcs.Status, error) {
	f.slots = append(f.slots, s)
	return f.status, f.err
}

func (f *fakeDriver) Diff(_ context.Context, s vcs.Slot, _ string) ([]api.DiffFile, error) {
	f.slots = append(f.slots, s)
	return f.diff, f.err
}

func (f *fakeDriver) Ready(_ context.Context, s vcs.Slot, verb vcs.Verb) error {
	f.call("Ready", s, string(verb))
	return f.fail["Ready"]
}

// SyncMethod merges by default, as the git driver does, or fails with
// fail["SyncMethod"]. It is not recorded: it is handed no slot.
func (f *fakeDriver) SyncMethod(method string) (string, error) {
	if err := f.fail["SyncMethod"]; err != nil {
		return "", err
	}
	if method == vcs.SyncDefault {
		return vcs.SyncMerge, nil
	}
	return method, nil
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
	if err := f.fail["PlanPush"]; err != nil {
		return vcs.PushPlan{}, err
	}
	switch {
	case f.publishes:
		branch = cmp.Or(ref, branch)
		return vcs.PushPlan{Track: branch, Publish: &vcs.PublishPlan{Remote: "origin", URL: "fake://origin", Branch: branch}}, nil
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
	if plan.Publish != nil {
		f.call("Push", s, "publish "+plan.Publish.Remote+" "+plan.Publish.Branch)
		return f.fail["Push"]
	}
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
		return *f.syncResult, f.fail["SyncBase"]
	}
	return f.result("SyncBase"), f.fail["SyncBase"]
}

func (f *fakeDriver) CheckRebase(_ context.Context, s vcs.Slot, onto string) error {
	f.call("CheckRebase", s, onto)
	return f.fail["CheckRebase"]
}

func (f *fakeDriver) Merge(_ context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	f.call("Merge", s, ref)
	return f.result("Merge"), nil
}

func (f *fakeDriver) Rebase(_ context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	f.call("Rebase", s, ref)
	return f.result("Rebase"), f.fail["Rebase"]
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
	if err := f.fail["PlanPublish"]; err != nil {
		return vcs.PublishPlan{}, err
	}
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
	return f.fail["PreparePublish"]
}

func (f *fakeDriver) Publish(_ context.Context, s vcs.Slot, plan vcs.PublishPlan) (api.GitOpResult, error) {
	f.call("Publish", s, plan.Remote, plan.Branch)
	return f.result("Publish"), f.fail["Publish"]
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
		}, want: []string{"Ready(pull)", "FetchURL", "Pull"}},
		{name: "push to the upstream", upstream: "origin/feature-a", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSourcePush(ctx, ws, "app", "")
			return "", err
		}, want: []string{"Ready(push)", "PushURL()", "PlanPush(,feature-a)", "Push()"}},
		{name: "push a new branch", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSourcePush(ctx, ws, "app", "")
			return "", err
		}, want: []string{"Ready(push)", "PushURL()", "PlanPush(,feature-a)", "Push(-u feature-a)"}},
		{name: "push a ref", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSourcePush(ctx, ws, "app", "v1")
			return "", err
		}, want: []string{"Ready(push)", "PushURL()", "PlanPush(v1,feature-a)", "Push(v1)"}},
		{name: "workspace push", upstream: "origin/feature-a", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspacePush(ctx, ws, "")
			return "", err
		}, want: []string{"Ready(push)", "PushURL()", "PlanPush(,feature-a)", "Push()"}},
		{name: "sync-base by default", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSyncBase(ctx, ws, "")
			return "", err
		}, want: []string{"Ready(sync-base)", "FetchURL", "Fetch", "ResolveBase", "SyncBase(fake/main,merge)"}},
		{name: "sync-base by merge", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSyncBase(ctx, ws, "merge")
			return "", err
		}, want: []string{"Ready(sync-base)", "FetchURL", "Fetch", "ResolveBase", "SyncBase(fake/main,merge)"}},
		{name: "sync-base by rebase", run: func(p *Platform, ws string) (string, error) {
			_, err := p.WorkspaceSyncBase(ctx, ws, "rebase")
			return "", err
		}, want: []string{"Ready(sync-base)", "FetchURL", "Fetch", "ResolveBase", "CheckRebase(fake/main)", "SyncBase(fake/main,rebase)"}},
		{name: "merge", run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourceMerge(ctx, ws, "app", "main")
			return result.Message, err
		}, want: []string{"Ready(merge)", "Merge(main)"}, wantMsg: "fake Merge"},
		{name: "rebase", run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourceRebase(ctx, ws, "app", "main")
			return result.Message, err
		}, want: []string{"Ready(rebase)", "Rebase(main)"}, wantMsg: "fake Rebase"},
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
		}, want: []string{"Ready(publish)", "PlanPublish(,feature-a,false)", "PreparePublish(feature-a,false)", "Publish(origin,feature-a)"}, wantMsg: "fake Publish"},
		{name: "publish from a detached HEAD", detached: true, run: func(p *Platform, ws string) (string, error) {
			result, err := p.WorkspaceSourcePublish(ctx, ws, "app", "fork", "new")
			return result.Message, err
		}, want: []string{"Ready(publish)", "PlanPublish(fork,new,true)", "PreparePublish(new,true)", "Publish(fork,new)"}, wantMsg: "fake Publish"},
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

// Creating and destroying a workspace cuts and removes its git slots through
// their driver, handed the manifest's facts about each, with no git on PATH:
// a worktree slot is added once the driver reports nothing listed at its path,
// a clone slot is added with the service's remote configuration to run before
// it is installed, and a create that fails after cutting them, or a destroy,
// removes them through the driver. A local source's link is the service's.
func TestWorkspaceLifecycleDispatchesToTheSlotDriver(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	template := f.template(t, "lifecycle", "app", "docs", "notes")
	// The cache exists, so with no git its refresh only warns.
	runGit(t, "", "clone", f.appRemote, f.cache())
	fake := &fakeDriver{status: vcs.Status{Pushed: true}, emptyDir: t.TempDir()}
	f.platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
	t.Setenv("PATH", t.TempDir())

	workspace := f.workspace("feature-a")
	lifecycle := map[string]vcs.Slot{
		"app":  {Path: filepath.Join(workspace, "app"), Mode: vcs.ModeWorktree, Primary: f.cache(), Branch: "feature-a", BaseRef: "main", BaseRemote: "origin"},
		"docs": {Path: filepath.Join(workspace, "docs"), Mode: vcs.ModeClone, CloneURL: f.docsRemote, BaseRef: "main", BaseRemote: "origin"},
	}
	// calls lists the driver calls since the last one, each with the slot it
	// was handed checked against the slot's lifecycle facts.
	calls := func() []string {
		var got []string
		for _, call := range fake.calls {
			name := filepath.Base(call.slot.Path)
			// Ready is handed the slot as its verbs see it, not its lifecycle.
			if call.method != "Ready" && call.slot != lifecycle[name] {
				t.Fatalf("%s slot = %#v, want %#v", call.method, call.slot, lifecycle[name])
			}
			got = append(got, strings.TrimSpace(call.method+" "+name+" "+strings.Join(call.args, " ")))
		}
		fake.calls = nil
		return got
	}
	created := []string{"Exists app", "Add app configure=false", "Add docs configure=true"}

	// An invalid TTL fails the create after its slots are cut.
	if _, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "feature-a", TTL: "not-a-duration"}); err == nil {
		t.Fatal("WorkspaceCreate() with an invalid TTL error = nil, want a failure")
	}
	if got, want := calls(), concat(created, []string{"Remove docs", "Remove app"}); !slices.Equal(got, want) {
		t.Fatalf("failed create driver calls = %q, want %q", got, want)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace after the failed create: Lstat error = %v, want it gone", err)
	}

	if _, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	if got := calls(); !slices.Equal(got, created) {
		t.Fatalf("create driver calls = %q, want %q", got, created)
	}
	if target, err := os.Readlink(filepath.Join(workspace, "notes")); err != nil || !strings.HasSuffix(target, "notes") {
		t.Fatalf("local slot link = %q, %v, want the service's link to the notes directory", target, err)
	}

	if err := f.platform.WorkspaceDestroy(ctx, "feature-a", true); err != nil {
		t.Fatalf("WorkspaceDestroy() error = %v", err)
	}
	// The guard asks each git slot's driver whether it may go; removal goes
	// deepest path first.
	if got, want := calls(), []string{"Ready app destroy", "Ready docs destroy", "Remove docs", "Remove app"}; !slices.Equal(got, want) {
		t.Fatalf("destroy driver calls = %q, want %q", got, want)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace after destroy: Lstat error = %v, want it gone", err)
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
		opErr.Slot != "app" || opErr.Source != "app" || opErr.Detail != "CONFLICT (content): fake.txt" || opErr.ToolLine != "" {
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
			`Sync base for workspace "feature-a": resolving the base of source slot "app" failed: cannot read the remotes.`},
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
		{"precondition", "Ready", func(p *Platform, ws string) error { _, err := p.WorkspaceSourcePull(ctx, ws, "app"); return err }},
		{"readiness", "Ready", func(p *Platform, ws string) error { _, err := p.WorkspaceSyncBase(ctx, ws, ""); return err }},
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

// sync-base asks each slot's driver whether it may sync (vcs.Driver.Ready)
// and words its answer: a branch mismatch names the fix the driver gives, and
// uncommitted changes list their paths. Either refuses before any slot is
// fetched.
func TestWorkspaceSyncBaseWordsTheDriversReadiness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ready     error
		cause     string
		wantStart string
		wantHint  string
		wantPaths []string
	}{
		{"branch mismatch", &vcs.BranchMismatchError{Reason: `on "other", expected "feature-a"`, Fix: "fake switch feature-a"}, CauseBranchMismatch,
			`Sync base for workspace "feature-a": workspace "feature-a" source "app" has branch mismatch: on "other", expected "feature-a" at `,
			"Switch it back with `fake switch feature-a`, then retry.", nil},
		{"uncommitted changes", &vcs.UncommittedError{Paths: []string{"a.txt", "b.txt"}}, CauseUncommittedChanges,
			`Sync base for workspace "feature-a": source slot "app" has uncommitted changes in a.txt, b.txt.`,
			"Commit or restore the changes, then retry.", []string{"a.txt", "b.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform, workspaceName, _, _ := setupGitWorkspace(t)
			fake := &fakeDriver{fail: map[string]error{"Ready": tc.ready}}
			platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
			t.Setenv("PATH", t.TempDir())

			_, err := platform.WorkspaceSyncBase(context.Background(), workspaceName, "")
			opErr := AsOperationError(err)
			if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != tc.cause || opErr.Slot != "app" || opErr.Hint != tc.wantHint ||
				!strings.HasPrefix(err.Error(), tc.wantStart) || (tc.wantPaths != nil && !slices.Equal(opErr.Paths, tc.wantPaths)) {
				t.Fatalf("WorkspaceSyncBase() error = %v (%+v), want %s starting %q with hint %q", err, opErr, tc.cause, tc.wantStart, tc.wantHint)
			}
			for _, call := range fake.calls {
				if call.method != "Ready" {
					t.Fatalf("driver called %s after the slot was refused", call.method)
				}
			}
		})
	}
}

// A sync or rebase its driver made, but whose result it could not read after
// (OK with an error), is not reported as a failure of the sync: sync-base
// gives the slot's state with what failed, and the slot's rebase says it was
// reading what the rebase left that failed.
func TestASyncThatWasMadeIsNotReportedAsFailed(t *testing.T) {
	ctx := context.Background()
	unread := errors.New("jj rebased it, but reading the conflicts it left failed: busy")
	platform, workspaceName, _, _ := setupGitWorkspace(t)
	fake := &fakeDriver{status: vcs.Status{CurrentRef: workspaceName, Pushed: true}, fail: map[string]error{"SyncBase": unread, "Rebase": unread}}
	platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
	t.Setenv("PATH", t.TempDir())

	states, err := platform.WorkspaceSyncBase(ctx, workspaceName, "rebase")
	if err != nil || len(states) != 1 || !strings.Contains(states[0].Error, "synced, but reading what rebasing onto fake/main left failed: "+unread.Error()) {
		t.Fatalf("WorkspaceSyncBase() = %+v, %v, want the slot's state saying what failed", states, err)
	}
	_, err = platform.WorkspaceSourceRebase(ctx, workspaceName, "app", "main")
	if want := `Rebase for workspace "feature-a" slot "app": reading what rebasing onto main left in source slot "app" failed: `; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("WorkspaceSourceRebase() error = %v, want it to start %q", err, want)
	}
}

// A sync or rebase its driver made with a warning (*vcs.Warning) is not a
// failure: sync-base puts the warning on the slot's state, and the slot's
// rebase returns its result, whose message carries it.
func TestASyncThatWarnsIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	warned := &vcs.Warning{Text: "Warning: 1 of those updates were skipped"}
	platform, workspaceName, _, _ := setupGitWorkspace(t)
	fake := &fakeDriver{status: vcs.Status{CurrentRef: workspaceName, Pushed: true}, fail: map[string]error{"SyncBase": warned, "Rebase": warned}}
	platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
	t.Setenv("PATH", t.TempDir())

	states, err := platform.WorkspaceSyncBase(ctx, workspaceName, "rebase")
	if err != nil || len(states) != 1 || states[0].Error != "synced, but jj warned while rebasing onto fake/main: "+warned.Text {
		t.Fatalf("WorkspaceSyncBase() = %+v, %v, want the warning on the slot's state", states, err)
	}
	if result, err := platform.WorkspaceSourceRebase(ctx, workspaceName, "app", "main"); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourceRebase() = %+v, %v, want its OK result", result, err)
	}
}

// A slot whose sync fails once earlier slots are synced names them, since
// they stay synced: in the message, before nothing else, and in the detail.
func TestAFailedSyncNamesTheSlotsSyncedBeforeIt(t *testing.T) {
	err := afterSynced(syncConflict(api.GitOpResult{Conflicted: true, ConflictFiles: []string{"f.txt"}}, gitStep{action: "merging origin/main into", object: `source slot "tree"`, slot: "tree"}), []string{"a", "b"})
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Cause != CauseMergeConflict || !strings.HasSuffix(opErr.Summary, `source slots "a", "b" synced before it, and stay synced.`) ||
		!strings.HasPrefix(opErr.Detail, `source slots "a", "b" synced before it`) {
		t.Fatalf("afterSynced() = %+v, want the conflict naming a and b as synced", opErr)
	}
	if plain := syncConflict(api.GitOpResult{Conflicted: true, ConflictFiles: []string{"f.txt"}}, gitStep{slot: "tree"}); afterSynced(plain, nil) != plain {
		t.Error("afterSynced() with nothing synced changed the error")
	}
}

// A rebase refused for what it would rewrite names it: the other checkouts,
// with the git worktrees in paths, or the immutable commits.
func TestRebaseRefusalWording(t *testing.T) {
	shared := rebaseRefusal(&vcs.SharedCommitsError{Workspaces: []string{"dev--b"}, Branches: []vcs.CheckedOutBranch{{Branch: "topic", Path: "/tree"}}}, "ws", "a", "app")
	if shared == nil || shared.Code != CodePreconditionFailed || shared.Cause != CauseSharedCommits || !slices.Equal(shared.Paths, []string{"/tree"}) ||
		!strings.Contains(shared.Summary, `source slot "a" was not rebased: the rebase would rewrite commits that other checkouts hold: jj workspace "dev--b", branch "topic" checked out at /tree.`) ||
		!strings.Contains(shared.Hint, "jj workspace forget") {
		t.Errorf("shared commits refusal = %+v", shared)
	}
	frozen := rebaseRefusal(&vcs.ImmutableCommitsError{Commits: []string{"0123456789ab"}}, "ws", "a", "app")
	if frozen == nil || frozen.Code != CodePreconditionFailed || frozen.Cause != CauseImmutableCommits ||
		!strings.Contains(frozen.Summary, "jj holds immutable: 0123456789ab") {
		t.Errorf("immutable commits refusal = %+v", frozen)
	}
	if rebaseRefusal(errors.New("other"), "ws", "a", "app") != nil {
		t.Error("rebaseRefusal() of another error, want nil")
	}
}

// A push whose driver carries it out as a publish (a jj slot's) is held to
// the manifest's rules for its remote, as a publish is, before anything is
// pushed: a branch the remote's `branches` do not accept is refused, and one
// they accept is pushed through the driver.
func TestAPushThatPublishesIsHeldToThePublishRules(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, _, _ := setupGitWorkspace(t)
	stack, err := manifest.LoadFile(manifest.Path(platform.Root()))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	source := stack.Sources["app"]
	source.Remotes = map[string]manifest.SourceRemote{"origin": {URL: "https://example.invalid/app.git", Branches: []string{"release/*"}}}
	source.Repo = ""
	stack.Sources["app"] = source
	if err := manifest.SaveFile(manifest.Path(platform.Root()), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	fake := &fakeDriver{publishes: true}
	platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
	t.Setenv("PATH", t.TempDir())

	for name, call := range map[string]func() error{
		"push":           func() error { _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", ""); return err },
		"workspace push": func() error { _, err := platform.WorkspacePush(ctx, workspaceName, ""); return err },
	} {
		err := call()
		var invalid *InvalidInputError
		if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeInvalidInput || !errors.As(err, &invalid) || invalid.Field != "branch" ||
			!strings.Contains(err.Error(), `remote "origin" of source "app" accepts only branches matching release/*; "feature-a" matches none`) {
			t.Errorf("%s error = %v (%+v), want the branch refused", name, err, opErr)
		}
	}
	for _, call := range fake.calls {
		if call.method == "Push" {
			t.Fatalf("a refused push was pushed: %+v", fake.calls)
		}
	}
	if _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", "release/1"); err != nil {
		t.Fatalf("WorkspaceSourcePush(release/1) error = %v", err)
	}
	if last := fake.calls[len(fake.calls)-1]; last.method != "Push" || !slices.Equal(last.args, []string{"publish origin release/1"}) {
		t.Fatalf("last driver call = %+v, want the publish pushed", last)
	}
}

// A publish whose slot moved after it was planned (vcs.ErrCheckoutMoved) is
// a CONFLICT to retry, and nothing is pushed; a driver's own reason a slot is
// not ready (vcs.NotReadyError) is a precondition naming its fix.
func TestPublishWordsAMovedOrUnreadySlot(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, method string
		err          error
		code, start  string
		hint         string
	}{
		{"moved", "PreparePublish", fmt.Errorf("the slot moved: %w", vcs.ErrCheckoutMoved), CodeConflict,
			`Publish for workspace "feature-a" slot "app": source slot "app" changed while its publish was checked, so nothing was pushed: `, "Run publish again."},
		{"not ready", "Ready", &vcs.NotReadyError{Reason: "its working-copy commit has 2 parents", Fix: "fake new"}, CodePreconditionFailed,
			`Publish for workspace "feature-a" slot "app": source slot "app" is not ready for publish: its working-copy commit has 2 parents. `, "Fix it with `fake new`, then retry."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform, workspaceName, _, _ := setupGitWorkspace(t)
			fake := &fakeDriver{fail: map[string]error{tc.method: tc.err}}
			platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
			t.Setenv("PATH", t.TempDir())
			_, err := platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "", "")
			if opErr := AsOperationError(err); opErr == nil || opErr.Code != tc.code || opErr.Slot != "app" || opErr.Hint != tc.hint || !strings.HasPrefix(err.Error(), tc.start) {
				t.Fatalf("WorkspaceSourcePublish() error = %v (%+v), want %s starting %q", err, opErr, tc.code, tc.start)
			}
			for _, call := range fake.calls {
				if call.method == "Publish" {
					t.Fatal("a refused publish was pushed")
				}
			}
		})
	}
}

// A failure to plan or prepare a push or publish is a git failure of that
// step only when a version-control tool reported it; a deadline stays a
// TIMEOUT and any other error INTERNAL, as classifyOperationError has them.
func TestPushAndPublishFailuresKeepTheirCodes(t *testing.T) {
	ctx := context.Background()
	command := &git.CommandError{Args: []string{"rev-parse"}, Err: errors.New("exit status 128"), Output: "fatal: bad revision"}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{command, CodeGitFailed},
		{context.DeadlineExceeded, CodeTimeout},
		{errors.New("boom"), CodeInternal},
	} {
		for _, method := range []string{"PlanPublish", "PreparePublish", "PlanPush"} {
			t.Run(method+" "+tc.code, func(t *testing.T) {
				platform, workspaceName, _, _ := setupGitWorkspace(t)
				fake := &fakeDriver{fail: map[string]error{method: tc.err}}
				platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
				t.Setenv("PATH", t.TempDir())
				var err error
				if method == "PlanPush" {
					_, err = platform.WorkspaceSourcePush(ctx, workspaceName, "app", "")
				} else {
					_, err = platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "", "")
				}
				if opErr := AsOperationError(err); opErr == nil || opErr.Code != tc.code || !errors.Is(err, tc.err) {
					t.Errorf("%s failing with %v: error = %v (%+v), want %s", method, tc.err, err, opErr, tc.code)
				}
			})
		}
	}
}

// A publish its driver made but warned of (a jj slot's: the remote branch
// read back elsewhere, its push remote not recorded) is the OK result,
// whose message says so, for a slot's publish and for a push that
// publishes alike; any other failure of the push is push_failed.
func TestAPublishMadeButWarnedOfIsAResult(t *testing.T) {
	ctx := context.Background()
	warned := &vcs.Warning{Text: "Pushed, but jj reads it elsewhere."}
	platform, workspaceName, _, _ := setupGitWorkspace(t)
	fake := &fakeDriver{publishes: true, fail: map[string]error{"Publish": warned, "Push": warned}}
	platform.slotDrivers = func(vcs.Slot) vcs.Driver { return fake }
	t.Setenv("PATH", t.TempDir())
	if result, err := platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "", ""); err != nil || !result.OK {
		t.Fatalf("WorkspaceSourcePublish() = %+v, %v, want the warned publish OK", result, err)
	}
	if _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", ""); err != nil {
		t.Fatalf("WorkspaceSourcePush() error = %v, want the warned push done", err)
	}
	fake.fail["Push"] = errors.New("refused")
	if _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", ""); AsOperationError(err) == nil || AsOperationError(err).Cause != CausePushFailed {
		t.Fatalf("WorkspaceSourcePush() of a refused push error = %v, want push_failed", err)
	}
}
