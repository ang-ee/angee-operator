package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

// gitRecorder puts a git on PATH that logs each command (the directory it
// runs in, the committer it runs as, its arguments) before running the real
// git, so a test can pin exactly what a verb asks git to do.
type gitRecorder struct {
	log   string
	bases []string
}

func recordGit(t *testing.T, base string) *gitRecorder {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	bin := t.TempDir()
	r := &gitRecorder{log: filepath.Join(bin, "git.log")}
	script := fmt.Sprintf(`#!/bin/sh
{
  printf '%%s [%%s]' "$(pwd -P)" "${GIT_COMMITTER_NAME:-}"
  for a in "$@"; do printf ' %%s' "$a"; done
  printf '\n'
} >> %q
exec %q "$@"
`, r.log, real)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(git) error = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s) error = %v", base, err)
	}
	r.bases = []string{resolved, base}
	return r
}

// run returns the git commands verb ran, with the fixture's base directory
// written as <base>.
func (r *gitRecorder) run(t *testing.T, verb func()) []string {
	t.Helper()
	if err := os.WriteFile(r.log, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(log) error = %v", err)
	}
	verb()
	data, err := os.ReadFile(r.log)
	if err != nil {
		t.Fatalf("ReadFile(log) error = %v", err)
	}
	return strings.Split(strings.TrimSuffix(r.clean(string(data)), "\n"), "\n")
}

// clean writes the fixture's base directory as <base>, and an abbreviated
// commit, which differs run to run, as <sha>.
func (r *gitRecorder) clean(text string) string {
	for _, base := range r.bases {
		text = strings.ReplaceAll(text, base, "<base>")
	}
	return abbreviatedCommit.ReplaceAllString(text, "CurrentRef:<sha>")
}

var abbreviatedCommit = regexp.MustCompile(`CurrentRef:[0-9a-f]{7,40}\b`)

// outcome writes what a verb returned the way the expectations below read.
func outcome(result any, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("%+v", result)
}

// pushPublishCase is one push or publish call, the git it must run and what
// it must return.
type pushPublishCase struct {
	name    string
	setup   func(t *testing.T)
	call    func() (any, error)
	git     []string
	outcome string
}

func checkPushPublishCases(t *testing.T, recorder *gitRecorder, cases []pushPublishCase) {
	t.Helper()
	for _, tc := range cases {
		if tc.setup != nil {
			tc.setup(t)
		}
		var (
			result any
			err    error
		)
		ran := recorder.run(t, func() { result, err = tc.call() })
		got := recorder.clean(outcome(result, err))
		if os.Getenv("ANGEE_RECORD_GIT") != "" {
			t.Logf("%s:\ngit: %q\noutcome: %q", tc.name, ran, got)
			continue
		}
		if strings.Join(ran, "\n") != strings.Join(tc.git, "\n") {
			t.Errorf("%s ran git:\n%s\nwant:\n%s", tc.name, strings.Join(ran, "\n"), strings.Join(tc.git, "\n"))
		}
		if got != tc.outcome {
			t.Errorf("%s returned %q, want %q", tc.name, got, tc.outcome)
		}
	}
}

