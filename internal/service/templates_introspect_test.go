package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/internal/query"
)

func TestTemplatesDiscoversBothDirectoryConventions(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
`)
	// Same name under the alternate convention should de-duplicate.
	altDir := filepath.Join(root, "templates", "workspaces", "dev-pr")
	if err := os.MkdirAll(altDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(alt) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(altDir, "copier.yml"), []byte(`_angee:
  kind: workspace
  name: dev-pr
`), 0o644); err != nil {
		t.Fatalf("WriteFile(alt copier.yml) error = %v", err)
	}
	// A second, distinct template.
	stacksDir := filepath.Join(root, ".templates", "stacks", "minimal")
	if err := os.MkdirAll(stacksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(stacks) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(stacksDir, "copier.yml"), []byte(`_angee:
  kind: stack
  name: minimal
`), 0o644); err != nil {
		t.Fatalf("WriteFile(stack copier.yml) error = %v", err)
	}

	p, _ := New(root)
	descs, _, err := p.Templates(context.Background(), query.Args{})
	if err != nil {
		t.Fatalf("Templates() error = %v", err)
	}
	if len(descs) != 2 {
		t.Fatalf("Templates() returned %d descriptors, want 2: %+v", len(descs), descs)
	}
	// Sorted: stacks/minimal, workspaces/dev-pr.
	if descs[0].Ref != "stacks/minimal" || descs[1].Ref != "workspaces/dev-pr" {
		t.Fatalf("refs = %q,%q, want stacks/minimal,workspaces/dev-pr", descs[0].Ref, descs[1].Ref)
	}
	if descs[1].Kind != "workspace" {
		t.Fatalf("workspace kind = %q, want workspace", descs[1].Kind)
	}
}

func TestTemplateReturnsInputDescriptors(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
      type: string
    branch:
      default: main
`)
	p, _ := New(root)
	desc, err := p.Template(context.Background(), "workspaces/dev-pr")
	if err != nil {
		t.Fatalf("Template() error = %v", err)
	}
	if desc.Ref != "workspaces/dev-pr" || desc.Kind != "workspace" {
		t.Fatalf("ref/kind = %q/%q, want workspaces/dev-pr / workspace", desc.Ref, desc.Kind)
	}
	byName := map[string]bool{}
	for _, in := range desc.Inputs {
		byName[in.Name] = in.Required
	}
	if !byName["topic"] {
		t.Fatalf("inputs missing required topic: %+v", desc.Inputs)
	}
	if _, ok := byName["branch"]; !ok {
		t.Fatalf("inputs missing branch: %+v", desc.Inputs)
	}
}

func TestTemplatesEmptyRoot(t *testing.T) {
	root := t.TempDir()
	p, _ := New(root)
	descs, _, err := p.Templates(context.Background(), query.Args{})
	if err != nil {
		t.Fatalf("Templates() error = %v", err)
	}
	if len(descs) != 0 {
		t.Fatalf("Templates() = %v, want empty", descs)
	}
}
