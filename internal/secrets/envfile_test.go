package secrets

import (
	"context"
	"os"
	"path/filepath"
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
