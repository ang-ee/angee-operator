package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/service"
)

// `workspace repair` prints one line per slot (or the result as JSON) and
// exits non-zero while a slot cannot be materialized.
func TestWorkspaceRepairCommand(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	docsSource := filepath.Join(base, "docs-src")
	workspacePath := filepath.Join(root, "workspaces", "feature-a")
	for _, dir := range []string{docsSource, workspacePath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{"docs": {Kind: "local", Path: docsSource}},
		Workspaces: map[string]manifest.Workspace{"feature-a": {
			Template: "workspaces/dev-pr",
			Sources: map[string]manifest.WorkspaceSource{
				"docs":  {Source: "docs", Subpath: "docs"},
				"ghost": {Source: "ghost", Subpath: "ghost"},
			},
		}},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := NewRoot(&stdout, &stderr)
		cmd.SetArgs(append([]string{"--root", root}, args...))
		err := cmd.Execute()
		return stdout.String(), err
	}
	docsPath := filepath.Join(workspacePath, "docs")
	ghostPath := filepath.Join(workspacePath, "ghost")

	out, err := run("workspace", "repair", "feature-a")
	var repairErr *service.WorkspaceRepairError
	if !errors.As(err, &repairErr) {
		t.Fatalf("workspace repair error = %v, want a *WorkspaceRepairError for ghost", err)
	}
	want := "docs\tcreated\tlinked to source \"docs\"\t" + docsPath + "\n" +
		"ghost\tfailed\tsource \"ghost\" is not declared\t" + ghostPath + "\n"
	if out != want {
		t.Fatalf("workspace repair output =\n%q\nwant\n%q", out, want)
	}
	if info, err := os.Lstat(docsPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("docs slot after repair: info=%v err=%v, want a link to the local source", info, err)
	}

	out, err = run("--json", "ws", "repair", "feature-a")
	if !errors.As(err, &repairErr) {
		t.Fatalf("workspace repair --json error = %v, want a *WorkspaceRepairError for ghost", err)
	}
	var result api.WorkspaceRepairResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("Unmarshal(repair --json) error = %v: %s", err, out)
	}
	if result.OK || len(result.Slots) != 2 || result.Slots[0].Action != api.WorkspaceRepairOK || result.Slots[0].Reason != "ready" || result.Slots[1].Action != api.WorkspaceRepairFailed {
		t.Fatalf("workspace repair --json = %+v, want docs ok and ghost failed", result)
	}

	workspace := stack.Workspaces["feature-a"]
	delete(workspace.Sources, "ghost")
	stack.Workspaces["feature-a"] = workspace
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	out, err = run("workspace", "repair", "feature-a")
	if err != nil {
		t.Fatalf("workspace repair with every slot present error = %v", err)
	}
	if want := "docs\tok\tready\t" + docsPath + "\n"; out != want {
		t.Fatalf("workspace repair output = %q, want %q", out, want)
	}

	workspace.Sources = nil
	stack.Workspaces["feature-a"] = workspace
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	out, err = run("workspace", "repair", "feature-a")
	if err != nil || out != "workspace feature-a declares no source slots\n" {
		t.Fatalf("workspace repair without slots = %q, %v; want the no-slots line", out, err)
	}
}
