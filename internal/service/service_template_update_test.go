package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"gopkg.in/yaml.v3"
)

func TestServiceUpdateFromTemplateRejectsUnsafeServiceName(t *testing.T) {
	p, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.ServiceUpdateFromTemplate(context.Background(), "../escape", api.ServiceUpdateTemplateRequest{})
	var invalid *InvalidInputError
	if !errors.As(err, &invalid) || invalid.Field != "name" {
		t.Fatalf("ServiceUpdateFromTemplate error = %v, want invalid name", err)
	}
}

func TestMergeRenderedService(t *testing.T) {
	base := manifest.Service{
		Runtime: manifest.RuntimeContainer,
		Image:   "agent:v1",
		Env:     map[string]string{"LOCAL": "base", "TEMPLATE": "base", "REMOVE": "base"},
		Command: manifest.StringList{"run", "v1"},
	}
	tests := []struct {
		name          string
		base          []byte
		current       manifest.Service
		newService    manifest.Service
		overwrite     bool
		wantEnv       map[string]string
		wantImage     string
		wantCommand   manifest.StringList
		wantConflicts []copierx.Conflict
	}{
		{
			name:        "independent recursive map changes merge",
			base:        serviceDocument(t, "agent", base),
			current:     serviceWith(base, func(service *manifest.Service) { service.Env["LOCAL"] = "local" }),
			newService:  serviceWith(base, func(service *manifest.Service) { service.Env["TEMPLATE"] = "template" }),
			wantEnv:     map[string]string{"LOCAL": "local", "TEMPLATE": "template", "REMOVE": "base"},
			wantImage:   "agent:v1",
			wantCommand: manifest.StringList{"run", "v1"},
		},
		{
			name:        "current-only map key is preserved and template deletion applies",
			base:        serviceDocument(t, "agent", base),
			current:     serviceWith(base, func(service *manifest.Service) { service.Env["USER"] = "keep" }),
			newService:  serviceWith(base, func(service *manifest.Service) { delete(service.Env, "REMOVE") }),
			wantEnv:     map[string]string{"LOCAL": "base", "TEMPLATE": "base", "USER": "keep"},
			wantImage:   "agent:v1",
			wantCommand: manifest.StringList{"run", "v1"},
		},
		{
			name:          "same scalar conflict",
			base:          serviceDocument(t, "agent", base),
			current:       serviceWith(base, func(service *manifest.Service) { service.Image = "agent:local" }),
			newService:    serviceWith(base, func(service *manifest.Service) { service.Image = "agent:template" }),
			wantImage:     "agent:local",
			wantEnv:       base.Env,
			wantCommand:   base.Command,
			wantConflicts: []copierx.Conflict{{Path: "services.agent.image", Reason: copierx.ConflictLocallyModified}},
		},
		{
			name:        "overwrite resolves scalar conflict",
			base:        serviceDocument(t, "agent", base),
			current:     serviceWith(base, func(service *manifest.Service) { service.Image = "agent:local" }),
			newService:  serviceWith(base, func(service *manifest.Service) { service.Image = "agent:template" }),
			overwrite:   true,
			wantImage:   "agent:template",
			wantEnv:     base.Env,
			wantCommand: base.Command,
		},
		{
			name:          "lists are atomic",
			base:          serviceDocument(t, "agent", base),
			current:       serviceWith(base, func(service *manifest.Service) { service.Command = manifest.StringList{"local"} }),
			newService:    serviceWith(base, func(service *manifest.Service) { service.Command = manifest.StringList{"template"} }),
			wantImage:     "agent:v1",
			wantEnv:       base.Env,
			wantCommand:   manifest.StringList{"local"},
			wantConflicts: []copierx.Conflict{{Path: "services.agent.command", Reason: copierx.ConflictLocallyModified}},
		},
		{
			name:        "legacy identical service is adopted",
			current:     base,
			newService:  base,
			wantImage:   "agent:v1",
			wantEnv:     base.Env,
			wantCommand: base.Command,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := serviceDocument(t, "agent", tt.newService)
			merged, _, conflicts, err := mergeRenderedService(tt.base, tt.current, rendered, "agent", tt.overwrite)
			if err != nil {
				t.Fatalf("mergeRenderedService: %v", err)
			}
			if merged.Image != tt.wantImage {
				t.Fatalf("image = %q, want %q", merged.Image, tt.wantImage)
			}
			if !yamlEqual(merged.Env, tt.wantEnv) {
				t.Fatalf("env = %#v, want %#v", merged.Env, tt.wantEnv)
			}
			if !yamlEqual(merged.Command, tt.wantCommand) {
				t.Fatalf("command = %#v, want %#v", merged.Command, tt.wantCommand)
			}
			if !yamlEqual(conflicts, tt.wantConflicts) {
				t.Fatalf("conflicts = %#v, want %#v", conflicts, tt.wantConflicts)
			}
		})
	}
}

