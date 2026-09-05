package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ang-ee/angee-operator/api"
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

func TestTemplateInputsPreserveQuestionOrderAndDetails(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    z_generated:
      type: int
      generated: true
    a_metadata:
      help: Metadata-only help
      immutable: true
      when: false
    runtime_mode:
      help: Overridden help
      default: obsolete
      secret: true
runtime_mode:
  type: str
  default: process
  help: Choose how to run services.
  placeholder: Pick a runtime
  multiselect: true
  choices:
    Local processes: process
    Docker containers: docker
  when: '{{ enabled }}'
  validator: '{{ "Select a runtime" if not runtime_mode else "" }}'
api_key:
  type: str
  required: true
  secret: true
  when: true
dynamic:
  choices: '{{ available_options }}'
`)
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := p.Template(context.Background(), "workspaces/dev-pr")
	if err != nil {
		t.Fatal(err)
	}
	want := []api.TemplateInputDescriptor{
		{
			Name: "runtime_mode", Type: "str", Default: "process", Question: true, Order: 0,
			Help: "Choose how to run services.", Placeholder: "Pick a runtime", Multiselect: true,
			Choices: []api.TemplateInputChoice{{Value: "process", Label: "Local processes"}, {Value: "docker", Label: "Docker containers"}},
			When:    "{{ enabled }}", Validator: `{{ "Select a runtime" if not runtime_mode else "" }}`,
		},
		{Name: "api_key", Type: "str", Required: true, Secret: true, Question: true, Order: 1, When: "true"},
		{Name: "dynamic", Question: true, Order: 2, ChoicesExpr: "{{ available_options }}"},
		{Name: "a_metadata", Order: -1, Immutable: true, Help: "Metadata-only help", When: "false"},
		{Name: "z_generated", Type: "int", Order: -1, Generated: true},
	}
	if !reflect.DeepEqual(desc.Inputs, want) {
		t.Fatalf("inputs = %+v, want %+v", desc.Inputs, want)
	}
}

func TestTemplateResolvesServiceRef(t *testing.T) {
	root := t.TempDir()
	templatePath := filepath.Join(root, ".templates", "services", "worker")
	if err := os.MkdirAll(templatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templatePath, "copier.yml"), []byte(`_angee:
  kind: service
  name: worker
runtime_mode:
  type: str
  default: process
`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := p.Template(context.Background(), "services/worker")
	if err != nil {
		t.Fatalf("Template(services/worker): %v", err)
	}
	if desc.Ref != "services/worker" || desc.Kind != "service" || desc.Name != "worker" || desc.Path != templatePath {
		t.Fatalf("service descriptor = %+v", desc)
	}
	if len(desc.Inputs) != 1 || desc.Inputs[0].Name != "runtime_mode" || desc.Inputs[0].Order != 0 {
		t.Fatalf("service inputs = %+v", desc.Inputs)
	}
}
