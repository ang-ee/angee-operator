package jjdriver

import (
	"context"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

// TestDiff returns the slot's uncommitted changes, recorded by jj or not,
// and with a ref the committed range from the slot to it, leaving the
// uncommitted changes out, as the git driver does.
func TestDiff(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
	f.write(f.slot, "g.txt", "unrecorded\n")

	uncommitted, err := driver().Diff(ctx, f.slotAt(""), "")
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if len(uncommitted) != 1 || uncommitted[0].NewPath != "g.txt" || !uncommitted[0].IsNew {
		t.Fatalf("Diff() = %#v, want the new g.txt only", uncommitted)
	}
	if got, want := uncommitted[0].Hunks, []api.DiffHunk{{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 1, Body: "+unrecorded\n"}}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Diff() hunks = %#v, want %#v", got, want)
	}

	committed, err := driver().Diff(ctx, f.slotAt(""), "main")
	if err != nil {
		t.Fatalf("Diff(main) error = %v", err)
	}
	if len(committed) != 1 || committed[0].OldPath != "f.txt" || committed[0].NewPath != "f.txt" {
		t.Fatalf("Diff(main) = %#v, want f.txt only", committed)
	}
	if got := committed[0].Hunks; len(got) != 1 || got[0].Body != " one\n-slot\n" {
		t.Fatalf("Diff(main) hunks = %#v, want the slot's line taken out", got)
	}
}

// TestDiffOfAMergeWorkingCopy runs the committed range from the first parent
// of a working-copy commit with two (`jj new A B`).
func TestDiffOfAMergeWorkingCopy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.commit(f.slot, "g.txt", "slot\n", "slot change")
	f.advanceBase("h.txt", "upstream\n")
	f.jj(f.slot, "new", "@-", base.String(), "-m", "merge the base")

	files, err := driver().Diff(ctx, f.slotAt(""), "main")
	if err != nil {
		t.Fatalf("Diff(main) error = %v", err)
	}
	if len(files) != 1 || files[0].OldPath != "g.txt" || !files[0].IsDeleted {
		t.Fatalf("Diff(main) = %#v, want the slot's g.txt taken out, from the first parent", files)
	}
	uncommitted, err := driver().Diff(ctx, f.slotAt(""), "")
	if err != nil || len(uncommitted) != 0 {
		t.Fatalf("Diff() of a clean merge = %#v, %v; want nothing", uncommitted, err)
	}
}
