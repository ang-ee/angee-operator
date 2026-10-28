package gitdriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
)

// identityEnvKeys are the variables of the operator's environment that git
// reads an identity, or the config files that hold one, from. git.IsolatedEnv
// leaves them out; commitIdentity's probes get them, so an identity the
// operator's environment names (a container's compose environment, say) is
// honoured as sync-base honours it.
var identityEnvKeys = []string{
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
}

// identityConfigKeys are the config keys, lower-cased, that the probes take
// from the operator's GIT_CONFIG_COUNT entries. Its other entries stay out:
// they can carry http.extraHeader tokens.
var identityConfigKeys = []string{"user.name", "user.email", "author.name", "author.email", "committer.name", "committer.email"}

// identLine is what `git var GIT_<ROLE>_IDENT` prints: the name, the email in
// angle brackets, a timestamp and its offset.
var identLine = regexp.MustCompile(`^(.*) <([^<>]*)> \d+ [+-]\d{4}$`)

// identityProbeEnv is what commitIdentity's probes run in on top of git's
// isolated environment: the operator's identityEnvKeys, its GIT_CONFIG_COUNT
// entries for identityConfigKeys renumbered in order, and the C locale, so a
// refusal reads the same whatever the operator's language.
func identityProbeEnv() []string {
	var env []string
	for _, key := range identityEnvKeys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	count, _ := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	var kept []int
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		index, err := strconv.Atoi(strings.TrimPrefix(name, "GIT_CONFIG_KEY_"))
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") && err == nil && index >= 0 && index < count &&
			slices.Contains(identityConfigKeys, strings.ToLower(os.Getenv(name))) {
			kept = append(kept, index)
		}
	}
	// A later entry overrides an earlier one, so their order is kept.
	slices.Sort(kept)
	for i, index := range kept {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, os.Getenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", index))),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, os.Getenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", index))))
	}
	if len(kept) > 0 {
		env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(kept)))
	}
	return append(env, "LC_ALL=C")
}

// commitIdentity is the environment a merge or rebase in dir commits in: each
// role's identity, as git resolves it for dir, named explicitly, so the
// operation commits as exactly what was resolved. The commits are the user's:
// git resolves them from dir's config (repository, global, system, author.*
// and committer.*) and identityProbeEnv, as for sync-base, and takes EMAIL
// for an email none of those gives, as a plain git commit does.
// user.useConfigOnly keeps git from guessing one from the host name instead.
// Only a role git reports none for, as for an operator in a container whose
// user never set one, commits as angee's placeholder. A role whose probe fails
// any other way, such as an invalid configured name, adds nothing, so the
// operation reports git's own refusal rather than commit as angee.
func (d Driver) commitIdentity(ctx context.Context, dir string) []string {
	probe := identityProbeEnv()
	var env []string
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		if name, email, ok := d.resolveIdentity(ctx, dir, probe, role); ok {
			env = append(env, "GIT_"+role+"_NAME="+name, "GIT_"+role+"_EMAIL="+email)
		}
	}
	return env
}

// resolveIdentity is role's identity for commitIdentity, and false when it
// names none.
func (d Driver) resolveIdentity(ctx context.Context, dir string, env []string, role string) (name, email string, ok bool) {
	args := []string{"-c", "user.useConfigOnly=true", "var", "GIT_" + role + "_IDENT"}
	out, combined, err := d.git.RunIsolated(ctx, dir, env, args...)
	if address := os.Getenv("EMAIL"); err != nil && address != "" && strings.Contains(combined, "no email was given") {
		// git takes EMAIL for an email no config gives, but not under
		// user.useConfigOnly; as user.email it ranks below every config.
		out, combined, err = d.git.RunIsolated(ctx, dir, env, append([]string{"-c", "user.email=" + address}, args...)...)
	}
	if err == nil {
		if match := identLine.FindStringSubmatch(strings.TrimSpace(string(out))); match != nil {
			return match[1], match[2], true
		}
		return "", "", false
	}
	if ctx.Err() == nil && strings.Contains(combined, "auto-detection is disabled") {
		return "angee", "angee@example.invalid", true
	}
	return "", "", false
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
