package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// A worktree slot with no branch starts on a detached HEAD at its base, so an
// untouched slot never leaves a branch behind. Push leaves it alone until it
// has commits, refuses it once it does (there is no branch to push), and
// publish creates the named branch and its upstream.
func TestBranchlessWorktreeSlotIsDetachedUntilPublished(t *testing.T) {
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
	req := api.WorkspaceCreateRequest{
		Template: workspaceTemplate,
		Name:     "feature-a",
		Inputs:   map[string]string{"branch": ""},
	}
	if _, err := platform.WorkspaceCreate(ctx, req); err != nil {
		t.Fatalf("WorkspaceCreate() without a branch error = %v", err)
	}
	slotPath := filepath.Join(root, "workspaces", "feature-a")
	cache := filepath.Join(root, ".cache", "app")
	assertDetached(t, slotPath)
	if got := strings.TrimSpace(runGitOutput(t, cache, "branch", "--format=%(refname:short)")); got != "main" {
		t.Fatalf("cache branches = %q, want only main (no branch cut for the slot)", got)
	}
	status, err := platform.WorkspaceStatus(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if status.State != "ready" || len(status.Sources) != 1 || status.Sources[0].State != "clean" || !status.Sources[0].Pushed {
		t.Fatalf("WorkspaceStatus() = state %q sources %#v, want a ready workspace with a clean, pushed slot", status.State, status.Sources)
	}

	if _, err := platform.WorkspacePush(ctx, "feature-a", ""); err != nil {
		t.Fatalf("WorkspacePush() of an untouched branchless slot error = %v", err)
	}
	result, err := platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "")
	if err != nil || !result.OK || !strings.Contains(result.Message, "nothing to publish") {
		t.Fatalf("WorkspaceSourcePublish() of an untouched branchless slot = %#v, %v, want OK with nothing to publish", result, err)
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{"main"}) {
		t.Fatalf("remote heads after pushing an untouched slot = %v, want only main", heads)
	}

	commitFile(t, slotPath, "change.txt")

	if _, err := platform.WorkspacePush(ctx, "feature-a", ""); AsOperationError(err).Cause != CauseDetachedHead || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("WorkspacePush() of a detached slot with commits error = %v, want a detached-HEAD conflict", err)
	}
	if _, err := platform.WorkspaceSourcePush(ctx, "feature-a", "app", ""); AsOperationError(err).Cause != CauseDetachedHead || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("WorkspaceSourcePush() of a detached slot with commits error = %v, want a detached-HEAD conflict", err)
	}
	if err := platform.WorkspaceDestroy(ctx, "feature-a", true); err == nil || !strings.Contains(err.Error(), "1 commit(s) on a detached HEAD that no branch holds") {
		t.Fatalf("WorkspaceDestroy() of a detached slot with its own commit error = %v, want the unpushed refusal", err)
	}
	var invalid *InvalidInputError
	if _, err := platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", ""); !errors.As(err, &invalid) || !strings.Contains(err.Error(), "pass an explicit branch") {
		t.Fatalf("WorkspaceSourcePublish() without a branch error = %v, want an explicit-branch refusal", err)
	}
	runGit(t, cache, "branch", "taken", "main")
	if _, err := platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "taken"); !errors.As(err, &invalid) || !strings.Contains(err.Error(), `branch "taken" already exists`) {
		t.Fatalf("WorkspaceSourcePublish() onto an existing branch error = %v, want an existing-branch refusal", err)
	}
	assertDetached(t, slotPath)

	result, err = platform.WorkspaceSourcePublish(ctx, "feature-a", "app", "", "feature-a")
	if err != nil || !result.OK {
		t.Fatalf("WorkspaceSourcePublish() with a branch = %#v, %v, want OK", result, err)
	}
	if got := strings.TrimSpace(runGitOutput(t, slotPath, "branch", "--show-current")); got != "feature-a" {
		t.Fatalf("slot branch after publish = %q, want feature-a", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, slotPath, "rev-parse", "--abbrev-ref", "@{upstream}")); got != "origin/feature-a" {
		t.Fatalf("slot upstream after publish = %q, want origin/feature-a", got)
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{"feature-a", "main"}) {
		t.Fatalf("remote heads after publish = %v, want feature-a and main", heads)
	}
	if _, err := platform.WorkspacePush(ctx, "feature-a", ""); err != nil {
		t.Fatalf("WorkspacePush() after publish error = %v", err)
	}
}

