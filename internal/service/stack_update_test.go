package service

import (
	"context"
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
