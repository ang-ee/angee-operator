package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
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
	}

	merged := mergeStackFromTemplate(ours, theirs, false)

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
	if merged.Ingress.Type != "caddy" || merged.Ingress.Domain != "agents.localhost" {
		t.Fatalf("ingress not refreshed: %+v", merged.Ingress)
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

// TestStackUpdateFromTemplateReconcilesWorkspacePorts covers the workspace
// inner-stack case: `stack update --template` must re-derive the workspace's
// allocated ports from the parent stack's authoritative record, not from the
// inner stack's frozen answers file — which copier can reset to template
// defaults, silently dropping the allocation.
func TestStackUpdateFromTemplateReconcilesWorkspacePorts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeWorkspacePortStackTemplate(t, root)
	writeWorkspacePortWorkspaceTemplate(t, root)

	host, err := New(root)
	if err != nil {
		t.Fatalf("New(root): %v", err)
	}
	host.portUnavailable = func(int) bool { return false } // deterministic allocation

	ref, err := host.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: "workspaces/dev", Name: "ws1"})
	if err != nil {
		t.Fatalf("WorkspaceCreate: %v", err)
	}
	alloc := ref.Allocations["web"]
	if alloc < 8000 || alloc > 8099 {
		t.Fatalf("allocated web port %d outside pool range 8000-8099", alloc)
	}

	innerRoot := filepath.Join(ref.Path, ".angee")
	sp, err := New(innerRoot)
	if err != nil {
		t.Fatalf("New(innerRoot): %v", err)
	}

	// The freshly created inner stack already carries the allocation, the
	// chain-injected non-port input (extra=topic), and the path input resolved
	// once relative to ANGEE_ROOT.
	inner, err := sp.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack: %v", err)
	}
	if got := inner.Ports["web"].Value; got != alloc {
		t.Fatalf("inner web port = %d, want allocated %d", got, alloc)
	}
	projectPath := inner.Services["web"].Env["PROJECT_PATH"]
	if !strings.Contains(projectPath, "..") {
		// The path branch is only meaningful if the resolved path has an
		// ANGEE_ROOT escape that a stray re-resolution would double.
		t.Fatalf("inner PROJECT_PATH env = %q, want a path with a `..` escape", projectPath)
	}

	// Simulate the inner stack drifting back to template defaults: the answers
	// file and the manifest both lose the allocation (the reported bug), and the
	// non-port input is changed too. The path input is left as-is so the test
	// can detect re-resolution (path doubling) across the update.
	answersPath := filepath.Join(ref.Path, ".copier-answers.yml")
	driftFile(t, answersPath, fmt.Sprintf("web_port: %d", alloc), "web_port: 9999")
	driftFile(t, answersPath, "extra: dev", "extra: drifted-extra")
	inner.Ports["web"] = manifest.Port{Value: 9999, ExportEnv: "WEB_PORT"}
	if svc, ok := inner.Services["web"]; ok {
		svc.Env = map[string]string{"WEB_PORT": "9999", "EXTRA": "drifted-extra", "PROJECT_PATH": projectPath}
		inner.Services["web"] = svc
	}
	if err := manifest.SaveFile(manifest.Path(innerRoot), inner); err != nil {
		t.Fatalf("SaveFile(drift): %v", err)
	}

	// A template re-render must restore the workspace's allocated port from the
	// parent record, not honour the drifted default.
	if _, err := sp.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err != nil {
		t.Fatalf("StackUpdateFromTemplate: %v", err)
	}
	got, err := sp.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack after update: %v", err)
	}
	if got.Ports["web"].Value != alloc {
		t.Fatalf("web port after update = %d, want reconciled %d", got.Ports["web"].Value, alloc)
	}
	if got.Ports["web"].ExportEnv != "WEB_PORT" {
		t.Fatalf("web port export_env = %q, want WEB_PORT (port struct refreshed from template)", got.Ports["web"].ExportEnv)
	}
	if env := got.Services["web"].Env["WEB_PORT"]; env != strconv.Itoa(alloc) {
		t.Fatalf("service WEB_PORT env = %q, want reconciled %d", env, alloc)
	}
	// Non-port chain inputs are NOT authoritative from the parent record: the
	// drifted answers value wins, not the record's topic ("dev").
	if env := got.Services["web"].Env["EXTRA"]; env != "drifted-extra" {
		t.Fatalf("service EXTRA env = %q, want drifted-extra (answers-sourced, not record)", env)
	}
	// Path inputs are reused verbatim from the answers file, not re-resolved
	// against the scratch dir (which would double the ANGEE_ROOT escape).
	if env := got.Services["web"].Env["PROJECT_PATH"]; env != projectPath {
		t.Fatalf("service PROJECT_PATH env = %q, want unchanged %q (no path re-resolution)", env, projectPath)
	}
}

func writeWorkspacePortStackTemplate(t *testing.T, root string) {
	t.Helper()
	tmpl := filepath.Join(root, ".templates", "stacks", "dev")
	inner := filepath.Join(tmpl, "template", "{{ ANGEE_ROOT }}")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir stack template: %v", err)
	}
	copierYML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: stack
  name: dev
ANGEE_ROOT:
  type: str
  default: .angee
web_port:
  type: int
  default: 9999
extra:
  type: str
  default: default-extra
project_path:
  type: path
  default: "."
`
	if err := os.WriteFile(filepath.Join(tmpl, "copier.yml"), []byte(copierYML), 0o644); err != nil {
		t.Fatalf("write stack copier.yml: %v", err)
	}
	manifestBody := `version: 1
kind: stack
name: demo
template:
  active: stacks/dev
  answers_file: .copier-answers.yml
ports:
  web: { value: {{ web_port }}, export_env: WEB_PORT }
services:
  web:
    runtime: container
    image: demo:latest
    env:
      WEB_PORT: "{{ web_port }}"
      EXTRA: "{{ extra }}"
      PROJECT_PATH: "{{ project_path }}"
`
	if err := os.WriteFile(filepath.Join(inner, "angee.yaml.jinja"), []byte(manifestBody), 0o644); err != nil {
		t.Fatalf("write stack angee.yaml.jinja: %v", err)
	}
}

func writeWorkspacePortWorkspaceTemplate(t *testing.T, root string) {
	t.Helper()
	tmpl := filepath.Join(root, ".templates", "workspaces", "dev")
	if err := os.MkdirAll(filepath.Join(tmpl, "template"), 0o755); err != nil {
		t.Fatalf("mkdir workspace template: %v", err)
	}
	copierYML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: dev
  inputs:
    topic:
      type: str
      default: dev
  ensure:
    operator.port_pool.web: { range: "8000-8099" }
  chain_root: .angee
  chain:
    - template: stacks/dev
      root: .
      inputs:
        web_port: "${alloc.web}"
        extra: "${inputs.topic}"
topic:
  type: str
  default: dev
`
	if err := os.WriteFile(filepath.Join(tmpl, "copier.yml"), []byte(copierYML), 0o644); err != nil {
		t.Fatalf("write workspace copier.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "template", "README.md.jinja"), []byte("workspace {{ topic }}\n"), 0o644); err != nil {
		t.Fatalf("write workspace README.md.jinja: %v", err)
	}
}

func driftFile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := strings.Replace(string(data), old, new, 1)
	if out == string(data) {
		t.Fatalf("drift target %q not found in %s", old, path)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