// The cache's local base only moves on `source pull`, while sync-base moves a
// slot to the remote base. Commits the slot gained that way are the remote's,
// not its own: status shows the slot ahead of the local base but pushed, push
// leaves it alone and destroy lets it go.
func TestBranchlessSlotAfterSyncBaseIsNotUnpushed(t *testing.T) {
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
	commitFile(t, filepath.Join(base, "seed"), "upstream.txt")
	runGit(t, filepath.Join(base, "seed"), "push", "origin", "main")

	if _, err := platform.WorkspaceSyncBase(ctx, "feature-a", "merge"); err != nil {
		t.Fatalf("WorkspaceSyncBase() error = %v", err)
	}
	slotPath := filepath.Join(root, "workspaces", "feature-a")
	if _, err := os.Stat(filepath.Join(slotPath, "upstream.txt")); err != nil {
		t.Fatalf("slot after sync-base is missing the upstream commit: %v", err)
	}
	assertDetached(t, slotPath)
	status, err := platform.WorkspaceStatus(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceStatus() after sync-base error = %v", err)
	}
	if slot := status.Sources[0]; slot.State != "ahead" || slot.Ahead != 1 || !slot.Pushed || slot.UnpushedReason != "" {
		t.Fatalf("slot status after sync-base = %#v, want ahead of the local base by 1 and pushed", slot)
	}
	if _, err := platform.WorkspacePush(ctx, "feature-a", ""); err != nil {
		t.Fatalf("WorkspacePush() after sync-base error = %v, want the slot left alone", err)
	}
	if err := platform.WorkspaceDestroy(ctx, "feature-a", false); err != nil {
		t.Fatalf("WorkspaceDestroy() after sync-base error = %v, want no unpushed work", err)
	}
}

// A base that exists only as origin/<ref> in the cache is used as such. Given
// the bare name, git would create and check out a local tracking branch of
// that name instead of the slot's branch, and --detach would refuse it.
// Status counts against origin/<ref> too, so the slot reads clean and can be
// destroyed.
func TestWorktreeSlotStartsFromRemoteOnlyBase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch string
	}{
		{name: "branchless", branch: ""},
		{name: "with branch", branch: "feature-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			remote := filepath.Join(base, "remote.git")
			root := filepath.Join(base, ".angee")
			fields := map[string]string{"mode": "worktree", "default_ref": "main", "ref": "develop"}
			if tc.branch != "" {
				fields["branch"] = tc.branch
			}
			workspaceTemplate := writeSlotWorkspaceTemplate(t, base, remote, fields)
			seedWorktreeRemote(t, base, remote)
			seed := filepath.Join(base, "seed")
			runGit(t, seed, "switch", "-c", "develop")
			commitFile(t, seed, "develop.txt")
			runGit(t, seed, "push", "origin", "develop")
			developHead := strings.TrimSpace(runGitOutput(t, seed, "rev-parse", "HEAD"))

			platform, err := New(root)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if _, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: workspaceTemplate, Name: "feature-a"}); err != nil {
				t.Fatalf("WorkspaceCreate() on a remote-only base error = %v", err)
			}
			slotPath := filepath.Join(root, "workspaces", "feature-a")
			if got := strings.TrimSpace(runGitOutput(t, slotPath, "rev-parse", "HEAD")); got != developHead {
				t.Fatalf("slot HEAD = %s, want develop's commit %s", got, developHead)
			}
			if tc.branch == "" {
				assertDetached(t, slotPath)
			} else {
				if got := strings.TrimSpace(runGitOutput(t, slotPath, "branch", "--show-current")); got != tc.branch {
					t.Fatalf("slot branch = %q, want %q", got, tc.branch)
				}
				if out := runGitOutputAllowFailure(t, slotPath, "rev-parse", "--abbrev-ref", "@{upstream}"); !strings.Contains(out, "no upstream") {
					t.Fatalf("slot upstream = %q, want none (it must not track the base)", out)
				}
			}
			if got := strings.TrimSpace(runGitOutput(t, filepath.Join(root, ".cache", "app"), "branch", "--list", "develop")); got != "" {
				t.Fatalf("cache gained a local develop branch %q, want none", got)
			}
			status, err := platform.WorkspaceStatus(ctx, "feature-a")
			if err != nil {
				t.Fatalf("WorkspaceStatus() error = %v", err)
			}
			if slot := status.Sources[0]; slot.State != "clean" || !slot.Pushed || slot.Error != "" {
				t.Fatalf("slot status on a remote-only base = %#v, want clean and pushed", slot)
			}
			if err := platform.WorkspaceDestroy(ctx, "feature-a", false); err != nil {
				t.Fatalf("WorkspaceDestroy() on a remote-only base error = %v", err)
			}
		})
	}
}

