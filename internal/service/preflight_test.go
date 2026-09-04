package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

func TestWorkspaceCreatePreflightFlagsMissingRequired(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
    branch:
      required: true
      default: main
    tier:
      type: int
`)
	p, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
		Template: "workspaces/dev-pr",
		Inputs:   map[string]string{"tier": "10"},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if resp.OK {
		t.Fatalf("OK = true, want false (topic is required)")
	}
	if len(resp.MissingRequired) != 1 || resp.MissingRequired[0] != "topic" {
		t.Fatalf("MissingRequired = %v, want [topic]", resp.MissingRequired)
	}
	if got := resp.EffectiveInputs["branch"]; got != "main" {
		t.Fatalf("EffectiveInputs[branch] = %q, want main (default)", got)
	}
	if got := resp.EffectiveInputs["tier"]; got != "10" {
		t.Fatalf("EffectiveInputs[tier] = %q, want 10 (provided)", got)
	}
}

func TestWorkspaceCreatePreflightFlagsTypeMismatch(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    count:
      type: int
    enabled:
      type: bool
`)
	p, _ := New(root)
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
		Template: "workspaces/dev-pr",
		Inputs:   map[string]string{"count": "abc", "enabled": "maybe"},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if resp.OK {
		t.Fatalf("OK = true, want false")
	}
	if len(resp.InvalidInputs) != 2 {
		t.Fatalf("InvalidInputs = %v, want 2 entries", resp.InvalidInputs)
	}
	// Sorted alphabetically: count, enabled.
	if resp.InvalidInputs[0].Field != "count" || resp.InvalidInputs[1].Field != "enabled" {
		t.Fatalf("InvalidInputs ordering = %+v", resp.InvalidInputs)
	}
}

func TestWorkspaceCreatePreflightOKWhenSatisfied(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
`)
	p, _ := New(root)
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
		Template: "workspaces/dev-pr",
		Inputs:   map[string]string{"topic": "feature"},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if !resp.OK {
		t.Fatalf("OK = false, want true (got missing=%v, invalid=%v)", resp.MissingRequired, resp.InvalidInputs)
	}
	if resp.ResolvedTemplate == "" {
		t.Fatalf("ResolvedTemplate empty, want resolved ref")
	}
}

func writePreflightTemplate(t *testing.T, root, copierYAML string) {
	t.Helper()
	templateRoot := filepath.Join(root, ".templates", "workspaces", "dev-pr")
	if err := os.MkdirAll(filepath.Join(templateRoot, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template) error = %v", err)
	}
	full := `_subdirectory: template
_templates_suffix: .jinja
` + copierYAML
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(full), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml) error = %v", err)
	}
}

func TestWorkspaceCreatePreflightAppliesStackWorkspaceDefaults(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
    work_state_source:
      type: str
      default: ""
`)
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "host",
		WorkspaceDefaults: map[string]manifest.WorkspaceDefaults{
			"dev-pr": {Inputs: map[string]string{"topic": "from-stack", "work_state_source": "work-angee"}},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	p, _ := New(root)

	// The stack default satisfies the required input on its own.
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{Template: "workspaces/dev-pr"})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if !resp.OK {
		t.Fatalf("OK = false, want true (missing=%v, invalid=%v)", resp.MissingRequired, resp.InvalidInputs)
	}
	if resp.EffectiveInputs["topic"] != "from-stack" || resp.EffectiveInputs["work_state_source"] != "work-angee" {
		t.Fatalf("EffectiveInputs = %v, want the stack defaults applied", resp.EffectiveInputs)
	}
	if resp.StackDefaults["work_state_source"] != "work-angee" {
		t.Fatalf("StackDefaults = %v, want the stack's inputs reported", resp.StackDefaults)
	}

	// An explicit input, even an empty one, wins over the stack default.
	resp, err = p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
		Template: "dev-pr",
		Inputs:   map[string]string{"topic": "mine", "work_state_source": ""},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if resp.EffectiveInputs["topic"] != "mine" || resp.EffectiveInputs["work_state_source"] != "" {
		t.Fatalf("EffectiveInputs = %v, want explicit inputs to win", resp.EffectiveInputs)
	}
}
