package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// setupServiceCreateFixture builds a stack with one workspace and the
// `acp` port pool declared so the canonical service template can
// allocate against it.
func setupServiceCreateFixture(t *testing.T) (*Platform, string) {
	t.Helper()
	return setupServiceCreateFixtureAt(t, t.TempDir())
}

// setupServiceCreateFixtureAt builds the fixture rooted at the given path,
// letting callers exercise the production control-root layout where the
// stack root itself is a `.angee` directory.
func setupServiceCreateFixtureAt(t *testing.T, root string) (*Platform, string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
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
	// Build context dir landed at <root>/services/agent-my-pa/.
	if _, err := os.Stat(filepath.Join(p.root, "services", "agent-my-pa", "docker", "Dockerfile")); err != nil {
		t.Fatalf("build context not installed: %v", err)
	}
}

// TestServiceCreateControlRootNoDoubling pins the fix for the `.angee/.angee`
// path doubling: when the stack root is itself a `.angee` control dir (the
// standard chain_root layout, and every workspace inner-stack), the build
// context must land at <root>/services/<name>, never <root>/.angee/services.
func TestServiceCreateControlRootNoDoubling(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".angee")
	p, templatePath := setupServiceCreateFixtureAt(t, root)
	if _, err := p.ServiceCreate(context.Background(), api.ServiceCreateRequest{
		Template:  templatePath,
		Workspace: "my-pa",
		Inputs:    map[string]string{"auth_mode": "api_key"},
	}); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	// Build context sits directly under the control root.
	if _, err := os.Stat(filepath.Join(p.root, "services", "agent-my-pa", "docker", "Dockerfile")); err != nil {
		t.Fatalf("build context not installed at <root>/services: %v", err)
	}
	// And crucially NOT one .angee too deep.
	if _, err := os.Stat(filepath.Join(p.root, ".angee", "services", "agent-my-pa")); !os.IsNotExist(err) {
		t.Fatalf("build context doubled into <root>/.angee/services (err=%v)", err)
	}
}

