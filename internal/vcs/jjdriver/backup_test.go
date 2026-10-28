package jjdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// TestBackupLayerPlanRemoteOnlyVerdict has jj's remote-only verdict read a
// real colocated workspace: a published one passes and names exactly jj's
// own metadata for the Git verdict to leave to it, and a dirty or described
// working-copy commit, or a commit no remote bookmark or tag holds, in this
// workspace or another of the store, is refused until it is gone.
func TestBackupLayerPlanRemoteOnlyVerdict(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	store, err := filepath.EvalSymlinks(filepath.Join(f.primary, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	slot := vcs.Slot{Path: f.slot, Primary: f.primary}
	main := strings.TrimSpace(f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", f.base().String(), "-T", `commit_id ++ "\n"`))
	published := func(name string, s vcs.Slot) vcs.BackupPlan {
		t.Helper()
		plan, err := driver().BackupLayerPlan(ctx, s, store, true)
		if err != nil {
			t.Fatalf("%s: BackupLayerPlan() error = %v", name, err)
		}
		return plan
	}
	refused := func(name string, s vcs.Slot) {
		t.Helper()
		var refusal *git.BackupPublicationError
		if _, err := driver().BackupLayerPlan(ctx, s, store, true); !errors.As(err, &refusal) {
			t.Fatalf("%s: BackupLayerPlan() error = %v, want a publication refusal", name, err)
		}
	}

	plan := published("published", slot)
	own := plan.Colocated
	if own == nil || !slices.Equal(own.Dirs, []string{filepath.Join(f.slot, ".jj")}) || !slices.Equal(own.GitRefs, []string{"refs/jj/"}) || !slices.Contains(own.Published, main) {
		t.Fatalf("Colocated = %+v, want the slot's .jj, refs/jj/ and %s published", own, main)
	}
	if len(plan.Components) != 2 || plan.Evidence.JJRevision == "" || plan.Evidence.JJOperation == "" {
		t.Fatalf("plan = %+v", plan)
	}

	f.write(f.slot, "dirty.txt", "snapshotted edit\n")
	f.jj(f.slot, "status")
	refused("dirty working copy", slot)
	if _, err := driver().BackupLayerPlan(ctx, slot, store, false); err != nil {
		t.Fatalf("bundle capture of a dirty working copy: %v", err)
	}
	f.jj(f.slot, "restore")
	published("restored working copy", slot)

	f.jj(f.slot, "describe", "-m", "a plan only this working copy holds")
	refused("described empty working copy", slot)
	f.jj(f.slot, "describe", "-m", "")
	published("undescribed working copy", slot)

	// A side commit no Git ref holds: jj alone can see it.
	f.write(f.slot, "side.txt", "jj-only work\n")
	f.jj(f.slot, "new", f.base().String())
	refused("commit beside the working copy", slot)
	refused("the same commit from another workspace", vcs.Slot{Path: f.primary, Primary: f.primary})
	f.jj(f.slot, "abandon", "-r", "visible_heads() ~ working_copies()")
	published("abandoned side commit", slot)

	f.commit(f.slot, "local.txt", "local\n", "unpushed")
	refused("committed but unpushed", slot)
}

// TestBackupLayerPlanCountsRemoteTags accepts a commit only a published tag
// holds, as Git's verdict accepts an advertised tag, and hands its commit to
// the Git verdict to find advertised.
func TestBackupLayerPlanCountsRemoteTags(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.git(f.seed, "checkout", "-q", "--detach")
	f.write(f.seed, "release.txt", "release\n")
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-q", "-m", "release off every branch")
	f.git(f.seed, "tag", "v1")
	f.git(f.seed, "push", "-q", "origin", "v1")
	f.jj(f.primary, "git", "fetch")
	tagged := strings.TrimSpace(f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", `remote_tags(exact:"v1")`, "-T", `commit_id ++ "\n"`))
	if tagged == "" {
		t.Fatal("jj git fetch brought no remote tag v1")
	}
	store, err := filepath.EvalSymlinks(filepath.Join(f.primary, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := driver().BackupLayerPlan(ctx, vcs.Slot{Path: f.slot, Primary: f.primary}, store, true)
	if err != nil {
		t.Fatalf("BackupLayerPlan() error = %v", err)
	}
	if !slices.Contains(plan.Colocated.Published, tagged) {
		t.Fatalf("Published = %v, want the tagged commit %s", plan.Colocated.Published, tagged)
	}
}

// TestBackupLayerPlanRefusesUnverifiedTopology names no metadata of jj's for
// a workspace whose backing Git store is not the one the Git plan verified,
// or whose repo pointer is a symlink rather than a pointer file.
func TestBackupLayerPlanRefusesUnverifiedTopology(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	slot := vcs.Slot{Path: f.slot, Primary: f.primary}
	var refusal *vcs.BackupCapabilityError
	if _, err := driver().BackupLayerPlan(ctx, slot, t.TempDir(), true); !errors.As(err, &refusal) {
		t.Fatalf("another Git store: BackupLayerPlan() error = %v, want a capability refusal", err)
	}
	store, err := filepath.EvalSymlinks(filepath.Join(f.primary, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(f.slot, ".jj", "repo")
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.primary, ".jj", "repo"), pointer); err != nil {
		t.Fatal(err)
	}
	if _, err := driver().BackupLayerPlan(ctx, slot, store, true); !errors.As(err, &refusal) {
		t.Fatalf("symlinked repo: BackupLayerPlan() error = %v, want a capability refusal", err)
	}
}
