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
// which refuses a working copy with changes: the merge would leave them
// behind on the old one.
//
// A ref the slot already holds (`@-` or an ancestor of it) makes no merge, as
// git's merge records nothing then: the result is OK, saying so. jj records a
// conflict in the merge commit and goes on (plan D5), so the result is OK,
// and Conflicted with the merge's conflicted files, read after it. When jj
// made the merge but what comes after it fails, the result is OK with the
// error.
func (d Driver) Merge(ctx context.Context, s vcs.Slot, ref string) (api.GitOpResult, error) {
	result := api.GitOpResult{ConflictFiles: []string{}}
	other := refRevision(ref).single()
	head := workingCopy.parents().single()
	op, err := d.snapshot(ctx, s.Path)
	if err != nil {
		return result, err
	}
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
	if _, err := d.jj.snapshot(ctx, s.Path, "new"); err != nil {
		return result, fmt.Errorf("jj merged %s in %s, but starting a working copy on the merge failed, so a publish would leave the merge out until `jj new` runs there: %w", ref, s.Path, err)
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
	return result, nil
}
