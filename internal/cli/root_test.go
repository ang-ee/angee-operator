package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/manifest"
)

func TestVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := "angee version " + Version
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestInitDevReportsTemplateAndRoot(t *testing.T) {
	root := t.TempDir()
	writeStackTemplate(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRootWithIO(strings.NewReader("\n"), &stdout, &stderr)
	cmd.SetArgs([]string{"init", "--dev"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "ANGEE_ROOT [.angee]:") {
		t.Fatalf("prompt = %q, want ANGEE_ROOT default prompt", got)
	}
	want := "stack template dev initialized as .angee"
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("init output = %q, want %q", got, want)
	}
}

func TestInitDevRefusesNonEmptyRoot(t *testing.T) {
	root := t.TempDir()
	writeStackTemplate(t, root)
	writeExistingStackRoot(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"init", "--dev", "--yes"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error is nil")
	}
	want := "stack template dev already exists as .angee; use --force to overwrite or `angee stack update` to update"
	if got := err.Error(); got != want {
		t.Fatalf("init error = %q, want %q", got, want)
	}
}

func TestInitDevForceAllowsNonEmptyRoot(t *testing.T) {
	root := t.TempDir()
	writeStackTemplate(t, root)
	writeExistingStackRoot(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"init", "--dev", "--force", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := "stack template dev initialized as .angee"
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("init output = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".angee", "angee.yaml")); err != nil {
		t.Fatalf("Stat(angee.yaml) error = %v", err)
	}
}

func TestInitStackTemplateInitializesNamedRoot(t *testing.T) {
	root := t.TempDir()
	templateRoot := writeStackTemplate(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"init", "stack", "--template", templateRoot, "angee-notes", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "angee-notes", "angee.yaml")); err != nil {
		t.Fatalf("Stat(angee-notes/angee.yaml) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "angee-notes", ".angee", "angee.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unexpected nested .angee manifest err = %v", err)
	}
}

func TestOperatorCommandForwardsDaemonFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"operator", "--bind", "127.0.0.1", "--port", "19000", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Run the Angee operator") || !strings.Contains(output, "--bind") {
		t.Fatalf("operator help output did not come from daemon parser:\n%s", output)
	}
}

func TestOperatorHTTPErrorPreservesStatusAndFields(t *testing.T) {
	body, err := json.Marshal(api.ErrorResponse{
		Kind:  "workspace",
		Name:  "missing",
		Error: `workspace "missing" is not declared`,
	})
	if err != nil {
		t.Fatalf("Marshal(ErrorResponse) error = %v", err)
	}

	err = operatorHTTPError(http.StatusNotFound, body)
	var notFound *RemoteNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("operatorHTTPError() = %T, want RemoteNotFound", err)
	}
	if notFound.Status != http.StatusNotFound || notFound.Body.Kind != "workspace" || notFound.Body.Name != "missing" {
		t.Fatalf("RemoteNotFound = %#v", notFound)
	}
	if got := err.Error(); !strings.Contains(got, "HTTP 404") || !strings.Contains(got, `workspace "missing" is not declared`) {
		t.Fatalf("error string = %q, want status and message", got)
	}
}

func TestStatusUsesOperatorURLFlag(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodGet || r.URL.Path != "/stack/status" {
			t.Fatalf("request = %s %s, want GET /stack/status", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.StackStatusResponse{Name: "remote", Root: "/remote"})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--operator", server.URL, "--json", "status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !called {
		t.Fatal("operator endpoint was not called")
	}
	if got := stdout.String(); !strings.Contains(got, `"name": "remote"`) || !strings.Contains(got, `"root": "/remote"`) {
		t.Fatalf("status output = %s", got)
	}
}

func TestStatusUsesOperatorURLEnv(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/stack/status" {
			t.Fatalf("request = %s %s, want GET /stack/status", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.StackStatusResponse{Name: "env-remote", Root: "/env"})
	}))
	defer server.Close()
	t.Setenv("ANGEE_OPERATOR_URL", server.URL)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--json", "status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, `"name": "env-remote"`) {
		t.Fatalf("status output = %s", got)
	}
}

func TestStatusDiscoversParentAngeeRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	if err := os.MkdirAll(filepath.Join(base, "app", "nested"), 0o755); err != nil {
		t.Fatalf("MkdirAll(nested) error = %v", err)
	}
	stack := &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "parent-root"}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	t.Chdir(filepath.Join(base, "app", "nested"))

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "parent-root") || !strings.Contains(output, "root: "+root) {
		t.Fatalf("status output = %q, want discovered parent root %s", output, root)
	}
}

func TestWorkspaceCreateUsesDotAngeeForTemplatesDirectory(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceTemplate(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{
		"workspace",
		"create",
		"dev-pr",
		"--name",
		"feature-a",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".angee", "angee.yaml")); err != nil {
		t.Fatalf("Stat(.angee/angee.yaml) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "angee.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unexpected root angee.yaml err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".angee", "workspaces", "feature-a", "README.md")); err != nil {
		t.Fatalf("Stat(workspace README) error = %v", err)
	}
}

func TestWorkspaceStatusInfersCurrentWorkspace(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	nested := filepath.Join(root, "workspaces", workspaceName, "angee-go", "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace) error = %v", err)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "parent-root",
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {Template: "workspaces/dev-pr"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	t.Chdir(nested)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"workspace", "status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "workspace\t"+workspaceName+"\tready") {
		t.Fatalf("workspace status output = %q, want inferred workspace %q", got, workspaceName)
	}
}

func TestWorkspaceSyncBaseInfersCurrentWorkspace(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	nested := filepath.Join(root, "workspaces", workspaceName, "angee-go", "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace) error = %v", err)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "parent-root",
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {Template: "workspaces/dev-pr"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	t.Chdir(nested)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"workspace", "sync-base", "--merge"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "" {
		t.Fatalf("workspace sync-base output = %q, want empty output for workspace without git sources", got)
	}
}

func writeStackTemplate(t *testing.T, root string) string {
	t.Helper()
	templateRoot := filepath.Join(root, ".templates", "stacks", "dev")
	manifestDir := filepath.Join(templateRoot, "template", "{{ ANGEE_ROOT }}")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: stack
  name: dev
ANGEE_ROOT:
  default: .angee
`
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml) error = %v", err)
	}
	manifestYAML := `version: 1
kind: stack
name: test
`
	if err := os.WriteFile(filepath.Join(manifestDir, "angee.yaml.jinja"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml.jinja) error = %v", err)
	}
	return templateRoot
}

func writeWorkspaceTemplate(t *testing.T, root string) string {
	t.Helper()
	templateRoot := filepath.Join(root, "templates", "workspaces", "dev-pr")
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
`
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(workspace copier.yml) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "README.md.jinja"), []byte("workspace {{ workspace_name }}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(workspace README template) error = %v", err)
	}
	return templateRoot
}

func writeExistingStackRoot(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".angee"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.angee) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".angee", "existing"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile(existing) error = %v", err)
	}
}
