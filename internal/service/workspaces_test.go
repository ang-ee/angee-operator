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

func TestWorkspaceUpdateRerendersOuterAndInnerTemplates(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "host",
		Operator: manifest.Operator{PortPool: map[string]manifest.PortPool{
			"web": {Range: "8123-8123"},
		}},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(host): %v", err)
	}
	workspaceTemplate, stackTemplate := writeRerenderWorkspaceTemplates(t, base, "outer v1\n", "inner v1\n")
	platform, _ := New(root)
	platform.portUnavailable = func(int) bool { return false }
	created, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: workspaceTemplate, Name: "feature"})
	if err != nil {
		t.Fatalf("WorkspaceCreate: %v", err)
	}
	if created.Allocations["web"] != 8123 {
		t.Fatalf("web allocation = %d, want 8123", created.Allocations["web"])
	}

	if err := os.WriteFile(filepath.Join(workspaceTemplate, "template", "outer.txt.jinja"), []byte("outer v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(outer update): %v", err)
	}
	if err := os.WriteFile(filepath.Join(stackTemplate, "template", "{{ ANGEE_ROOT }}", "inner.txt.jinja"), []byte("inner v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(inner update): %v", err)
	}
	updated, err := platform.WorkspaceUpdate(ctx, "feature", api.WorkspaceUpdateRequest{})
	if err != nil {
		t.Fatalf("WorkspaceUpdate: %v", err)
	}
	if updated.Allocations["web"] != 8123 {
		t.Fatalf("updated web allocation = %d, want 8123", updated.Allocations["web"])
	}
	outer, _ := os.ReadFile(filepath.Join(created.Path, "outer.txt"))
	if string(outer) != "outer v2\n" {
		t.Fatalf("outer.txt = %q", outer)
	}
	inner, _ := os.ReadFile(filepath.Join(created.Path, ".angee", "inner.txt"))
	if string(inner) != "inner v2\n" {
		t.Fatalf("inner.txt = %q", inner)
	}
	innerStack, err := manifest.LoadFile(filepath.Join(created.Path, ".angee", "angee.yaml"))
	if err != nil {
		t.Fatalf("LoadFile(inner stack): %v", err)
	}
	if innerStack.Ports["web"].Value != 8123 {
		t.Fatalf("inner stack web port = %d, want 8123", innerStack.Ports["web"].Value)
	}
}

func TestWorkspaceUpdatePreservesConflictUnlessOverwrite(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := manifest.SaveFile(manifest.Path(root), &manifest.Stack{
		Version: 1, Kind: "stack", Name: "host",
		Operator: manifest.Operator{PortPool: map[string]manifest.PortPool{"web": {Range: "8124-8124"}}},
	}); err != nil {
		t.Fatalf("SaveFile(host): %v", err)
	}
	workspaceTemplate, _ := writeRerenderWorkspaceTemplates(t, base, "outer v1\n", "inner v1\n")
	platform, _ := New(root)
	platform.portUnavailable = func(int) bool { return false }
	created, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: workspaceTemplate, Name: "feature"})
	if err != nil {
		t.Fatalf("WorkspaceCreate: %v", err)
	}
	outerPath := filepath.Join(created.Path, "outer.txt")
	if err := os.WriteFile(outerPath, []byte("local edit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(local outer): %v", err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "user-only.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(user-only): %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceTemplate, "template", "outer.txt.jinja"), []byte("outer v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(template outer): %v", err)
	}
	parentBefore, _ := os.ReadFile(manifest.Path(root))

	if _, err := platform.WorkspaceUpdate(ctx, "feature", api.WorkspaceUpdateRequest{TTL: "1h"}); err == nil {
		t.Fatal("WorkspaceUpdate succeeded despite a locally modified template file")
	}
	outer, _ := os.ReadFile(outerPath)
	if string(outer) != "local edit\n" {
		t.Fatalf("conflicting outer.txt changed: %q", outer)
	}
	parentAfterConflict, _ := os.ReadFile(manifest.Path(root))
	if string(parentAfterConflict) != string(parentBefore) {
		t.Fatal("parent manifest changed despite workspace template conflict")
	}

	updated, err := platform.WorkspaceUpdate(ctx, "feature", api.WorkspaceUpdateRequest{TTL: "1h", Overwrite: true})
	if err != nil {
		t.Fatalf("WorkspaceUpdate(overwrite): %v", err)
	}
	if updated.TTL != "1h" {
		t.Fatalf("updated TTL = %q, want 1h", updated.TTL)
	}
	outer, _ = os.ReadFile(outerPath)
	if string(outer) != "outer v2\n" {
		t.Fatalf("overwritten outer.txt = %q", outer)
	}
	if data, err := os.ReadFile(filepath.Join(created.Path, "user-only.txt")); err != nil || string(data) != "keep\n" {
		t.Fatalf("user-only file = %q, %v", data, err)
	}
}

