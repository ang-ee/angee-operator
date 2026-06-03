package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fyltr/angee/internal/manifest"
)

// --- merge unit tests (no rendering) ---

func TestMergeStackFromTemplatePreservesRuntimeAndRefreshesTemplate(t *testing.T) {
	ours := &manifest.Stack{
		Version: 1, Kind: "stack", Name: "demo",
		Services: map[string]manifest.Service{
			"web":        {Runtime: manifest.RuntimeContainer, Image: "nginx:1.0"}, // refreshed by theirs
			"user-extra": {Runtime: manifest.RuntimeContainer, Image: "extra:1.0"}, // user-added, preserved
		},
		Ports: map[string]manifest.Port{
			"web": {Value: 8005}, // allocated value must be kept
		},
		Operator:   manifest.Operator{Domain: "op.local", PortPool: map[string]manifest.PortPool{"web": {Range: "8000-8099"}}},
		Workspaces: map[string]manifest.Workspace{"ws1": {Template: "workspaces/dev"}},
		PortLeases: map[string][]manifest.PortLease{"web": {{Port: 8005, Owner: "service/web/web"}}},
	}
	theirs := &manifest.Stack{
		Version: 1, Kind: "stack", Name: "demo",
		Services: map[string]manifest.Service{
			"web":      {Runtime: manifest.RuntimeContainer, Image: "nginx:2.0"}, // newer image
			"frontend": {Runtime: manifest.RuntimeContainer, Image: "vite:1.0"},  // new template service
		},
		Ports: map[string]manifest.Port{
			"web": {Value: 3000}, // template default, must NOT override allocated 8005
		},
		Operator: manifest.Operator{Domain: "TEMPLATE-SHOULD-NOT-WIN"},
		Template: &manifest.Template{Active: "stacks/dev", AnswersFile: ".copier-answers.yml"},
	}

	merged := mergeStackFromTemplate(ours, theirs)

	if got := merged.Services["web"].Image; got != "nginx:2.0" {
		t.Fatalf("web image = %q, want refreshed nginx:2.0", got)
	}
	if _, ok := merged.Services["frontend"]; !ok {
		t.Fatal("frontend service not added from template")
	}
	if _, ok := merged.Services["user-extra"]; !ok {
		t.Fatal("user-added service was dropped")
	}
	if got := merged.Ports["web"].Value; got != 8005 {
		t.Fatalf("port web value = %d, want allocated 8005 preserved", got)
	}
	if merged.Operator.Domain != "op.local" {
		t.Fatalf("operator clobbered by template: %q", merged.Operator.Domain)
	}
	if len(merged.Operator.PortPool) == 0 {
		t.Fatal("operator.port_pool lost")
	}
	if _, ok := merged.Workspaces["ws1"]; !ok {
		t.Fatal("workspace lost")
	}
	if len(merged.PortLeases["web"]) == 0 {
		t.Fatal("port lease lost")
	}
	if merged.Template == nil || merged.Template.Active != "stacks/dev" {
		t.Fatalf("template metadata not refreshed: %+v", merged.Template)
	}
	// ours must be untouched (merge builds fresh maps).
	if ours.Services["web"].Image != "nginx:1.0" {
		t.Fatal("merge mutated ours")
	}
}

func TestSummarizeStackChangesReportsAddedAndModified(t *testing.T) {
	ours := &manifest.Stack{Services: map[string]manifest.Service{
		"web":  {Image: "nginx:1.0"},
		"keep": {Image: "keep:1.0"},
	}}
	merged := &manifest.Stack{
		Services: map[string]manifest.Service{
			"web":      {Image: "nginx:2.0"}, // modified
			"keep":     {Image: "keep:1.0"},  // unchanged → no entry
			"frontend": {Image: "vite:1.0"},  // added
		},
		Jobs: map[string]manifest.Job{"deps": {}}, // added
	}
	changes := summarizeStackChanges(ours, merged)
	want := map[string]bool{
		"~ services/web":      true,
		"+ services/frontend": true,
		"+ jobs/deps":         true,
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %v, want keys %v", changes, want)
	}
	for _, c := range changes {
		if !want[c] {
			t.Fatalf("unexpected change %q (changes=%v)", c, changes)
		}
	}
}