// A slot on its branch with no upstream and no commits beyond its base has
// nothing to publish: push and publish leave it alone instead of creating an
// empty remote branch, an explicit ref or branch still publishes it, and it is
// pushed normally once it has a commit.
func TestWorkspacePushLeavesSlotWithoutCommitsUnpushed(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, slotPath, cache := setupGitWorkspace(t)
	remote := filepath.Join(filepath.Dir(cache), "remote.git")

	if _, err := platform.WorkspacePush(ctx, workspaceName, ""); err != nil {
		t.Fatalf("WorkspacePush() error = %v", err)
	}
	if _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", ""); err != nil {
		t.Fatalf("WorkspaceSourcePush() error = %v", err)
	}
	// The fixture's only remote is "fork", so a publish that reached the push
	// to the default "origin" would fail: success proves it stopped first.
	result, err := platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "", "")
	if err != nil || !result.OK || !strings.Contains(result.Message, "nothing to publish: no commits beyond main") {
		t.Fatalf("WorkspaceSourcePublish() = %#v, %v, want OK with nothing to publish", result, err)
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{"main"}) {
		t.Fatalf("remote heads before any commit = %v, want only main", heads)
	}

	if _, err := platform.WorkspaceSourcePush(ctx, workspaceName, "app", workspaceName); err != nil {
		t.Fatalf("WorkspaceSourcePush() with an explicit ref error = %v", err)
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{workspaceName, "main"}) {
		t.Fatalf("remote heads after an explicit-ref push = %v, want %s and main", heads, workspaceName)
	}
	runGit(t, "", "--git-dir", remote, "branch", "-D", workspaceName)

	result, err = platform.WorkspaceSourcePublish(ctx, workspaceName, "app", "fork", workspaceName)
	if err != nil || !result.OK {
		t.Fatalf("WorkspaceSourcePublish() with an explicit branch = %#v, %v, want it published anyway", result, err)
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{workspaceName, "main"}) {
		t.Fatalf("remote heads after an explicit-branch publish = %v, want %s and main", heads, workspaceName)
	}
	runGit(t, slotPath, "branch", "--unset-upstream")
	runGit(t, "", "--git-dir", remote, "branch", "-D", workspaceName)

	commitFile(t, slotPath, "change.txt")
	if _, err := platform.WorkspacePush(ctx, workspaceName, ""); err != nil {
		t.Fatalf("WorkspacePush() with a commit error = %v", err)
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{workspaceName, "main"}) {
		t.Fatalf("remote heads after pushing a commit = %v, want %s and main", heads, workspaceName)
	}
}

