package jjdriver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// Merge records a merge of the ref into the commit under the working copy
// and starts a fresh working copy on it, so the merge is `@-`, described
// "Merge <ref>", with the slot's commit and the ref as its parents. A ref the
// slot already holds makes no merge.
func TestMerge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	own := f.commitID(f.slot, workingCopy.parents())
	f.advanceBase("h.txt", "upstream\n")
	moved := f.commitID(f.slot, base)

	result, err := d.Merge(ctx, s, "main@origin")
	if err != nil || !result.OK || result.Conflicted || len(result.ConflictFiles) != 0 {
		t.Fatalf("Merge(main@origin) = %+v, %v, want a clean merge", result, err)
	}
	merge := f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", "@-", "-T", `parents.map(|p| p.commit_id()).join(",") ++ "|" ++ description`)
	if want := own + "," + moved + "|Merge main@origin\n"; merge != want {
		t.Fatalf("@- = %q, want the merge %q", merge, want)
	}
	if got := f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", "@", "-T", `json(empty)`); got != "true" {
		t.Fatalf("the working copy after the merge is empty = %s, want a fresh one", got)
	}
	tip := f.commitID(f.slot, workingCopy.parents())
	for _, ref := range []string{"main@origin", "main"} {
		if result, err := d.Merge(ctx, s, ref); err != nil || !result.OK || !strings.Contains(result.Message, "Nothing to merge") {
			t.Fatalf("Merge(%s) of a ref the slot holds = %+v, %v, want nothing merged", ref, result, err)
		}
	}
	if got := f.commitID(f.slot, workingCopy.parents()); got != tip {
		t.Fatalf("a merge of nothing moved @- from %s to %s", tip, got)
	}
	if _, err := d.Merge(ctx, s, "origin/main"); err == nil {
		t.Fatal("Merge(origin/main) error = nil, want git's spelling to name nothing")
	}
}

// A merge that conflicts is a result: jj records the conflict in the merge
// commit, so the result is OK and Conflicted, with the files.
func TestMergeThatConflicts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
	f.advanceBase("f.txt", "one\nupstream\n")
	result, err := driver().Merge(ctx, f.slotAt(""), "main@origin")
	if err != nil || !result.OK || !result.Conflicted || !slices.Equal(result.ConflictFiles, []string{"f.txt"}) {
		t.Fatalf("Merge() = %+v, %v, want OK and conflicted in f.txt", result, err)
	}
	status, err := driver().Status(ctx, f.slotAt(""))
	if err != nil || !status.Conflicted || status.Dirty {
		t.Fatalf("Status() after the merge = %+v, %v, want the merge conflicted under a clean working copy", status, err)
	}
}

// A publish after a merge sends the merge: it is the commit under the
// working copy.
func TestMergeThenPublishPushesTheMerge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("feature")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	f.advanceBase("h.txt", "upstream\n")
	if result, err := d.Merge(ctx, s, "main@origin"); err != nil || !result.OK {
		t.Fatalf("Merge() = %+v, %v", result, err)
	}
	if err := d.Ready(ctx, s, vcs.VerbPublish); err != nil {
		t.Fatalf("Ready(publish) after the merge error = %v, want a clean working copy", err)
	}
	merge := f.commitID(f.slot, workingCopy.parents())
	f.publish(s, vcs.PublishIntent{Branch: "feature"})
	if got := f.remoteBranch(f.remote, "feature"); got != merge {
		t.Fatalf("the remote has feature at %q, want the merge %s", got, merge)
	}
}

// Merge checks the working copy again as it starts, so changes made after
// the service asked Ready are refused, with jj's command, and nothing is
// merged; it merges onto the commit it found under the working copy.
func TestMergeRefusesChangesMadeSinceReady(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	d := driver()
	s := f.slotAt("")
	f.commit(f.slot, "g.txt", "mine\n", "slot change")
	f.advanceBase("h.txt", "upstream\n")
	if err := d.Ready(ctx, s, vcs.VerbMerge); err != nil {
		t.Fatalf("Ready(merge) error = %v", err)
	}
	tip := f.commitID(f.slot, workingCopy.parents())
	f.write(f.slot, "late.txt", "after the check\n")
	_, err := d.Merge(ctx, s, "main@origin")
	var uncommitted *vcs.UncommittedError
	if !errors.As(err, &uncommitted) || !slices.Equal(uncommitted.Paths, []string{"late.txt"}) || uncommitted.Fix == "" {
		t.Fatalf("Merge() over a change made since Ready error = %v, want it refused", err)
	}
	if got := f.commitID(f.slot, workingCopy.parents()); got != tip {
		t.Fatalf("a refused merge moved @- from %s to %s", tip, got)
	}
}
