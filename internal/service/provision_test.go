package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/config"
	"github.com/fyltr/angee/internal/root"
	"github.com/fyltr/angee/internal/runtime"
	"github.com/fyltr/angee/internal/state"
)

func TestStackInitRendersTemplateAndResolvesState(t *testing.T) {
	if _, err := exec.LookPath("copier"); err != nil {
		t.Skip("copier executable not available")
	}
	worktree := t.TempDir()
	templateDir := writeStackTemplate(t)
	initGitWorktree(t, templateDir)
	rootPath := filepath.Join(worktree, ".angee")
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}

	resp, err := platform.StackInit(context.Background(), api.StackInitRequest{
		Name:     "dev",
		Path:     worktree,
		Root:     rootPath,
		Template: templateDir,
		Ports:    map[string]int{"web": 12348},
		Yes:      true,
	})
	if err != nil {
		t.Fatalf("StackInit() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %q", resp.Status)
	}
	manifest := filepath.Join(rootPath, "angee.yaml")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	cfg, err := config.Load(manifest)
	if err != nil {
		t.Fatalf("Load manifest: %v", err)
	}
	if cfg.Name != filepath.Base(worktree) {
		t.Fatalf("cfg.Name = %q, want %q", cfg.Name, filepath.Base(worktree))
	}
	store := state.New(rootPath)
	ports, err := store.LoadPortLeases()
	if err != nil {
		t.Fatalf("LoadPortLeases: %v", err)
	}
	if ports["web"].Port != 12348 {
		t.Fatalf("web port = %d", ports["web"].Port)
	}
	secrets, err := store.LoadSecrets()
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if len(secrets["token"].Value) != 12 {
		t.Fatalf("token length = %d", len(secrets["token"].Value))
	}
	env, err := os.ReadFile(filepath.Join(rootPath, ".env"))
	if err != nil {
		t.Fatalf("reading .env: %v", err)
	}
	if !strings.Contains(string(env), "TOKEN=") {
		t.Fatalf(".env missing token: %s", env)
	}
}

func TestStackInitRequiresYes(t *testing.T) {
	worktree := t.TempDir()
	platform, err := NewPlatform(filepath.Join(worktree, ".angee"), nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	_, err = platform.StackInit(context.Background(), api.StackInitRequest{Name: "dev", Path: worktree})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected --yes error, got %v", err)
	}
}

