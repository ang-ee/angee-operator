package jjdriver

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The lines of jj's `jj status` report that parseUntracked reads (jj 0.45).
// Before the working-copy commit's lines, the report holds statusChanges and
// a line per changed path, then statusUntracked and an untrackedPrefix line
// per untracked path, each only when there are any, or statusNoChanges when
// there are neither. A directory jj tracks nothing in is one untracked entry,
// ending in "/". Ignored paths are not listed.
const (
	statusChanges   = "Working copy changes:"
	statusNoChanges = "The working copy has no changes."
	statusUntracked = "Untracked paths:"
	untrackedPrefix = "? "
	// sectionCommits is where parseUntracked is once past the working-copy
	// commit's line ("Working copy  (@) : …"), which ends the listings.
	sectionCommits = "(@)"
)

// untracked snapshots the slot and returns the paths its working-copy commit
// leaves out, as `jj status` lists them: new files over
// snapshot.max-new-file-size, which jj refuses to snapshot, and new files
// outside snapshot.auto-track, which it leaves untracked (plan Q10). Nothing
// else in the repository holds them. jj reports them only in that
// human-readable listing, and only for the snapshot the same command takes,
// so `jj status` is the call's snapshot.
//
// It does not see everything only the slot holds: jj also leaves out, and
// does not list, a directory holding a repository of its own (a .git or .jj
// in it) and new files outside the slot's sparse patterns.
func (d Driver) untracked(ctx context.Context, dir string) ([]string, error) {
	out, err := d.jj.snapshot(ctx, dir, "status")
	if err != nil {
		return nil, err
	}
	paths, err := parseUntracked(out)
	if err != nil {
		return nil, fmt.Errorf("read jj status in %s: %w", dir, err)
	}
	return paths, nil
}

// parseUntracked reads the untracked paths of a `jj status` report, in the
// order jj lists them. The listings end at the working-copy commit's line,
// after which come only the commits and conflicts; a line inside the
// untracked listing that starts no entry is the rest of a name holding a
// line break.
//
// It fails closed, so a jj that rewords, rearranges or cuts short its report
// cannot make a slot holding files nothing else has read as clean. These are
// errors, never no untracked paths:
//   - a line it does not expect before the commit's line;
//   - one of the lines above anywhere but its place: out of order, twice,
//     inside the untracked listing, or after the commit's line;
//   - a report without them, or without the commit's line;
//   - an untracked listing with no path.
func parseUntracked(out []byte) ([]string, error) {
	var paths []string
	section := ""
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		heading := line == statusChanges || line == statusUntracked || line == statusNoChanges
		switch {
		case section == sectionCommits:
			if heading {
				return nil, fmt.Errorf("jj status printed %q after the working-copy commit's line", line)
			}
		case strings.HasPrefix(line, "Working copy ") && strings.Contains(line, "(@)"):
			switch {
			case section == "":
				return nil, fmt.Errorf("jj status printed none of %q, %q and %q", statusChanges, statusUntracked, statusNoChanges)
			case section == statusUntracked && len(paths) == 0:
				return nil, fmt.Errorf("jj status printed %q with no path after it", statusUntracked)
			}
			section = sectionCommits
		case line == statusChanges && section == "",
			line == statusNoChanges && section == "",
			line == statusUntracked && (section == "" || section == statusChanges):
			section = line
		case heading:
			return nil, fmt.Errorf("jj status printed %q where the driver does not expect it", line)
		case section == statusUntracked && strings.HasPrefix(line, untrackedPrefix):
			paths = append(paths, line[len(untrackedPrefix):])
		case section == statusUntracked && len(paths) > 0:
			paths[len(paths)-1] += "\n" + line
		case section == statusChanges, line == "":
		default:
			return nil, fmt.Errorf("jj status printed %.80q, which the driver does not read", line)
		}
	}
	switch section {
	case sectionCommits:
		return paths, nil
	case "":
		return nil, fmt.Errorf("jj status printed none of %q, %q and %q", statusChanges, statusUntracked, statusNoChanges)
	default:
		return nil, errors.New("jj status printed no working-copy commit line: its report is cut short")
	}
}
