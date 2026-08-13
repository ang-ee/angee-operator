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

// ParseMetadata is the entry point for callers that already hold copier.yml
// bytes from a guarded read, so it must agree with ReadMetadata on well-formed
// input and fail loudly rather than silently on malformed input.
func TestParseMetadata(t *testing.T) {
	t.Run("reads angee metadata", func(t *testing.T) {
		metadata, err := ParseMetadata([]byte("_subdirectory: template\n_angee:\n  kind: stack\n  name: dev\n  include_root: \"../..\"\n"))
		if err != nil {
			t.Fatalf("ParseMetadata: %v", err)
		}
		if metadata.Kind != "stack" || metadata.Name != "dev" {
			t.Fatalf("metadata kind/name = %q/%q, want stack/dev", metadata.Kind, metadata.Name)
		}
		if metadata.IncludeRoot != "../.." {
			t.Fatalf("IncludeRoot = %q, want ../..", metadata.IncludeRoot)
		}
	})
	t.Run("absent angee block is zero", func(t *testing.T) {
		metadata, err := ParseMetadata([]byte("_subdirectory: template\n"))
		if err != nil {
			t.Fatalf("ParseMetadata: %v", err)
		}
		if metadata.Kind != "" || metadata.IncludeRoot != "" {
			t.Fatalf("metadata = %+v, want the zero value", metadata)
		}
	})
	t.Run("empty input is zero", func(t *testing.T) {
		if _, err := ParseMetadata(nil); err != nil {
			t.Fatalf("ParseMetadata(nil): %v", err)
		}
	})
	t.Run("malformed yaml errors", func(t *testing.T) {
		if _, err := ParseMetadata([]byte("_angee:\n  kind: [unterminated\n")); err == nil {
			t.Fatal("ParseMetadata accepted malformed YAML")
		}
	})
	t.Run("agrees with ReadMetadata", func(t *testing.T) {
		body := "_subdirectory: template\n_angee:\n  kind: stack\n  include_root: \"..\"\n"
		templatePath := writeTemplate(t, filepath.Join(t.TempDir(), "tpl"), body)
		fromPath, err := ReadMetadata(templatePath)
		if err != nil {
			t.Fatalf("ReadMetadata: %v", err)
		}
		fromBytes, err := ParseMetadata([]byte(body))
		if err != nil {
			t.Fatalf("ParseMetadata: %v", err)
		}
		if fromPath.Kind != fromBytes.Kind || fromPath.IncludeRoot != fromBytes.IncludeRoot {
			t.Fatalf("ReadMetadata = %+v, ParseMetadata = %+v", fromPath, fromBytes)
		}
	})
}
