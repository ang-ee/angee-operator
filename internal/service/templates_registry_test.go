package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeRegistryRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "registry")
	templateDir := filepath.Join(repo, "templates", "stacks", "dev", "template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(registry template): %v", err)
	}
	copierYAML := "_subdirectory: template\n_templates_suffix: .jinja\n_angee:\n  kind: stack\n  name: dev\n"
	if err := os.WriteFile(filepath.Join(repo, "templates", "stacks", "dev", "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "angee.yaml.jinja"), []byte("version: 1\nkind: stack\nname: registry-dev\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml.jinja): %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "registry"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

func TestResolveTemplateFallsBackToTheRegistry(t *testing.T) {
	// A bare name that no local candidate answers resolves from the template
	// registry, and the recorded active ref is the kind-qualified NAME so the
	// stack keeps resolving locally first afterwards.
	registry := writeRegistryRepo(t)
	t.Setenv(templateRegistryEnv, registry)
	// The registry cache is content-addressed on repo+pin; isolate it per test.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	platform, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	path, activeRef, err := platform.resolveTemplate(context.Background(), "dev", "stack")
	if err != nil {
		t.Fatalf("resolveTemplate(dev): %v", err)
	}
	if activeRef != "stacks/dev" {
		t.Fatalf("activeRef = %q, want stacks/dev", activeRef)
	}
	if _, err := os.Stat(filepath.Join(path, "copier.yml")); err != nil {
		t.Fatalf("resolved template has no copier.yml: %v", err)
	}
	if !strings.Contains(path, "angee") {
		t.Fatalf("resolved path %q does not look like the shared template cache", path)
	}

	// A pinned name resolves the same template at the pinned ref.
	pinned, pinnedRef, err := platform.resolveTemplate(context.Background(), "dev@main", "stack")
	if err != nil {
		t.Fatalf("resolveTemplate(dev@main): %v", err)
	}
	if pinnedRef != "stacks/dev" {
		t.Fatalf("pinned activeRef = %q, want stacks/dev", pinnedRef)
	}
	if _, err := os.Stat(filepath.Join(pinned, "copier.yml")); err != nil {
		t.Fatalf("pinned template has no copier.yml: %v", err)
	}
}

func TestSplitTemplateRefPin(t *testing.T) {
	cases := []struct{ ref, base, pin string }{
		{"dev", "dev", ""},
		{"dev@v1.2", "dev", "v1.2"},
		{"stacks/dev@main", "stacks/dev", "main"},
		{"acme/tpl//templates/stacks/dev@main", "acme/tpl//templates/stacks/dev", "main"},
		{"@scope/pkg", "@scope/pkg", ""},
		{"dev@feature/branch", "dev@feature/branch", ""},
	}
	for _, c := range cases {
		base, pin := splitTemplateRefPin(c.ref)
		if base != c.base || pin != c.pin {
			t.Fatalf("splitTemplateRefPin(%q) = (%q, %q), want (%q, %q)", c.ref, base, pin, c.base, c.pin)
		}
	}
}

func TestSplitOwnerRepoTemplateRef(t *testing.T) {
	owner, repo, subpath, ok := splitOwnerRepoTemplateRef("acme/tpl//templates/stacks/dev")
	if !ok || owner != "acme" || repo != "tpl" || subpath != "templates/stacks/dev" {
		t.Fatalf("splitOwnerRepoTemplateRef = (%q,%q,%q,%v)", owner, repo, subpath, ok)
	}
	for _, ref := range []string{"stacks/dev", "dev", "acme/tpl", "//x", "a/b/c//"} {
		if _, _, _, ok := splitOwnerRepoTemplateRef(ref); ok {
			t.Fatalf("splitOwnerRepoTemplateRef(%q) unexpectedly matched", ref)
		}
	}
}
