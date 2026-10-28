package gitdriver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
)

// identityEnvKeys are the variables git reads an identity, or the config files
// that hold one, from. git.IsolatedEnv leaves them out; commitIdentity passes
// the operator's on, so an identity its environment names (a container's
// compose environment, say) is honoured as sync-base honours it.
// GIT_CONFIG_COUNT and its pairs stay out: they can carry http.extraHeader
// tokens.
var identityEnvKeys = []string{
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
}

// commitIdentity is the environment a merge or rebase in dir commits in. The
// commits are the user's: the identity git resolves for dir from the
// operator's identityEnvKeys and its config (repository, global, system,
// author.* and committer.*). Only a role git reports no identity for, such as
// an operator in a container whose user never set one, commits as angee's
// placeholder; user.useConfigOnly keeps git from guessing one from the host
// name instead. A probe that fails any other way adds nothing, so a commit is
// never misattributed to angee; the operation then fails or succeeds as git
// decides.
func (d Driver) commitIdentity(ctx context.Context, dir string) []string {
	var env []string
	for _, key := range identityEnvKeys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	probe := slices.Clone(env)
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		_, _, err := d.git.RunIsolated(ctx, dir, probe, "-c", "user.useConfigOnly=true", "var", "GIT_"+role+"_IDENT")
		var exit *exec.ExitError
		// git dies with 128 for an unknown identity.
		if errors.As(err, &exit) && exit.ExitCode() == 128 && ctx.Err() == nil {
			env = append(env, "GIT_"+role+"_NAME=angee", "GIT_"+role+"_EMAIL=angee@example.invalid")
		}
	}
	return env
}

// runOp runs a git operation that may stop on conflicts (merge, rebase and
// their abort and continue, publish's push) in dir, with env on top of git's
// isolated environment. A conflict is a result: Conflicted with the paths git
// ls-files -u reports. Any other failure is the *git.CommandError carrying
// git's output.
func (d Driver) runOp(ctx context.Context, dir string, env []string, args ...string) (api.GitOpResult, error) {
	_, combined, runErr := d.git.RunIsolated(ctx, dir, env, args...)
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
	stdout, _, err := d.git.RunIsolated(ctx, dir, nil, "-c", "core.quotepath=false", "ls-files", "-u")
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