// The skip counts HEAD's commits, so it does not apply when HEAD is not the
// branch being pushed. A clone slot (no branch guard) checked out on another
// branch is pushed as before: its manifest branch, with an upstream.
func TestWorkspacePushIgnoresSkipWhenHeadIsAnotherBranch(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, slotPath, cache := setupGitWorkspace(t)
	remote := filepath.Join(filepath.Dir(cache), "remote.git")
	stack, err := manifest.LoadFile(manifest.Path(platform.Root()))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	workspace := stack.Workspaces[workspaceName]
	slot := workspace.Sources["app"]
	slot.Mode = manifest.WorkspaceSourceModeClone
	workspace.Sources["app"] = slot
	stack.Workspaces[workspaceName] = workspace
	if err := manifest.SaveFile(manifest.Path(platform.Root()), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	runGit(t, slotPath, "switch", "-c", "other")

	if _, err := platform.WorkspacePush(ctx, workspaceName, ""); err != nil {
		t.Fatalf("WorkspacePush() error = %v", err)
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{workspaceName, "main"}) {
		t.Fatalf("remote heads = %v, want the manifest branch pushed as before", heads)
	}
}

// When the slot's base cannot be read, push behaves as it did before: it
// pushes the slot's branch and sets its upstream.
func TestWorkspacePushPushesWhenBaseIsUnreadable(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, slotPath, cache := setupGitWorkspace(t)
	remote := filepath.Join(filepath.Dir(cache), "remote.git")
	stack, err := manifest.LoadFile(manifest.Path(platform.Root()))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	workspace := stack.Workspaces[workspaceName]
	slot := workspace.Sources["app"]
	slot.Ref = "no-such-ref"
	workspace.Sources["app"] = slot
	stack.Workspaces[workspaceName] = workspace
	if err := manifest.SaveFile(manifest.Path(platform.Root()), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}

	if _, err := platform.WorkspacePush(ctx, workspaceName, ""); err != nil {
		t.Fatalf("WorkspacePush() error = %v", err)
	}
	if heads := remoteHeads(t, remote); !slices.Equal(heads, []string{workspaceName, "main"}) {
		t.Fatalf("remote heads = %v, want the branch pushed as before", heads)
	}
	if got := strings.TrimSpace(runGitOutput(t, slotPath, "rev-parse", "--abbrev-ref", "@{upstream}")); got != "fork/"+workspaceName {
		t.Fatalf("slot upstream = %q, want fork/%s", got, workspaceName)
	}
}

// A template can choose a slot's mode through an input. The resolved mode is
// validated before any slot is materialized, so an unsupported one fails the
// create and leaves nothing behind; so does a worktree with no branch and no
// base to start from.
func TestWorkspaceCreateResolvesAndValidatesSlotMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fields   map[string]string
		slotMode string
		wantMode string
		wantErr  string
	}{
		{name: "worktree from input", fields: map[string]string{"mode": `"${inputs.slot_mode}"`}, slotMode: "worktree", wantMode: "worktree"},
		{name: "clone from input", fields: map[string]string{"mode": `"${inputs.slot_mode}"`}, slotMode: "clone", wantMode: "clone"},
		{name: "unknown literal", fields: map[string]string{"mode": "worktre"}, wantErr: `workspace "feature-a" source "app": mode "worktre" is not supported`},
		{name: "unknown from input", fields: map[string]string{"mode": `"${inputs.slot_mode}"`}, slotMode: "jj", wantErr: `workspace "feature-a" source "app": mode "jj" is not supported`},
		{name: "branchless without a base", fields: map[string]string{"mode": "worktree", "branch": `""`, "ref": `""`, "default_ref": `""`}, wantErr: `workspace "feature-a" source "app": a worktree without a branch needs a ref`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			remote := filepath.Join(base, "remote.git")
			root := filepath.Join(base, ".angee")
			fields := map[string]string{"branch": "feature-a", "ref": "main", "default_ref": "main"}
			for key, value := range tc.fields {
				fields[key] = value
			}
			workspaceTemplate := writeSlotWorkspaceTemplate(t, base, remote, fields)
			seedWorktreeRemote(t, base, remote)
			platform, err := New(root)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			inputs := map[string]string{}
			if tc.slotMode != "" {
				inputs["slot_mode"] = tc.slotMode
			}
			_, err = platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: workspaceTemplate, Name: "feature-a", Inputs: inputs})
			workspacePath := filepath.Join(root, "workspaces", "feature-a")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("WorkspaceCreate() error = %v, want %q", err, tc.wantErr)
				}
				if _, statErr := os.Stat(workspacePath); !os.IsNotExist(statErr) {
					t.Fatalf("workspace directory after a rejected source: Stat err = %v, want it absent", statErr)
				}
				if _, statErr := os.Stat(filepath.Join(root, ".cache", "app")); !os.IsNotExist(statErr) {
					t.Fatalf("source cache after a rejected source: Stat err = %v, want nothing cloned", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("WorkspaceCreate() error = %v", err)
			}
			stack, err := manifest.LoadFile(manifest.Path(root))
			if err != nil {
				t.Fatalf("LoadFile() error = %v", err)
			}
			if got := stack.Workspaces["feature-a"].Sources["app"].Mode; got != tc.wantMode {
				t.Fatalf("recorded slot mode = %q, want %q", got, tc.wantMode)
			}
			info, err := os.Stat(filepath.Join(workspacePath, ".git"))
			if err != nil {
				t.Fatalf("Stat(.git) error = %v", err)
			}
			// A worktree's .git is a pointer file; a clone's is a directory.
			if info.IsDir() != (tc.wantMode == "clone") {
				t.Fatalf(".git is a directory = %v, want %v for mode %q", info.IsDir(), tc.wantMode == "clone", tc.wantMode)
			}
		})
	}
}

