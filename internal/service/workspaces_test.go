package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

func TestWorkspaceCreateNoHostStackWithTemplateSourceAndRelativeChain(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	sourceRoot := filepath.Join(base, "app-source")
	workspaceTemplate := writeWorkspaceTemplate(t, base, sourceRoot)
	writeChainedStackTemplate(t, sourceRoot)

	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ref, err := platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{
		Template: workspaceTemplate,
		Name:     "feature-a",
		Inputs: map[string]string{
			"source_path": sourceRoot,
			"topic":       "Feature A",
		},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	if ref.Name != "feature-a" {
		t.Fatalf("workspace name = %q, want feature-a", ref.Name)
	}

	stack, err := manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile(angee.yaml) error = %v", err)
	}
	workspace, ok := stack.Workspaces["feature-a"]
	if !ok {
		t.Fatalf("workspace feature-a missing from manifest: %#v", stack.Workspaces)
	}
	if stack.Sources["app"].Kind != "local" || stack.Sources["app"].Path != sourceRoot {
		t.Fatalf("template-declared source not persisted: %#v", stack.Sources["app"])
	}
	if workspace.Sources["app"].Subpath != "app" {
		t.Fatalf("workspace source subpath = %q, want app", workspace.Sources["app"].Subpath)
	}
	if workspace.Resolved.ChainRoot != "app/.angee" {
		t.Fatalf("chain root = %q, want app/.angee", workspace.Resolved.ChainRoot)
	}
	if len(workspace.Resolved.Chain) != 2 || workspace.Resolved.Chain[1] != "app/.templates/stacks/dev" {
		t.Fatalf("resolved chain = %#v, want relative chained template", workspace.Resolved.Chain)
	}

	readme, err := os.ReadFile(filepath.Join(root, "workspaces", "feature-a", "README.md"))
	if err != nil {
		t.Fatalf("ReadFile(README.md) error = %v", err)
	}
	if !strings.Contains(string(readme), "feature-a") {
		t.Fatalf("README was not rendered with workspace_name: %s", readme)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", "feature-a", "app", ".angee", "angee.yaml")); err != nil {
		t.Fatalf("chained stack manifest was not rendered: %v", err)
	}
	linkPath := filepath.Join(root, "workspaces", "feature-a", "app")
	linkTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink(workspace local source) error = %v", err)
	}
	if filepath.IsAbs(linkTarget) {
		t.Fatalf("workspace local source symlink target = %q, want relative path", linkTarget)
	}
	resolvedTarget, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(workspace local source) error = %v", err)
	}
	if resolvedTarget != sourceRoot {
		canonicalSourceRoot, err := filepath.EvalSymlinks(sourceRoot)
		if err != nil {
			t.Fatalf("EvalSymlinks(sourceRoot) error = %v", err)
		}
		if resolvedTarget != canonicalSourceRoot {
			t.Fatalf("workspace local source symlink resolves to %q, want %q", resolvedTarget, canonicalSourceRoot)
		}
	}
}

