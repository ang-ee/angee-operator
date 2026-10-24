package gitdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

func TestBackupPlanOwnsLinkedMetadataAndDeclaredBoundary(t *testing.T) {
	root, _ := clone(t)
	slot := filepath.Join(t.TempDir(), "slot")
	run(t, root, "worktree", "add", "--detach", slot, "HEAD")
	driver := New(git.New())
	plan, err := driver.BackupPlan(context.Background(), vcs.Slot{Path: slot, Primary: root}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Components) != 2 || plan.Components[0].Name != "git" || plan.Components[1].Name != "git-worktree" || plan.Evidence.GitRevision == "" {
		t.Fatalf("plan=%+v", plan)
	}
	_, err = driver.BackupPlan(context.Background(), vcs.Slot{Path: slot, Primary: t.TempDir()}, false)
	var refused *vcs.BackupCapabilityError
	if !errors.As(err, &refused) {
		t.Fatalf("external metadata accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "objects", "info", "alternates"), []byte("/outside/objects\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = driver.BackupPlan(context.Background(), vcs.Slot{Path: slot, Primary: root}, false)
	if !errors.As(err, &refused) {
		t.Fatalf("external alternates accepted: %v", err)
	}
}
