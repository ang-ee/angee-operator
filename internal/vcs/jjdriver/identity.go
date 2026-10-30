package jjdriver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ang-ee/angee-operator/internal/git"
)

// identity is the identity a jj run that may record commits gets when the
// operator's jj has none: JJ_USER and JJ_EMAIL, which override jj's config,
// set only for what jj's config leaves empty. jj records an empty author and
// committer otherwise (every snapshot rewrites the working-copy commit), and
// `jj git push` refuses such commits. The fallback is the git driver's rule
// (git.Client.Identity): git's configured identity, else angee's placeholder
// only when git has none either. It is resolved once per runner, which the
// service makes for one slot operation, on the first run that needs it. There
// is no cache across operations, so a long-running operator sees a user's
// identity change on the next one; each costs the first recording run of an
// operation one `jj config list`, and a `git var` when jj has no identity.
type identity struct {
	git      git.Client
	mu       sync.Mutex
	resolved bool
	vars     []string
}

// env returns the fallback for a run whose commits belong to the repository
// at dir, resolving it on the first call. A resolution its context cut short
// is not kept, so the next run tries again. A nil identity has none.
func (id *identity) env(ctx context.Context, r runner, dir string) []string {
	if id == nil {
		return nil
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	if !id.resolved {
		vars := id.resolve(ctx, runner{bin: r.bin}, dir)
		if ctx.Err() != nil {
			return vars
		}
		id.vars, id.resolved = vars, true
	}
	return id.vars
}

// resolve reads user.name and user.email as jj resolves them for dir
// (identityConfig, which JJ_USER and JJ_EMAIL from the operator's environment
// override), and names git's author identity for each that is empty. git runs
// in dir when it is colocated with git, and in the system temporary directory
// otherwise, where it reads the user's own config only: git run in a jj
// store without a .git would read whatever repository encloses it. When jj's
// config cannot be read, or git names none and refuses otherwise (an invalid
// configured name, say), it adds nothing, so jj records what it has, as the
// git driver's commit then reports git's refusal: a guess must never
// override an identity jj has.
func (id *identity) resolve(ctx context.Context, jj runner, dir string) []string {
	user, email := os.Getenv("JJ_USER"), os.Getenv("JJ_EMAIL")
	if user == "" || email == "" {
		var ok bool
		if user, email, ok = jj.identityConfig(ctx, dir); !ok {
			return nil
		}
	}
	if user != "" && email != "" {
		return nil
	}
	gitDir := dir
	if ok, err := colocated(dir); err != nil || !ok {
		gitDir = os.TempDir()
	}
	name, address, ok := id.git.Identity(ctx, gitDir, "AUTHOR")
	if !ok {
		return nil
	}
	var vars []string
	if user == "" {
		vars = append(vars, "JJ_USER="+name)
	}
	if email == "" {
		vars = append(vars, "JJ_EMAIL="+address)
	}
	return vars
}

// identityConfig is jj's user.name and user.email for the repository at dir,
// as one `jj config list --include-defaults` read of the user table, which
// touches no working copy and prints the value each one takes, JJ_USER and
// JJ_EMAIL included. jj's defaults are empty strings. For a dir that holds no
// jj repository yet (a cache before `jj git init`), jj reads the user's
// config alone, in the system temporary directory: run in dir, it would load
// any jj repository above it. ok is false when the read fails.
func (r runner) identityConfig(ctx context.Context, dir string) (user, email string, ok bool) {
	args := []string{"--ignore-working-copy", "config", "list", "--include-defaults", "-T", identityConfigTemplate, "user"}
	if _, err := os.Lstat(filepath.Join(dir, ".jj")); err == nil {
		args = append([]string{"-R", "."}, args...)
	} else {
		dir = os.TempDir()
	}
	out, err := r.run(ctx, dir, args)
	if err != nil {
		return "", "", false
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, raw, ok := strings.Cut(line, "\t")
		var value string
		if !ok || json.Unmarshal([]byte(raw), &value) != nil {
			continue
		}
		switch name {
		case "user.name":
			user = value
		case "user.email":
			email = value
		}
	}
	return user, email, true
}

// identityConfigTemplate prints a config entry as its name, a tab, and its
// value as JSON, so a value holding a tab or a line break reads whole.
const identityConfigTemplate = `name ++ "\t" ++ json(value) ++ "\n"`
