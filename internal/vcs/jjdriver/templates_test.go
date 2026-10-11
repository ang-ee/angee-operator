package jjdriver

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate testdata from the installed jj")

// The recorded output of each template, regenerated with -update from the
// installed jj (CI pins it in .github/jj-rev). The scenario is the conflict
// probe: a slot that committed and published `feature`, then was rebased onto
// a base that conflicts with it.
const (
	commitsFixture    = "testdata/commits.ndjson"
	bookmarksFixture  = "testdata/bookmarks.ndjson"
	workspacesFixture = "testdata/workspaces.ndjson"
)

var (
	commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	changeIDPattern = regexp.MustCompile(`^[k-z]{32}$`)
)

// TestTemplateFixtures decodes what jj printed for each template through the
// driver's decode structs. It needs no jj, and fails when a template or a
// struct drifts from what jj prints: after a jj bump, run it with -update and
// read the diff.
func TestTemplateFixtures(t *testing.T) {
	if *update {
		regenerateFixtures(t)
	}

	commits := decodeFixture[commitEntry](t, commitsFixture)
	if len(commits) != 2 {
		t.Fatalf("%s holds %d commits, want the working copy and the slot's commit", commitsFixture, len(commits))
	}
	wc, own := commits[0], commits[1]
	for _, c := range commits {
		if !commitIDPattern.MatchString(c.Commit.CommitID) || !changeIDPattern.MatchString(c.Commit.ChangeID) || len(c.Commit.Parents) != 1 {
			t.Fatalf("commit %#v lacks jj's ids or its parent", c.Commit)
		}
		if len(c.ParentsShort) != 1 || len(c.ParentsShort[0]) != 12 || !strings.HasPrefix(c.Commit.Parents[0], c.ParentsShort[0]) {
			t.Fatalf("parents_short %q is not the short id of parents %q", c.ParentsShort, c.Commit.Parents)
		}
		if !c.Conflict || !slices.Equal(c.ConflictedFiles, []string{"f.txt"}) {
			t.Fatalf("commit %q: conflict %v, files %q; want conflicted in f.txt", c.Commit.Description, c.Conflict, c.ConflictedFiles)
		}
	}
	if !wc.Empty || !slices.Equal(wc.WorkingCopies, []string{"stack--ws"}) || wc.Commit.Parents[0] != own.Commit.CommitID {
		t.Fatalf("working copy %#v: want empty, stack--ws's, on the slot's commit", wc)
	}
	if own.Empty || own.Commit.Description != "slot change\n" || len(own.WorkingCopies) != 0 || !own.hasLocalBookmark("feature") {
		t.Fatalf("slot commit %#v: want non-empty, described, with local bookmark feature", own)
	}

	marks := decodeFixture[bookmark](t, bookmarksFixture)
	find := func(name, remote string) bookmark {
		t.Helper()
		for _, m := range marks {
			if m.Name == name && m.Remote == remote {
				return m
			}
		}
		t.Fatalf("%s has no bookmark %s@%q", bookmarksFixture, name, remote)
		return bookmark{}
	}
	if b := find("feature", ""); !b.present() || !slices.Equal(b.Target, []string{own.Commit.CommitID}) {
		t.Fatalf("local feature = %#v, want it on the slot's commit", b)
	}
	if b := find("feature", "origin"); !b.present() || b.Target[0] == own.Commit.CommitID || len(b.TrackingTarget) != 1 {
		t.Fatalf("feature@origin = %#v, want the tracked, published commit from before the rebase", b)
	}
	if b := find("main", "origin"); !slices.Equal(b.Target, []string{own.Commit.Parents[0]}) {
		t.Fatalf("main@origin = %#v, want the moved base the slot was rebased onto", b)
	}
	find("main", gitRemote)

	workspaces := decodeFixture[workspace](t, workspacesFixture)
	var names []string
	for _, w := range workspaces {
		if !commitIDPattern.MatchString(w.Target.CommitID) || !changeIDPattern.MatchString(w.Target.ChangeID) {
			t.Fatalf("workspace %#v lacks its working-copy commit", w)
		}
		names = append(names, w.Name)
	}
	if !slices.Equal(names, []string{"default", "stack--ws"}) {
		t.Fatalf("workspaces %q, want default and stack--ws", names)
	}
	if workspaces[1].Target.CommitID != wc.Commit.CommitID {
		t.Fatalf("stack--ws's working copy %s, want %s", workspaces[1].Target.CommitID, wc.Commit.CommitID)
	}
}

func decodeFixture[T any](t *testing.T, path string) []T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	values, err := decodeLines[T](data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return values
}

// regenerateFixtures records each template from the installed jj. The ids
// repeat from run to run (deterministic), so regenerating with the same jj
// changes nothing, and a jj that prints a field differently shows up as a
// diff.
func regenerateFixtures(t *testing.T) {
	t.Helper()
	isolate(t)
	if err := Probe(t.Context()).Err(); err != nil {
		t.Fatalf("-update needs a jj that can create slots: %v", err)
	}
	f := newFixture(t, deterministic)
	f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
	f.jj(f.slot, "bookmark", "create", "feature", "-r", "@-")
	f.jj(f.slot, "git", "push", "--bookmark", "feature")
	f.advanceBase("f.txt", "one\nupstream\n")
	f.jj(f.slot, "rebase", "-b", "@", "--onto", base.String())

	record := func(path, out string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", path, err)
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	record(commitsFixture, f.jj(f.slot, "--ignore-working-copy", "log", "--no-graph", "-r", base.to(workingCopy).String(), "-T", commitsTemplate))
	record(bookmarksFixture, f.jj(f.slot, "--ignore-working-copy", "bookmark", "list", "--all-remotes", "-T", bookmarksTemplate))
	record(workspacesFixture, f.jj(f.slot, "--ignore-working-copy", "workspace", "list", "-T", workspacesTemplate))
}