func TestProvisioningRejectsInvalidResourceNames(t *testing.T) {
	worktree := t.TempDir()
	platform, err := NewPlatform(filepath.Join(worktree, ".angee"), nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	for _, name := range []string{" bad", "Bad", "bad/name", "bad.name"} {
		_, err := platform.WorkspaceInit(context.Background(), api.WorkspaceInitRequest{Name: name, Yes: true})
		if err == nil {
			t.Fatalf("expected invalid name error for %q", name)
		}
	}
}

func TestStackUpdateUsesCopierAndPreservesSecrets(t *testing.T) {
	if _, err := exec.LookPath("copier"); err != nil {
		t.Skip("copier executable not available")
	}
	worktree := t.TempDir()
	templateDir := writeStackTemplate(t)
	rootPath := filepath.Join(worktree, ".angee")
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	if _, err := platform.StackInit(context.Background(), api.StackInitRequest{
		Name:     "dev",
		Path:     worktree,
		Root:     rootPath,
		Template: templateDir,
		Ports:    map[string]int{"web": 12349},
		Yes:      true,
	}); err != nil {
		t.Fatalf("StackInit() error: %v", err)
	}
	initGitWorktree(t, worktree)
	store := state.New(rootPath)
	secrets, err := store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	firstToken := secrets["token"].Value
	updatedManifest := `version: "1"
kind: stack
name: {{ project_name }}
sources:
  app: { kind: local, ref: current, target: . }
secrets:
  token: { generated: true, length: 24 }
  added: { generated: true, length: 8 }
port_leases:
  web: { default: 0 }
services:
  web:
    runtime: docker
    source: app
    image: nginx:updated
    ports:
      - { name: web, target: "80" }
`
	if err := os.WriteFile(filepath.Join(templateDir, "template", ".angee", "angee.yaml.jinja"), []byte(updatedManifest), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.StackUpdate(context.Background(), api.StackUpdateRequest{Root: rootPath, Yes: true}); err != nil {
		t.Fatalf("StackUpdate() error: %v", err)
	}
	cfg, err := config.Load(filepath.Join(rootPath, "angee.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Services["web"].Image != "nginx:updated" {
		t.Fatalf("web image = %q", cfg.Services["web"].Image)
	}
	secrets, err = store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if secrets["token"].Value != firstToken {
		t.Fatal("expected token to be preserved")
	}
	if len(secrets["added"].Value) != 8 {
		t.Fatalf("added secret length = %d", len(secrets["added"].Value))
	}
}

func TestReconcileResolvesStateAndAppliesBackend(t *testing.T) {
	worktree := t.TempDir()
	rootPath := filepath.Join(worktree, ".angee")
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `version: "1"
kind: stack
name: reconcile-test
sources:
  app: { kind: local, target: . }
secrets:
  token: { generated: true, length: 10 }
port_leases:
  web: { default: 0 }
services:
  web:
    runtime: docker
    source: app
    image: nginx
    ports:
      - { name: web, target: "80" }
`
	if err := os.WriteFile(filepath.Join(rootPath, "angee.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	fake := &fakeBackend{}
	platform.Backend = fake

	resp, err := platform.Reconcile(context.Background(), api.ReconcileRequest{Root: rootPath, Mode: "dev"})
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %q", resp.Status)
	}
	if !fake.applied {
		t.Fatal("expected backend Apply to be called")
	}
	store := state.New(rootPath)
	ports, err := store.LoadPortLeases()
	if err != nil {
		t.Fatal(err)
	}
	if ports["web"].Port == 0 {
		t.Fatal("expected web port to be allocated")
	}
	secrets, err := store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets["token"].Value) != 10 {
		t.Fatalf("token length = %d", len(secrets["token"].Value))
	}
}

func TestReconcileDevRunsStackLocalRuntime(t *testing.T) {
	worktree := t.TempDir()
	rootPath := filepath.Join(worktree, ".angee")
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `version: "1"
kind: stack
name: stack-local-test
sources:
  app: { kind: local, target: . }
secrets:
  token: { default: stack-secret }
port_leases:
  web: { default: 12356, export_env: APP_PORT }
jobs:
  prep:
    kind: process
    source: app
    cwd: .
    command: ["/bin/sh", "-c", "printf '%s:%s' \"$1\" \"$2\" > job-marker && printf 'job-log\\n'", "sh", "${ports.web}", "${secret:token}"]
services:
  web:
    runtime: local
    source: app
    cwd: .
    command: ["/bin/sh", "-c", "printf '%s:%s:%s' \"$APP_PORT\" \"$TOKEN\" \"$1\" > service-marker && printf 'service-log\\n' && sleep 60", "sh", "${ports.web}"]
    env:
      TOKEN: "${secret:token}"
    after: [prep]
`
	if err := os.WriteFile(filepath.Join(rootPath, "angee.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	fake := &fakeBackend{}
	platform.Backend = fake
	defer func() { _ = platform.cleanupStackLocalRuns(&config.AngeeConfig{}) }()

	sink := &recordingSink{}
	resp, err := platform.ReconcileWithOutput(context.Background(), api.ReconcileRequest{Root: rootPath, Mode: "dev"}, sink)
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %q", resp.Status)
	}
	if fake.applied {
		t.Fatal("did not expect Docker backend apply for local-only stack")
	}
	jobMarker := waitReadFile(t, filepath.Join(worktree, "job-marker"))
	if string(jobMarker) != "12356:stack-secret" {
		t.Fatalf("job marker = %q", jobMarker)
	}
	serviceMarker := waitReadFile(t, filepath.Join(worktree, "service-marker"))
	if string(serviceMarker) != "12356:stack-secret:12356" {
		t.Fatalf("service marker = %q", serviceMarker)
	}
	waitForString(t, sink.String, "prep:job-log")
	waitForString(t, sink.String, "web:service-log")
	record, err := os.ReadFile(filepath.Join(rootPath, "state", "runs", "stack-web.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), "stack-secret") {
		t.Fatalf("local run record leaked secret: %s", record)
	}
	if err := os.Remove(filepath.Join(worktree, "service-marker")); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Reconcile(context.Background(), api.ReconcileRequest{Root: rootPath, Mode: "dev", Only: []string{"prep"}}); err != nil {
		t.Fatalf("Reconcile() with --only error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "service-marker")); !os.IsNotExist(err) {
		t.Fatalf("service should not have run with --only prep, got %v", err)
	}
}

func TestWorkspaceInitRendersTemplateRegistersAndResolvesState(t *testing.T) {
	if _, err := exec.LookPath("copier"); err != nil {
		t.Skip("copier executable not available")
	}
	worktree := t.TempDir()
	templateDir := writeWorkspaceTemplate(t)
	rootPath := filepath.Join(worktree, ".angee")
	initStackManifest(t, rootPath, templateDir)
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}

	resp, err := platform.WorkspaceInit(context.Background(), api.WorkspaceInitRequest{
		Name:      "feat-x",
		Root:      rootPath,
		Branch:    "feat-x-branch",
		Overrides: map[string]string{"app": "override-ref"},
		Secrets:   map[string]string{"api-key": "supplied-secret"},
		Ports:     map[string]int{"web": 12350},
		Yes:       true,
	})
	if err != nil {
		t.Fatalf("WorkspaceInit() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %q", resp.Status)
	}

	manifest := filepath.Join(rootPath, "workspaces", "feat-x", "workspace.yaml")
	workspaceCfg, err := config.Load(manifest)
	if err != nil {
		t.Fatalf("Load workspace manifest: %v", err)
	}
	if workspaceCfg.Kind != "workspace" || workspaceCfg.Name != "feat-x" {
		t.Fatalf("workspace identity = %s/%s", workspaceCfg.Kind, workspaceCfg.Name)
	}
	if workspaceCfg.Sources["app"].Ref != "override-ref" {
		t.Fatalf("source ref = %q", workspaceCfg.Sources["app"].Ref)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "workspaces", "feat-x", "code")); err != nil {
		t.Fatalf("workspace source not materialized: %v", err)
	}

	rootCfg, err := config.Load(filepath.Join(rootPath, "angee.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := rootCfg.Workspaces.Items["feat-x"]
	if workspace.Path != "workspaces/feat-x" {
		t.Fatalf("workspace path = %q", workspace.Path)
	}
	if workspace.Template != templateDir {
		t.Fatalf("workspace template = %q", workspace.Template)
	}
	if workspace.Sources["app"] != "override-ref" {
		t.Fatalf("registered source ref = %q", workspace.Sources["app"])
	}

	store := state.New(rootPath)
	ports, err := store.LoadPortLeases()
	if err != nil {
		t.Fatal(err)
	}
	if ports["workspaces/feat-x/web"].Port != 12350 {
		t.Fatalf("workspace web port = %d", ports["workspaces/feat-x/web"].Port)
	}
	secrets, err := store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if secrets["workspaces/feat-x/api-key"].Value != "supplied-secret" {
		t.Fatalf("workspace api-key = %q", secrets["workspaces/feat-x/api-key"].Value)
	}
	env, err := os.ReadFile(filepath.Join(rootPath, "workspaces", "feat-x", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "API_KEY=supplied-secret") {
		t.Fatalf("workspace .env missing supplied secret: %s", env)
	}

	list, err := platform.WorkspaceList(context.Background(), api.WorkspaceListRequest{Root: rootPath})
	if err != nil {
		t.Fatalf("WorkspaceList() error: %v", err)
	}
	if !strings.Contains(list.Message, "feat-x") {
		t.Fatalf("workspace list message = %q", list.Message)
	}
}

func TestWorkspaceUpdateUsesCopierAndPreservesState(t *testing.T) {
	if _, err := exec.LookPath("copier"); err != nil {
		t.Skip("copier executable not available")
	}
	worktree := t.TempDir()
	templateDir := writeWorkspaceTemplate(t)
	rootPath := filepath.Join(worktree, ".angee")
	initStackManifest(t, rootPath, templateDir)
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	if _, err := platform.WorkspaceInit(context.Background(), api.WorkspaceInitRequest{
		Name:    "feat-y",
		Root:    rootPath,
		Secrets: map[string]string{"api-key": "initial-secret"},
		Ports:   map[string]int{"web": 12351},
		Yes:     true,
	}); err != nil {
		t.Fatalf("WorkspaceInit() error: %v", err)
	}
	store := state.New(rootPath)
	secrets, err := store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	firstToken := secrets["workspaces/feat-y/token"].Value
	ports, err := store.LoadPortLeases()
	if err != nil {
		t.Fatal(err)
	}
	ports["workspaces/feat-y/old-port"] = state.PortLease{Name: "workspaces/feat-y/old-port", Port: 12353, UpdatedAt: time.Now().UTC()}
	if err := store.SavePortLeases(ports); err != nil {
		t.Fatal(err)
	}
	secrets["workspaces/feat-y/old-secret"] = state.Secret{Name: "workspaces/feat-y/old-secret", Value: "old", UpdatedAt: time.Now().UTC()}
	if err := store.SaveSecrets(secrets); err != nil {
		t.Fatal(err)
	}
	fake := &fakeBackend{}
	platform.Backend = fake

	updatedManifest := `version: "1"
kind: workspace
name: {{ workspace_name }}
sources:
  app:
    kind: local
    ref: {{ source_ref }}
    target: code
secrets:
  api-key: { required: true }
  token: { generated: true, length: 24 }
  added: { generated: true, length: 8 }
port_leases:
  web: { default: 0 }
services:
  web:
    runtime: docker
    image: nginx:updated
    source: app
    cwd: code-updated
    ports:
      - { name: web, target: "8000" }
`
	if err := os.WriteFile(filepath.Join(templateDir, "template", "workspace.yaml.jinja"), []byte(updatedManifest), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.WorkspaceUpdate(context.Background(), api.WorkspaceUpdateRequest{Root: rootPath, Name: "feat-y", Restart: true, Yes: true}); err != nil {
		t.Fatalf("WorkspaceUpdate() error: %v", err)
	}
	if !fake.applied {
		t.Fatal("expected restart to apply backend")
	}

	workspaceCfg, err := config.Load(filepath.Join(rootPath, "workspaces", "feat-y", "workspace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if workspaceCfg.Services["web"].Cwd != "code-updated" {
		t.Fatalf("web cwd = %q", workspaceCfg.Services["web"].Cwd)
	}
	secrets, err = store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if secrets["workspaces/feat-y/token"].Value != firstToken {
		t.Fatal("expected workspace token to be preserved")
	}
	if len(secrets["workspaces/feat-y/added"].Value) != 8 {
		t.Fatalf("added secret length = %d", len(secrets["workspaces/feat-y/added"].Value))
	}
	if _, ok := secrets["workspaces/feat-y/old-secret"]; ok {
		t.Fatal("expected stale workspace secret to be removed")
	}
	ports, err = store.LoadPortLeases()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ports["workspaces/feat-y/old-port"]; ok {
		t.Fatal("expected stale workspace port lease to be removed")
	}
}

func TestWorkspaceDevMaterializesSourcesAndAppliesDockerServices(t *testing.T) {
	worktree := t.TempDir()
	rootPath := filepath.Join(worktree, ".angee")
	r, err := root.Initialize(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteGitignore(); err != nil {
		t.Fatal(err)
	}
	workspaceDir := filepath.Join(rootPath, "workspaces", "feat-docker")
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		t.Fatal(err)
	}
	rootManifest := `version: "1"
kind: stack
name: workspace-dev-test
workspaces:
  items:
    feat-docker:
      path: workspaces/feat-docker
`
	if err := os.WriteFile(filepath.Join(rootPath, "angee.yaml"), []byte(rootManifest), 0644); err != nil {
		t.Fatal(err)
	}
	workspaceManifest := `version: "1"
kind: workspace
name: feat-docker
sources:
  app:
    kind: local
    target: code
port_leases:
  web: { default: 12354 }
services:
  web:
    runtime: docker
    source: app
    image: nginx
    ports:
      - { name: web, target: "80" }
`
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.yaml"), []byte(workspaceManifest), 0644); err != nil {
		t.Fatal(err)
	}
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	fake := &fakeBackend{}
	platform.Backend = fake

	resp, err := platform.WorkspaceDev(context.Background(), api.WorkspaceDevRequest{Name: "feat-docker", Root: rootPath})
	if err != nil {
		t.Fatalf("WorkspaceDev() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %q", resp.Status)
	}
	if !fake.applied {
		t.Fatal("expected backend Apply to be called")
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "code")); err != nil {
		t.Fatalf("workspace source not materialized: %v", err)
	}
	compose, err := os.ReadFile(filepath.Join(rootPath, "docker-compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "workspace-feat-docker-web:") {
		t.Fatalf("compose missing workspace service:\n%s", compose)
	}
	if !strings.Contains(string(compose), "12354:80") {
		t.Fatalf("compose missing leased port:\n%s", compose)
	}
}

func TestWorkspaceDevStartsLocalServices(t *testing.T) {
	worktree := t.TempDir()
	rootPath := filepath.Join(worktree, ".angee")
	r, err := root.Initialize(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteGitignore(); err != nil {
		t.Fatal(err)
	}
	workspaceDir := filepath.Join(rootPath, "workspaces", "feat-local")
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		t.Fatal(err)
	}
	rootManifest := `version: "1"
kind: stack
name: workspace-local-test
workspaces:
  items:
    feat-local:
      path: workspaces/feat-local
`
	if err := os.WriteFile(filepath.Join(rootPath, "angee.yaml"), []byte(rootManifest), 0644); err != nil {
		t.Fatal(err)
	}
	workspaceManifest := `version: "1"
kind: workspace
name: feat-local
sources:
  app:
    kind: local
    target: code
secrets:
  token: { default: local-secret }
port_leases:
  web: { default: 12355, export_env: APP_PORT }
services:
  web:
    runtime: local
    source: app
    cwd: code
    command: ["/bin/sh", "-c", "printf '%s:%s' \"$APP_PORT\" \"$TOKEN\" > marker && sleep 60"]
    env:
      TOKEN: "${secret:token}"
`
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.yaml"), []byte(workspaceManifest), 0644); err != nil {
		t.Fatal(err)
	}
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	fake := &fakeBackend{}
	platform.Backend = fake
	defer func() { _ = platform.stopWorkspaceLocalServices("feat-local") }()

	resp, err := platform.WorkspaceDev(context.Background(), api.WorkspaceDevRequest{Name: "feat-local", Root: rootPath})
	if err != nil {
		t.Fatalf("WorkspaceDev() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %q", resp.Status)
	}
	if fake.applied {
		t.Fatal("did not expect Docker backend apply for local-only workspace")
	}
	marker := filepath.Join(workspaceDir, "code", "marker")
	data := waitReadFile(t, marker)
	if string(data) != "12355:local-secret" {
		t.Fatalf("marker = %q", data)
	}
	recordPath := filepath.Join(rootPath, "state", "runs", "workspace-feat-local-web.json")
	info, err := os.Stat(recordPath)
	if err != nil {
		t.Fatalf("local run record missing: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("local run record mode = %v", info.Mode().Perm())
	}
	record, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), "local-secret") {
		t.Fatalf("local run record leaked secret: %s", record)
	}

	resp, err = platform.WorkspaceDev(context.Background(), api.WorkspaceDevRequest{Name: "feat-local", Root: rootPath})
	if err != nil {
		t.Fatalf("second WorkspaceDev() error: %v", err)
	}
	if resp.Message != "Prepared workspace feat-local" {
		t.Fatalf("second message = %q", resp.Message)
	}
	withoutServices := `version: "1"
kind: workspace
name: feat-local
sources:
  app:
    kind: local
    target: code
`
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.yaml"), []byte(withoutServices), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.WorkspaceDev(context.Background(), api.WorkspaceDevRequest{Name: "feat-local", Root: rootPath}); err != nil {
		t.Fatalf("WorkspaceDev() after removing services error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "state", "runs", "workspace-feat-local-web.json")); !os.IsNotExist(err) {
		t.Fatalf("expected stale local run record to be removed, got %v", err)
	}
}

func TestAgentInitRendersTemplateRegistersAndResolvesState(t *testing.T) {
	if _, err := exec.LookPath("copier"); err != nil {
		t.Skip("copier executable not available")
	}
	worktree := t.TempDir()
	templateDir := writeAgentTemplate(t)
	rootPath := filepath.Join(worktree, ".angee")
	initAgentStackManifest(t, rootPath, templateDir)
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}

	resp, err := platform.AgentInit(context.Background(), api.AgentInitRequest{
		Name:      "devbot",
		Root:      rootPath,
		Branch:    "devbot-branch",
		Overrides: map[string]string{"app": "override-ref"},
		Secrets:   map[string]string{"api-key": "agent-secret"},
		Yes:       true,
	})
	if err != nil {
		t.Fatalf("AgentInit() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %q", resp.Status)
	}

	agentManifest := filepath.Join(rootPath, "agents", "devbot", "agent.yaml")
	agentCfg, err := config.Load(agentManifest)
	if err != nil {
		t.Fatalf("Load agent manifest: %v", err)
	}
	if agentCfg.Kind != "agent" || agentCfg.Name != "devbot" {
		t.Fatalf("agent identity = %s/%s", agentCfg.Kind, agentCfg.Name)
	}
	if agentCfg.Sources["app"].Ref != "override-ref" {
		t.Fatalf("source ref = %q", agentCfg.Sources["app"].Ref)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "agents", "devbot", "workspace", "code")); err != nil {
		t.Fatalf("agent source not materialized: %v", err)
	}

	rootCfg, err := config.Load(filepath.Join(rootPath, "angee.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	agent := rootCfg.Agents.Items["devbot"]
	if agent.Template != templateDir {
		t.Fatalf("agent template = %q", agent.Template)
	}
	if agent.Image != "agent:test" {
		t.Fatalf("agent image = %q", agent.Image)
	}
	if agent.Env["API_KEY"] != "${secret:api-key}" {
		t.Fatalf("agent env = %#v", agent.Env)
	}
	if len(agent.MCPServers) != 1 || agent.MCPServers[0] != "files" {
		t.Fatalf("agent MCP servers = %#v", agent.MCPServers)
	}
	if _, ok := rootCfg.MCPServers["files"]; !ok {
		t.Fatal("root config missing files MCP server")
	}

	store := state.New(rootPath)
	secrets, err := store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if secrets["api-key"].Value != "agent-secret" {
		t.Fatalf("api-key = %q", secrets["api-key"].Value)
	}
	rootEnv, err := os.ReadFile(filepath.Join(rootPath, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rootEnv), "API_KEY=agent-secret") {
		t.Fatalf("root .env missing agent secret: %s", rootEnv)
	}
	agentEnv, err := os.ReadFile(filepath.Join(rootPath, "agents", "devbot", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agentEnv), "API_KEY=agent-secret") {
		t.Fatalf("agent .env missing agent secret: %s", agentEnv)
	}
}

func TestAgentUpdateUsesCopierAndPreservesState(t *testing.T) {
	if _, err := exec.LookPath("copier"); err != nil {
		t.Skip("copier executable not available")
	}
	worktree := t.TempDir()
	templateDir := writeAgentTemplate(t)
	rootPath := filepath.Join(worktree, ".angee")
	initAgentStackManifest(t, rootPath, templateDir)
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	if _, err := platform.AgentInit(context.Background(), api.AgentInitRequest{
		Name:    "updatebot",
		Root:    rootPath,
		Secrets: map[string]string{"api-key": "initial-secret"},
		Yes:     true,
	}); err != nil {
		t.Fatalf("AgentInit() error: %v", err)
	}
	store := state.New(rootPath)
	secrets, err := store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	firstToken := secrets["token"].Value

	updatedManifest := `version: "1"
kind: agent
name: {{ agent_name }}
sources:
  app:
    kind: local
    ref: {{ source_ref }}
    target: code
secrets:
  api-key: { required: true }
  token: { generated: true, length: 24 }
  added: { generated: true, length: 8 }
mcp_servers:
  files:
    transport: stdio
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/workspace"]
services:
  {{ agent_name }}:
    runtime: docker
    image: agent:updated
    source: app
    env:
      API_KEY: "${secret:api-key}"
`
	if err := os.WriteFile(filepath.Join(templateDir, "template", "agent.yaml.jinja"), []byte(updatedManifest), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.AgentUpdate(context.Background(), api.AgentUpdateRequest{Root: rootPath, Name: "updatebot", Yes: true}); err != nil {
		t.Fatalf("AgentUpdate() error: %v", err)
	}

	rootCfg, err := config.Load(filepath.Join(rootPath, "angee.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if rootCfg.Agents.Items["updatebot"].Image != "agent:updated" {
		t.Fatalf("agent image = %q", rootCfg.Agents.Items["updatebot"].Image)
	}
	secrets, err = store.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if secrets["token"].Value != firstToken {
		t.Fatal("expected agent token to be preserved")
	}
	if len(secrets["added"].Value) != 8 {
		t.Fatalf("added secret length = %d", len(secrets["added"].Value))
	}
}

func TestAgentDestroyStopsUnregistersAndRemovesDirectory(t *testing.T) {
	if _, err := exec.LookPath("copier"); err != nil {
		t.Skip("copier executable not available")
	}
	worktree := t.TempDir()
	templateDir := writeAgentTemplate(t)
	rootPath := filepath.Join(worktree, ".angee")
	initAgentStackManifest(t, rootPath, templateDir)
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	if _, err := platform.AgentInit(context.Background(), api.AgentInitRequest{
		Name:    "destroybot",
		Root:    rootPath,
		Secrets: map[string]string{"api-key": "destroy-secret"},
		Yes:     true,
	}); err != nil {
		t.Fatalf("AgentInit() error: %v", err)
	}
	fake := &fakeBackend{}
	platform.Backend = fake

	resp, err := platform.AgentDestroy(context.Background(), api.AgentActionRequest{Name: "destroybot", Root: rootPath})
	if err != nil {
		t.Fatalf("AgentDestroy() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %q", resp.Status)
	}
	if len(fake.stopped) != 1 || fake.stopped[0] != "agent-destroybot" {
		t.Fatalf("stopped services = %#v", fake.stopped)
	}
	rootCfg, err := config.Load(filepath.Join(rootPath, "angee.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rootCfg.Agents.Items["destroybot"]; ok {
		t.Fatal("destroyed agent is still registered")
	}
	if _, err := os.Stat(filepath.Join(rootPath, "agents", "destroybot")); !os.IsNotExist(err) {
		t.Fatalf("expected agent directory to be removed, got %v", err)
	}
}

func TestAgentStartMaterializesSourcesAndStopValidatesAgent(t *testing.T) {
	if _, err := exec.LookPath("copier"); err != nil {
		t.Skip("copier executable not available")
	}
	worktree := t.TempDir()
	templateDir := writeAgentTemplate(t)
	rootPath := filepath.Join(worktree, ".angee")
	initAgentStackManifest(t, rootPath, templateDir)
	platform, err := NewPlatform(rootPath, nil)
	if err != nil {
		t.Fatalf("NewPlatform() error: %v", err)
	}
	if _, err := platform.AgentInit(context.Background(), api.AgentInitRequest{
		Name:    "startbot",
		Root:    rootPath,
		Secrets: map[string]string{"api-key": "start-secret"},
		Yes:     true,
	}); err != nil {
		t.Fatalf("AgentInit() error: %v", err)
	}
	codeDir := filepath.Join(rootPath, "agents", "startbot", "workspace", "code")
	if err := os.RemoveAll(codeDir); err != nil {
		t.Fatal(err)
	}
	fake := &fakeBackend{}
	platform.Backend = fake

	if _, err := platform.AgentStart(context.Background(), "startbot"); err != nil {
		t.Fatalf("AgentStart() error: %v", err)
	}
	if !fake.applied {
		t.Fatal("expected backend Apply to be called")
	}
	if _, err := os.Stat(codeDir); err != nil {
		t.Fatalf("agent source not materialized on start: %v", err)
	}
	agentEnv, err := os.ReadFile(filepath.Join(rootPath, "agents", "startbot", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agentEnv), "API_KEY=start-secret") {
		t.Fatalf("agent .env missing secret: %s", agentEnv)
	}
	compose, err := os.ReadFile(filepath.Join(rootPath, "docker-compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "agent-startbot:") {
		t.Fatalf("compose missing agent service:\n%s", compose)
	}
	if err := platform.AgentStop(context.Background(), "startbot"); err != nil {
		t.Fatalf("AgentStop() error: %v", err)
	}
	if len(fake.stopped) != 1 || fake.stopped[0] != "agent-startbot" {
		t.Fatalf("stopped services = %#v", fake.stopped)
	}
	if err := platform.AgentStop(context.Background(), "missing"); err == nil {
		t.Fatal("expected missing agent stop error")
	}
}

func writeStackTemplate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "template", ".angee"), 0755); err != nil {
		t.Fatal(err)
	}
	copierYAML := `_min_copier_version: "9.0"
_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  schema: 1
  kind: stack
  name: dev
project_name:
  type: str
  default: demo
`
	if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte(copierYAML), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := `version: "1"
kind: stack
name: {{ project_name }}
sources:
  app:
    kind: local
    ref: current
    target: .
secrets:
  token:
    generated: true
    length: 12
port_leases:
  web:
    default: 0
services:
  web:
    runtime: docker
    source: app
    image: nginx
    ports:
      - name: web
        target: "80"
`
	if err := os.WriteFile(filepath.Join(dir, "template", ".angee", "angee.yaml.jinja"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeWorkspaceTemplate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "template"), 0755); err != nil {
		t.Fatal(err)
	}
	copierYAML := `_min_copier_version: "9.0"
_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  schema: 1
  kind: workspace
  name: feature-dev
workspace_name:
  type: str
  default: feature
source_ref:
  type: str
  default: same-name
`
	if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte(copierYAML), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := `version: "1"
kind: workspace
name: {{ workspace_name }}
sources:
  app:
    kind: local
    ref: {{ source_ref }}
    target: code
secrets:
  api-key:
    required: true
  token:
    generated: true
    length: 12
port_leases:
  web:
    default: 0
services:
  web:
    runtime: local
    source: app
    cwd: code
    ports:
      - name: web
        target: "8000"
`
	if err := os.WriteFile(filepath.Join(dir, "template", "workspace.yaml.jinja"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeAgentTemplate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "template"), 0755); err != nil {
		t.Fatal(err)
	}
	copierYAML := `_min_copier_version: "9.0"
_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  schema: 1
  kind: agent
  name: developer
agent_name:
  type: str
  default: developer
source_ref:
  type: str
  default: same-name
`
	if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte(copierYAML), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := `version: "1"
kind: agent
name: {{ agent_name }}
sources:
  app:
    kind: local
    ref: {{ source_ref }}
    target: code
secrets:
  api-key:
    required: true
  token:
    generated: true
    length: 12
mcp_servers:
  files:
    transport: stdio
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/workspace"]
services:
  {{ agent_name }}:
    runtime: docker
    image: agent:test
    source: app
    env:
      API_KEY: "${secret:api-key}"
`
	if err := os.WriteFile(filepath.Join(dir, "template", "agent.yaml.jinja"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func initStackManifest(t *testing.T, rootPath, workspaceTemplate string) {
	t.Helper()
	r, err := root.Initialize(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteGitignore(); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`version: "1"
kind: stack
name: workspace-test
workspaces:
  default_template: %q
`, workspaceTemplate)
	if err := os.WriteFile(filepath.Join(rootPath, "angee.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
}

func initAgentStackManifest(t *testing.T, rootPath, agentTemplate string) {
	t.Helper()
	r, err := root.Initialize(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteGitignore(); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`version: "1"
kind: stack
name: agent-test
agents:
  default_template: %q
`, agentTemplate)
	if err := os.WriteFile(filepath.Join(rootPath, "angee.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
}

func initGitWorktree(t *testing.T, worktree string) {
	t.Helper()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = worktree
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.name", "angee-test")
	runGit("config", "user.email", "angee-test@example.invalid")
	runGit("add", ".")
	runGit("commit", "-m", "initial")
}

func waitReadFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return data
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", path, lastErr)
	return nil
}

func waitForString(t *testing.T, read func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(read(), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for output %q in %q", want, read())
}

type recordingSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *recordingSink) Writer(name string) io.Writer {
	return recordingWriter{sink: s, name: name}
}

func (s *recordingSink) SystemLine(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(&s.b, "angee:"+format+"\n", args...)
}

func (s *recordingSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type recordingWriter struct {
	sink *recordingSink
	name string
}

func (w recordingWriter) Write(data []byte) (int, error) {
	w.sink.mu.Lock()
	defer w.sink.mu.Unlock()
	_, _ = fmt.Fprintf(&w.sink.b, "%s:%s", w.name, data)
	return len(data), nil
}

type fakeBackend struct {
	applied bool
	stopped []string
}

func (f *fakeBackend) Diff(ctx context.Context, composeFile string) (*runtime.ChangeSet, error) {
	return &runtime.ChangeSet{}, nil
}

func (f *fakeBackend) Apply(ctx context.Context, composeFile string) (*runtime.ApplyResult, error) {
	f.applied = true
	return &runtime.ApplyResult{ServicesStarted: []string{"web"}}, nil
}

func (f *fakeBackend) Pull(ctx context.Context) error { return nil }

func (f *fakeBackend) Status(ctx context.Context) ([]*runtime.ServiceStatus, error) { return nil, nil }

func (f *fakeBackend) Logs(ctx context.Context, service string, opts runtime.LogOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeBackend) Scale(ctx context.Context, service string, replicas int) error { return nil }

func (f *fakeBackend) Stop(ctx context.Context, services ...string) error {
	f.stopped = append(f.stopped, services...)
	return nil
}

func (f *fakeBackend) Down(ctx context.Context) error { return nil }
