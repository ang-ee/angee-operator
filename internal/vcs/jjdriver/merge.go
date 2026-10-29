package jjdriver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// Merge records a merge of ref into the slot (plan phase 8 step 5): `jj new
// @- <ref> -m "Merge <ref>"` in the slot, which makes the merge on the commit
// under the working copy, then `jj new`, which leaves the merge as `@-` under
// a fresh working copy, so a publish after it sends the merge. ref is jj's
// spelling (refRevision), exactly one commit. The service asks Ready first,
// which refuses a working copy with changes or untracked paths: the merge
// would leave them behind on the old one. Merge snapshots again and refuses
// changes made since, and merges onto the commit that snapshot found under
// the working copy, by its id.
//
// A ref the slot already holds (`@-` or an ancestor of it) makes no merge, as
// git's merge records nothing then: the result is OK, saying so. jj records a
// conflict in the merge commit and goes on (plan D5), so the result is OK,
// and Conflicted with the merge's conflicted files, read after it. When jj
// made the merge but warned (it skipped updating a file the working copy
// held, say), or the fresh working copy after it could not be started, the
// result is OK with a *vcs.Warning saying so; when what the merge left
// cannot be read, it is OK with that failure.
func (d Driver) Merge(ctx context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	result := api.GitOpResult{ConflictFiles: []string{}}
	other := refRevision(ref).single()
	op, err := d.snapshot(ctx, s.Path)
	if err != nil {
		return result, err
	}
	wc, err := d.workingCopy(ctx, s.Path, op)
	if err != nil {
		return result, err
	}
	if !wc.Empty {
		changed, err := d.changedFiles(ctx, s.Path, op)
		if err != nil {
			return result, err
		}
		return result, uncommittedChanges(s.Path, changed)
	}
	if len(wc.Commit.Parents) != 1 {
		return result, severalParents(s.Path, len(wc.Commit.Parents), "merge into")
	}
	head := commitOf(wc.Commit.Parents[0]).single()
	held, err := d.count(ctx, s.Path, op, other.and(head.ancestors()))
	if err != nil {
		return result, err
	}
	if held > 0 {
		result.OK = true
		result.Message = fmt.Sprintf("Nothing to merge: the slot already holds %s.", ref)
		return result, nil
	}
	_, stderr, err := d.jj.snapshotOutputs(ctx, s.Path, "new", head.String(), other.String(), "-m", "Merge "+ref)
	result.Message = logctx.RedactText(strings.TrimSpace(string(stderr)))
	if err != nil {
		return result, err
	}
	result.OK = true
	_, after, err := d.jj.snapshotOutputs(ctx, s.Path, "new")
	if err != nil {
		warned := logctx.RedactText(fmt.Sprintf("The merge was made, but starting a working copy on it failed, so a publish leaves it out until `jj -R %s new` runs: %v", s.Path, err))
		result.Message += "\n" + warned
		return result, &vcs.Warning{Text: warned}
	}
	if after := logctx.RedactText(strings.TrimSpace(string(after))); after != "" {
		result.Message += "\n" + after
	}
	latest, err := d.latestOperation(ctx, s.Path)
	if err != nil {
		return result, fmt.Errorf("jj merged %s in %s, but reading the conflicts it left failed: %w", ref, s.Path, err)
	}
	merge, err := d.commits(ctx, s.Path, latest.ID, workingCopy.parents())
	if err != nil {
		return result, fmt.Errorf("jj merged %s in %s, but reading the conflicts it left failed: %w", ref, s.Path, err)
	}
	for _, c := range merge {
		if c.Conflict {
			result.Conflicted = true
			result.ConflictFiles = append(result.ConflictFiles, c.ConflictedFiles...)
		}
	}
	slices.Sort(result.ConflictFiles)
	result.ConflictFiles = slices.Compact(result.ConflictFiles)
	if warned := warnings(result.Message); warned != "" {
		return result, &vcs.Warning{Text: warned}
	}
	return result, nil
}
