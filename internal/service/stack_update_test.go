package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

// --- merge unit tests (no rendering) ---

func TestMergeStackFromTemplatePreservesRuntimeAndRefreshesTemplate(t *testing.T) {
	ours := &manifest.Stack{
		Version: 1, Kind: "stack", Name: "demo",
		Ingress: manifest.Ingress{Type: "none"},
		Services: map[string]manifest.Service{
			"web":        {Runtime: manifest.RuntimeContainer, Image: "nginx:1.0"}, // refreshed by theirs
			"user-extra": {Runtime: manifest.RuntimeContainer, Image: "extra:1.0"}, // user-added, preserved
		},
		Ports: map[string]manifest.Port{
			"web": {Value: 8005}, // allocated value must be kept
		},
		Operator:   manifest.Operator{Domain: "op.local", PortPool: map[string]manifest.PortPool{"web": {Range: "8000-8099"}}},
		Workspaces: map[string]manifest.Workspace{"ws1": {Template: "workspaces/dev"}},
		WorkspaceDefaults: map[string]manifest.WorkspaceDefaults{
			"workspaces/src":   {Inputs: map[string]string{"work_state_source": "old-work"}}, // refreshed by theirs
			"workspaces/extra": {Inputs: map[string]string{"note": "user"}},                  // user-added, preserved
		},
		PortLeases: map[string][]manifest.PortLease{"web": {{Port: 8005, Owner: "service/web/web"}}},
	}
	theirs := &manifest.Stack{
		Version: 1, Kind: "stack", Name: "demo",
		Ingress: manifest.Ingress{Type: "caddy", Domain: "agents.localhost"},
		Services: map[string]manifest.Service{
			"web":      {Runtime: manifest.RuntimeContainer, Image: "nginx:2.0"}, // newer image
			"frontend": {Runtime: manifest.RuntimeContainer, Image: "vite:1.0"},  // new template service
		},
		Ports: map[string]manifest.Port{
			"web": {Value: 3000}, // template default, must NOT override allocated 8005
		},
		Operator: manifest.Operator{Domain: "TEMPLATE-SHOULD-NOT-WIN"},
		Template: &manifest.Template{Active: "stacks/dev", AnswersFile: ".copier-answers.yml"},
		WorkspaceDefaults: map[string]manifest.WorkspaceDefaults{
			"workspaces/src": {Inputs: map[string]string{"work_state_source": "work-angee"}},
		},
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
	if got := merged.WorkspaceDefaults["workspaces/src"].Inputs["work_state_source"]; got != "work-angee" {
		t.Fatalf("workspace_defaults/workspaces/src not refreshed from template: %q", got)
	}
	if _, ok := merged.WorkspaceDefaults["workspaces/extra"]; !ok {
		t.Fatal("user-added workspace_defaults entry was dropped")
	}
	if len(merged.PortLeases["web"]) == 0 {
		t.Fatal("port lease lost")
	}
	if merged.Template == nil || merged.Template.Active != "stacks/dev" {
		t.Fatalf("template metadata not refreshed: %+v", merged.Template)
	}
	if merged.Ingress.Type != "caddy" || merged.Ingress.Domain != "agents.localhost" {
		t.Fatalf("ingress not refreshed: %+v", merged.Ingress)
	}
	// ours must be untouched (merge builds fresh maps).
	if ours.Services["web"].Image != "nginx:1.0" {
		t.Fatal("merge mutated ours")
	}
}

// Sources are what users add by hand between renders (#60): a re-render must
// keep a source the template does not emit, refresh one it does, and report
// both through the change summary.
func TestMergeStackFromTemplateKeepsUserSourcesAndRefreshesTemplateSources(t *testing.T) {
	ours := &manifest.Stack{
		Version: 1, Kind: "stack", Name: "demo",
		Sources: map[string]manifest.Source{
			"app":      {Kind: "git", Repo: "https://github.com/acme/app.git", DefaultRef: "main", CachePath: "/shared/app"}, // template-known, CachePath set by hand
			"site":     {Kind: "local", Path: "../site"},                                                                     // template-known, unchanged
			"user-lib": {Kind: "git", Repo: "git@github.com:me/lib.git", CachePath: "sources/lib"},                           // user-added, preserved
		},
	}
	theirs := &manifest.Stack{
		Version: 1, Kind: "stack", Name: "demo",
		Sources: map[string]manifest.Source{
			"app":  {Kind: "git", Repo: "https://github.com/acme/app.git", DefaultRef: "develop"},
			"site": {Kind: "local", Path: "../site"},
			"docs": {Kind: "local", Path: "../docs"}, // new in the template
		},
	}

	merged := mergeStackFromTemplate(ours, theirs)

	if got, want := merged.Sources["user-lib"], ours.Sources["user-lib"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("user-added source = %+v, want it preserved as %+v", got, want)
	}
	// The template's definition replaces ours whole: the hand-set CachePath,
	// which the template does not render, is gone too.
	if got, want := merged.Sources["app"], theirs.Sources["app"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("template source app = %+v, want the template's %+v", got, want)
	}
	if got, want := merged.Sources["docs"], theirs.Sources["docs"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("new template source docs = %+v, want %+v", got, want)
	}
	if len(merged.Sources) != 4 {
		t.Fatalf("merged sources = %v, want app, docs, site and user-lib", sortedKeys(merged.Sources))
	}
	if ours.Sources["app"].DefaultRef != "main" {
		t.Fatal("merge mutated ours.Sources")
	}

	// The summary reports sources through reportKeyChanges("sources", ...).
	if changes, want := summarizeStackChanges(ours, merged), []string{"~ sources/app", "+ sources/docs"}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("source changes = %v, want %v", changes, want)
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
		WorkspaceDefaults: map[string]manifest.WorkspaceDefaults{ // added
			"workspaces/src": {Inputs: map[string]string{"work_state_source": "work-angee"}},
		},
	}
	changes := summarizeStackChanges(ours, merged)
	want := map[string]bool{
		"~ services/web":                      true,
		"+ services/frontend":                 true,
		"+ jobs/deps":                         true,
		"+ workspace_defaults/workspaces/src": true,
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

const localServiceTemplate = `version: 1
kind: stack
name: demo
template:
  active: stacks/dev
  answers_file: .copier-answers.yml
services:
  web:
    runtime: local
    command: ["echo", "ready"]
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

func TestStackUpdateFromTemplateRemovesObsoleteRuntimeArtifacts(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writeStackTemplate(t, project, oneServiceTemplate)
	p, err := New(project)
	if err != nil {
		t.Fatalf("New(project): %v", err)
	}
	initialized, err := p.StackInit(ctx, "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	stackPlatform, err := New(initialized.Root)
	if err != nil {
		t.Fatalf("New(stack root): %v", err)
	}
	if _, err := stackPlatform.StackPrepare(ctx); err != nil {
		t.Fatalf("StackPrepare(container): %v", err)
	}
	composePath := filepath.Join(initialized.Root, "docker-compose.yaml")
	if _, err := os.Stat(composePath); err != nil {
		t.Fatalf("container runtime artifact missing: %v", err)
	}
	secretPath := filepath.Join(initialized.Root, "run", "secrets.env")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(run): %v", err)
	}
	if err := os.WriteFile(secretPath, []byte("STALE=secret\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(stale secrets): %v", err)
	}

	writeStackTemplate(t, project, localServiceTemplate)
	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err != nil {
		t.Fatalf("StackUpdateFromTemplate(local): %v", err)
	}
	if _, err := os.Stat(composePath); !os.IsNotExist(err) {
		t.Fatalf("obsolete docker-compose.yaml remains, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(initialized.Root, "process-compose.yaml")); err != nil {
		t.Fatalf("local runtime artifact missing: %v", err)
	}
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Fatalf("obsolete run/secrets.env remains, stat error = %v", err)
	}
}

func TestStackUpdateFromTemplateAddsRenderedFile(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	template := writeStackTemplate(t, project, oneServiceTemplate)
	p, err := New(project)
	if err != nil {
		t.Fatalf("New(project): %v", err)
	}
	initialized, err := p.StackInit(ctx, "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	stackPlatform, err := New(initialized.Root)
	if err != nil {
		t.Fatalf("New(stack root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(template, "template", "{{ ANGEE_ROOT }}", "AGENTS.md.jinja"), []byte("# Agent instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md.jinja): %v", err)
	}

	dryRun, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{DryRun: true})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate(dry-run): %v", err)
	}
	if !dryRun.Changed || !containsString(dryRun.Changes, "+ files/AGENTS.md") {
		t.Fatalf("dry-run result = %+v, want + files/AGENTS.md", dryRun)
	}
	if _, err := os.Stat(filepath.Join(initialized.Root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created AGENTS.md, stat err = %v", err)
	}

	applied, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate(apply): %v", err)
	}
	if !applied.Changed || !containsString(applied.Changes, "+ files/AGENTS.md") {
		t.Fatalf("apply result = %+v, want + files/AGENTS.md", applied)
	}
	data, err := os.ReadFile(filepath.Join(initialized.Root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("ReadFile(AGENTS.md): %v", err)
	}
	if string(data) != "# Agent instructions\n" {
		t.Fatalf("AGENTS.md = %q", data)
	}
}

func TestStackUpdateFromTemplatePreservesConflictUnlessOverwrite(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	template := writeStackTemplate(t, project, oneServiceTemplate)
	agentsTemplate := filepath.Join(template, "template", "{{ ANGEE_ROOT }}", "AGENTS.md.jinja")
	if err := os.WriteFile(agentsTemplate, []byte("template v1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md.jinja): %v", err)
	}
	p, _ := New(project)
	initialized, err := p.StackInit(ctx, "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	stackPlatform, _ := New(initialized.Root)
	agentsPath := filepath.Join(initialized.Root, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("local edit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(local AGENTS.md): %v", err)
	}
	if err := os.WriteFile(agentsTemplate, []byte("template v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(updated template): %v", err)
	}
	dryRun, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{DryRun: true})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate(dry-run conflict): %v", err)
	}
	if len(dryRun.Conflicts) != 1 || !strings.HasSuffix(dryRun.Conflicts[0].Path, "AGENTS.md") {
		t.Fatalf("dry-run conflicts = %+v, want AGENTS.md", dryRun.Conflicts)
	}

	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err == nil {
		t.Fatal("StackUpdateFromTemplate succeeded despite a locally modified tracked file")
	}
	data, _ := os.ReadFile(agentsPath)
	if string(data) != "local edit\n" {
		t.Fatalf("conflicting file changed without overwrite: %q", data)
	}

	result, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate(overwrite): %v", err)
	}
	if !containsString(result.Changes, "~ files/AGENTS.md") {
		t.Fatalf("overwrite changes = %v", result.Changes)
	}
	data, _ = os.ReadFile(agentsPath)
	if string(data) != "template v2\n" {
		t.Fatalf("overwritten AGENTS.md = %q", data)
	}
}

func TestStackUpdateFromTemplateDeletesTrackedUnchangedFile(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	template := writeStackTemplate(t, project, oneServiceTemplate)
	managedTemplate := filepath.Join(template, "template", "{{ ANGEE_ROOT }}", "obsolete.txt.jinja")
	if err := os.WriteFile(managedTemplate, []byte("obsolete\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(obsolete template): %v", err)
	}
	p, _ := New(project)
	initialized, err := p.StackInit(ctx, "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	if err := os.Remove(managedTemplate); err != nil {
		t.Fatalf("Remove(obsolete template): %v", err)
	}
	stackPlatform, _ := New(initialized.Root)
	result, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate: %v", err)
	}
	if !containsString(result.Changes, "- files/obsolete.txt") {
		t.Fatalf("changes = %v, want tracked deletion", result.Changes)
	}
	if _, err := os.Stat(filepath.Join(initialized.Root, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("obsolete.txt remains, stat err = %v", err)
	}
}

func TestStackUpdateFromTemplateSkipKeepsExistingFilesAndStillAddsMissing(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	template := writeStackTemplate(t, project, oneServiceTemplate)
	output := filepath.Join(template, "template", "{{ ANGEE_ROOT }}")
	for name, body := range map[string]string{
		"kept.txt.jinja":     "template v1\n",
		"conflict.txt.jinja": "template v1\n",
		"obsolete.txt.jinja": "obsolete\n",
	} {
		if err := os.WriteFile(filepath.Join(output, name), []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	p, err := New(project)
	if err != nil {
		t.Fatalf("New(project): %v", err)
	}
	initialized, err := p.StackInit(ctx, "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	stackPlatform, err := New(initialized.Root)
	if err != nil {
		t.Fatalf("New(stack root): %v", err)
	}
	for name, body := range map[string]string{"kept.txt": "user kept\n", "conflict.txt": "user conflict\n"} {
		if err := os.WriteFile(filepath.Join(initialized.Root, name), []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(output, "kept.txt.jinja"), []byte("template v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(kept template): %v", err)
	}
	if err := os.WriteFile(filepath.Join(output, "conflict.txt.jinja"), []byte("template v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(conflict template): %v", err)
	}
	if err := os.WriteFile(filepath.Join(output, "added.txt.jinja"), []byte("added\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(added template): %v", err)
	}
	if err := os.Remove(filepath.Join(output, "obsolete.txt.jinja")); err != nil {
		t.Fatalf("Remove(obsolete template): %v", err)
	}

	dryRun, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{DryRun: true, Skip: []string{"kept.txt", "obsolete.txt"}})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate(dry-run): %v", err)
	}
	if len(dryRun.Conflicts) != 1 || !strings.HasSuffix(dryRun.Conflicts[0].Path, "/conflict.txt") {
		t.Fatalf("conflicts = %#v, want only unmatched conflict.txt", dryRun.Conflicts)
	}
	if !containsString(dryRun.Changes, "+ files/added.txt") || containsString(dryRun.Changes, "- files/obsolete.txt") {
		t.Fatalf("changes = %v, want added.txt and no obsolete deletion", dryRun.Changes)
	}
	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Skip: []string{"kept.txt", "obsolete.txt"}}); err == nil {
		t.Fatal("StackUpdateFromTemplate succeeded with an unskipped conflict")
	}

	result, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Skip: []string{"kept.txt", "conflict.txt", "obsolete.txt"}})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate(skip all conflicts): %v", err)
	}
	if !containsString(result.Changes, "+ files/added.txt") {
		t.Fatalf("changes = %v, want added.txt", result.Changes)
	}
	for name, want := range map[string]string{
		"kept.txt": "user kept\n", "conflict.txt": "user conflict\n", "obsolete.txt": "obsolete\n", "added.txt": "added\n",
	} {
		got, readErr := os.ReadFile(filepath.Join(initialized.Root, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, readErr, want)
		}
	}
}

func TestStackUpdateFromTemplateSkipRejectsManagedDocument(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writeStackTemplate(t, project, oneServiceTemplate)
	p, err := New(project)
	if err != nil {
		t.Fatalf("New(project): %v", err)
	}
	initialized, err := p.StackInit(ctx, "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	stackPlatform, err := New(initialized.Root)
	if err != nil {
		t.Fatalf("New(stack root): %v", err)
	}
	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Skip: []string{"angee.yaml"}}); err == nil || !strings.Contains(err.Error(), "managed reconciliation path") {
		t.Fatalf("StackUpdateFromTemplate skip managed document error = %v", err)
	}
}

// pathInputStackCopier is a stack template that renders under ANGEE_ROOT=.angee
// with a `type: path` input, so its recorded answer is relative to the .angee
// root rather than to the project directory copier renders into.
const pathInputStackCopier = `_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: stack
  name: dev
ANGEE_ROOT:
  type: str
  default: .angee
PROJECT_PATH:
  type: path
  default: "."
`

// writePathInputStackTemplate writes a stack template with the given copier.yml
// (pathInputStackCopier or a variant), rendering each named path input into
// <ANGEE_ROOT>/<name>.txt.
func writePathInputStackTemplate(t *testing.T, project, copierYML string, pathInputs ...string) {
	t.Helper()
	dir := writeStackTemplate(t, project, oneServiceTemplate)
	if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte(copierYML), 0o644); err != nil {
		t.Fatalf("write copier.yml: %v", err)
	}
	for _, name := range pathInputs {
		file := filepath.Join(dir, "template", "{{ ANGEE_ROOT }}", name+".txt.jinja")
		if err := os.WriteFile(file, []byte("{{ "+name+" }}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
}

// assertStackPathInput checks a path input both where copier recorded it, the
// answers file in the project directory, and where the stack rendered it.
func assertStackPathInput(t *testing.T, project, stackRoot, name, want string) {
	t.Helper()
	_, answers, err := readTemplateAnswers(filepath.Join(project, ".copier-answers.yml"))
	if err != nil {
		t.Fatalf("read answers: %v", err)
	}
	if got := answers[name]; got != want {
		t.Fatalf("recorded %s = %q, want %q", name, got, want)
	}
	rendered, err := os.ReadFile(filepath.Join(stackRoot, name+".txt"))
	if err != nil {
		t.Fatalf("read rendered %s: %v", name, err)
	}
	if got := strings.TrimSuffix(string(rendered), "\n"); got != want {
		t.Fatalf("rendered %s = %q, want %q", name, got, want)
	}
}

func initPathInputStack(t *testing.T, ctx context.Context, project string) *Platform {
	t.Helper()
	return initStackWith(t, ctx, project, map[string]string{"ANGEE_ROOT": ".angee"})
}

// initStackWith runs stack init in project with the given inputs and returns
// the platform of the stack root it created.
func initStackWith(t *testing.T, ctx context.Context, project string, inputs map[string]string) *Platform {
	t.Helper()
	p, err := New(project)
	if err != nil {
		t.Fatalf("New(project): %v", err)
	}
	initialized, err := p.StackInit(ctx, "dev", "", inputs, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	stackPlatform, err := New(initialized.Root)
	if err != nil {
		t.Fatalf("New(stack root): %v", err)
	}
	return stackPlatform
}

// The answers file records a path input as the last render resolved it,
// relative to ANGEE_ROOT. An update replays it as it is: resolving it again
// as fresh input moved it one level further up on every update (#121).
func TestStackUpdateFromTemplateKeepsRecordedPathInputs(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writePathInputStackTemplate(t, project, pathInputStackCopier, "PROJECT_PATH")
	stack := initPathInputStack(t, ctx, project)
	assertStackPathInput(t, project, stack.root, "PROJECT_PATH", "..")

	for run := 1; run <= 2; run++ {
		if _, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err != nil {
			t.Fatalf("StackUpdateFromTemplate run %d: %v", run, err)
		}
		assertStackPathInput(t, project, stack.root, "PROJECT_PATH", "..")
	}
}

// A path input passed on the update is read like one passed to stack init,
// relative to the directory copier renders into (here the project directory,
// the parent of ANGEE_ROOT), and resolved once. The next update replays the
// recorded value.
func TestStackUpdateFromTemplateResolvesGivenPathInputOnce(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writePathInputStackTemplate(t, project, pathInputStackCopier, "PROJECT_PATH")
	stack := initPathInputStack(t, ctx, project)

	if _, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{"PROJECT_PATH": "app"}}); err != nil {
		t.Fatalf("StackUpdateFromTemplate with input: %v", err)
	}
	assertStackPathInput(t, project, stack.root, "PROJECT_PATH", "../app")
	if _, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err != nil {
		t.Fatalf("StackUpdateFromTemplate replay: %v", err)
	}
	assertStackPathInput(t, project, stack.root, "PROJECT_PATH", "../app")
}

// A path question added to the template after the stack was created has no
// recorded answer, so its default is resolved as stack init would have
// resolved it; from then on its recorded answer is replayed.
func TestStackUpdateFromTemplateResolvesDefaultOfNewPathQuestion(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writePathInputStackTemplate(t, project, pathInputStackCopier, "PROJECT_PATH")
	stack := initPathInputStack(t, ctx, project)

	writePathInputStackTemplate(t, project, pathInputStackCopier+"DATA_PATH:\n  type: path\n  default: data\n", "PROJECT_PATH", "DATA_PATH")
	for run := 1; run <= 2; run++ {
		if _, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err != nil {
			t.Fatalf("StackUpdateFromTemplate run %d: %v", run, err)
		}
		assertStackPathInput(t, project, stack.root, "PROJECT_PATH", "..")
		assertStackPathInput(t, project, stack.root, "DATA_PATH", "../data")
	}
}

// A value passed on an update that equals the recorded answer is that answer
// unchanged, as the -i form and the immutability check take it, so it is
// replayed rather than resolved as new input. For an immutable path input,
// passing the recorded value is the one override the check accepts.
func TestStackUpdateFromTemplateReplaysAnInputEqualToTheRecordedAnswer(t *testing.T) {
	immutable := strings.Replace(pathInputStackCopier, "  name: dev\n", "  name: dev\n  inputs:\n    PROJECT_PATH:\n      immutable: true\n", 1)
	for _, tc := range []struct {
		name      string
		copierYML string
		immutable bool
	}{
		{name: "plain", copierYML: pathInputStackCopier},
		{name: "immutable", copierYML: immutable, immutable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			project := t.TempDir()
			writePathInputStackTemplate(t, project, tc.copierYML, "PROJECT_PATH")
			stack := initPathInputStack(t, ctx, project)
			assertStackPathInput(t, project, stack.root, "PROJECT_PATH", "..")

			for run := 1; run <= 2; run++ {
				if _, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{"PROJECT_PATH": ".."}}); err != nil {
					t.Fatalf("StackUpdateFromTemplate run %d with the recorded value: %v", run, err)
				}
				assertStackPathInput(t, project, stack.root, "PROJECT_PATH", "..")
			}
			if !tc.immutable {
				return
			}
			_, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{"PROJECT_PATH": "app"}})
			var invalid *InvalidInputError
			if !errors.As(err, &invalid) || invalid.Field != "PROJECT_PATH" {
				t.Fatalf("StackUpdateFromTemplate with another value: error = %v, want InvalidInputError on PROJECT_PATH", err)
			}
			assertStackPathInput(t, project, stack.root, "PROJECT_PATH", "..")
		})
	}
}

// copier writes the answers file in the directory ANGEE_ROOT is relative to,
// which for deploy/.angee is two levels above the stack root. An update finds
// it there, however ANGEE_ROOT is spelled, and replays the recorded path input
// as it is (#128). The `.` and `.angee` layouts find it where they always did.
func TestStackUpdateFromTemplateFindsAnswersAboveTheStackRoot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		angeeRoot string
		stackRoot string
		recorded  string
	}{
		{name: "dot", angeeRoot: ".", stackRoot: ".", recorded: "."},
		{name: "dot-angee", angeeRoot: ".angee", stackRoot: ".angee", recorded: ".."},
		{name: "nested", angeeRoot: "deploy/.angee", stackRoot: "deploy/.angee", recorded: "../.."},
		{name: "nested-dot-prefix", angeeRoot: "./deploy/.angee", stackRoot: "deploy/.angee", recorded: "../.."},
		{name: "nested-trailing-slash", angeeRoot: "deploy/.angee/", stackRoot: "deploy/.angee", recorded: "../.."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			writePathInputStackTemplate(t, base, pathInputStackCopier, "PROJECT_PATH")
			project := filepath.Join(base, "project")
			stack := initStackWith(t, ctx, project, map[string]string{"ANGEE_ROOT": tc.angeeRoot})
			if want := filepath.Join(project, filepath.FromSlash(tc.stackRoot)); stack.root != want {
				t.Fatalf("stack root = %q, want %q", stack.root, want)
			}
			assertStackUpdatesKeepPathInput(t, ctx, stack, project, tc.recorded)
		})
	}
}

// assertStackUpdatesKeepPathInput checks PROJECT_PATH after init, then as
// StackTemplateInputs reports it (the -i form's lookup), then after each of two
// template updates.
func assertStackUpdatesKeepPathInput(t *testing.T, ctx context.Context, stack *Platform, project, want string) {
	t.Helper()
	assertStackPathInput(t, project, stack.root, "PROJECT_PATH", want)
	described, err := stack.StackTemplateInputs(ctx)
	if err != nil {
		t.Fatalf("StackTemplateInputs: %v", err)
	}
	if got := described.Recorded["PROJECT_PATH"]; got != want {
		t.Fatalf("StackTemplateInputs recorded PROJECT_PATH = %q, want %q", got, want)
	}
	for run := 1; run <= 2; run++ {
		if _, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err != nil {
			t.Fatalf("StackUpdateFromTemplate run %d: %v", run, err)
		}
		assertStackPathInput(t, project, stack.root, "PROJECT_PATH", want)
	}
}

// writeTopLevelStackTemplate writes a stack template that renders angee.yaml
// and PROJECT_PATH.txt at the top of its render target, where angee-django's
// stack templates render angee.yaml.
func writeTopLevelStackTemplate(t *testing.T, base, copierYML string) {
	t.Helper()
	dir := filepath.Join(base, ".templates", "stacks", "dev")
	for name, body := range map[string]string{
		"copier.yml":                      copierYML,
		"template/angee.yaml.jinja":       oneServiceTemplate,
		"template/PROJECT_PATH.txt.jinja": "{{ PROJECT_PATH }}\n",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A template that doesn't make ANGEE_ROOT a copier question records no
// ANGEE_ROOT, since copier records question answers only: angee-django's
// stacks/dev and stacks/local don't declare it at all. Here it sits under
// _angee.inputs with the default `.`. The stack sits in the render target, so
// an answers file there with no ANGEE_ROOT is the stack's.
func TestStackUpdateFromTemplateFindsAnswersThatRecordNoAngeeRoot(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	writeTopLevelStackTemplate(t, base, `_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: stack
  name: dev
  inputs:
    ANGEE_ROOT:
      type: string
      default: "."
PROJECT_PATH:
  type: path
  default: "."
`)
	project := filepath.Join(base, "project")
	stack := initStackWith(t, ctx, project, nil)
	if stack.root != project {
		t.Fatalf("stack root = %q, want %q", stack.root, project)
	}
	_, answers, err := readTemplateAnswers(filepath.Join(project, ".copier-answers.yml"))
	if err != nil {
		t.Fatalf("read answers: %v", err)
	}
	if angeeRoot, recorded := answers["ANGEE_ROOT"]; recorded {
		t.Fatalf("answers record ANGEE_ROOT = %q, want none", angeeRoot)
	}
	assertStackUpdatesKeepPathInput(t, ctx, stack, project, ".")
}

// An absolute ANGEE_ROOT names one stack root. A template that renders
// angee.yaml at its top, initialized with its target as an absolute
// ANGEE_ROOT, records that path, and its updates find the answers by it.
func TestStackUpdateFromTemplateFindsAnswersByAnAbsoluteAngeeRoot(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	writeTopLevelStackTemplate(t, base, pathInputStackCopier)
	project := filepath.Join(base, "project")
	stack := initStackWith(t, ctx, project, map[string]string{"ANGEE_ROOT": project})
	if stack.root != project {
		t.Fatalf("stack root = %q, want %q", stack.root, project)
	}
	_, answers, err := readTemplateAnswers(filepath.Join(project, ".copier-answers.yml"))
	if err != nil {
		t.Fatalf("read answers: %v", err)
	}
	if got := answers["ANGEE_ROOT"]; got != project {
		t.Fatalf("answers record ANGEE_ROOT = %q, want %q", got, project)
	}
	assertStackUpdatesKeepPathInput(t, ctx, stack, project, ".")
}

func writeAnswers(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The walk reads answers files nearest first and stops at the stack's own, so
// a file above it is never read, even one that is not YAML. A file below it
// that is not YAML stops the update instead: it may be the stack's own answers,
// damaged, and the walk can't tell.
func TestStackUpdateFromTemplateReadsAnswersNearestFirst(t *testing.T) {
	for _, tc := range []struct{ name, angeeRoot string }{
		{name: "above-dot", angeeRoot: "."},
		{name: "above-dot-angee", angeeRoot: ".angee"},
		{name: "above-nested", angeeRoot: "deploy/.angee"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			writePathInputStackTemplate(t, base, pathInputStackCopier, "PROJECT_PATH")
			project := filepath.Join(base, "project")
			stack := initStackWith(t, ctx, project, map[string]string{"ANGEE_ROOT": tc.angeeRoot})
			writeAnswers(t, base, "{{{\n")
			for run := 1; run <= 2; run++ {
				if _, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err != nil {
					t.Fatalf("StackUpdateFromTemplate run %d: %v", run, err)
				}
			}
		})
	}
	t.Run("below-nested", func(t *testing.T) {
		ctx := context.Background()
		base := t.TempDir()
		writePathInputStackTemplate(t, base, pathInputStackCopier, "PROJECT_PATH")
		project := filepath.Join(base, "project")
		stack := initStackWith(t, ctx, project, map[string]string{"ANGEE_ROOT": "deploy/.angee"})
		writeAnswers(t, filepath.Join(project, "deploy"), "{{{\n")
		_, err := stack.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{})
		var invalid *InvalidInputError
		bad := filepath.Join(project, "deploy", ".copier-answers.yml")
		if !errors.As(err, &invalid) || invalid.Field != "template" || !strings.Contains(invalid.Reason, bad) {
			t.Fatalf("StackUpdateFromTemplate error = %v, want an InvalidInputError on template naming %s", err, bad)
		}
	})
}

// An answers file nearer than the stack's own is skipped when it does not
// name the stack root, whatever its other answers hold: only its ANGEE_ROOT is
// read, so a mapping-valued answer, which no stack answer may be, doesn't stop
// the update.
func TestStackUpdateFromTemplateSkipsAnswersThatNameAnotherRoot(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	writePathInputStackTemplate(t, base, pathInputStackCopier, "PROJECT_PATH")
	project := filepath.Join(base, "project")
	stack := initStackWith(t, ctx, project, map[string]string{"ANGEE_ROOT": "deploy/.angee"})
	for dir, angeeRoot := range map[string]string{
		// Another stack's root, absolute.
		stack.root: filepath.Join(base, "other", ".angee"),
		// This stack's root, but only by climbing out of project/deploy, which
		// stack init refuses.
		filepath.Join(project, "deploy"): "../deploy/.angee",
	} {
		writeAnswers(t, dir, fmt.Sprintf("_src_path: stacks/other\nANGEE_ROOT: %q\nPROJECT_PATH: elsewhere\ncfg:\n  nested: true\n", angeeRoot))
	}
	assertStackUpdatesKeepPathInput(t, ctx, stack, project, "../..")
}

// locateStackAnswers walks from the stack root up to top, nearest first. Each
// case bounds the walk at its temporary directory, so answers files above it
// on the machine running the tests can't change the outcome.
func TestLocateStackAnswersWalk(t *testing.T) {
	const ours = "_src_path: stacks/dev\nANGEE_ROOT: deploy/.angee\nPROJECT_PATH: ours\n"
	const directory = "<directory>" // a directory where the answers file would be
	for _, tc := range []struct {
		name  string
		files map[string]string // answers file content by directory, relative to the top
		// target is the directory relative to the top whose answers are found,
		// "" for none; failing names the directory whose file the walk stops at.
		target, failing string
	}{
		{name: "own", files: map[string]string{"project": ours}, target: "project"},
		{name: "none", files: map[string]string{}},
		{name: "only-unrelated", files: map[string]string{
			"project/deploy/.angee": "ANGEE_ROOT: /elsewhere/.angee\n",
			"project/deploy":        "ANGEE_ROOT: ../deploy/.angee\n",
			"project":               "ANGEE_ROOT: .angee\n",
			".":                     "ANGEE_ROOT: .\n",
		}},
		{name: "nearest-match", files: map[string]string{"project/deploy": "ANGEE_ROOT: .angee\n", "project": ours}, target: "project/deploy"},
		{name: "unrelated-answers-not-judged", files: map[string]string{"project/deploy": "ANGEE_ROOT: elsewhere\ncfg:\n  nested: true\n", "project": ours}, target: "project"},
		{name: "no-angee-root-above-the-stack-root", files: map[string]string{"project/deploy": "PROJECT_PATH: x\n", "project": ours}, target: "project"},
		{name: "no-angee-root-in-the-stack-root", files: map[string]string{"project/deploy/.angee": "PROJECT_PATH: x\n"}, target: "project/deploy/.angee"},
		{name: "list-angee-root-in-the-stack-root", files: map[string]string{"project/deploy/.angee": "ANGEE_ROOT: [a]\n", "project": ours}, target: "project"},
		{name: "not-yaml-above-the-match", files: map[string]string{"project": ours, ".": "{{{\n"}, target: "project"},
		{name: "not-yaml-below-the-match", files: map[string]string{"project/deploy": "{{{\n", "project": ours}, failing: "project/deploy"},
		{name: "not-a-mapping-below-the-match", files: map[string]string{"project/deploy": "- a\n", "project": ours}, failing: "project/deploy"},
		{name: "directory-below-the-match", files: map[string]string{"project/deploy": directory, "project": ours}, failing: "project/deploy"},
		{name: "own-with-an-unsupported-answer", files: map[string]string{"project": ours + "cfg:\n  nested: true\n"}, failing: "project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			top := t.TempDir()
			root := filepath.Join(top, "project", "deploy", ".angee")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			for dir, body := range tc.files {
				dir = filepath.Join(top, filepath.FromSlash(dir))
				if body == directory {
					if err := os.MkdirAll(filepath.Join(dir, ".copier-answers.yml"), 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				writeAnswers(t, dir, body)
			}
			target, _, _, ok, err := locateStackAnswers(root, top, ".copier-answers.yml")
			if tc.failing != "" {
				path := filepath.Join(top, filepath.FromSlash(tc.failing), ".copier-answers.yml")
				var invalid *InvalidInputError
				if !errors.As(err, &invalid) || invalid.Field != "template" || strings.Count(invalid.Reason, path) != 1 {
					t.Fatalf("locateStackAnswers error = %v, want an InvalidInputError on template naming %s once", err, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("locateStackAnswers: %v", err)
			}
			want := ""
			if tc.target != "" {
				want = filepath.Join(top, filepath.FromSlash(tc.target))
			}
			if target != want || ok != (want != "") {
				t.Fatalf("locateStackAnswers = %q (found %v), want %q", target, ok, want)
			}
		})
	}
}

func TestRendersStackRoot(t *testing.T) {
	work := filepath.FromSlash("/work")
	project := filepath.Join(work, "project")
	deploy := filepath.Join(project, "deploy")
	root := filepath.Join(deploy, ".angee")
	for _, tc := range []struct {
		name, dir, angeeRoot string
		root                 string // the stack root, when not root
		want                 bool
	}{
		{name: "empty-in-the-stack-root", dir: root, angeeRoot: "", want: true},
		{name: "empty-above-the-stack-root", dir: project, angeeRoot: ""},
		{name: "dot", dir: root, angeeRoot: ".", want: true},
		{name: "dot-slash", dir: root, angeeRoot: "./", want: true},
		{name: "dot-above-the-stack-root", dir: deploy, angeeRoot: "."},
		{name: "one-segment", dir: deploy, angeeRoot: ".angee", want: true},
		{name: "nested", dir: project, angeeRoot: "deploy/.angee", want: true},
		{name: "nested-dot-prefix", dir: project, angeeRoot: "./deploy/.angee", want: true},
		{name: "nested-trailing-slash", dir: project, angeeRoot: "deploy/.angee/", want: true},
		{name: "nested-one-level-too-few", dir: deploy, angeeRoot: "deploy/.angee"},
		{name: "nested-one-level-too-many", dir: work, angeeRoot: "deploy/.angee"},
		{name: "dot-dot-inside", dir: project, angeeRoot: "deploy/../ops/.angee", root: filepath.Join(project, "ops", ".angee"), want: true},
		{name: "dot-dot-inside-another-root", dir: project, angeeRoot: "deploy/../ops/.angee"},
		{name: "climbs-out", dir: filepath.Join(deploy, "x"), angeeRoot: "../.angee"},
		{name: "climbs-out-after-a-segment", dir: deploy, angeeRoot: "a/../../deploy/.angee"},
		{name: "absolute-in-the-stack-root", dir: root, angeeRoot: root, want: true},
		{name: "absolute-above-the-stack-root", dir: project, angeeRoot: root, want: true},
		{name: "absolute-trailing-slash", dir: root, angeeRoot: root + string(filepath.Separator), want: true},
		{name: "absolute-another-root", dir: root, angeeRoot: filepath.Join(work, "other", ".angee")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stackRoot := root
			if tc.root != "" {
				stackRoot = tc.root
			}
			if got := rendersStackRoot(tc.dir, tc.angeeRoot, stackRoot); got != tc.want {
				t.Fatalf("rendersStackRoot(%q, %q, %q) = %v, want %v", tc.dir, tc.angeeRoot, stackRoot, got, tc.want)
			}
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// A render that names a vcs sets the stack's, which decides only for slots
// created afterwards: an existing slot that names no mode keeps the vcs it
// records, and one recorded before create recorded it is given the stack's
// old vcs, so neither is read as another kind of checkout. A slot that names
// a mode is left alone, and a render that names no vcs keeps ours.
func TestMergeStackFromTemplateTakesTheTemplatesVCS(t *testing.T) {
	ours := &manifest.Stack{Version: 1, Kind: "stack", Name: "demo", Workspaces: map[string]manifest.Workspace{
		"ws": {Template: "t", Sources: map[string]manifest.WorkspaceSource{
			"old":      {Source: "app"},
			"recorded": {Source: "app", VCS: manifest.VCSJJ},
			"tree":     {Source: "app", Mode: manifest.WorkspaceSourceModeWorktree},
		}},
	}}
	merged := mergeStackFromTemplate(ours, &manifest.Stack{Version: 1, Kind: "stack", Name: "demo", VCS: manifest.VCSJJ})
	if merged.VCS != manifest.VCSJJ {
		t.Fatalf("merged vcs = %q, want the template's jj", merged.VCS)
	}
	want := map[string]manifest.WorkspaceSource{
		"old":      {Source: "app", VCS: manifest.VCSGit},
		"recorded": {Source: "app", VCS: manifest.VCSJJ},
		"tree":     {Source: "app", Mode: manifest.WorkspaceSourceModeWorktree},
	}
	if got := merged.Workspaces["ws"].Sources; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged slots = %+v, want %+v", got, want)
	}
	for slot, ws := range want {
		if got, before := merged.EffectiveSourceMode(merged.Workspaces["ws"].Sources[slot]), ours.EffectiveSourceMode(ours.Workspaces["ws"].Sources[slot]); got != before {
			t.Errorf("slot %s (%+v) reads as %q after the merge, was %q", slot, ws, got, before)
		}
	}
	if ours.Workspaces["ws"].Sources["old"].VCS != "" {
		t.Fatal("merge mutated ours.Workspaces")
	}
	if changes := summarizeStackChanges(ours, merged); !reflect.DeepEqual(changes, []string{"~ vcs"}) {
		t.Fatalf("changes = %v, want the vcs change alone", changes)
	}
	if kept := mergeStackFromTemplate(merged, &manifest.Stack{Version: 1, Kind: "stack", Name: "demo"}); kept.VCS != manifest.VCSJJ || !reflect.DeepEqual(kept.Workspaces, merged.Workspaces) {
		t.Fatalf("a render with no vcs = %q, %+v, want ours kept", kept.VCS, kept.Workspaces)
	}
}
