package vcs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CanonicalPath resolves the links of the part of a path that exists and
// keeps the rest, so a missing slot compares equal however it is spelled;
// PathInside sees through a link that leads out of the root.
func TestCanonicalPath(t *testing.T) {
	base := t.TempDir()
	real, outside := filepath.Join(base, "real"), filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(real, "a"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	if err := os.Symlink(real, filepath.Join(base, "link")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(real, "away")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	for in, want := range map[string]string{
		filepath.Join(base, "link", "a"):                 filepath.Join(resolvedReal, "a"),
		filepath.Join(base, "link", "missing", "deeper"): filepath.Join(resolvedReal, "missing", "deeper"),
		filepath.Join(real, "a", "..", "a", "gone"):      filepath.Join(resolvedReal, "a", "gone"),
	} {
		if got := CanonicalPath(in); got != want {
			t.Errorf("CanonicalPath(%s) = %s, want %s", in, got, want)
		}
	}
	for path, want := range map[string]bool{
		real:                                  true,
		filepath.Join(base, "link", "a", "x"): true,
		filepath.Join(real, "away", "x"):      false,
		outside:                               false,
		filepath.Join(base, "realish"):        false,
	} {
		if got := PathInside(real, path); got != want {
			t.Errorf("PathInside(%s, %s) = %v, want %v", real, path, got, want)
		}
	}
}

// NamePaths names up to its limit and counts the rest, quoting a name that
// does not print or that holds the separator, and nothing else.
func TestNamePaths(t *testing.T) {
	many := make([]string, 12)
	for i := range many {
		many[i] = "p" + strconv.Itoa(i)
	}
	for _, tc := range []struct {
		paths []string
		limit int
		want  string
	}{
		{paths: nil, limit: 10, want: ""},
		{paths: []string{"a.txt", "dir/", "ünï.txt"}, limit: 10, want: "a.txt, dir/, ünï.txt"},
		{paths: many, limit: 10, want: strings.Join(many[:10], ", ") + " and 2 more"},
		{paths: []string{"a", "b"}, limit: 0, want: " and 2 more"},
		{paths: []string{"nl\nname", "esc\x1b[31m", "a, b", "a,b"}, limit: 10, want: `"nl\nname", "esc\x1b[31m", "a, b", a,b`},
	} {
		if got := NamePaths(tc.paths, tc.limit); got != tc.want {
			t.Errorf("NamePaths(%q, %d) = %q, want %q", tc.paths, tc.limit, got, tc.want)
		}
	}
}