// Push and publish of a slot cut without a branch run exactly this git, in
// this order, as this committer, in every case: untouched, refused (detached
// with commits, no branch named, a branch that exists), published with a new
// branch, then pushed to the upstream publish set.
func TestBranchlessSlotPushAndPublishRunTheirGit(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	root := filepath.Join(base, ".angee")
	workspaceTemplate := writeRootGitWorkspaceTemplateWithCachePath(t, base, remote, ".cache/app")
	seedWorktreeRemote(t, base, remote)
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: workspaceTemplate, Name: "feature-a", Inputs: map[string]string{"branch": ""}}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	slot := filepath.Join(root, "workspaces", "feature-a")
	cache := filepath.Join(root, ".cache", "app")
	recorder := recordGit(t, base)
	const dir = "<base>/.angee/workspaces/feature-a"
	dirty := dir + " [] status --porcelain=v1 -z --untracked-files=all"
	pushURL := []string{dir + " [] config --get remote.pushDefault", dir + " [] config --get-all --end-of-options remote.origin.url"}
	beyondBase := []string{
		dir + " [] rev-parse --verify --quiet --symbolic-full-name --end-of-options main@{upstream}",
		dir + " [] rev-list --count HEAD --not --end-of-options main origin/main",
	}
	checkPushPublishCases(t, recorder, []pushPublishCase{
		{
			name: "push untouched",
			call: func() (any, error) { return platform.WorkspacePush(ctx, "feature-a", "") },
			git: concat([]string{dirty}, pushURL, beyondBase, []string{
				dirty,
				dir + " [] rev-parse --verify --quiet --symbolic-full-name --end-of-options main@{upstream}",
				dir + " [] rev-parse --git-dir --git-common-dir",
				dir + " [] rev-list --count HEAD --not --branches --remotes --tags",
				dir + " [] rev-list --left-right --count --end-of-options main...HEAD",
			}),
			outcome: "[{Name:app Slot:app Kind:git Path:<base>/.angee/workspaces/feature-a Exists:true State:clean Branch: Ref:main CurrentRef:<sha> Dirty:false Upstream: Ahead:0 Commits:[] Behind:0 Pushed:true UnpushedReason: Error:}]",
		},
		{
			name:    "publish untouched",
			call:    func() (any, error) { return platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "") },
			git:     beyondBase,
			outcome: "{OK:true Conflicted:false ConflictFiles:[] Message:nothing to publish: no commits beyond main; pass a branch to publish anyway Source:<nil>}",
		},
		{
			name:    "push detached with a commit",
			setup:   func(t *testing.T) { commitFile(t, slot, "change.txt") },
			call:    func() (any, error) { return platform.WorkspacePush(ctx, "feature-a", "") },
			git:     concat([]string{dirty}, pushURL, beyondBase),
			outcome: "error: Push workspace \"feature-a\": source slot \"app\" is on a detached HEAD, so there is no branch to push; publish it with `angee workspace source publish feature-a app --branch <name>`",
		},
		{
			name:    "slot push detached with a commit",
			call:    func() (any, error) { return platform.WorkspaceSourcePush(ctx, "feature-a", "app", "") },
			git:     concat([]string{dirty}, pushURL, beyondBase),
			outcome: "error: Push for workspace \"feature-a\" slot \"app\": source slot \"app\" is on a detached HEAD, so there is no branch to push; publish it with `angee workspace source publish feature-a app --branch <name>`",
		},
		{
			name:    "publish detached without a branch",
			call:    func() (any, error) { return platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "") },
			git:     beyondBase,
			outcome: "error: branch: worktree is in detached HEAD; pass an explicit branch",
		},
		{
			name:    "publish detached onto an existing branch",
			setup:   func(t *testing.T) { runGit(t, cache, "branch", "taken", "main") },
			call:    func() (any, error) { return platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "taken") },
			git:     []string{""},
			outcome: "error: branch: worktree is in detached HEAD and branch \"taken\" already exists; switch to it or pass another branch",
		},
		{
			name: "publish detached with a branch",
			call: func() (any, error) { return platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "feature-a") },
			git: []string{
				dir + " [] switch -c feature-a",
				dir + " [] config --get-all --end-of-options remote.origin.url",
				dir + " [angee] push --set-upstream --end-of-options origin refs/heads/feature-a:refs/heads/feature-a",
			},
			outcome: "{OK:true Conflicted:false ConflictFiles:[] Message:branch 'feature-a' set up to track 'origin/feature-a'.\n\nTo <base>/remote.git\n * [new branch]      feature-a -> feature-a Source:<nil>}",
		},
		{
			name: "push after publish",
			call: func() (any, error) { return platform.WorkspacePush(ctx, "feature-a", "") },
			git: []string{
				dirty,
				dir + " [] config --get branch.feature-a.pushRemote",
				dir + " [] config --get remote.pushDefault",
				dir + " [] config --get branch.feature-a.remote",
				dir + " [] config --get-all --end-of-options remote.origin.url",
				dir + " [] push",
				dirty,
				dir + " [] rev-list --left-right --count --end-of-options origin/feature-a...HEAD",
			},
			outcome: "[{Name:app Slot:app Kind:git Path:<base>/.angee/workspaces/feature-a Exists:true State:clean Branch: Ref:main CurrentRef:feature-a Dirty:false Upstream:origin/feature-a Ahead:0 Commits:[] Behind:0 Pushed:true UnpushedReason: Error:}]",
		},
	})
}

// concat joins command lists.
func concat(lists ...[]string) []string {
	var all []string
	for _, list := range lists {
		all = append(all, list...)
	}
	return all
}

