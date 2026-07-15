package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/copierx"
)

// chainHostCopier is a minimal host template (`kind: stack`) chained by the
// stack templates below. Its rendered files are what a stack overlays.
const chainHostCopier = `_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: stack
  name: web
`

// writeChainTemplate writes a Copier template (copier.yml plus a `template/`
// subdirectory of files) rooted at dir and returns dir.
func writeChainTemplate(t *testing.T, dir, copierYML string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte(copierYML), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}
	for name, content := range files {
		path := filepath.Join(dir, "template", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	return dir
}

func readChainFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(b)
}

// A chain entry's inputs use the project-wide `${...}` grammar, the same as
// workspace chains — not a bespoke `{{ }}` syntax — so a documented chain block
// resolves against the stack's inputs instead of landing verbatim in host files.
func TestRenderStackChainForwardsInputsViaSubstitution(t *testing.T) {
	base := t.TempDir()
	writeChainTemplate(t, filepath.Join(base, "templates", "projects", "web"),
		chainHostCopier, map[string]string{"manage.py.jinja": "# host for {{ project_name }}\n"})
	stackDir := writeChainTemplate(t, filepath.Join(base, "templates", "stacks", "local"),
		`_subdirectory: template
_angee:
  kind: stack
  name: local
  chain:
    - template: "../../projects/web"
      inputs:
        project_name: "${inputs.project_name}"
`, nil)

	p, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	target := filepath.Join(base, "out")
	if err := p.renderStackChain(context.Background(), stackDir, target, copierx.Inputs{"project_name": "acme"}); err != nil {
		t.Fatalf("renderStackChain: %v", err)
	}

	got := readChainFile(t, filepath.Join(target, "manage.py"))
	if !strings.Contains(got, "acme") {
		t.Fatalf("manage.py = %q, want it to contain resolved input %q", got, "acme")
	}
	if strings.Contains(got, "${") {
		t.Fatalf("manage.py = %q, still contains an unresolved ${...} reference", got)
	}
}

// An unresolved chain input fails loudly rather than being written through as a
// literal, matching how every other substitution site behaves.
func TestRenderStackChainErrorsOnUnknownInputRef(t *testing.T) {
	base := t.TempDir()
	writeChainTemplate(t, filepath.Join(base, "templates", "projects", "web"),
		chainHostCopier, map[string]string{"manage.py.jinja": "# {{ project_name }}\n"})
	stackDir := writeChainTemplate(t, filepath.Join(base, "templates", "stacks", "local"),
		`_subdirectory: template
_angee:
  kind: stack
  name: local
  chain:
    - template: "../../projects/web"
      inputs:
        project_name: "${inputs.missing}"
`, nil)

	p, _ := New(base)
	err := p.renderStackChain(context.Background(), stackDir, filepath.Join(base, "out"), copierx.Inputs{})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v, want an error naming the unresolved input", err)
	}
}

// A `stacks/<name>` ref (the documented form used by workspace chains) resolves
// through the shared template resolver, not only relative-to-template-dir refs.
func TestRenderStackChainResolvesKindTemplateRef(t *testing.T) {
	base := t.TempDir()
	writeChainTemplate(t, filepath.Join(base, ".templates", "stacks", "host"),
		chainHostCopier, map[string]string{"host.txt.jinja": "host for {{ project_name }}\n"})
	stackDir := writeChainTemplate(t, filepath.Join(base, "mystack"),
		`_subdirectory: template
_angee:
  kind: stack
  name: mystack
  chain:
    - template: "stacks/host"
      inputs:
        project_name: "${inputs.project_name}"
`, nil)

	p, _ := New(base)
	target := filepath.Join(base, "out")
	if err := p.renderStackChain(context.Background(), stackDir, target, copierx.Inputs{"project_name": "acme"}); err != nil {
		t.Fatalf("renderStackChain: %v", err)
	}
	if got := readChainFile(t, filepath.Join(target, "host.txt")); !strings.Contains(got, "acme") {
		t.Fatalf("host.txt = %q, want resolved input", got)
	}
}

// A chain entry's `root` places the host in a sub-directory instead of overlaying
// in place, honoring the shared ChainEntry.Root field rather than ignoring it.
func TestRenderStackChainHonorsEntryRoot(t *testing.T) {
	base := t.TempDir()
	writeChainTemplate(t, filepath.Join(base, "templates", "projects", "web"),
		chainHostCopier, map[string]string{"manage.py.jinja": "# host\n"})
	stackDir := writeChainTemplate(t, filepath.Join(base, "templates", "stacks", "local"),
		`_subdirectory: template
_angee:
  kind: stack
  name: local
  chain:
    - template: "../../projects/web"
      root: host
`, nil)

	p, _ := New(base)
	target := filepath.Join(base, "out")
	if err := p.renderStackChain(context.Background(), stackDir, target, copierx.Inputs{}); err != nil {
		t.Fatalf("renderStackChain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "host", "manage.py")); err != nil {
		t.Fatalf("expected host rendered under root sub-directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "manage.py")); !os.IsNotExist(err) {
		t.Fatalf("host must not render flat into the target when root is set")
	}
}

func TestStackUpdateFromTemplateReconcilesChainWithStackLast(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	hostDir := writeChainTemplate(t, filepath.Join(base, ".templates", "projects", "web"),
		chainHostCopier, map[string]string{
			"host.txt.jinja":   "host v1\n",
			"shared.txt.jinja": "host v1\n",
		})
	stackDir := writeChainTemplate(t, filepath.Join(base, ".templates", "stacks", "local"),
		`_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: stack
  name: local
  chain:
    - template: "../../projects/web"
ANGEE_ROOT:
  type: str
  default: .angee
`, map[string]string{
			"shared.txt.jinja": "stack v1\n",
			"{{ ANGEE_ROOT }}/angee.yaml.jinja": `version: 1
kind: stack
name: demo
template:
  active: stacks/local
  answers_file: .copier-answers.yml
`,
		})

	p, _ := New(base)
	initialized, err := p.StackInit(ctx, stackDir, base, map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	if got := readChainFile(t, filepath.Join(base, "shared.txt")); got != "stack v1\n" {
		t.Fatalf("initial shared.txt = %q", got)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "template", "host.txt.jinja"), []byte("host v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(host update): %v", err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "template", "shared.txt.jinja"), []byte("host v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(host shared update): %v", err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "template", "shared.txt.jinja"), []byte("stack v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(stack shared update): %v", err)
	}

	stackPlatform, _ := New(initialized.Root)
	result, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{})
	if err != nil {
		t.Fatalf("StackUpdateFromTemplate: %v", err)
	}
	if !result.Changed {
		t.Fatalf("result = %+v, want changed", result)
	}
	if got := readChainFile(t, filepath.Join(base, "host.txt")); got != "host v2\n" {
		t.Fatalf("updated host.txt = %q", got)
	}
	if got := readChainFile(t, filepath.Join(base, "shared.txt")); got != "stack v2\n" {
		t.Fatalf("updated shared.txt = %q, want stack layer to win", got)
	}
}
