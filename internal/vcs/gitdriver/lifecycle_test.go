package gitdriver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

var errPathSwapped = errors.New("path swapped")

// recordingDest installs a slot by copying the stage to its path. At each
// identity check, which must name that path, it records whether the cache
// already lists a worktree at the path, and it fails the check numbered fail
// (counting from 1).
type recordingDest struct {
	client     git.Client
	primary    string
	path       string
	fail       int
	registered []bool
}

func (d *recordingDest) Lstat() (fs.FileInfo, bool, error) {
	info, err := os.Lstat(d.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return info, err == nil, err
}

func (d *recordingDest) ReplaceFrom(_ context.Context, stage string) error {
	return os.CopyFS(d.path, os.DirFS(stage))
}

func (d *recordingDest) VerifyPathIdentity(path string) error {
	if path != d.path {
		return fmt.Errorf("VerifyPathIdentity(%s), want the installed path %s", path, d.path)
	}
	registered, err := d.client.WorktreeRegistered(context.Background(), d.primary, d.path)
	if err != nil {
		return err
	}
	d.registered = append(d.registered, registered)
	if len(d.registered) == d.fail {
		return errPathSwapped
	}
	return nil
}

func (d *recordingDest) RemoveAll() error { return os.RemoveAll(d.path) }

// A worktree slot is installed, its path checked, the cache's registration
// pointed at the path, and the path checked again. When the check after the
// repair fails, Add fails, removes the checkout and prunes the registration.
func TestAddWorktreeChecksThePathAfterTheRepair(t *testing.T) {
	ctx := context.Background()
	primary, _ := clone(t)
	client := git.New()
	slot := vcs.Slot{Mode: vcs.ModeWorktree, Primary: primary, Path: filepath.Join(t.TempDir(), "slot"), BaseRef: "main", BaseRemote: "origin"}
	dest := &recordingDest{client: client, primary: primary, path: slot.Path, fail: 2}

	if err := New(client).Add(ctx, slot, dest, nil); !errors.Is(err, errPathSwapped) {
		t.Fatalf("Add() error = %v, want the failed check after the repair", err)
	}
	if want := []bool{false, true}; !slices.Equal(dest.registered, want) {
		t.Fatalf("cache listed the slot path at each check = %v, want %v (one check before the repair, one after)", dest.registered, want)
	}
	if _, err := os.Lstat(slot.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("slot after the failed Add: Lstat error = %v, want it removed", err)
	}
	if list := gitOutput(t, primary, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("cache worktrees after the failed Add:\n%s\nwant only the cache's own", list)
	}
}

// A worktree starts at the first ref baseRefs lists, the one Status counts a
// slot without an upstream against: the local base, else its remote
// counterpart, else the base as given, for git to resolve.
func TestStartRefIsTheFirstBaseRef(t *testing.T) {
	ctx := context.Background()
	dir, remote := clone(t)
	advanceRemote(t, dir, remote, "develop")
	client := git.New()
	for _, tc := range []struct {
		base       string
		bases      []string
		start      string
		fromRemote bool
	}{
		{base: "main", bases: []string{"main", "origin/main"}, start: "main"},
		{base: "develop", bases: []string{"origin/develop"}, start: "origin/develop", fromRemote: true},
		{base: "missing", start: "missing"},
		{base: ""},
	} {
		if got := baseRefs(ctx, client, dir, tc.base, "origin"); !slices.Equal(got, tc.bases) {
			t.Errorf("baseRefs(%q) = %q, want %q", tc.base, got, tc.bases)
		}
		start, fromRemote := startRef(ctx, client, dir, tc.base, "origin")
		if start != tc.start || fromRemote != tc.fromRemote {
			t.Errorf("startRef(%q) = %q, %v, want %q, %v", tc.base, start, fromRemote, tc.start, tc.fromRemote)
		}
	}
}

// A slot mode the git driver does not know is refused before anything is
// made, and a clone, which no cache lists, never exists as a registration.
func TestLifecycleModes(t *testing.T) {
	ctx := context.Background()
	driver := New(git.New())
	if err := driver.Add(ctx, vcs.Slot{Mode: "jj"}, nil, nil); !errors.Is(err, vcs.ErrUnsupported) {
		t.Fatalf("Add(mode jj) error = %v, want vcs.ErrUnsupported", err)
	}
	if exists, err := driver.Exists(ctx, vcs.Slot{Mode: vcs.ModeClone, Path: t.TempDir()}); exists || err != nil {
		t.Fatalf("Exists(clone) = %v, %v, want false", exists, err)
	}
}