// Push and publish of a slot on its branch run exactly this git: nothing to
// publish while it has no commits, a first push that sets its upstream, a
// push of an explicit ref, a publish to a named remote under a new branch
// name, and a publish whose push git refuses.
func TestBranchSlotPushAndPublishRunTheirGit(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, slot, _ := setupGitWorkspace(t)
	base := filepath.Dir(platform.root)
	recorder := recordGit(t, base)
	const dir = "<base>/.angee/workspaces/feature-a/app"
	dirty := dir + " [] status --porcelain=v1 -z --untracked-files=all"
	pushRemote := []string{
		dir + " [] config --get branch.feature-a.pushRemote",
		dir + " [] config --get remote.pushDefault",
		dir + " [] config --get branch.feature-a.remote",
	}
	forkURL := dir + " [] config --get-all --end-of-options remote.fork.url"
	status := []string{dirty, dir + " [] rev-list --left-right --count --end-of-options fork/feature-a...HEAD"}
	slotState := "{Slot:app Source:app Kind:git Mode:worktree Branch:feature-a Ref:main Subpath:app Path:<base>/.angee/workspaces/feature-a/app Exists:true State:clean CurrentRef:feature-a Dirty:false Upstream:fork/feature-a Ahead:0 Behind:0 Pushed:true UnpushedReason: Error:}"
	checkPushPublishCases(t, recorder, []pushPublishCase{
		{
			name: "publish untouched to a named remote",
			call: func() (any, error) { return platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "fork", "") },
			git: []string{
				dir + " [] rev-parse --verify --quiet --symbolic-full-name --end-of-options main@{upstream}",
				dir + " [] rev-list --count HEAD --not --end-of-options main fork/main",
			},
			outcome: "{OK:true Conflicted:false ConflictFiles:[] Message:nothing to publish: no commits beyond main; pass a branch to publish anyway Source:<nil>}",
		},
		{
			name:  "slot push without an upstream",
			setup: func(t *testing.T) { commitFile(t, slot, "change.txt") },
			call:  func() (any, error) { return platform.WorkspaceSourcePush(ctx, workspaceName, "app", "") },
			git: concat([]string{dirty}, pushRemote, []string{
				forkURL,
				dir + " [] rev-parse --verify --quiet --symbolic-full-name --end-of-options main@{upstream}",
				dir + " [] rev-list --count HEAD --not --end-of-options main fork/main",
			}, pushRemote, []string{dir + " [] push -u --end-of-options fork feature-a"}, status),
			outcome: slotState,
		},
		{
			name:    "workspace push to the upstream",
			setup:   func(t *testing.T) { commitFile(t, slot, "more.txt") },
			call:    func() (any, error) { return platform.WorkspacePush(ctx, workspaceName, "") },
			git:     concat([]string{dirty}, pushRemote, []string{forkURL, dir + " [] push"}, status),
			outcome: "[{Name:app Slot:app Kind:git Path:<base>/.angee/workspaces/feature-a/app Exists:true State:clean Branch:feature-a Ref:main CurrentRef:feature-a Dirty:false Upstream:fork/feature-a Ahead:0 Commits:[] Behind:0 Pushed:true UnpushedReason: Error:}]",
		},
		{
			name:    "slot push of an explicit ref",
			call:    func() (any, error) { return platform.WorkspaceSourcePush(ctx, workspaceName, "app", "main") },
			git:     concat([]string{dirty}, pushRemote, []string{forkURL}, pushRemote, []string{dir + " [] push --end-of-options fork main"}, status),
			outcome: slotState,
		},
		{
			name: "publish under a branch the slot lacks",
			call: func() (any, error) {
				return platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "fork", "missing")
			},
			git: []string{
				forkURL,
				dir + " [angee] push --set-upstream --end-of-options fork refs/heads/missing:refs/heads/missing",
				dir + " [angee] -c core.quotepath=false ls-files -u",
			},
			outcome: "error: Publish for workspace \"feature-a\" slot \"app\": pushing branch \"missing\" of source slot \"app\" (<base>/remote.git) failed. git: error: src refspec refs/heads/missing does not match any",
		},
		{
			name: "publish its branch to a named remote",
			call: func() (any, error) {
				return platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "fork", workspaceName)
			},
			git:     []string{forkURL, dir + " [angee] push --set-upstream --end-of-options fork refs/heads/feature-a:refs/heads/feature-a"},
			outcome: "{OK:true Conflicted:false ConflictFiles:[] Message:branch 'feature-a' set up to track 'fork/feature-a'.\n\nEverything up-to-date Source:<nil>}",
		},
	})
}
