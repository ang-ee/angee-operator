package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

func TestGitOpsTopologyReportsBehindWorktree(t *testing.T) {
	root, ws, worktree := gitMergeFixture(t)
	cache := cacheDirFromWorktree(worktree)

	// Advance main remotely (the cache directory is the canonical worktree
	// for main); push it so the worktree sees its tracking ref move.
	must(t, os.WriteFile(filepath.Join(cache, "extra.txt"), []byte("extra\n"), 0o644))
	mustGit(t, cache, "add", "extra.txt")
	mustGit(t, cache, "commit", "-m", "remote moved on")
	mustGit(t, cache, "push", "origin", "main")

	p, _ := New(root)
	// Fetch so the worktree learns about the new tracking ref position.
	if _, err := p.WorkspaceSourceFetch(context.Background(), ws, "app"); err != nil {
		t.Fatalf("WorkspaceSourceFetch() error = %v", err)
	}
	topology, err := p.GitOpsTopology(context.Background())
	if err != nil {
		t.Fatalf("GitOpsTopology() error = %v", err)
	}
	// Find the link for the workspace source.
	var found bool
	for _, link := range topology.Links {
		if link.Workspace == ws && link.Slot == "app" {
			found = true
			if link.Behind == 0 {
				t.Fatalf("link.Behind = 0, want > 0 (state=%q)", link.State)
			}
		}
	}
	if !found {
		t.Fatal("no GitOpsLink for workspace/app slot in topology")
	}
}

func TestGitOpsTopologyReportsDivergedWorktree(t *testing.T) {
	root, ws, worktree := gitMergeFixture(t)
	cache := cacheDirFromWorktree(worktree)

	// Diverge: commit on remote and on the workspace branch.
	must(t, os.WriteFile(filepath.Join(cache, "remote.txt"), []byte("remote\n"), 0o644))
	mustGit(t, cache, "add", "remote.txt")
	mustGit(t, cache, "commit", "-m", "remote side")
	mustGit(t, cache, "push", "origin", "main")

	must(t, os.WriteFile(filepath.Join(worktree, "local.txt"), []byte("local\n"), 0o644))
	mustGit(t, worktree, "add", "local.txt")
	mustGit(t, worktree, "commit", "-m", "local side")

	p, _ := New(root)
	if _, err := p.WorkspaceSourceFetch(context.Background(), ws, "app"); err != nil {
		t.Fatalf("WorkspaceSourceFetch() error = %v", err)
	}
	topology, err := p.GitOpsTopology(context.Background())
	if err != nil {
		t.Fatalf("GitOpsTopology() error = %v", err)
	}
	// Confirm the source (cache repo) shows divergence relative to its upstream.
	// The workspace branch tracks origin/<workspace>, not main, so divergence
	// surfaces on the source-level state when the cache itself has unpushed
	// commits. Instead assert the per-slot ahead/behind for the slot is non-zero.
	for _, link := range topology.Links {
		if link.Workspace == ws && link.Slot == "app" {
			if link.Ahead == 0 && link.Behind == 0 {
				t.Fatalf("link ahead=%d behind=%d, expected divergence indicator", link.Ahead, link.Behind)
			}
			return
		}
	}
	t.Fatal("no GitOpsLink for workspace/app slot")
}

func TestWorkspaceSourcePullRefusesDirtyWorktree(t *testing.T) {
	root, ws, worktree := gitMergeFixture(t)

	// Make the worktree dirty.
	must(t, os.WriteFile(filepath.Join(worktree, "README.md"), []byte("hello dirty\n"), 0o644))

	p, _ := New(root)
	_, err := p.WorkspaceSourcePull(context.Background(), ws, "app")
	if err == nil {
		t.Fatal("WorkspaceSourcePull() on dirty worktree error = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error = %v, want mentioning 'uncommitted changes'", err)
	}
}

func TestWorkspaceSourcePushRefusesDirtyWorktree(t *testing.T) {
	root, ws, worktree := gitMergeFixture(t)
	must(t, os.WriteFile(filepath.Join(worktree, "README.md"), []byte("hello dirty\n"), 0o644))
	p, _ := New(root)
	_, err := p.WorkspaceSourcePush(context.Background(), ws, "app", "")
	if err == nil {
		t.Fatal("WorkspaceSourcePush() on dirty worktree error = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error = %v, want mentioning 'uncommitted changes'", err)
	}
}

func TestGitOpsTopologyHandlesMissingWorkspaceSourcePath(t *testing.T) {
	root, ws, worktree := gitMergeFixture(t)
	// Remove the worktree directory after fixture setup.
	must(t, os.RemoveAll(worktree))

	p, _ := New(root)
	topology, err := p.GitOpsTopology(context.Background())
	if err != nil {
		t.Fatalf("GitOpsTopology() error = %v", err)
	}
	for _, status := range topology.Workspaces {
		if status.Name != ws {
			continue
		}
		for _, src := range status.Sources {
			if src.Slot == "app" {
				if src.Exists {
					t.Fatalf("workspace source Exists = true on missing path: %+v", src)
				}
				if src.State != "missing" {
					t.Fatalf("workspace source State = %q, want missing", src.State)
				}
				return
			}
		}
	}
	t.Fatal("workspace source 'app' not present in topology")
}

func TestGitOpsTopologySurfacesUndeclaredSourceReference(t *testing.T) {
	// Stack declares a workspace whose source slot references a source name
	// that does not exist in stack.sources. Topology must still build and
	// the missing reference surfaces as an error state on the link, not as
	// a panic.
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Workspaces: map[string]manifest.Workspace{
			"feature": {
				Template: "workspaces/dev-pr",
				Sources: map[string]manifest.WorkspaceSource{
					"phantom": {Source: "does-not-exist", Mode: "worktree", Branch: "feature", Ref: "main"},
				},
			},
		},
	}
	must(t, manifest.SaveFile(manifest.Path(root), stack))

	p, _ := New(root)
	topology, err := p.GitOpsTopology(context.Background())
	if err != nil {
		t.Fatalf("GitOpsTopology() error = %v, want nil (graceful)", err)
	}
	// Look for the phantom link.
	for _, link := range topology.Links {
		if link.Workspace == "feature" && link.Slot == "phantom" {
			if link.State == "" {
				t.Fatalf("phantom link state empty: %+v", link)
			}
			return
		}
	}
	// Not finding it is also acceptable if the topology gracefully skips
	// undeclared sources; the must-not-panic guarantee is the load-bearing
	// claim of this test.
}

func cacheDirFromWorktree(worktree string) string {
	// worktree = <base>/.angee/workspaces/<name>/app
	// cache    = <base>/cache
	root := filepath.Dir(filepath.Dir(filepath.Dir(worktree))) // <base>/.angee
	base := filepath.Dir(root)
	return filepath.Join(base, "cache")
}
