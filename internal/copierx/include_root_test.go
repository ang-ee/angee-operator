package copierx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// _angee.include_root must be a relative ancestor of the template, and not the
// filesystem root.
func TestTemplateIncludeRoot(t *testing.T) {
	base := t.TempDir()
	template := filepath.Join(base, "stacks", "dev")
	if err := os.MkdirAll(template, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedBase, err := filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		includeRoot string
		want        string
		wantErr     string
	}{
		{includeRoot: "", want: ""},
		{includeRoot: ".", want: ""},
		{includeRoot: "../..", want: resolvedBase},
		{includeRoot: "..", want: filepath.Join(resolvedBase, "stacks")},
		{includeRoot: "/etc", wantErr: "must be relative"},
		{includeRoot: "../other", wantErr: "not an ancestor"},
		{includeRoot: strings.Repeat("../", 64), wantErr: "filesystem root"},
	} {
		got, err := TemplateIncludeRoot(template, tc.includeRoot)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("TemplateIncludeRoot(%q) = %q, %v; want an error containing %q", tc.includeRoot, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("TemplateIncludeRoot(%q) = %q, %v; want %q", tc.includeRoot, got, err, tc.want)
		}
	}
}

// A stack template that includes its collection's shared files, laid out as
// angee-django's templates/stacks/{dev,_shared}, renders when it declares the
// include root. Include names resolve against the template root, as upstream
// Copier resolves them: from stacks/dev, ../_shared is stacks/_shared.
func TestCopyRendersIncludesUnderIncludeRoot(t *testing.T) {
	base := t.TempDir()
	template := filepath.Join(base, "stacks", "dev")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(template, "copier.yml"), "_subdirectory: template\n_templates_suffix: .jinja\n_angee:\n  kind: stack\n  name: dev\n  include_root: \"../..\"\nrestart_job:\n  type: str\n  default: deps\n")
	write(filepath.Join(template, "template", "AGENTS.md.jinja"), "{% include \"../_shared/AGENTS.md.jinja\" %}")
	write(filepath.Join(base, "stacks", "_shared", "AGENTS.md.jinja"), "shared, restart {{ restart_job }}\n")

	dest := t.TempDir()
	if err := (LocalRenderer{}).Copy(context.Background(), CopyRequest{Template: template, Dest: dest, Inputs: Inputs{}}); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "AGENTS.md"))
	if err != nil || string(got) != "shared, restart deps\n" {
		t.Fatalf("AGENTS.md = %q, %v; want the shared file rendered", got, err)
	}
}
