package jjdriver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The templates the driver reads jj through. Each renders one JSON object
// per line and has a decode struct below; testdata holds what the jj in
// .github/jj-rev prints for each, so a renamed field fails a test rather than
// reading as empty. (Operations are recorded in templates_test.go instead: an
// operation's id hashes the command lines before it, which hold the test's
// temporary paths.) None uses the workspace `root` keyword: jj's record of a
// workspace's path is wrong once the workspace has moved (plan section 2).
const (
	// workspacesTemplate is for `jj workspace list`: the name and the
	// working-copy commit.
	workspacesTemplate = `json(self) ++ "\n"`

	// commitsTemplate is for `jj log --no-graph`: jj's json(self) under
	// "commit", then what Status reads that json(self) leaves out.
	commitsTemplate = `"{\"commit\":" ++ json(self)` +
		` ++ ",\"empty\":" ++ json(empty)` +
		` ++ ",\"conflict\":" ++ json(conflict)` +
		` ++ ",\"bookmarks\":" ++ json(bookmarks)` +
		` ++ ",\"working_copies\":" ++ json(working_copies.map(|w| w.name()))` +
		` ++ ",\"conflicted_files\":" ++ json(conflicted_files.map(|f| f.path()))` +
		` ++ ",\"parents_short\":" ++ json(parents.map(|p| p.commit_id().short()))` +
		` ++ "}\n"`

	// bookmarksTemplate is for `jj bookmark list`, local and remote entries
	// alike.
	bookmarksTemplate = `json(self) ++ "\n"`

	// countTemplate renders one byte per commit, for counting a revset.
	countTemplate = `"."`

	// operationTemplate is for `jj op log`: the operation's id.
	operationTemplate = `id ++ "\n"`

	// operationEntryTemplate is for `jj op log` too: jj's json(self) of an
	// operation, for statusSnapshot's check of what came after a snapshot.
	operationEntryTemplate = `json(self) ++ "\n"`
)

// gitRemote is the pseudo-remote a colocated repository lists its own git
// branches under. It is never a base's remote or an upstream's.
const gitRemote = "git"

// commit is jj's json(self) of a commit.
type commit struct {
	CommitID    string   `json:"commit_id"`
	ChangeID    string   `json:"change_id"`
	Parents     []string `json:"parents"`
	Description string   `json:"description"`
}

// commitEntry is one line of commitsTemplate.
type commitEntry struct {
	Commit   commit `json:"commit"`
	Empty    bool   `json:"empty"`
	Conflict bool   `json:"conflict"`
	// Bookmarks are the local bookmarks on the commit, and the remote ones
	// whose target differs from their local bookmark's.
	Bookmarks       []bookmark `json:"bookmarks"`
	WorkingCopies   []string   `json:"working_copies"`
	ConflictedFiles []string   `json:"conflicted_files"`
	ParentsShort    []string   `json:"parents_short"`
}

// hasLocalBookmark reports whether local bookmark name points at the commit.
func (c commitEntry) hasLocalBookmark(name string) bool {
	for _, b := range c.Bookmarks {
		if b.Remote == "" && b.Name == name {
			return true
		}
	}
	return false
}

// bookmark is jj's json(self) of a bookmark: a local one when Remote is
// empty. Target lists more than one commit when the bookmark is conflicted,
// and holds an empty entry (JSON null) for a deleted local bookmark that
// still tracks a remote one.
type bookmark struct {
	Name           string   `json:"name"`
	Remote         string   `json:"remote,omitempty"`
	Target         []string `json:"target"`
	TrackingTarget []string `json:"tracking_target,omitempty"`
}

// present reports whether the bookmark points anywhere.
func (b bookmark) present() bool {
	for _, id := range b.Target {
		if id != "" {
			return true
		}
	}
	return false
}

// operationEntry is jj's json(self) of an operation: its id, the operations
// it follows, and whether it only recorded a snapshot of a working copy.
type operationEntry struct {
	ID         string   `json:"id"`
	Parents    []string `json:"parents"`
	IsSnapshot bool     `json:"is_snapshot"`
}

// workspace is jj's json(self) of a workspace.
type workspace struct {
	Name   string `json:"name"`
	Target commit `json:"target"`
}

// decodeLines decodes one JSON value of type T per line of out.
func decodeLines[T any](out []byte) ([]T, error) {
	var values []T
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var v T
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return values, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode jj template output: %w", err)
		}
		values = append(values, v)
	}
}
