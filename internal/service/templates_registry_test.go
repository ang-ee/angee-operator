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

// When no local template answers, the error says why the registry did not
// either: the template is absent, git is missing, or the registry could not
// be fetched. Credentials in a registry override never reach the message.
func TestResolveTemplateExplainsRegistryFailures(t *testing.T) {
	registry := writeRegistryRepo(t)
	for _, tc := range []struct {
		name     string
		registry string
		ref      string
		noGit    bool
		want     string
		mustNot  string
	}{
		{name: "template absent", registry: registry, ref: "nope", want: `template "nope" was not found locally or in the template registry ` + registry},
		{name: "git missing", registry: registry, ref: "dev", noGit: true, want: `template "dev" was not found locally, and fetching it from the template registry needs git, which is not installed or not on PATH`},
		{name: "registry unreachable", registry: filepath.Join(t.TempDir(), "missing"), ref: "dev", want: `template "dev" was not found locally, and the template registry could not be fetched: `},
		{name: "credentials redacted", registry: "https://angee:hunter22@127.0.0.1:1/registry.git", ref: "dev", want: "could not be fetched", mustNot: "hunter22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(templateRegistryEnv, tc.registry)
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			if tc.noGit {
				t.Setenv("PATH", t.TempDir())
			}
			platform, err := New(t.TempDir())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, _, err = platform.resolveTemplate(context.Background(), tc.ref, "stack")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolveTemplate(%s) error = %v, want it to contain %q", tc.ref, err, tc.want)
			}
			if tc.mustNot != "" && strings.Contains(err.Error(), tc.mustNot) {
				t.Fatalf("resolveTemplate(%s) error = %v, want %q redacted", tc.ref, err, tc.mustNot)
			}
		})
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
	// An org-wide `.github` repository is a common home for templates.
	if _, repo, _, ok := splitOwnerRepoTemplateRef("acme/.github//templates/stacks/dev"); !ok || repo != ".github" {
		t.Fatalf("splitOwnerRepoTemplateRef(acme/.github//...) = (%q, %v), want (.github, true)", repo, ok)
	}
	for _, ref := range []string{"stacks/dev", "dev", "acme/tpl", "//x", "a/b/c//", "gh:acme/tpl//templates/stacks/dev", "../tpl//x", "acme/..//x"} {
		if _, _, _, ok := splitOwnerRepoTemplateRef(ref); ok {
			t.Fatalf("splitOwnerRepoTemplateRef(%q) unexpectedly matched", ref)
		}
	}
}

// A ref of the wrong shape or kind must say what a ref of that kind looks
// like (#54). None of these reach the network: the kind check runs before any
// registry clone.
func TestResolveTemplateKindMismatchNamesAcceptedForms(t *testing.T) {
	t.Setenv(templateRegistryEnv, t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	platform, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, ref := range []string{
		"workspaces/dev-pr",                            // another kind's name
		"workspaces/dev-pr@main",                       // pinned: checked by the registry resolver
		"./templates/stacks/dev",                       // a relative path is not a ref
		"gh:ang-ee/angee-django//templates/stacks/dev", // the guess from #54
	} {
		_, _, err := platform.resolveTemplate(context.Background(), ref, "stack")
		if err == nil {
			t.Fatalf("resolveTemplate(%q) error is nil", ref)
		}
		for _, want := range []string{
			`template "` + ref + `" is not a stack template ref`,
			"<name> or stacks/<name>",
			"<owner>/<repo>//<path>",
			"@<ref>",
			"an absolute path",
			"https://github.com/<owner>/<repo>/tree/<ref>/<path>",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("resolveTemplate(%q) error = %q, want it to contain %q", ref, err, want)
			}
		}
	}

	// Template() infers the kind from the ref's prefix and refuses absolute
	// paths, so a ref with no kind prefix lists the prefixes instead.
	_, err = platform.Template(context.Background(), "agents/claude")
	if err == nil {
		t.Fatal("Template(agents/claude) error is nil")
	}
	for _, want := range []string{`template "agents/claude" is not a template ref`, "stacks/<name>, workspaces/<name> or services/<name>", "<owner>/<repo>//<path>"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Template(agents/claude) error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("Template(agents/claude) error = %q, want no absolute-path form", err)
	}
}
