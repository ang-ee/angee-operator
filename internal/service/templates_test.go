package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseGitHubTemplateRefWithSubpath(t *testing.T) {
	repo, branch, subpath, err := parseGitHubTemplateRef("https://github.com/fyltr/django-angee/examples/angee-notes/.templates/stack/staging")
	if err != nil {
		t.Fatalf("parseGitHubTemplateRef() error = %v", err)
	}
	if repo != "https://github.com/fyltr/django-angee.git" {
		t.Fatalf("repo = %q", repo)
	}
	if branch != "" {
		t.Fatalf("branch = %q, want empty", branch)
	}
	wantSubpath := "examples/angee-notes/.templates/stack/staging"
	if subpath != wantSubpath {
		t.Fatalf("subpath = %q, want %q", subpath, wantSubpath)
	}
}

func TestParseGitHubTemplateRefWithTreeBranch(t *testing.T) {
	_, branch, subpath, err := parseGitHubTemplateRef("https://github.com/fyltr/django-angee/tree/main/examples/angee-notes/.templates/stacks/dev")
	if err != nil {
		t.Fatalf("parseGitHubTemplateRef() error = %v", err)
	}
	if branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	wantSubpath := "examples/angee-notes/.templates/stacks/dev"
	if subpath != wantSubpath {
		t.Fatalf("subpath = %q, want %q", subpath, wantSubpath)
	}
}

func TestResolveTemplateWalksUpFromRoot(t *testing.T) {
	repoRoot := t.TempDir()
	templateDir := filepath.Join(repoRoot, ".templates", "stacks", "dev")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(template) = %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "copier.yml"), []byte("_angee:\n  kind: stack\n"), 0o644); err != nil {
		t.Fatalf("write copier.yml: %v", err)
	}
	consumerRoot := filepath.Join(repoRoot, "examples", "consumer", ".angee")
	if err := os.MkdirAll(consumerRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(consumer) = %v", err)
	}

	platform, err := New(consumerRoot)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	resolved, kindRef, err := platform.resolveTemplate(context.Background(), "dev", "stack")
	if err != nil {
		t.Fatalf("resolveTemplate() = %v", err)
	}
	if resolved != templateDir {
		t.Fatalf("resolved = %q, want %q", resolved, templateDir)
	}
	if kindRef != "stacks/dev" {
		t.Fatalf("kindRef = %q, want stacks/dev", kindRef)
	}
}

func TestResolveTemplateRespectsExistingPrecedence(t *testing.T) {
	// p.root has a closer template; the walk-up must NOT shadow it.
	root := t.TempDir()
	near := filepath.Join(root, ".templates", "stacks", "dev")
	far := filepath.Join(filepath.Dir(root), ".templates", "stacks", "dev")
	for _, dir := range []string{near, far} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) = %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte("_angee:\n  kind: stack\n"), 0o644); err != nil {
			t.Fatalf("write copier.yml: %v", err)
		}
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	resolved, _, err := platform.resolveTemplate(context.Background(), "dev", "stack")
	if err != nil {
		t.Fatalf("resolveTemplate() = %v", err)
	}
	if resolved != near {
		t.Fatalf("resolved = %q, want %q (closer template should win)", resolved, near)
	}
}

func TestAncestorTemplatePathsTerminatesAtRoot(t *testing.T) {
	paths := ancestorTemplatePaths("/a/b/c", "stacks/dev")
	want := []string{
		"/a/b/.templates/stacks/dev",
		"/a/.templates/stacks/dev",
		"/.templates/stacks/dev",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v (len %d), want %v (len %d)", paths, len(paths), want, len(want))
	}
	for i, got := range paths {
		if got != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, got, want[i])
		}
	}
}