// TestValidateRenderedServiceBuildContext locks the containment property of the
// build.context validator directly: the rendered context must resolve inside
// <root>/services/<name>/ and nowhere else. A hostile template must not be able
// to escape via `../`, an absolute path, or a sibling-prefix dir.
func TestValidateRenderedServiceBuildContext(t *testing.T) {
	const name = "agent-x"
	cases := []struct {
		desc    string
		build   any
		wantErr bool
	}{
		{"nil build", nil, false},
		{"canonical docker subdir", "./services/agent-x/docker", false},
		{"bare service dir", "services/agent-x", false},
		{"leading-dot bare dir", "./services/agent-x", false},
		{"parent escape via subpath", "./services/agent-x/../../../etc", true},
		{"parent escape direct", "../../../etc", true},
		{"absolute path", "/etc/passwd", true},
		{"sibling-prefix bypass", "./services/agent-x-evil/docker", true},
		{"wrong service dir", "./services/other/docker", true},
		{"legacy doubled path rejected", "./.angee/services/agent-x/docker", true},
		{"map context ok", map[string]any{"context": "./services/agent-x/docker"}, false},
		{"map context absolute", map[string]any{"context": "/etc"}, true},
		{"map context non-string", map[string]any{"context": 123}, true},
		{"map no context key", map[string]any{"dockerfile": "Dockerfile"}, false},
		{"mapany context ok", map[any]any{"context": "./services/agent-x/docker"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			err := validateRenderedServiceBuildContext(manifest.Service{Build: tc.build}, name)
			if tc.wantErr && err == nil {
				t.Fatalf("build=%v: expected error, got nil", tc.build)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("build=%v: unexpected error: %v", tc.build, err)
			}
		})
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

func TestAllocateServicePortsReleasesRenderedRoutedCaddyService(t *testing.T) {
	p := &Platform{portUnavailable: func(int) bool { return false }}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Operator: manifest.Operator{
			PortPool: map[string]manifest.PortPool{
				"acp": {Range: "3000-3999"},
			},
		},
		Ingress: manifest.Ingress{Type: "caddy"},
		Services: map[string]manifest.Service{
			"agent": {
				Runtime: manifest.RuntimeContainer,
				Image:   "nginx",
				Route:   &manifest.Route{Port: 3008},
			},
			"db": {
				Runtime: manifest.RuntimeContainer,
				Image:   "postgres",
			},
		},
	}

	alloc, err := p.allocateServicePorts(stack, "agent")
	if err != nil {
		t.Fatalf("allocateServicePorts(agent): %v", err)
	}
	if len(alloc) == 0 {
		t.Fatalf("allocateServicePorts(agent) = %v, want non-empty map", alloc)
	}

	alloc, err = p.allocateServicePorts(stack, "db")
	if err != nil {
		t.Fatalf("allocateServicePorts(db): %v", err)
	}
	if len(alloc) == 0 {
		t.Fatalf("allocateServicePorts(db) = %v, want non-empty map", alloc)
	}

	if isRouted(stack, stack.Services["agent"]) {
		releaseServicePortLeases(stack, "agent")
	}

	agentOwner := servicePortOwner("agent", "acp")
	dbOwner := servicePortOwner("db", "acp")
	foundDB := false
	for _, lease := range stack.PortLeases["acp"] {
		if lease.Owner == dbOwner {
			foundDB = true
		}
		if lease.Owner == agentOwner {
			t.Fatalf("routed service lease was not released: %+v", lease)
		}
	}
	if !foundDB {
		t.Fatalf("port lease for %q not persisted: %+v", dbOwner, stack.PortLeases)
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
	if _, err := os.Stat(filepath.Join(p.root, "services", "agent-my-pa")); err != nil {
		t.Fatalf("expected build context pre-destroy: %v", err)
	}
	statePath := renderPlanStatePath(p.root, "services", "agent-my-pa")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected render state pre-destroy: %v", err)
	}
	if err := p.ServiceDestroy(context.Background(), "agent-my-pa", false); err != nil {
		t.Fatalf("ServiceDestroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.root, "services", "agent-my-pa")); !os.IsNotExist(err) {
		t.Fatalf("build context not removed after destroy: %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("render state not removed after destroy: %v", err)
	}
	stack, _ := manifest.LoadFile(manifest.Path(p.root))
	for _, lease := range stack.PortLeases["acp"] {
		if strings.HasPrefix(lease.Owner, "service/agent-my-pa/") {
			t.Fatalf("port lease not released after destroy: %+v", lease)
		}
	}
}

func TestServiceDestroyDoesNotUseLegacyServiceNameAsPath(t *testing.T) {
	root := t.TempDir()
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stack := p.EmptyStack("test")
	stack.Services = map[string]manifest.Service{
		"../outside": {Runtime: manifest.RuntimeContainer, Image: "example:latest"},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	marker := filepath.Join(root, "outside", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatalf("MkdirAll(marker): %v", err)
	}
	if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(marker): %v", err)
	}
	if err := p.ServiceDestroy(context.Background(), "../outside", false); err != nil {
		t.Fatalf("ServiceDestroy: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep\n" {
		t.Fatalf("legacy service destroy touched outside path: data=%q err=%v", data, err)
	}
}

func TestServiceDestroyRejectsSymlinkedServicesRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	outside := filepath.Join(base, "outside")
	for _, path := range []string{root, filepath.Join(outside, "agent")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stack := p.EmptyStack("test")
	stack.Services = map[string]manifest.Service{
		"agent": {Runtime: manifest.RuntimeContainer, Image: "example:latest"},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	marker := filepath.Join(outside, "agent", "keep.txt")
	if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(marker): %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "services")); err != nil {
		t.Fatalf("Symlink(services): %v", err)
	}
	if err := p.ServiceDestroy(context.Background(), "agent", false); err == nil {
		t.Fatal("ServiceDestroy succeeded through a symlinked services root")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep\n" {
		t.Fatalf("outside marker = %q, %v; want unchanged", data, err)
	}
	saved, err := manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if _, exists := saved.Services["agent"]; !exists {
		t.Fatal("service was removed from the manifest after destination validation failed")
	}
}

// TestServiceCreateDeclaresReferencedSecret is the end-to-end counterpart to
// the helper test below: it proves a referenced `${secret.NAME}` lands in the
// persisted manifest as a value-less declaration and that the post-create
// compose render resolves it from the backend. This is the exact scenario the
// commit fixes — a per-agent token set via secretSet (never declared in a
// template) that previously failed with `secret "…" is not resolved`.
func TestServiceCreateDeclaresReferencedSecret(t *testing.T) {
	p, _ := setupServiceCreateFixture(t)
	ctx := context.Background()

	// Seed the per-agent token in the backend only — no manifest declaration.
	if _, err := p.SecretSet(ctx, "agent-token", "s3cr3t"); err != nil {
		t.Fatalf("SecretSet: %v", err)
	}

	// A service template that references the secret in its env. Templates are
	// forbidden from declaring secrets, so ServiceCreate must declare it.
	tmpl := filepath.Join(p.root, ".templates", "services", "secret-ref")
	if err := os.MkdirAll(filepath.Join(tmpl, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "copier.yml"), []byte(`_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: service
  name: secret-ref
  name_pattern: "agent-${workspace.name}"
  ensure:
    operator.port_pool.acp:
      range: "3000-3999"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "template", "service.yaml.jinja"), []byte(`services:
  {{ service_name }}:
    runtime: container
    image: nginx
    env:
      AGENT_TOKEN: "${secret.agent-token}"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(service.yaml.jinja): %v", err)
	}

	// Succeeds only because the referenced secret is now declared and resolves;
	// before this behavior the post-create compose render would error.
	if _, err := p.ServiceCreate(ctx, api.ServiceCreateRequest{Template: tmpl, Workspace: "my-pa"}); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}

	stack, err := manifest.LoadFile(manifest.Path(p.root))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	spec, ok := stack.Secrets["agent-token"]
	if !ok {
		t.Fatalf("referenced secret not declared in persisted manifest: %#v", stack.Secrets)
	}
	// The declaration carries no value semantics — referencing grants no value.
	if spec.Generated || spec.Import != "" || spec.Required {
		t.Fatalf("declared secret carries value semantics: %#v", spec)
	}
}

func TestEnsureServiceSecretsDeclaresReferenced(t *testing.T) {
	stack := &manifest.Stack{Secrets: map[string]manifest.Secret{"existing": {}}}
	svc := manifest.Service{
		Env:     map[string]string{"TOKEN": "${secret.agent-x-inference}", "OTHER": "${secret.existing}"},
		Command: []string{"run", "${secret.cmd-secret}"},
	}

	added := ensureServiceSecrets(stack, svc)

	for _, name := range []string{"agent-x-inference", "cmd-secret"} {
		if _, ok := stack.Secrets[name]; !ok {
			t.Fatalf("ensureServiceSecrets did not declare %q", name)
		}
	}
	if len(added) != 2 {
		t.Fatalf("added = %v, want 2 new secrets", added)
	}
	// An already-declared secret is left untouched and not reported as added.
	for _, name := range added {
		if name == "existing" {
			t.Fatal("ensureServiceSecrets re-declared an existing secret")
		}
	}
	// Idempotent: a second pass with the same service adds nothing.
	if again := ensureServiceSecrets(stack, svc); len(again) != 0 {
		t.Fatalf("second ensureServiceSecrets added %v, want none", again)
	}
}
