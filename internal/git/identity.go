package git

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Angee's placeholder identity, which a commit angee records takes only when
// the user has none configured anywhere, as for an operator in a container
// whose user never set one.
const (
	PlaceholderName  = "angee"
	PlaceholderEmail = "angee@example.invalid"
)

// IdentityEnvKeys are the variables of the operator's environment that git
// reads an identity, or the config files that hold one, from. IsolatedEnv
// leaves them out; Identity's probes get them, so an identity the operator's
// environment names (a container's compose environment, say) is honoured as
// sync-base honours it.
var IdentityEnvKeys = []string{
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

// identityProbeEnv is what Identity's probes run in on top of IsolatedEnv:
// the operator's IdentityEnvKeys, its GIT_CONFIG_COUNT entries for
// identityConfigKeys renumbered in order, and the C locale, so a refusal reads
// the same whatever the operator's language.
func identityProbeEnv() []string {
	var env []string
	for _, key := range IdentityEnvKeys {
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

// Identity is the identity a commit made in dir records for role, AUTHOR or
// COMMITTER, as git resolves it there (`git var GIT_<role>_IDENT`): from dir's
// config (repository, global, system, author.* and committer.*) and the
// operator's identity variables (identityProbeEnv), taking EMAIL for an email
// none of those gives, as a plain git commit does. user.useConfigOnly keeps
// git from guessing one from the host name instead. When git reports none,
// it is angee's placeholder (PlaceholderName, PlaceholderEmail). When the
// probe fails any other way, such as for an invalid configured name, ok is
// false, so the caller adds nothing and the commit reports git's own refusal
// rather than record angee.
func (c Client) Identity(ctx context.Context, dir, role string) (name, email string, ok bool) {
	env := identityProbeEnv()
	args := []string{"-c", "user.useConfigOnly=true", "var", "GIT_" + role + "_IDENT"}
	out, combined, err := c.RunIsolated(ctx, dir, env, args...)
	if address := os.Getenv("EMAIL"); err != nil && address != "" && strings.Contains(combined, "no email was given") {
		// git takes EMAIL for an email no config gives, but not under
		// user.useConfigOnly; as user.email it ranks below every config.
		out, combined, err = c.RunIsolated(ctx, dir, env, append([]string{"-c", "user.email=" + address}, args...)...)
	}
	if err == nil {
		if match := identLine.FindStringSubmatch(strings.TrimSpace(string(out))); match != nil {
			return match[1], match[2], true
		}
		return "", "", false
	}
	if ctx.Err() == nil && strings.Contains(combined, "auto-detection is disabled") {
		return PlaceholderName, PlaceholderEmail, true
	}
	return "", "", false
}
