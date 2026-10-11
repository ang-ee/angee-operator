package gitdriver

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
)

// angeeIdentity is who the commits of runOp's operations are made by: merge
// and rebase in a slot record angee, not whoever runs the operator.
var angeeIdentity = []string{
	"GIT_AUTHOR_NAME=angee",
	"GIT_AUTHOR_EMAIL=angee@example.invalid",
	"GIT_COMMITTER_NAME=angee",
	"GIT_COMMITTER_EMAIL=angee@example.invalid",
}

// runOp runs a git operation that may stop on conflicts (merge, rebase and
// their abort and continue, publish's push) in dir as angee. A conflict is a
// result: Conflicted with the paths git ls-files -u reports. Any other
// failure is the *git.CommandError carrying git's output.
func (d Driver) runOp(ctx context.Context, dir string, args ...string) (api.GitOpResult, error) {
	_, combined, runErr := d.git.RunIsolated(ctx, dir, angeeIdentity, args...)
	result := api.GitOpResult{
		Message:       combined,
		ConflictFiles: []string{},
	}
	if runErr == nil {
		result.OK = true
		return result, nil
	}
	// git ls-files -u is safe even when no conflict is in flight (it lists
	// nothing). A listing that fails still counts for what it printed, since
	// the result reports the conflict either way; syncResult does not.
	files, _ := d.conflictedPaths(ctx, dir)
	result.ConflictFiles = files
	result.Conflicted = len(files) > 0
	if result.Conflicted {
		return result, nil
	}
	// git refused to start the operation at all, as opposed to stopping on
	// conflicts: the caller classifies and quotes git's output.
	return result, runErr
}

// conflictedPaths lists the paths git ls-files -u reports in dir, non-ASCII
// names unquoted (core.quotepath=false) so they come back as raw UTF-8.
func (d Driver) conflictedPaths(ctx context.Context, dir string) ([]string, error) {
	stdout, _, err := d.git.RunIsolated(ctx, dir, angeeIdentity, "-c", "core.quotepath=false", "ls-files", "-u")
	return parseConflictedPaths(string(stdout)), err
}

// syncResult reads what a failed merge or rebase run by git.Client left: a
// conflict is a result carrying git's output, anything else stays an error.
func (d Driver) syncResult(ctx context.Context, dir string, err error) (api.GitOpResult, error) {
	result := api.GitOpResult{ConflictFiles: []string{}}
	if err == nil {
		result.OK = true
		return result, nil
	}
	// Unlike runOp, a listing that fails is not trusted: sync-base calls it a
	// conflict only on a complete listing, else reports the sync's failure.
	files, lsErr := d.conflictedPaths(ctx, dir)
	if lsErr != nil || len(files) == 0 {
		return result, err
	}
	result.Conflicted = true
	result.ConflictFiles = files
	var command *git.CommandError
	if errors.As(err, &command) {
		result.Message = command.Output
	}
	return result, nil
}

func parseConflictedPaths(lsFilesOutput string) []string {
	if strings.TrimSpace(lsFilesOutput) == "" {
		return []string{}
	}
	seen := map[string]struct{}{}
	for line := range strings.SplitSeq(lsFilesOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// `git ls-files -u` emits `<mode> <sha> <stage>\t<path>` per stage entry.
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		seen[strings.TrimSpace(line[tab+1:])] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
