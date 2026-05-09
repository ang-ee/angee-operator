package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/manifest"
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