func writeRerenderWorkspaceTemplates(t *testing.T, base, outer, inner string) (string, string) {
	t.Helper()
	workspaceTemplate := filepath.Join(base, ".templates", "workspaces", "rerender")
	if err := os.MkdirAll(filepath.Join(workspaceTemplate, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace template): %v", err)
	}
	workspaceCopier := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: rerender
  chain_root: .angee
  chain:
    - template: stacks/rerender-inner
      root: .
      inputs:
        web_port: "${alloc.web}"
`
	if err := os.WriteFile(filepath.Join(workspaceTemplate, "copier.yml"), []byte(workspaceCopier), 0o644); err != nil {
		t.Fatalf("WriteFile(workspace copier.yml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceTemplate, "template", "outer.txt.jinja"), []byte(outer), 0o644); err != nil {
		t.Fatalf("WriteFile(outer template): %v", err)
	}

	stackTemplate := filepath.Join(base, ".templates", "stacks", "rerender-inner")
	innerRoot := filepath.Join(stackTemplate, "template", "{{ ANGEE_ROOT }}")
	if err := os.MkdirAll(innerRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(stack template): %v", err)
	}
	stackCopier := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: stack
  name: rerender-inner
ANGEE_ROOT:
  type: str
  default: .angee
web_port:
  type: int
  default: 9999
`
	if err := os.WriteFile(filepath.Join(stackTemplate, "copier.yml"), []byte(stackCopier), 0o644); err != nil {
		t.Fatalf("WriteFile(stack copier.yml): %v", err)
	}
	manifestBody := `version: 1
kind: stack
name: inner
ports:
  web:
    value: {{ web_port }}
`
	if err := os.WriteFile(filepath.Join(innerRoot, "angee.yaml.jinja"), []byte(manifestBody), 0o644); err != nil {
		t.Fatalf("WriteFile(inner angee.yaml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(innerRoot, "inner.txt.jinja"), []byte(inner), 0o644); err != nil {
		t.Fatalf("WriteFile(inner template): %v", err)
	}
	return workspaceTemplate, stackTemplate
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

// seedWorktreeRemote initializes a bare remote with a single committed main
// branch, ready to back a worktree-mode workspace source.
func seedWorktreeRemote(t *testing.T, base, remote string) {
	t.Helper()
	seed := filepath.Join(base, "seed")
	runGit(t, "", "init", "--bare", remote)
	runGit(t, "", "clone", remote, seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(seed, "README.md"), "repo readme\n")
	mustWriteFile(t, filepath.Join(seed, ".gitignore"), ".angee/\n.copier-answers.yml\n")
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
}

func TestWorkspaceCreateReclaimsLeftoverWorktree(t *testing.T) {
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
		Inputs:   map[string]string{"branch": "feature-a"},
	}
	if _, err := platform.WorkspaceCreate(ctx, req); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	workspacePath := filepath.Join(root, "workspaces", "feature-a")

	// freeWorkspaceName drops the manifest entry a successful create recorded so
	// the same name can be created again, mimicking a create that died before
	// reaching the manifest save.
	freeWorkspaceName := func() {
		stack, err := manifest.LoadFile(manifest.Path(root))
		if err != nil {
			t.Fatalf("LoadFile(angee.yaml) error = %v", err)
		}
		delete(stack.Workspaces, "feature-a")
		if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
			t.Fatalf("SaveFile(angee.yaml) error = %v", err)
		}
	}

	// Case 1: a stale "missing but already registered" worktree — the directory
	// is gone but the registration remains. It has no working tree to lose, so a
	// plain create reclaims it without sync.
	freeWorkspaceName()
	if err := os.RemoveAll(workspacePath); err != nil {
		t.Fatalf("RemoveAll(workspace) error = %v", err)
	}
	if _, err := platform.WorkspaceCreate(ctx, req); err != nil {
		t.Fatalf("WorkspaceCreate() reclaiming missing worktree error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, workspacePath, "branch", "--show-current")); got != "feature-a" {
		t.Fatalf("workspace git branch after missing-worktree reclaim = %q, want feature-a", got)
	}

	// Case 2: a populated leftover worktree — the directory is still present with
	// its contents. A plain create must refuse it; only sync reclaims it.
	freeWorkspaceName()
	leftoverFile := filepath.Join(workspacePath, "README.md")
	// Run the plain create twice: each must refuse identically, and — crucially —
	// the refusal must never delete the leftover (the rollback only undoes what a
	// create itself materialized, not a pre-existing leftover the sync gate
	// protects).
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := platform.WorkspaceCreate(ctx, req); err == nil {
			t.Fatalf("WorkspaceCreate() attempt %d over populated worktree error = nil, want already-exists failure", attempt)
		} else if !strings.Contains(err.Error(), "already exists and is not empty") {
			t.Fatalf("WorkspaceCreate() attempt %d over populated worktree error = %v, want \"already exists and is not empty\"", attempt, err)
		}
		if _, err := os.Stat(leftoverFile); err != nil {
			t.Fatalf("leftover worktree file after refused create attempt %d: Stat err = %v, want preserved", attempt, err)
		}
	}
	syncReq := req
	syncReq.Sync = true
	if _, err := platform.WorkspaceCreate(ctx, syncReq); err != nil {
		t.Fatalf("WorkspaceCreate(sync) error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, workspacePath, "branch", "--show-current")); got != "feature-a" {
		t.Fatalf("workspace git branch after sync reclaim = %q, want feature-a", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, workspacePath, "status", "--porcelain")); got != "" {
		t.Fatalf("workspace git status after sync reclaim = %q, want clean", got)
	}
}

// TestWorkspaceCreateSyncLeavesNonWorktreeDestinationIntact checks that sync
// only reclaims a genuine worktree: over a populated directory that is not a
// reclaimable worktree, it returns the clear "already exists" refusal (not a
// raw git error) and leaves the directory's contents untouched.
func TestWorkspaceCreateSyncLeavesNonWorktreeDestinationIntact(t *testing.T) {
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
	// A populated, non-worktree directory sitting at the destination.
	workspacePath := filepath.Join(root, "workspaces", "feature-a")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace) error = %v", err)
	}
	strayFile := filepath.Join(workspacePath, "precious.txt")
	mustWriteFile(t, strayFile, "do not delete\n")

	if _, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{
		Template: workspaceTemplate,
		Name:     "feature-a",
		Inputs:   map[string]string{"branch": "feature-a"},
		Sync:     true,
	}); err == nil {
		t.Fatal("WorkspaceCreate(sync) over non-worktree dir error = nil, want already-exists failure")
	} else if !strings.Contains(err.Error(), "already exists and is not empty") {
		t.Fatalf("WorkspaceCreate(sync) over non-worktree dir error = %v, want \"already exists and is not empty\"", err)
	}
	if got, err := os.ReadFile(strayFile); err != nil || string(got) != "do not delete\n" {
		t.Fatalf("stray file after refused sync create: contents=%q err=%v, want preserved", got, err)
	}
}

