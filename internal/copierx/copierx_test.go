package copierx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplate stages a minimal copier template with the given
// copier.yml body and returns its absolute path.
func writeTemplate(t *testing.T, dir, copierYAML string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) = %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("write copier.yml: %v", err)
	}
	return dir
}

func TestResolvePathInputsRewritesRelativePathsAsAngeeRootRelative(t *testing.T) {
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_path:",
		"  type: path",
		"  default: examples/foo",
		"ANGEE_ROOT:",
		"  type: str",
		"  default: .angee",
	}, "\n"))
	dest := filepath.Join(tmp, "host")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("MkdirAll(host) = %v", err)
	}
	out, err := ResolvePathInputs(tpl, Inputs{"project_path": "examples/foo", "ANGEE_ROOT": ".angee"}, dest, ".angee")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if got := out["project_path"]; got != "../examples/foo" {
		t.Fatalf("project_path = %q, want %q", got, "../examples/foo")
	}
}

func TestResolvePathInputsKeepsAbsolutePathsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_path:",
		"  type: path",
		"  default: \"/abs/dummy\"",
	}, "\n"))
	abs := "/some/absolute/path"
	out, err := ResolvePathInputs(tpl, Inputs{"project_path": abs, "ANGEE_ROOT": ".angee"}, tmp, ".angee")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if got := out["project_path"]; got != abs {
		t.Fatalf("project_path = %q, want %q (absolute should pass through)", got, abs)
	}
}

func TestResolvePathInputsHonoursDeeperAngeeRoot(t *testing.T) {
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_path:",
		"  type: path",
		"  default: \".\"",
	}, "\n"))
	out, err := ResolvePathInputs(tpl, Inputs{"project_path": ".", "ANGEE_ROOT": "state/dev"}, tmp, "state/dev")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	// "." resolves to dest itself; relative from <dest>/state/dev is "../..".
	if got := out["project_path"]; got != "../.." {
		t.Fatalf("project_path = %q, want %q", got, "../..")
	}
}

func TestResolvePathInputsLeavesNonPathInputsAlone(t *testing.T) {
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_name:",
		"  type: str",
		"  default: foo",
		"port:",
		"  type: int",
		"  default: 8100",
	}, "\n"))
	out, err := ResolvePathInputs(tpl, Inputs{"project_name": "foo", "port": "8100", "extra": "untouched"}, tmp, ".angee")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if out["project_name"] != "foo" || out["port"] != "8100" || out["extra"] != "untouched" {
		t.Fatalf("non-path inputs were mutated: %#v", out)
	}
}

func TestResolvePathInputsHandlesAngeeInputsBlock(t *testing.T) {
	// Workspace templates conventionally declare inputs under `_angee.inputs`
	// rather than at top level. Both forms must trigger path resolution.
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: workspace",
		"  name: dev",
		"  inputs:",
		"    project_path:",
		"      type: path",
		"      default: examples/foo",
	}, "\n"))
	dest := filepath.Join(tmp, "host")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	out, err := ResolvePathInputs(tpl, Inputs{"project_path": "examples/foo"}, dest, ".angee")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if got := out["project_path"]; got != "../examples/foo" {
		t.Fatalf("project_path = %q, want %q", got, "../examples/foo")
	}
}
