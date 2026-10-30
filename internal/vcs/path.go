package vcs

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// CanonicalPath returns path, absolute and clean, with the symbolic links of
// its longest existing prefix resolved, so two spellings of one place compare
// equal even when its last components do not exist (yet, or any more), as
// for a slot being made or one whose directory was deleted.
func CanonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	missing := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, missing)
		}
		if filepath.Dir(dir) == dir {
			return abs
		}
		missing = filepath.Join(filepath.Base(dir), missing)
	}
}

// PathInside reports whether path is root or lies below it, once both are
// canonical (CanonicalPath): a path that reaches outside root through a
// symbolic link is not inside it.
func PathInside(root, path string) bool {
	rel, err := filepath.Rel(CanonicalPath(root), CanonicalPath(path))
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && (rel == "." || !startsWithParent(rel))
}

func startsWithParent(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

// NamePaths names up to limit of paths in a message, joined with ", ", and
// counts the rest ("and 2 more"). jj and git print a file's name as it is,
// escape sequences included, and a message reaches terminals and API
// clients, so a name holding a character that does not print is quoted
// (strconv.Quote), and so is one holding ", ", which would read as two.
func NamePaths(paths []string, limit int) string {
	shown := paths[:min(len(paths), max(limit, 0))]
	names := make([]string, len(shown))
	for i, path := range shown {
		if strings.Contains(path, ", ") || strings.ContainsFunc(path, func(r rune) bool { return !unicode.IsPrint(r) }) {
			path = strconv.Quote(path)
		}
		names[i] = path
	}
	named := strings.Join(names, ", ")
	if more := len(paths) - len(shown); more > 0 {
		named += fmt.Sprintf(" and %d more", more)
	}
	return named
}