// A clone slot is a repository of its own: its branches go with it, so a
// commit only one of them holds is unpushed, and destroy refuses the slot.
// (Tags count as holding a commit: git keeps fetched tags with local ones.)
func TestCloneSlotCommitOnlyItsOwnBranchHoldsIsUnpushed(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	root := filepath.Join(base, ".angee")
	workspaceTemplate := writeSlotWorkspaceTemplate(t, base, remote, map[string]string{"mode": "clone", "branch": "feature-a", "ref": "main", "default_ref": "main"})
	seedWorktreeRemote(t, base, remote)
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: workspaceTemplate, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	slotPath := filepath.Join(root, "workspaces", "feature-a")
	runGit(t, slotPath, "switch", "-c", "mywork")
	commitFile(t, slotPath, "change.txt")
	runGit(t, slotPath, "switch", "--detach")

	const reason = "1 commit(s) on a detached HEAD that no remote branch or tag holds"
	status, err := platform.WorkspaceStatus(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if slot := status.Sources[0]; slot.Pushed || slot.UnpushedReason != reason {
		t.Fatalf("clone slot status = %#v, want unpushed with %q", slot, reason)
	}
	if err := platform.WorkspaceDestroy(ctx, "feature-a", false); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("WorkspaceDestroy() error = %v, want the unpushed refusal", err)
	}
}

// writeSlotWorkspaceTemplate writes a workspace template with one git slot,
// "app", at the workspace root and its cache at .cache/app. fields become the
// slot's keys (mode, branch, ref, default_ref) verbatim; an omitted key is left
// out. The template declares a slot_mode input defaulting to worktree.
func writeSlotWorkspaceTemplate(t *testing.T, base, repo string, fields map[string]string) string {
	t.Helper()
	templateRoot := filepath.Join(base, ".templates", "workspaces", "slot")
	if err := os.MkdirAll(filepath.Join(templateRoot, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(slot workspace template) error = %v", err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var slot strings.Builder
	for _, key := range keys {
		slot.WriteString("      " + key + ": " + fields[key] + "\n")
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: slot
  inputs:
    slot_mode:
      type: str
      default: worktree
  sources:
    app:
      kind: git
      repo: ` + repo + `
      cache_path: .cache/app
      subpath: .
` + slot.String() + `slot_mode:
  type: str
  default: worktree
`
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(slot workspace copier.yml) error = %v", err)
	}
	return templateRoot
}

// commitFile commits a new file named name in the checkout at dir.
func commitFile(t *testing.T, dir, name string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(dir, name), name+"\n")
	runGit(t, dir, "add", name)
	runGit(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", name)
}

func assertDetached(t *testing.T, dir string) {
	t.Helper()
	if got := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD")); got != "HEAD" {
		t.Fatalf("HEAD of %s = %q, want a detached HEAD", dir, got)
	}
}

// runGitOutputAllowFailure runs git and returns its combined output whether or
// not it succeeds.
func runGitOutputAllowFailure(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// remoteHeads lists the branch names a bare remote holds, sorted.
func remoteHeads(t *testing.T, remote string) []string {
	t.Helper()
	var heads []string
	for line := range strings.Lines(runGitOutput(t, "", "ls-remote", "--heads", remote)) {
		if _, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			heads = append(heads, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	slices.Sort(heads)
	return heads
}
