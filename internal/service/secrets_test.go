package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fyltr/angee/internal/manifest"
)

func setupSecretsFixture(t *testing.T) *Platform {
	t.Helper()
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Secrets: map[string]manifest.Secret{
			"db-password": {Required: true},
			"jwt-key":     {Generated: true, Length: 32},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestSecretsListShowsDeclaredOnly(t *testing.T) {
	p := setupSecretsFixture(t)
	// Manually drop an undeclared secret into the env-file backend; List
	// should ignore it (declared-only contract).
	if _, err := p.SecretSet(context.Background(), "unrelated-key", "value"); err != nil {
		t.Fatalf("SecretSet(undeclared) error = %v", err)
	}
	refs, err := p.SecretsList(context.Background())
	if err != nil {
		t.Fatalf("SecretsList error = %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2 (only declared): %+v", len(refs), refs)
	}
	byName := map[string]bool{}
	for _, r := range refs {
		byName[r.Name] = r.Declared
	}
	if !byName["db-password"] || !byName["jwt-key"] {
		t.Fatalf("expected declared secrets, got %+v", refs)
	}
}

func TestSecretSetGetValueRoundtrip(t *testing.T) {
	p := setupSecretsFixture(t)
	ref, err := p.SecretSet(context.Background(), "db-password", "hunter2")
	if err != nil {
		t.Fatalf("SecretSet error = %v", err)
	}
	if !ref.Declared || !ref.HasValue || ref.EnvVar != "ANGEE_SECRET_DB_PASSWORD" {
		t.Fatalf("ref = %+v, want declared+hasValue+ANGEE_SECRET_DB_PASSWORD env var", ref)
	}
	val, err := p.SecretValue(context.Background(), "db-password")
	if err != nil {
		t.Fatalf("SecretValue error = %v", err)
	}
	if val.Value != "hunter2" {
		t.Fatalf("value = %q, want hunter2", val.Value)
	}
}

func TestSecretValueOnMissingReturnsNotFound(t *testing.T) {
	p := setupSecretsFixture(t)
	_, err := p.SecretValue(context.Background(), "db-password")
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want NotFoundError", err)
	}
}

func TestSecretSetRejectsEmptyValue(t *testing.T) {
	p := setupSecretsFixture(t)
	_, err := p.SecretSet(context.Background(), "db-password", "")
	var invalid *InvalidInputError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want InvalidInputError", err)
	}
}

func TestSecretNameValidation(t *testing.T) {
	cases := []string{
		"",
		"../escape",
		"name with spaces",
		"name\nwith\nnewlines",
		"name$shell",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateSecretName(name); err == nil {
				t.Fatalf("validateSecretName(%q) accepted invalid name", name)
			}
		})
	}
	for _, valid := range []string{"db", "DB_KEY", "db-password", "v1.0.0"} {
		if err := validateSecretName(valid); err != nil {
			t.Fatalf("validateSecretName(%q) rejected: %v", valid, err)
		}
	}
}

func TestSecretDeleteIsIdempotent(t *testing.T) {
	p := setupSecretsFixture(t)
	// Delete on missing must not error.
	if err := p.SecretDelete(context.Background(), "db-password"); err != nil {
		t.Fatalf("SecretDelete(missing) error = %v", err)
	}
	if _, err := p.SecretSet(context.Background(), "db-password", "v"); err != nil {
		t.Fatalf("SecretSet error = %v", err)
	}
	if err := p.SecretDelete(context.Background(), "db-password"); err != nil {
		t.Fatalf("SecretDelete(present) error = %v", err)
	}
	val, _ := p.SecretValue(context.Background(), "db-password")
	if val.Value != "" {
		t.Fatalf("value = %q after delete, want empty", val.Value)
	}
}

func TestSecretSetPersistsToEnvFile(t *testing.T) {
	p := setupSecretsFixture(t)
	if _, err := p.SecretSet(context.Background(), "db-password", "hunter2"); err != nil {
		t.Fatalf("SecretSet error = %v", err)
	}
	// Env-file default path is `.env` under the stack root.
	data, err := os.ReadFile(filepath.Join(p.root, ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !contains(string(data), "ANGEE_SECRET_DB_PASSWORD") {
		t.Fatalf(".env = %q, want ANGEE_SECRET_DB_PASSWORD key", data)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
