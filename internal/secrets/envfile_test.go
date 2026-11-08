package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

func TestEnvFileBackendRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	backend := NewEnvFileBackend(path)
	ctx := context.Background()

	if err := backend.Set(ctx, "postgres-password", "secret value"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	value, ok, err := backend.Get(ctx, "postgres-password")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok || value != "secret value" {
		t.Fatalf("Get() = %q, %v", value, ok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %v, want 0600", got)
	}
}

func TestResolveDeclarationsGeneratesAndImports(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	backend := NewEnvFileBackend(path)
	ctx := context.Background()

	resolved, err := ResolveDeclarations(ctx, backend, map[string]manifest.Secret{
		"generated": {Generated: true, Length: 24},
		"imported":  {Required: true, Import: "env:APP_TOKEN"},
	}, func(key string) (string, bool) {
		if key == "APP_TOKEN" {
			return "from-env", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("ResolveDeclarations() error = %v", err)
	}
	if len(resolved["generated"]) != 24 {
		t.Fatalf("generated length = %d", len(resolved["generated"]))
	}
	if resolved["imported"] != "from-env" {
		t.Fatalf("imported = %q", resolved["imported"])
	}

	again, err := ResolveDeclarations(ctx, backend, map[string]manifest.Secret{
		"generated": {Generated: true, Length: 24},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveDeclarations() second error = %v", err)
	}
	if again["generated"] != resolved["generated"] {
		t.Fatal("generated secret was not stable across resolutions")
	}
}

func TestReadDeclarationsReturnsStoredValuesAndNeverGenerates(t *testing.T) {
	backend := NewEnvFileBackend(filepath.Join(t.TempDir(), ".env"))
	ctx := t.Context()
	if err := backend.Set(ctx, "stored", "declared-value"); err != nil {
		t.Fatal(err)
	}

	resolved, err := ReadDeclarations(ctx, backend, map[string]manifest.Secret{
		"stored": {Generated: true, Length: 24},
	})
	if err != nil {
		t.Fatalf("ReadDeclarations(stored): %v", err)
	}
	if resolved["stored"] != "declared-value" {
		t.Fatalf("stored value = %q, want declared-value", resolved["stored"])
	}

	_, err = ReadDeclarations(ctx, backend, map[string]manifest.Secret{
		"missing": {Generated: true, Length: 24},
	})
	if err == nil || !strings.Contains(err.Error(), `original secret "missing" is missing`) {
		t.Fatalf("ReadDeclarations(missing) error = %v", err)
	}
	if _, ok, getErr := backend.Get(ctx, "missing"); getErr != nil || ok {
		t.Fatalf("missing secret after read = present %t, error %v; want absent", ok, getErr)
	}
}
