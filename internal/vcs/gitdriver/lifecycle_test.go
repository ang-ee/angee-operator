package gitdriver

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

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
