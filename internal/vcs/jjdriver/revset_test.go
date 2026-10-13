package jjdriver

import (
	"context"
	"slices"
	"testing"
)

// hostileNames are names a manifest or a request may carry that mean
// something in a revset unquoted: quotes, backslashes, @, .., spaces, an
// injected expression, and a control character.
var hostileNames = []string{
	`a"b`,
	`back\slash`,
	`at@sign`,
	`dots..range`,
	`with space`,
	`x" | all() | "`,
	`\"`,
	"tab\there",
	"ünïcode-é",
}

func TestQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"main", `"main"`},
		{"", `""`},
		{`a"b`, `"a\"b"`},
		{`back\slash`, `"back\\slash"`},
		{`\"`, `"\\\""`},
		{`x" | all() | "`, `"x\" | all() | \""`},
		{"main..@", `"main..@"`},
		{"with space", `"with space"`},
		{"tab\tcr\rlf\nnul\x00esc\x1b", `"tab\tcr\rlf\nnul\0esc\e"`},
		{"\x01\x1f\x7f", `"\x01\x1f\x7f"`},
		{"ünïcode-é", `"ünïcode-é"`},
	} {
		if got := quote(tc.in); got != tc.want {
			t.Errorf("quote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestRevsetConstructors(t *testing.T) {
	for _, tc := range []struct {
		got  revset
		want string
	}{
		{symbol(`a"b`), `"a\"b"`},
		{localBookmark(`a"b`), `bookmarks(exact:"a\"b")`},
		{remoteBookmark(`main..@`, `o"rigin`), `remote_bookmarks(exact:"main..@", remote=exact:"o\"rigin")`},
		{workspaceCommit(`st"ack--ws`), `"st\"ack--ws"@`},
		{workingCopy.parents(), `(@)-`},
		{localBookmark("main").to(workingCopy.parents()), `(bookmarks(exact:"main"))..((@)-)`},
		{coalesce(localBookmark(`a"b`), remoteBookmark("main", "origin")), `coalesce(bookmarks(exact:"a\"b"), remote_bookmarks(exact:"main", remote=exact:"origin"))`},
	} {
		if tc.got.String() != tc.want {
			t.Errorf("revset = %s, want %s", tc.got, tc.want)
		}
	}
}

// TestRevsetsResolveHostileNames has jj itself parse what the constructors
// build: each hostile name, created as a bookmark and as a workspace, is
// stored under exactly that name and resolves to its own commit, and Status
// and Diff take it as a branch, a base and a ref.
func TestRevsetsResolveHostileNames(t *testing.T) {
	ctx := context.Background()
	workspaceName := `we"ird \..@ws`
	f := newFixture(t, named(workspaceName))
	if got, want := f.changeID(f.primary, workspaceCommit(workspaceName)), f.changeID(f.slot, workingCopy); got != want {
		t.Fatalf("workspace %q resolves to %s, want the slot's working copy %s", workspaceName, got, want)
	}
	op, err := driver().snapshot(ctx, f.slot)
	if err != nil {
		t.Fatalf("snapshot() error = %v", err)
	}
	wc, err := driver().workingCopy(ctx, f.slot, op)
	if err != nil {
		t.Fatalf("workingCopy() error = %v", err)
	}
	if !slices.Equal(wc.WorkingCopies, []string{workspaceName}) {
		t.Fatalf("working copy is %q's, want %q's", wc.WorkingCopies, workspaceName)
	}

	f.commit(f.slot, "f.txt", "one\nslot\n", "slot change")
	slotCommit := f.shortID(f.slot, workingCopy.parents())
	for _, name := range hostileNames {
		// jj parses a bookmark name argument as a revset symbol too.
		f.jj(f.slot, "--ignore-working-copy", "bookmark", "create", quote(name), "-r", "@-")
	}
	// The name keyword prints a name quoted as a revset symbol; json() prints
	// it as it is.
	marks, err := decodeLines[bookmark]([]byte(f.jj(f.slot, "--ignore-working-copy", "bookmark", "list", "-T", bookmarksTemplate)))
	if err != nil {
		t.Fatalf("decode bookmarks: %v", err)
	}
	var listed []string
	for _, m := range marks {
		listed = append(listed, m.Name)
	}
	for _, name := range hostileNames {
		if !slices.Contains(listed, name) {
			t.Fatalf("bookmark %q is not listed by its name; jj lists %q", name, listed)
		}
		for _, r := range []revset{symbol(name), localBookmark(name)} {
			if got := f.shortID(f.slot, r); got != slotCommit {
				t.Errorf("%s resolves to %q, want %s", r, got, slotCommit)
			}
		}
	}

	branch := hostileNames[0]
	baseName := hostileNames[len(hostileNames)-1] + "-base"
	f.jj(f.slot, "--ignore-working-copy", "bookmark", "create", quote(baseName), "-r", base.String())
	slot := f.slotAt(branch)
	slot.BaseRef = baseName
	got, err := driver().Status(ctx, slot)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got.CurrentRef != branch || got.Ahead != 1 || got.UnpushedReason != "1 commit(s) ahead of base ref "+baseName+" with no upstream" {
		t.Fatalf("Status() = %#v, want on %q, 1 ahead of the local base %q", got, branch, baseName)
	}

	files, err := driver().Diff(ctx, slot, baseName)
	if err != nil {
		t.Fatalf("Diff(%q) error = %v", baseName, err)
	}
	if len(files) != 1 || files[0].NewPath != "f.txt" {
		t.Fatalf("Diff(%q) = %#v, want f.txt", baseName, files)
	}
}