// TestWorkspaceCreateRefusesBranchLiveInSiblingWorktree is the regression for
// the data-loss path: sync must not force a second checkout of a branch that is
// already live in another workspace's worktree (which would let commits in one
// silently clobber the other). Sibling workspaces share one source cache, so a
// reclaim must never reach for --force.
func TestWorkspaceCreateRefusesBranchLiveInSiblingWorktree(t *testing.T) {
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
	first := api.WorkspaceCreateRequest{
		Template: workspaceTemplate,
		Name:     "alpha",
		Inputs:   map[string]string{"branch": "shared"},
	}
	if _, err := platform.WorkspaceCreate(ctx, first); err != nil {
		t.Fatalf("WorkspaceCreate(alpha) error = %v", err)
	}
	alphaPath := filepath.Join(root, "workspaces", "alpha")

	// A second workspace resolving the same branch, even with sync, must be
	// refused — the branch is live in alpha's worktree.
	second := api.WorkspaceCreateRequest{
		Template: workspaceTemplate,
		Name:     "beta",
		Inputs:   map[string]string{"branch": "shared"},
		Sync:     true,
	}
	if _, err := platform.WorkspaceCreate(ctx, second); err == nil {
		t.Fatal("WorkspaceCreate(beta, sync) error = nil, want branch-already-checked-out failure")
	} else if !strings.Contains(err.Error(), "already used by worktree") {
		t.Fatalf("WorkspaceCreate(beta, sync) error = %v, want \"already used by worktree\"", err)
	}

	// alpha is untouched: still on its branch and clean, with no clobbering.
	if got := strings.TrimSpace(runGitOutput(t, alphaPath, "branch", "--show-current")); got != "shared" {
		t.Fatalf("alpha git branch = %q, want shared", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, alphaPath, "status", "--porcelain")); got != "" {
		t.Fatalf("alpha git status = %q, want clean", got)
	}
}

// TestWorkspaceCreateRollsBackFailedWorktree is the regression for the
// non-transactional create: a create that fails after materializing a worktree
// must roll the worktree back, leaving no stranded registration in the shared
// cache and no half-rendered workspace directory.
func TestWorkspaceCreateRollsBackFailedWorktree(t *testing.T) {
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
	// An invalid TTL fails after the worktree is materialized.
	if _, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{
		Template: workspaceTemplate,
		Name:     "feature-a",
		Inputs:   map[string]string{"branch": "feature-a"},
		TTL:      "not-a-duration",
	}); err == nil {
		t.Fatal("WorkspaceCreate() with invalid TTL error = nil, want parse failure")
	}

	workspacePath := filepath.Join(root, "workspaces", "feature-a")
	if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
		t.Fatalf("workspace directory after failed create: Stat err = %v, want not-exist", err)
	}
	// The cache holds only its own main checkout — the failed worktree was
	// deregistered, not stranded as a prunable entry.
	cachePath := filepath.Join(root, ".cache", "app")
	if got := strings.Count(runGitOutput(t, cachePath, "worktree", "list", "--porcelain"), "worktree "); got != 1 {
		t.Fatalf("registered worktrees in cache after rollback = %d, want 1 (cache only)", got)
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