func TestWorkspaceCreateAllowsGitSourceAtWorkspaceRoot(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	helperRemote := filepath.Join(base, "helper.git")
	seed := filepath.Join(base, "seed")
	helperSeed := filepath.Join(base, "helper-seed")
	root := filepath.Join(base, ".angee")
	workspaceTemplate := writeRootGitWorkspaceTemplate(t, base, remote, helperRemote)

	runGit(t, "", "init", "--bare", remote)
	runGit(t, "", "clone", remote, seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(seed, "README.md"), "repo readme\n")
	mustWriteFile(t, filepath.Join(seed, ".gitignore"), ".angee/\n.copier-answers.yml\n.dev-sibling\n")
	writeRootSourceStackTemplate(t, seed)
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, "", "init", "--bare", helperRemote)
	runGit(t, "", "clone", helperRemote, helperSeed)
	runGit(t, helperSeed, "config", "user.email", "test@example.com")
	runGit(t, helperSeed, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(helperSeed, "helper.txt"), "helper\n")
	runGit(t, helperSeed, "add", ".")
	runGit(t, helperSeed, "commit", "-m", "initial")
	runGit(t, helperSeed, "branch", "-M", "main")
	runGit(t, helperSeed, "push", "-u", "origin", "main")

	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ref, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{
		Template: workspaceTemplate,
		Name:     "feature-a",
		Inputs: map[string]string{
			"branch": "feature-a",
		},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	workspacePath := filepath.Join(root, "workspaces", "feature-a")
	if ref.Path != workspacePath {
		t.Fatalf("workspace path = %q, want %q", ref.Path, workspacePath)
	}
	readme, err := os.ReadFile(filepath.Join(workspacePath, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile(workspace README.md) error = %v", err)
	}
	if string(readme) != "repo readme\n" {
		t.Fatalf("workspace README.md = %q, want source checkout content", readme)
	}
	if _, err := os.Stat(filepath.Join(workspacePath, ".copier-answers.yml")); err != nil {
		t.Fatalf("workspace root .copier-answers.yml was not rendered: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspacePath, ".angee", "angee.yaml")); err != nil {
		t.Fatalf("inner stack manifest was not rendered under .angee: %v", err)
	}
	siblingLink := filepath.Join(workspacePath, ".dev-sibling")
	if _, err := os.Stat(filepath.Join(siblingLink, "helper.txt")); err != nil {
		t.Fatalf("sibling source was not materialized under root source: %v", err)
	}

	stack, err := manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile(angee.yaml) error = %v", err)
	}
	workspace := stack.Workspaces["feature-a"]
	if workspace.Sources["app"].Subpath != "." {
		t.Fatalf("workspace source subpath = %q, want .", workspace.Sources["app"].Subpath)
	}
	if workspace.Sources["helper"].Subpath != ".dev-sibling" {
		t.Fatalf("workspace sibling subpath = %q, want .dev-sibling", workspace.Sources["helper"].Subpath)
	}
	if workspace.Resolved.ChainRoot != ".angee" {
		t.Fatalf("chain root = %q, want .angee", workspace.Resolved.ChainRoot)
	}
	if got := strings.TrimSpace(runGitOutput(t, workspacePath, "branch", "--show-current")); got != "feature-a" {
		t.Fatalf("workspace git branch = %q, want feature-a", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, workspacePath, "status", "--porcelain")); got != "" {
		t.Fatalf("workspace git status = %q, want clean", got)
	}

	status, err := platform.WorkspaceStatus(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if len(status.Sources) != 2 {
		t.Fatalf("workspace status sources = %#v, want two sources", status.Sources)
	}
	foundRoot := false
	foundSibling := false
	for _, source := range status.Sources {
		switch source.Slot {
		case "app":
			foundRoot = true
			if source.Path != workspacePath || source.State != "clean" || source.Dirty {
				t.Fatalf("workspace root source status = %#v, want clean source at workspace root", source)
			}
		case "helper":
			foundSibling = true
			if source.Path != siblingLink || source.State != "clean" || source.Dirty {
				t.Fatalf("workspace sibling source status = %#v, want clean git source at sibling path", source)
			}
		}
	}
	if !foundRoot || !foundSibling {
		t.Fatalf("workspace sources = %#v, want app and helper", status.Sources)
	}
}

func TestOrderWorkspaceSourceMaterializationsRejectsImpossibleRootLayouts(t *testing.T) {
	tests := []struct {
		name  string
		items []workspaceSourceMaterialization
		want  string
	}{
		{
			name: "root local source",
			items: []workspaceSourceMaterialization{
				{slot: "app", source: manifest.Source{Kind: "local"}, resolved: manifest.WorkspaceSource{Subpath: "."}},
			},
			want: "only supported for git sources",
		},
		{
			name: "multiple roots",
			items: []workspaceSourceMaterialization{
				{slot: "app", source: manifest.Source{Kind: "git"}, resolved: manifest.WorkspaceSource{Subpath: "."}},
				{slot: "docs", source: manifest.Source{Kind: "git"}, resolved: manifest.WorkspaceSource{Subpath: "."}},
			},
			want: "only one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := orderWorkspaceSourceMaterializations(tt.items)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("orderWorkspaceSourceMaterializations() error = %v, want %q", err, tt.want)
			}
		})
	}
	ordered, err := orderWorkspaceSourceMaterializations([]workspaceSourceMaterialization{
		{slot: "helper", source: manifest.Source{Kind: "local"}, resolved: manifest.WorkspaceSource{Subpath: ".dev-sibling"}},
		{slot: "app", source: manifest.Source{Kind: "git"}, resolved: manifest.WorkspaceSource{Subpath: "."}},
	})
	if err != nil {
		t.Fatalf("orderWorkspaceSourceMaterializations(root+sibling) error = %v", err)
	}
	if len(ordered) != 2 || ordered[0].slot != "app" || ordered[1].slot != "helper" {
		t.Fatalf("ordered materializations = %#v, want root source first", ordered)
	}
	if _, err := normalizeWorkspaceSubpath("../outside"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("normalizeWorkspaceSubpath(../outside) error = %v, want escape error", err)
	}
}

