package jjdriver

import (
	"strconv"
	"strings"
)

// revset is a jj revset expression. Names (workspaces, bookmarks, remotes,
// refs) enter one only through the constructors in this file, which quote
// them as jj string literals, so a name is never read as revset syntax: a
// bookmark called `main..@` or `x" | all() | "` stays one name (plan D13).
// Every revset the driver runs is built here; nothing else concatenates one.
type revset string

// workingCopy is the working-copy commit of the workspace jj runs in. Slot
// commands run in the slot, so it is the slot's, and no workspace name is
// needed.
const workingCopy revset = "@"

// published is what the remotes hold as far as the repository knows: the
// targets of every remote bookmark and remote tag. Neither function selects
// jj's git pseudo-remote, which mirrors the colocated repository's own refs.
const published revset = "remote_bookmarks() | remote_tags()"

// unpublished is every visible commit that a capture from the remotes alone
// would lose: all but the ancestors of published, less the working-copy
// commits jj discards itself, empty and undescribed. A working-copy commit
// with changes or a description, in any workspace, counts.
const unpublished revset = "all() ~ ::(" + published + `) ~ (working_copies() & empty() & description(exact:""))`

// symbol is one revision named the way a user names it (a bookmark, tag,
// commit id or change id, resolved by jj's own rules), always as one quoted
// name.
func symbol(name string) revset { return revset(quote(name)) }

// localBookmark is the target of local bookmark name; nothing when there is
// no such bookmark.
func localBookmark(name string) revset {
	return revset("bookmarks(" + exact(name) + ")")
}

// remoteBookmark is the target of bookmark name on remote; nothing when there
// is no such remote bookmark.
func remoteBookmark(name, remote string) revset {
	return revset("remote_bookmarks(" + exact(name) + ", remote=" + exact(remote) + ")")
}

// workspaceCommit is the working-copy commit of workspace name, from any
// workspace of the repository ("name"@).
func workspaceCommit(name string) revset { return revset(quote(name) + "@") }

// parents is the parents of r (r-).
func (r revset) parents() revset { return "(" + r + ")-" }

// firstParent is the first parent of each commit of r.
func (r revset) firstParent() revset { return "first_parent(" + r + ")" }

// to is the commits that reach reaches and r does not (r..reach).
func (r revset) to(reach revset) revset { return "(" + r + ")..(" + reach + ")" }

func (r revset) String() string { return string(r) }

// exact is a string pattern matching s and nothing else, for revset
// functions and for the pattern arguments of jj's commands (bookmark names,
// remotes).
func exact(s string) string { return "exact:" + quote(s) }

// quote returns s as a jj string literal. jj's double-quoted literals know
// the escapes \" \\ \t \r \n \0 \e and \xHH; every other character stands
// for itself. So the double quote and the backslash are escaped, ASCII
// control characters are written as escapes, and every other byte, including
// non-ASCII UTF-8, is copied as it is.
func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		case 0:
			b.WriteString(`\0`)
		case 0x1b:
			b.WriteString(`\e`)
		default:
			if c < 0x20 || c == 0x7f {
				b.WriteString(`\x`)
				if c < 0x10 {
					b.WriteByte('0')
				}
				b.WriteString(strconv.FormatUint(uint64(c), 16))
				continue
			}
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
