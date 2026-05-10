package manifest

import (
	"bytes"
	"path/filepath"
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
