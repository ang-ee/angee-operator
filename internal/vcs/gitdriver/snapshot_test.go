package gitdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

func TestSnapshotPlanOwnsLinkedMetadataAndDeclaredBoundary(t *testing.T) {
	root, _ := clone(t)
	slot := filepath.Join(t.TempDir(), "slot")
	run(t, root, "worktree", "add", "--detach", slot, "HEAD")
	driver := New(git.New())
	plan, err := driver.SnapshotPlan(context.Background(), vcs.Slot{Path: slot, Primary: root}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Components) != 2 || plan.Components[0].Name != "git" || plan.Components[1].Name != "git-worktree" || plan.Evidence.GitRevision == "" {
		t.Fatalf("plan=%+v", plan)
	}
	_, err = driver.SnapshotPlan(context.Background(), vcs.Slot{Path: slot, Primary: t.TempDir()}, false, nil)
	var refused *vcs.SnapshotCapabilityError
	if !errors.As(err, &refused) {
		t.Fatalf("external metadata accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "objects", "info", "alternates"), []byte("/outside/objects\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = driver.SnapshotPlan(context.Background(), vcs.Slot{Path: slot, Primary: root}, false, nil)
	if !errors.As(err, &refused) {
		t.Fatalf("external alternates accepted: %v", err)
	}
}

// fakeLayer is a colocated layer that returns plan or err without running
// anything, and records the Git store it was handed.
type fakeLayer struct {
	plan  vcs.SnapshotPlan
	err   error
	store string
}

func (l *fakeLayer) SnapshotLayerPlan(_ context.Context, _ vcs.Slot, gitStore string, _ bool) (vcs.SnapshotPlan, error) {
	l.store = gitStore
	return l.plan, l.err
}

// TestSnapshotPlanLeavesOnlyVerifiedLayerMetadataToTheLayer drives the layer
// path with a fake layer over a published clone carrying an ignored .jj: the
// layer gets the verified store and its error back as it is, its plan is
// merged after Git's, the Git verdict stays strict unless the layer names its
// metadata, and metadata anywhere but directly in the root is refused when
// the verdict would use it.
func TestSnapshotPlanLeavesOnlyVerifiedLayerMetadataToTheLayer(t *testing.T) {
	ctx := context.Background()
	root, _ := clone(t)
	slot := vcs.Slot{Path: root, Primary: root}
	driver := New(git.New())
	store, err := filepath.EvalSymlinks(filepath.Join(root, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	jj := filepath.Join(root, ".jj")
	if err := os.MkdirAll(filepath.Join(jj, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{".gitignore": "/*\n", "repo/op": "layer metadata"} {
		if err := os.WriteFile(filepath.Join(jj, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	failed := errors.New("layer failed")
	if _, err := driver.SnapshotPlan(ctx, slot, false, &fakeLayer{err: failed}); !errors.Is(err, failed) {
		t.Fatalf("layer error = %v, want it returned as it is", err)
	}

	layer := &fakeLayer{plan: vcs.SnapshotPlan{
		Components:      []vcs.SnapshotComponent{{Name: "layer", Root: filepath.Join(jj, "repo")}},
		WorktreeExclude: []string{jj},
		Evidence:        api.SnapshotSourceEvidence{JJRevision: "layer-revision"},
	}}
	var publication *git.SnapshotPublicationError
	if _, err := driver.SnapshotPlan(ctx, slot, true, layer); !errors.As(err, &publication) {
		t.Fatalf("a layer naming no metadata: err=%v, want the strict Git verdict", err)
	}
	if layer.store != store {
		t.Fatalf("layer got store %q, want the verified %q", layer.store, store)
	}

	layer.plan.Colocated = &vcs.ColocatedMetadata{Dirs: []string{jj}, GitRefs: []string{"refs/jj/"}}
	plan, err := driver.SnapshotPlan(ctx, slot, true, layer)
	if err != nil {
		t.Fatalf("a layer naming its metadata: %v", err)
	}
	names := []string{}
	for _, component := range plan.Components {
		names = append(names, component.Name)
	}
	if !slices.Equal(names, []string{"git", "layer"}) || !slices.Equal(plan.WorktreeExclude, []string{filepath.Join(root, ".git"), jj}) || plan.Evidence.JJRevision != "layer-revision" || plan.Evidence.Policy != "fetch_published" {
		t.Fatalf("plan=%+v", plan)
	}

	var capability *vcs.SnapshotCapabilityError
	for _, dir := range []string{root, filepath.Join(root, "sub", ".jj"), filepath.Join(t.TempDir(), ".jj")} {
		layer.plan.Colocated = &vcs.ColocatedMetadata{Dirs: []string{dir}}
		if _, err := driver.SnapshotPlan(ctx, slot, true, layer); !errors.As(err, &capability) {
			t.Fatalf("layer metadata at %s: err=%v, want a capability refusal", dir, err)
		}
		if _, err := driver.SnapshotPlan(ctx, slot, false, layer); err != nil {
			t.Fatalf("bundle snapshot never reads layer metadata, got %v for %s", err, dir)
		}
	}
}
