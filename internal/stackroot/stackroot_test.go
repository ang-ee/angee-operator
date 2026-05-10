package stackroot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFindsStackRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "app", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(nested) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "angee.yaml"), []byte("version: 1\nkind: stack\nname: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}

	got, err := Resolve(nested)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != root {
		t.Fatalf("Resolve() = %q, want %q", got, root)
	}
}

func TestResolveFindsControlRoot(t *testing.T) {
	projectRoot := t.TempDir()
	controlRoot := filepath.Join(projectRoot, ".angee")
	nested := filepath.Join(projectRoot, "app", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(nested) error = %v", err)
	}
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(.angee) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(controlRoot, "angee.yaml"), []byte("version: 1\nkind: stack\nname: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}

	got, err := Resolve(nested)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != controlRoot {
		t.Fatalf("Resolve() = %q, want %q", got, controlRoot)
	}
}

func TestResolveFindsTemplateOnlyRoot(t *testing.T) {
	projectRoot := t.TempDir()
	nested := filepath.Join(projectRoot, "app", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(nested) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "templates", "workspaces"), 0o755); err != nil {
		t.Fatalf("MkdirAll(templates/workspaces) error = %v", err)
	}

	got, err := Resolve(nested)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := filepath.Join(projectRoot, ".angee")
	if got != want {
		t.Fatalf("Resolve() = %q, want %q", got, want)
	}
}

func TestResolveStopsAtFilesystemRoot(t *testing.T) {
	start := t.TempDir()
	got, err := Resolve(start)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != start {
		t.Fatalf("Resolve() = %q, want original start %q", got, start)
	}
}