func TestServiceUpdateFromTemplateUpdatesManifestAndAssets(t *testing.T) {
	p, fixture := setupServiceCreateFixture(t)
	template := copyServiceTemplateFixture(t, fixture)
	ctx := context.Background()
	if _, err := p.ServiceCreate(ctx, api.ServiceCreateRequest{
		Template: template, Workspace: "my-pa", Inputs: map[string]string{"auth_mode": "api_key"},
	}); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack: %v", err)
	}
	service := stack.Services["agent-my-pa"]
	service.Env["AUTH_MODE"] = "local-override"
	stack.Services["agent-my-pa"] = service
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatalf("SaveFile(local service edit): %v", err)
	}
	if err := os.WriteFile(filepath.Join(template, "template", "docker", "Dockerfile"), []byte("FROM alpine:3.22\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(Dockerfile template): %v", err)
	}
	serviceTemplate := `services:
  {{ service_name }}:
    runtime: container
    build:
      context: ./services/{{ service_name }}/docker
    ports:
      - "{{ alloc_acp }}:3007"
    mounts:
      - "workspace://{{ workspace_name }}:/workspace"
    env:
      AUTH_MODE: "{{ auth_mode }}"
      ACP_LOG_LEVEL: debug
`
	if err := os.WriteFile(filepath.Join(template, "template", "service.yaml.jinja"), []byte(serviceTemplate), 0o644); err != nil {
		t.Fatalf("WriteFile(service template): %v", err)
	}

	dryRun, err := p.ServiceUpdateFromTemplate(ctx, "agent-my-pa", api.ServiceUpdateTemplateRequest{DryRun: true})
	if err != nil {
		t.Fatalf("ServiceUpdateFromTemplate(dry-run): %v", err)
	}
	if !dryRun.Changed {
		t.Fatalf("dry-run result = %+v, want changed", dryRun)
	}
	dockerfilePath := filepath.Join(p.root, "services", "agent-my-pa", "docker", "Dockerfile")
	dockerfile, _ := os.ReadFile(dockerfilePath)
	if string(dockerfile) != "FROM alpine:3.21\nCMD [\"sleep\", \"infinity\"]\n" {
		t.Fatalf("dry-run changed Dockerfile: %q", dockerfile)
	}

	result, err := p.ServiceUpdateFromTemplate(ctx, "agent-my-pa", api.ServiceUpdateTemplateRequest{})
	if err != nil {
		t.Fatalf("ServiceUpdateFromTemplate: %v", err)
	}
	if !result.Changed {
		t.Fatalf("result = %+v, want changed", result)
	}
	updated, _ := p.LoadStack()
	if got := updated.Services["agent-my-pa"].Env["AUTH_MODE"]; got != "local-override" {
		t.Fatalf("AUTH_MODE = %q, want local override preserved", got)
	}
	if got := updated.Services["agent-my-pa"].Env["ACP_LOG_LEVEL"]; got != "debug" {
		t.Fatalf("ACP_LOG_LEVEL = %q, want template update", got)
	}
	dockerfile, _ = os.ReadFile(dockerfilePath)
	if string(dockerfile) != "FROM alpine:3.22\n" {
		t.Fatalf("updated Dockerfile = %q", dockerfile)
	}
}

