package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "angee.yaml")

	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "notes",
		SecretsBackend: SecretsBackend{
			Type: "env-file",
			Path: ".env",
		},
		Secrets: map[string]Secret{
			"postgres-password": {Generated: true, Length: 32},
		},
		Services: map[string]Service{
			"postgres": {
				Runtime: RuntimeContainer,
				Image:   "postgres:16",
				Env:     map[string]string{"POSTGRES_PASSWORD": "${secret.postgres-password}"},
			},
			"web": {
				Runtime: RuntimeLocal,
				Command: []string{"go", "run", "./cmd/web"},
				Workdir: "source://app",
			},
		},
	}

	if err := SaveFile(path, stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Name != "notes" {
		t.Fatalf("Name = %q, want notes", loaded.Name)
	}
	if loaded.Services["postgres"].Runtime != RuntimeContainer {
		t.Fatalf("postgres runtime = %q", loaded.Services["postgres"].Runtime)
	}
	if got := loaded.EnvFilePath(root); got != filepath.Join(root, ".env") {
		t.Fatalf("EnvFilePath() = %q", got)
	}
}

func TestManifestRejectsInvalidLocalService(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "bad",
		Services: map[string]Service{
			"web": {Runtime: RuntimeLocal, Image: "example/web:latest"},
		},
	}
	if err := stack.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
}

// TestLoadFileToleratesLegacyLifecycleField guards the
// backwards-compat path for manifests written before commit f48784c
// (workspace lifecycle removal). Older files persist
// `workspaces[*].resolved.lifecycle`, which the strict YAML loader
// would otherwise reject. The field must load successfully and be
// silently dropped on the next save.
func TestLoadFileToleratesLegacyLifecycleField(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "angee.yaml")
	legacy := `version: 1
kind: stack
name: legacy
workspaces:
  feature-a:
    template: workspaces/dev-pr
    resolved:
      chain_root: ".angee"
      lifecycle: auto
      allocations:
        custom: 10002
`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stack, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile rejected legacy lifecycle field: %v", err)
	}
	resolved := stack.Workspaces["feature-a"].Resolved
	if resolved.ChainRoot != ".angee" {
		t.Fatalf("ChainRoot = %q, want .angee", resolved.ChainRoot)
	}
	// LegacyLifecycle is intentionally not part of the persisted form;
	// saving must drop it from the file.
	if err := SaveFile(path, stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	roundtripped, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(roundtripped), "lifecycle") {
		t.Fatalf("saved manifest still carries lifecycle field:\n%s", roundtripped)
	}
}

func TestValidateDoesNotMutate(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "pure",
		SecretsBackend: SecretsBackend{
			Type: "env-file",
		},
		Services: map[string]Service{
			"web": {Runtime: RuntimeContainer, Image: "nginx:latest"},
		},
	}
	before, err := yaml.Marshal(stack)
	if err != nil {
		t.Fatalf("Marshal(before) error = %v", err)
	}
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	after, err := yaml.Marshal(stack)
	if err != nil {
		t.Fatalf("Marshal(after) error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("Validate() mutated stack\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
