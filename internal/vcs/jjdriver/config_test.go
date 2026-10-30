package jjdriver

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
)

// TestNothingReachesTheDevelopersConfig runs the probe, the integration
// helpers and the slot lifecycle from inside a jj repository, as `go test`
// runs in a jj checkout, with a HOME and XDG_CONFIG_HOME standing for the
// developer's, and finds nothing written there. jj records every repository
// it loads in the user's config directory, even for `jj --version`, so the
// probe and `jj git init` run outside any repository and the fixtures run
// under a HOME of their own. Nor does any jj the driver runs load the
// repository the test runs in.
func TestNothingReachesTheDevelopersConfig(t *testing.T) {
	isolate(t)
	requireJJ(t)
	// A repository gets a config id once something writes its config, as a
	// clone's `jj git init --colocate` does; from then on any jj run inside
	// it records it under the user's config directory.
	repo := t.TempDir()
	runTool(t, repo, nil, "jj", "--no-pager", "git", "init")
	runTool(t, repo, nil, "jj", "--no-pager", "config", "set", "--repo", "user.name", "repo-user")
	if _, err := os.Stat(filepath.Join(repo, ".jj", "repo", "config-id")); err != nil {
		t.Fatalf("the repository has no config id: %v", err)
	}

	developer := t.TempDir()
	t.Setenv("HOME", developer)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(developer, ".config"))
	t.Chdir(repo)

	caps, complete := probe(context.Background(), runner{}, git.New(), runtime.GOOS)
	if !complete || caps.JJErr != nil {
		t.Fatalf("probe() = %#v, complete %v; want jj found", caps, complete)
	}
	f := newFixture(t)
	f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
	if _, err := driver().Status(context.Background(), f.slotAt("")); err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if _, err := driver().Diff(context.Background(), f.slotAt(""), ""); err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	lifecycle := newFixture(t, primaryOnly)
	ctx := context.Background()
	if err := driver().EnsurePrimary(ctx, lifecycle.primary); err != nil {
		t.Fatalf("EnsurePrimary() error = %v", err)
	}
	s := lifecycle.slotSpec("workspaces/ws/src", "stack--ws", "")
	if err := lifecycle.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if ok, err := driver().Exists(ctx, s); !ok || err != nil {
		t.Fatalf("Exists() = %v, %v; want true", ok, err)
	}
	if err := driver().Remove(ctx, s, lifecycle.dest(s)); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	id, err := os.ReadFile(filepath.Join(repo, ".jj", "repo", "config-id"))
	if err != nil {
		t.Fatalf("ReadFile(config-id) error = %v", err)
	}
	if record := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "jj", "repos", strings.TrimSpace(string(id))); exists(t, record) {
		t.Errorf("jj loaded the repository the test runs in: %s exists", record)
	}

	var written []string
	err = filepath.WalkDir(developer, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != developer {
			written = append(written, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir() error = %v", err)
	}
	if len(written) > 0 {
		t.Fatalf("the developer's HOME gained %q", written)
	}
}