func TestWorkspaceSourceStatusRejectsPersistedEscapingSubpath(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), ".angee")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {Kind: "git", Repo: "https://example.invalid/app.git"},
		},
		Workspaces: map[string]manifest.Workspace{
			"feature-a": {
				Template: "workspaces/dev-pr",
				Sources: map[string]manifest.WorkspaceSource{
					"app": {Source: "app", Mode: "worktree", Branch: "feature-a", Subpath: "../outside"},
				},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	status, err := platform.WorkspaceStatus(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if len(status.Sources) != 1 || status.Sources[0].State != "error" || !strings.Contains(status.Sources[0].Error, "escapes") {
		t.Fatalf("workspace source status = %#v, want escaping subpath error", status.Sources)
	}
}

func TestWorkspaceCreateRejectsRootWorktreeCacheInsideDestination(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	root := filepath.Join(base, ".angee")
	workspaceTemplate := writeRootGitWorkspaceTemplateWithCachePath(t, base, remote, "workspaces/feature-a/.cache/app")

	runGit(t, "", "init", "--bare", remote)
	runGit(t, "", "clone", remote, seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(seed, "README.md"), "repo readme\n")
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")

	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{
		Template: workspaceTemplate,
		Name:     "feature-a",
		Inputs: map[string]string{
			"branch": "feature-a",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cache path") {
		t.Fatalf("WorkspaceCreate() error = %v, want cache path overlap error", err)
	}
}

func TestWorkspaceStatusIncludesRuntimeFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".angee")
	if err := os.MkdirAll(filepath.Join(root, "workspaces", "feature-storage"), 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace) error = %v", err)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Workspaces: map[string]manifest.Workspace{
			"feature-storage": {
				Template: "workspace",
				Resolved: manifest.WorkspaceResolved{
					Allocations: map[string]int{
						"custom":     10002,
						"playwright": 9225,
					},
				},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	status, err := platform.WorkspaceStatus(context.Background(), "feature-storage")
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if status.ProcessComposePort != 10002 {
		t.Fatalf("ProcessComposePort = %d, want 10002", status.ProcessComposePort)
	}
	if status.PlaywrightMCPName != "playwright-feature-storage" {
		t.Fatalf("PlaywrightMCPName = %q, want playwright-feature-storage", status.PlaywrightMCPName)
	}
	if status.PlaywrightMCPURL != "http://127.0.0.1:9225/mcp" {
		t.Fatalf("PlaywrightMCPURL = %q, want http://127.0.0.1:9225/mcp", status.PlaywrightMCPURL)
	}
}

func TestWorkspaceDestroyRefusesUnpushedGitSource(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	workspaceSourcePath := filepath.Join(root, "workspaces", workspaceName, "app")

	runGit(t, "", "init", "--bare", remote)
	runGit(t, "", "clone", remote, cache)
	runGit(t, cache, "remote", "rename", "origin", "fork")
	runGit(t, cache, "config", "user.email", "test@example.com")
	runGit(t, cache, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(cache, "README.md"), "hello\n")
	runGit(t, cache, "add", "README.md")
	runGit(t, cache, "commit", "-m", "initial")
	runGit(t, cache, "branch", "-M", "main")
	runGit(t, cache, "push", "-u", "fork", "main")

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	runGit(t, cache, "worktree", "add", "-b", workspaceName, workspaceSourcePath, "main")
	mustWriteFile(t, filepath.Join(workspaceSourcePath, "change.txt"), "change\n")
	runGit(t, workspaceSourcePath, "add", "change.txt")
	runGit(t, workspaceSourcePath, "commit", "-m", "workspace change")

	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {
				Kind:       "git",
				Repo:       remote,
				DefaultRef: "main",
				CachePath:  cache,
			},
		},
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {
				Template: "workspaces/dev-pr",
				Sources: map[string]manifest.WorkspaceSource{
					"app": {
						Source:  "app",
						Mode:    "worktree",
						Branch:  workspaceName,
						Ref:     "main",
						Subpath: "app",
					},
				},
			},
		},
		Services: map[string]manifest.Service{
			"worker": {
				Runtime: manifest.RuntimeLocal,
				Command: []string{"true"},
				Mounts:  manifest.StringList{"workspace://" + workspaceName + ":/workspace"},
				Workdir: "workspace://" + workspaceName + "/app",
				Env: map[string]string{
					"WORKSPACE_PATH": "${workspace." + workspaceName + ".path}",
				},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}

	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = platform.WorkspaceDestroy(ctx, workspaceName, true)
	if err == nil {
		t.Fatal("WorkspaceDestroy() error = nil, want unpushed git source error")
	}
	if !strings.Contains(err.Error(), "not been pushed") || !strings.Contains(err.Error(), "app") {
		t.Fatalf("WorkspaceDestroy() error = %q, want unpushed app source", err)
	}
	if _, err := os.Stat(workspaceSourcePath); err != nil {
		t.Fatalf("workspace source was removed after refused destroy: %v", err)
	}
	saved, err := manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile(angee.yaml) error = %v", err)
	}
	if _, ok := saved.Workspaces[workspaceName]; !ok {
		t.Fatalf("workspace was removed from manifest after refused destroy")
	}
	status, err := platform.WorkspaceStatus(ctx, workspaceName)
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if status.State != "ready" || !status.Exists {
		t.Fatalf("workspace status state=%q exists=%v, want ready/true", status.State, status.Exists)
	}
	if len(status.Sources) != 1 {
		t.Fatalf("workspace status sources = %#v, want one source", status.Sources)
	}
	sourceStatus := status.Sources[0]
	if sourceStatus.State != "ahead" || sourceStatus.Pushed || sourceStatus.Ahead != 1 || sourceStatus.Upstream != "" {
		t.Fatalf("workspace source status = %#v, want unpushed ahead source without upstream", sourceStatus)
	}
	if len(status.MountedBy) != 3 {
		t.Fatalf("workspace mounted_by = %#v, want mount, workdir, and env refs", status.MountedBy)
	}

	states, err := platform.WorkspacePush(ctx, workspaceName, "")
	if err != nil {
		t.Fatalf("WorkspacePush() error = %v", err)
	}
	if len(states) != 1 || states[0].Slot != "app" {
		t.Fatalf("WorkspacePush() states = %#v, want app state", states)
	}
	upstream := strings.TrimSpace(runGitOutput(t, workspaceSourcePath, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"))
	if upstream != "fork/"+workspaceName {
		t.Fatalf("workspace upstream = %q, want fork/%s", upstream, workspaceName)
	}
	status, err = platform.WorkspaceStatus(ctx, workspaceName)
	if err != nil {
		t.Fatalf("WorkspaceStatus() after push error = %v", err)
	}
	if len(status.Sources) != 1 || status.Sources[0].State != "clean" || !status.Sources[0].Pushed || status.Sources[0].Upstream != "fork/"+workspaceName {
		t.Fatalf("workspace source status after push = %#v, want clean pushed fork upstream", status.Sources)
	}
	if err := platform.WorkspaceDestroy(ctx, workspaceName, true); err != nil {
		t.Fatalf("WorkspaceDestroy() after push error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", workspaceName)); !os.IsNotExist(err) {
		t.Fatalf("workspace dir still exists after purge, stat error = %v", err)
	}
	saved, err = manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile(angee.yaml) after destroy error = %v", err)
	}
	if _, ok := saved.Workspaces[workspaceName]; ok {
		t.Fatalf("workspace still present in manifest after destroy")
	}
}

func TestWorkspaceStatusReportsBranchMismatchAndGuardsMutations(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, workspaceSourcePath, cache := setupGitWorkspace(t)

	runGit(t, workspaceSourcePath, "switch", "-c", "codex/feature-a")

	status, err := platform.WorkspaceStatus(ctx, workspaceName)
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if len(status.Sources) != 1 {
		t.Fatalf("workspace sources = %#v, want one source", status.Sources)
	}
	if status.State != "discrepancy" {
		t.Fatalf("workspace state = %q, want discrepancy", status.State)
	}
	source := status.Sources[0]
	if source.State != workspaceSourceStateBranchMismatch || source.CurrentRef != "codex/feature-a" || source.Pushed {
		t.Fatalf("workspace source status = %#v, want branch mismatch on codex/feature-a", source)
	}
	if !strings.Contains(source.UnpushedReason, "expected workspace branch \"feature-a\"") {
		t.Fatalf("branch mismatch reason = %q, want expected branch detail", source.UnpushedReason)
	}

	if _, err := platform.WorkspacePush(ctx, workspaceName, ""); err == nil || !strings.Contains(err.Error(), "branch mismatch") {
		t.Fatalf("WorkspacePush() error = %v, want branch mismatch", err)
	}
	if err := platform.WorkspaceDestroy(ctx, workspaceName, true); err == nil || !strings.Contains(err.Error(), "branch mismatch") {
		t.Fatalf("WorkspaceDestroy() error = %v, want branch mismatch", err)
	}

	runGit(t, workspaceSourcePath, "switch", workspaceName)
	runGit(t, cache, "switch", "-c", "cache-holder")
	runGit(t, workspaceSourcePath, "switch", "main")

	status, err = platform.WorkspaceStatus(ctx, workspaceName)
	if err != nil {
		t.Fatalf("WorkspaceStatus() on main error = %v", err)
	}
	source = status.Sources[0]
	if status.State != "discrepancy" || source.State != workspaceSourceStateBranchMismatch || source.CurrentRef != "main" {
		t.Fatalf("workspace status on main = %#v source = %#v, want branch mismatch", status, source)
	}
}

func TestWorkspaceSyncBaseKeepsWorkspaceBranch(t *testing.T) {
	ctx := context.Background()
	platform, workspaceName, workspaceSourcePath, cache := setupGitWorkspace(t)

	mustWriteFile(t, filepath.Join(workspaceSourcePath, "workspace.txt"), "workspace update\n")
	runGit(t, workspaceSourcePath, "add", "workspace.txt")
	runGit(t, workspaceSourcePath, "commit", "-m", "workspace update")
	mustWriteFile(t, filepath.Join(cache, "main.txt"), "main update\n")
	runGit(t, cache, "add", "main.txt")
	runGit(t, cache, "commit", "-m", "main update")
	runGit(t, cache, "push", "fork", "main")

	states, err := platform.WorkspaceSyncBase(ctx, workspaceName, "merge")
	if err != nil {
		t.Fatalf("WorkspaceSyncBase() error = %v", err)
	}
	if len(states) != 1 || states[0].Slot != "app" || states[0].Branch != workspaceName || states[0].CurrentRef != workspaceName {
		t.Fatalf("WorkspaceSyncBase() states = %#v, want app still on workspace branch", states)
	}
	if states[0].State != "ahead" || states[0].Pushed {
		t.Fatalf("WorkspaceSyncBase() state = %#v, want ahead and not pushed after local base sync", states[0])
	}
	branch := strings.TrimSpace(runGitOutput(t, workspaceSourcePath, "branch", "--show-current"))
	if branch != workspaceName {
		t.Fatalf("current branch = %q, want %q", branch, workspaceName)
	}
	if _, err := os.Stat(filepath.Join(workspaceSourcePath, "main.txt")); err != nil {
		t.Fatalf("synced file missing after sync-base: %v", err)
	}

	status, err := platform.WorkspaceStatus(ctx, workspaceName)
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if status.Sources[0].State == workspaceSourceStateBranchMismatch {
		t.Fatalf("workspace source status = %#v, sync-base should not switch branches", status.Sources[0])
	}
}

func setupGitWorkspace(t *testing.T) (*Platform, string, string, string) {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	workspaceSourcePath := filepath.Join(root, "workspaces", workspaceName, "app")

	runGit(t, "", "init", "--bare", remote)
	runGit(t, "", "clone", remote, cache)
	runGit(t, cache, "remote", "rename", "origin", "fork")
	runGit(t, cache, "config", "user.email", "test@example.com")
	runGit(t, cache, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(cache, "README.md"), "hello\n")
	runGit(t, cache, "add", "README.md")
	runGit(t, cache, "commit", "-m", "initial")
	runGit(t, cache, "branch", "-M", "main")
	runGit(t, cache, "push", "-u", "fork", "main")

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	runGit(t, cache, "worktree", "add", "-b", workspaceName, workspaceSourcePath, "main")
	runGit(t, workspaceSourcePath, "config", "user.email", "test@example.com")
	runGit(t, workspaceSourcePath, "config", "user.name", "Test User")

	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {
				Kind:       "git",
				Repo:       remote,
				DefaultRef: "main",
				CachePath:  cache,
			},
		},
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {
				Template: "workspaces/dev-pr",
				Sources: map[string]manifest.WorkspaceSource{
					"app": {
						Source:  "app",
						Mode:    "worktree",
						Branch:  workspaceName,
						Ref:     "main",
						Subpath: "app",
					},
				},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return platform, workspaceName, workspaceSourcePath, cache
}

func writeWorkspaceTemplate(t *testing.T, base, sourceRoot string) string {
	t.Helper()
	templateRoot := filepath.Join(base, ".templates", "workspaces", "dev-pr")
	templateDir := filepath.Join(templateRoot, "template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: dev-pr
  instance_naming:
    pattern: "${inputs.topic | slug}"
  inputs:
    topic:
      type: str
      default: dev-pr
    source_path:
      type: str
      default: ` + sourceRoot + `
  sources:
    app:
      kind: local
      path: "${inputs.source_path}"
      subpath: app
  chain_root: app/.angee
  chain:
    - template: app/.templates/stacks/dev
      root: app
topic:
  type: str
  default: dev-pr
source_path:
  type: str
  default: ` + sourceRoot + `
`
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(workspace copier.yml) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "README.md.jinja"), []byte("workspace {{ workspace_name }}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md.jinja) error = %v", err)
	}
	return templateRoot
}

func writeChainedStackTemplate(t *testing.T, sourceRoot string) {
	t.Helper()
	manifestDir := filepath.Join(sourceRoot, ".templates", "stacks", "dev", "template", "{{ ANGEE_ROOT }}")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(stack template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: stack
  name: dev
ANGEE_ROOT:
  type: str
  default: .angee
`
	if err := os.WriteFile(filepath.Join(sourceRoot, ".templates", "stacks", "dev", "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(stack copier.yml) error = %v", err)
	}
	manifestYAML := `version: 1
kind: stack
name: chained
`
	if err := os.WriteFile(filepath.Join(manifestDir, "angee.yaml.jinja"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml.jinja) error = %v", err)
	}
}

func writeRootGitWorkspaceTemplate(t *testing.T, base, repo, helperRepo string) string {
	t.Helper()
	templateRoot := filepath.Join(base, ".templates", "workspaces", "root-pr")
	templateDir := filepath.Join(templateRoot, "template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root workspace template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: root-pr
  inputs:
    branch:
      type: str
      default: feature-a
  sources:
    app:
      kind: git
      repo: ` + repo + `
      default_ref: main
      mode: worktree
      branch: "${inputs.branch}"
      ref: main
      subpath: .
    helper:
      kind: git
      repo: ` + helperRepo + `
      default_ref: main
      mode: worktree
      branch: "${inputs.branch}-helper"
      ref: main
      subpath: .dev-sibling
  chain_root: .angee
  chain:
    - template: .templates/stacks/dev
      root: .angee
branch:
  type: str
  default: feature-a
`
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(root workspace copier.yml) error = %v", err)
	}
	return templateRoot
}

func writeRootGitWorkspaceTemplateWithCachePath(t *testing.T, base, repo, cachePath string) string {
	t.Helper()
	templateRoot := filepath.Join(base, ".templates", "workspaces", "root-cache")
	templateDir := filepath.Join(templateRoot, "template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root cache workspace template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: root-cache
  inputs:
    branch:
      type: str
      default: feature-a
  sources:
    app:
      kind: git
      repo: ` + repo + `
      default_ref: main
      cache_path: ` + cachePath + `
      mode: worktree
      branch: "${inputs.branch}"
      ref: main
      subpath: .
branch:
  type: str
  default: feature-a
`
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(root cache workspace copier.yml) error = %v", err)
	}
	return templateRoot
}

func writeRootSourceStackTemplate(t *testing.T, sourceRoot string) {
	t.Helper()
	templateDir := filepath.Join(sourceRoot, ".templates", "stacks", "dev", "template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root source stack template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: stack
  name: dev
`
	if err := os.WriteFile(filepath.Join(sourceRoot, ".templates", "stacks", "dev", "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(root source stack copier.yml) error = %v", err)
	}
	manifestYAML := `version: 1
kind: stack
name: root-source-dev
`
	if err := os.WriteFile(filepath.Join(templateDir, "angee.yaml.jinja"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(root source stack angee.yaml.jinja) error = %v", err)
	}
}

func mustWriteFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = runGitOutput(t, dir, args...)
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v error = %v: %s", args, err, out)
	}
	return string(out)
}
