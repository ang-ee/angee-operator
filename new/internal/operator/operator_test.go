package operator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewServerRequiresTokenForNonLoopbackBind(t *testing.T) {
	_, err := NewServer(Config{Root: t.TempDir(), Bind: "0.0.0.0", Port: 9000})
	if err == nil {
		t.Fatal("NewServer() error = nil, want token requirement")
	}
}

func TestNewServerResolvesProjectRootToControlRoot(t *testing.T) {
	projectRoot := t.TempDir()
	controlRoot := filepath.Join(projectRoot, ".angee")
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(.angee) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(controlRoot, "angee.yaml"), []byte("version: 1\nkind: stack\nname: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}

	server, err := NewServer(Config{Root: projectRoot, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.config.Root != controlRoot {
		t.Fatalf("server root = %q, want %q", server.config.Root, controlRoot)
	}
}