func TestStackUpdateFromTemplateRequiresAnswersFile(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writeStackTemplate(t, project, oneServiceTemplate)
	p, err := New(project)
	if err != nil {
		t.Fatalf("New(project): %v", err)
	}
	res, err := p.StackInit(ctx, "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	// Simulate a legacy stack with no recorded answers. Copier writes the file
	// at the render target (the project dir, parent of the .angee root).
	for _, dir := range []string{res.Root, filepath.Dir(res.Root)} {
		_ = os.Remove(filepath.Join(dir, ".copier-answers.yml"))
	}
	sp, _ := New(res.Root)
	if _, err := sp.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err == nil {
		t.Fatal("expected error when answers file is missing")
	}
}

// --- end-to-end via a real template render ---

func writeStackTemplate(t *testing.T, projectRoot, manifestBody string) string {
	t.Helper()
	tmplDir := filepath.Join(projectRoot, ".templates", "stacks", "dev")
	inner := filepath.Join(tmplDir, "template", "{{ ANGEE_ROOT }}")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir template: %v", err)
	}
	copierYML := "_subdirectory: template\n_templates_suffix: .jinja\n_angee:\n  kind: stack\n  name: dev\n  inputs:\n    ANGEE_ROOT:\n      type: string\n      default: .angee\nANGEE_ROOT:\n  type: str\n  default: .angee\n"
	if err := os.WriteFile(filepath.Join(tmplDir, "copier.yml"), []byte(copierYML), 0o644); err != nil {
		t.Fatalf("write copier.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inner, "angee.yaml.jinja"), []byte(manifestBody), 0o644); err != nil {
		t.Fatalf("write angee.yaml.jinja: %v", err)
	}
	return tmplDir
}

const oneServiceTemplate = `version: 1
kind: stack
name: demo
template:
  active: stacks/dev
  answers_file: .copier-answers.yml
services:
  web:
    runtime: container
    image: nginx:latest
`

const twoServiceTemplate = `version: 1
kind: stack
name: demo
template:
  active: stacks/dev
  answers_file: .copier-answers.yml
services:
  web:
    runtime: container
    image: nginx:latest
  frontend:
    runtime: container
    image: vite:latest
`

func TestStackUpdateFromTemplateEndToEnd(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writeStackTemplate(t, project, oneServiceTemplate)

	p, err := New(project)
	if err != nil {
		t.Fatalf("New(project): %v", err)
	}
	res, err := p.StackInit(ctx, "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	sp, err := New(res.Root)
	if err != nil {
		t.Fatalf("New(stackRoot): %v", err)
	}

	// Inject operator-managed runtime state that must survive the re-render.
	stack, err := sp.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack: %v", err)
	}
	stack.Operator.PortPool = map[string]manifest.PortPool{"web": {Range: "8000-8099"}}
	stack.PortLeases = map[string][]manifest.PortLease{"web": {{Port: 8005, Owner: "service/web/web"}}}
	stack.Workspaces = map[string]manifest.Workspace{"ws1": {Template: "workspaces/dev"}}
	if err := manifest.SaveFile(manifest.Path(res.Root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}

	// 1) No-op when the template is unchanged.
	noop, err := sp.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate (no-op): %v", err)
	}
	if noop.Changed {
		t.Fatalf("expected no change on unchanged template, got changes=%v", noop.Changes)
	}

	// The template gains a frontend service.
	writeStackTemplate(t, project, twoServiceTemplate)

	// 2) Dry-run reports the change but does not write the manifest.
	beforeBytes, _ := os.ReadFile(manifest.Path(res.Root))
	dry, err := sp.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{DryRun: true})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate (dry-run): %v", err)
	}
	if !dry.Changed {
		t.Fatal("dry-run: expected Changed=true")
	}
	afterDryBytes, _ := os.ReadFile(manifest.Path(res.Root))
	if string(beforeBytes) != string(afterDryBytes) {
		t.Fatal("dry-run wrote the manifest")
	}

	// 3) The real update adds the service and preserves runtime state.
	applied, err := sp.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate (apply): %v", err)
	}
	if !applied.Changed {
		t.Fatal("apply: expected Changed=true")
	}
	updated, err := sp.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack after update: %v", err)
	}
	if _, ok := updated.Services["frontend"]; !ok {
		t.Fatalf("frontend service not added; services=%v", sortedKeys(updated.Services))
	}
	if _, ok := updated.Services["web"]; !ok {
		t.Fatal("web service lost")
	}
	if len(updated.Operator.PortPool) == 0 {
		t.Fatal("operator.port_pool lost across re-render")
	}
	if len(updated.PortLeases["web"]) == 0 {
		t.Fatal("port lease lost across re-render")
	}
	if _, ok := updated.Workspaces["ws1"]; !ok {
		t.Fatal("workspace lost across re-render")
	}
}
