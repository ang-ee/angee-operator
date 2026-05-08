package service

import (
	"context"
	"os"
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
