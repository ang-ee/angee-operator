package service

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyltr/angee/internal/manifest"
	"github.com/fyltr/angee/internal/runtime"
)

type stubStatusBackend struct {
	statuses []runtime.ServiceStatus
}

func (b stubStatusBackend) Build(context.Context, runtime.Target) error { return nil }
func (b stubStatusBackend) Up(context.Context, runtime.Target) error    { return nil }
func (b stubStatusBackend) UpForeground(context.Context, runtime.Target, io.Writer, io.Writer) error {
	return nil
}
func (b stubStatusBackend) Down(context.Context, runtime.Target) error    { return nil }
func (b stubStatusBackend) Start(context.Context, runtime.Target) error   { return nil }
func (b stubStatusBackend) Stop(context.Context, runtime.Target) error    { return nil }
func (b stubStatusBackend) Restart(context.Context, runtime.Target) error { return nil }
func (b stubStatusBackend) Logs(context.Context, runtime.LogsRequest) (<-chan string, error) {
	ch := make(chan string)
	close(ch)
	return ch, nil
}
func (b stubStatusBackend) Status(context.Context, string) ([]runtime.ServiceStatus, error) {
	return b.statuses, nil
}

func TestStackPrepareWritesSecretSafeGeneratedFiles(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "notes",
		SecretsBackend: manifest.SecretsBackend{
			Type: "env-file",
			Path: ".env",
		},
		Secrets: map[string]manifest.Secret{
			"postgres-password": {Required: true, Import: "env:POSTGRES_PASSWORD"},
		},
		Ports: map[string]manifest.Port{
			"postgres": {Value: 5432},
		},
		Services: map[string]manifest.Service{
			"postgres": {
				Runtime: manifest.RuntimeContainer,
				Image:   "postgres:16",
				Env: map[string]string{
					"POSTGRES_PASSWORD": "${secret.postgres-password}",
				},
				Ports: []string{"127.0.0.1:${ports.postgres}:5432"},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	t.Setenv("POSTGRES_PASSWORD", "super-secret")

	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	compiled, err := platform.StackPrepare(context.Background())
	if err != nil {
		t.Fatalf("StackPrepare() error = %v", err)
	}
	text, err := compiled.Text()
	if err != nil {
		t.Fatalf("Text() error = %v", err)
	}
	if strings.Contains(text, "super-secret") {
		t.Fatal("compiled runtime files contain resolved secret")
	}
	if !strings.Contains(text, "${ANGEE_SECRET_POSTGRES_PASSWORD}") {
		t.Fatalf("compiled text missing secret env placeholder:\n%s", text)
	}
	envData, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		t.Fatalf("ReadFile(.env) error = %v", err)
	}
	if !strings.Contains(string(envData), "ANGEE_SECRET_POSTGRES_PASSWORD") || !strings.Contains(string(envData), "super-secret") {
		t.Fatalf("env file does not contain runtime secret env var: %s", envData)
	}
}

func TestStackStatusMergesRuntimeStateAndHealth(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "demo",
		Services: map[string]manifest.Service{
			"web":     {Runtime: manifest.RuntimeContainer, Image: "nginx"},
			"db":      {Runtime: manifest.RuntimeContainer, Image: "postgres:16"},
			"unknown": {Runtime: manifest.RuntimeContainer, Image: "alpine"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	stub := stubStatusBackend{statuses: []runtime.ServiceStatus{
		{Name: "web", Runtime: "container", State: "running", Health: "healthy"},
		{Name: "db", Runtime: "container", State: "running", Health: "unhealthy"},
	}}
	platform, err := NewWithBackends(root, stub, stubStatusBackend{})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	resp, err := platform.StackStatus(context.Background())
	if err != nil {
		t.Fatalf("StackStatus: %v", err)
	}
	if got := resp.Services["web"]; got.Status != "running" || got.Health != "healthy" {
		t.Fatalf("web = %+v, want running/healthy", got)
	}
	if got := resp.Services["db"]; got.Status != "running" || got.Health != "unhealthy" {
		t.Fatalf("db = %+v, want running/unhealthy", got)
	}
	if got := resp.Services["unknown"]; got.Status != "declared" || got.Health != "" {
		t.Fatalf("unknown = %+v, want declared/empty health", got)
	}
}
