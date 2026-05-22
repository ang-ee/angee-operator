package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/manifest"
)

// setupServiceCreateFixture builds a stack with one workspace and the
// `acp` port pool declared so the canonical service template can
// allocate against it.
func setupServiceCreateFixture(t *testing.T) (*Platform, string) {
	t.Helper()
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Operator: manifest.Operator{
			PortPool: map[string]manifest.PortPool{
				"acp": {Range: "3000-3999"},
			},
		},
		Workspaces: map[string]manifest.Workspace{
			"my-pa": {Template: "workspaces/dev-pr"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "workspaces", "my-pa"), 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace dir): %v", err)
	}
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.portUnavailable = func(int) bool { return false }
	return p, fixtureTemplatePath(t, "claude-code")
}

func fixtureTemplatePath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "service-templates", name)
}

func TestServiceCreateHappyPath(t *testing.T) {
	p, templatePath := setupServiceCreateFixture(t)
	state, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{
		Template:  templatePath,
		Workspace: "my-pa",
		Inputs:    map[string]string{"auth_mode": "api_key"},
	})
	if err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	if state.Name != "agent-my-pa" {
		t.Fatalf("resolved name = %q, want agent-my-pa", state.Name)
	}
	if state.Runtime != "container" {
		t.Fatalf("runtime = %q, want container", state.Runtime)
	}

	stack, err := manifest.LoadFile(manifest.Path(p.root))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	svc, ok := stack.Services["agent-my-pa"]
	if !ok {
		t.Fatalf("service entry missing from manifest: %#v", stack.Services)
	}
	if len(svc.Ports) != 1 || !strings.Contains(string(svc.Ports[0]), ":3007") {
		t.Fatalf("rendered ports = %v, want one mapping to 3007", svc.Ports)
	}
	// Lease persisted with the service-prefixed owner.
	owner := servicePortOwner("agent-my-pa", "acp")
	found := false
	for _, lease := range stack.PortLeases["acp"] {
		if lease.Owner == owner {
			found = true
		}
	}
	if !found {
		t.Fatalf("port lease for %q not persisted: %+v", owner, stack.PortLeases)
	}
	// Build context dir landed at .angee/services/agent-my-pa/.
	if _, err := os.Stat(filepath.Join(p.root, ".angee", "services", "agent-my-pa", "docker", "Dockerfile")); err != nil {
		t.Fatalf("build context not installed: %v", err)
	}
}

func TestServiceCreateSkipsUnavailableHostPorts(t *testing.T) {
	p, templatePath := setupServiceCreateFixture(t)
	p.portUnavailable = func(port int) bool { return port == 3000 }
	state, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{
		Template:  templatePath,
		Workspace: "my-pa",
		Inputs:    map[string]string{"auth_mode": "api_key"},
	})
	if err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	if state.Name != "agent-my-pa" {
		t.Fatalf("resolved name = %q, want agent-my-pa", state.Name)
	}

	stack, err := manifest.LoadFile(manifest.Path(p.root))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	svc := stack.Services["agent-my-pa"]
	if len(svc.Ports) != 1 || !strings.HasPrefix(string(svc.Ports[0]), "3001:") {
		t.Fatalf("rendered ports = %v, want host port 3001", svc.Ports)
	}
}

func TestServiceCreateRejectsMissingWorkspace(t *testing.T) {
	p, templatePath := setupServiceCreateFixture(t)
	_, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{
		Template:  templatePath,
		Workspace: "nope",
	})
	var nf *NotFoundError
	if !errors.As(err, &nf) || nf.Kind != "workspace" {
		t.Fatalf("err = %v, want NotFoundError{Kind: workspace}", err)
	}
}

func TestServiceCreateRejectsDuplicateName(t *testing.T) {
	p, templatePath := setupServiceCreateFixture(t)
	if _, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{Template: templatePath, Workspace: "my-pa"}); err != nil {
		t.Fatalf("first ServiceCreate: %v", err)
	}
	_, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{Template: templatePath, Workspace: "my-pa"})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
}

func TestServiceCreateRejectsWrongTemplateKind(t *testing.T) {
	p, _ := setupServiceCreateFixture(t)
	// Build a one-off workspace template under the stack root and try
	// to use it as a service template.
	wsTemplate := filepath.Join(p.root, ".templates", "services", "wrong")
	if err := os.MkdirAll(filepath.Join(wsTemplate, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsTemplate, "copier.yml"), []byte(`_subdirectory: template
_angee:
  kind: workspace
  name: wrong
`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{Template: wsTemplate, Workspace: "my-pa"})
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("err = %v, want kind-mismatch", err)
	}
}

func TestServiceCreateRollsBackOnRenderFailure(t *testing.T) {
	p, _ := setupServiceCreateFixture(t)
	// Build a service template that renders an invalid YAML output;
	// the render itself succeeds but parsePartialServiceManifest fails.
	bad := filepath.Join(p.root, ".templates", "services", "broken")
	if err := os.MkdirAll(filepath.Join(bad, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bad, "copier.yml"), []byte(`_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: service
  name: broken
  ensure:
    operator.port_pool.acp:
      range: "3000-3999"
`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// service.yaml contains an unknown top-level key — partial parser rejects.
	if err := os.WriteFile(filepath.Join(bad, "template", "service.yaml.jinja"), []byte(`services:
  agent-{{ workspace_name }}:
    runtime: container
    image: nginx
jobs:
  forbidden:
    runtime: container
`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{Template: bad, Workspace: "my-pa"})
	if err == nil || !strings.Contains(err.Error(), "jobs") {
		t.Fatalf("err = %v, want rejection mentioning forbidden field", err)
	}

	// Port lease must have been released.
	stack, err := manifest.LoadFile(manifest.Path(p.root))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	for _, lease := range stack.PortLeases["acp"] {
		if strings.HasPrefix(lease.Owner, "service/agent-my-pa/") {
			t.Fatalf("port lease not released after rollback: %+v", lease)
		}
	}
	// And no service entry was persisted.
	if _, exists := stack.Services["agent-my-pa"]; exists {
		t.Fatalf("service was persisted despite rollback")
	}
}

func TestServiceDestroyReleasesLeaseAndBuildContext(t *testing.T) {
	p, templatePath := setupServiceCreateFixture(t)
	if _, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{Template: templatePath, Workspace: "my-pa"}); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	// Confirm build context exists pre-destroy.
	if _, err := os.Stat(filepath.Join(p.root, ".angee", "services", "agent-my-pa")); err != nil {
		t.Fatalf("expected build context pre-destroy: %v", err)
	}
	if err := p.ServiceDestroy(context.Background(), "agent-my-pa", false); err != nil {
		t.Fatalf("ServiceDestroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.root, ".angee", "services", "agent-my-pa")); !os.IsNotExist(err) {
		t.Fatalf("build context not removed after destroy: %v", err)
	}
	stack, _ := manifest.LoadFile(manifest.Path(p.root))
	for _, lease := range stack.PortLeases["acp"] {
		if strings.HasPrefix(lease.Owner, "service/agent-my-pa/") {
			t.Fatalf("port lease not released after destroy: %+v", lease)
		}
	}
}