func TestServiceUpdateFromTemplatePreservesAssetConflictUnlessOverwrite(t *testing.T) {
	p, fixture := setupServiceCreateFixture(t)
	template := copyServiceTemplateFixture(t, fixture)
	ctx := context.Background()
	if _, err := p.ServiceCreate(ctx, api.ServiceCreateRequest{Template: template, Workspace: "my-pa"}); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	dockerfilePath := filepath.Join(p.root, "services", "agent-my-pa", "docker", "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte("local edit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(local Dockerfile): %v", err)
	}
	if err := os.WriteFile(filepath.Join(template, "template", "docker", "Dockerfile"), []byte("template edit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(template Dockerfile): %v", err)
	}

	if _, err := p.ServiceUpdateFromTemplate(ctx, "agent-my-pa", api.ServiceUpdateTemplateRequest{}); err == nil {
		t.Fatal("ServiceUpdateFromTemplate succeeded despite asset conflict")
	}
	data, _ := os.ReadFile(dockerfilePath)
	if string(data) != "local edit\n" {
		t.Fatalf("conflicting Dockerfile changed: %q", data)
	}
	if _, err := p.ServiceUpdateFromTemplate(ctx, "agent-my-pa", api.ServiceUpdateTemplateRequest{Overwrite: true}); err != nil {
		t.Fatalf("ServiceUpdateFromTemplate(overwrite): %v", err)
	}
	data, _ = os.ReadFile(dockerfilePath)
	if string(data) != "template edit\n" {
		t.Fatalf("overwritten Dockerfile = %q", data)
	}
}

func TestServiceUpdateFromTemplateAdoptsLegacyInstance(t *testing.T) {
	p, fixture := setupServiceCreateFixture(t)
	template := copyServiceTemplateFixture(t, fixture)
	ctx := context.Background()
	if _, err := p.ServiceCreate(ctx, api.ServiceCreateRequest{Template: template, Workspace: "my-pa"}); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	statePath := renderPlanStatePath(p.root, "services", "agent-my-pa")
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("Remove(render state): %v", err)
	}
	result, err := p.ServiceUpdateFromTemplate(ctx, "agent-my-pa", api.ServiceUpdateTemplateRequest{})
	if err != nil {
		t.Fatalf("ServiceUpdateFromTemplate(legacy): %v", err)
	}
	if result.Changed || len(result.Conflicts) != 0 {
		t.Fatalf("legacy adoption result = %+v", result)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("render state not recreated: %v", err)
	}
}

func TestServiceUpdateFromTemplateRejectsReservedInput(t *testing.T) {
	p, fixture := setupServiceCreateFixture(t)
	template := copyServiceTemplateFixture(t, fixture)
	ctx := context.Background()
	if _, err := p.ServiceCreate(ctx, api.ServiceCreateRequest{Template: template, Workspace: "my-pa"}); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	if _, err := p.ServiceUpdateFromTemplate(ctx, "agent-my-pa", api.ServiceUpdateTemplateRequest{
		Inputs: map[string]string{"workspace_name": "other"},
	}); err == nil {
		t.Fatal("ServiceUpdateFromTemplate accepted workspace_name override")
	}
}

func serviceWith(base manifest.Service, update func(*manifest.Service)) manifest.Service {
	service := base
	service.Env = map[string]string{}
	for key, value := range base.Env {
		service.Env[key] = value
	}
	service.Command = append(manifest.StringList(nil), base.Command...)
	update(&service)
	return service
}

func serviceDocument(t *testing.T, name string, service manifest.Service) []byte {
	t.Helper()
	data, err := yaml.Marshal(partialServiceManifest{Services: map[string]manifest.Service{name: service}})
	if err != nil {
		t.Fatalf("Marshal(service document): %v", err)
	}
	return data
}

func copyServiceTemplateFixture(t *testing.T, source string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "service-template")
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	}); err != nil {
		t.Fatalf("copy service template fixture: %v", err)
	}
	return target
}
