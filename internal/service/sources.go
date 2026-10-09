package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
)

func (p *Platform) materializeReferencedSources(ctx context.Context, stack *manifest.Stack) error {
	seen, err := referencedSourceNames(stack)
	if err != nil {
		return err
	}
	for _, name := range seen {
		if err := p.materializeSource(ctx, name, stack.Sources[name], true); err != nil {
			return err
		}
	}
	return nil
}

func referencedSourceNames(stack *manifest.Stack) ([]string, error) {
	seen := map[string]bool{}
	for name := range stack.Sources {
		seen[name] = true
	}
	collect := func(value string) {
		if !strings.HasPrefix(value, "source://") {
			return
		}
		rest := strings.TrimPrefix(value, "source://")
		name := rest
		if left, _, ok := strings.Cut(rest, ":"); ok {
			name = left
		}
		if n, _, ok := strings.Cut(name, "/"); ok {
			name = n
		}
		if name != "" {
			seen[name] = true
		}
	}
	for _, service := range stack.Services {
		for _, raw := range service.Mounts {
			collect(raw)
		}
		collect(service.Workdir)
	}
	for _, job := range stack.Jobs {
		for _, raw := range job.Mounts {
			collect(raw)
		}
		collect(job.Workdir)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		_, ok := stack.Sources[name]
		if !ok {
			return nil, fmt.Errorf("source %q is referenced but not declared", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// stageReferencedSources validates existing sources and installs newly cloned
// git sources through retained destination capabilities. It deliberately does
// not fetch existing repositories: reconciliation must be rollback-safe, while
// fetching remains part of StackPrepare and explicit source operations.
type stagedSourcePath struct {
	path           string
	dest           *copierx.GuardedPath
	created        bool
	validationRoot *copierx.TrustedRoot
	validationPath string
}

func (p *Platform) stageReferencedSources(ctx context.Context, stack *manifest.Stack, openAbsolute func(string) (*copierx.GuardedPath, error)) (func() error, func() error, func() error, error) {
	names, err := referencedSourceNames(stack)
	if err != nil {
		return nil, nil, nil, err
	}
	retained := []stagedSourcePath{}
	closeGuards := func() error {
		var result error
		for _, path := range retained {
			result = errors.Join(result, path.dest.Close())
			if path.validationRoot != nil {
				result = errors.Join(result, path.validationRoot.Close())
			}
		}
		return result
	}
	rolledBack := false
	rollback := func() error {
		if rolledBack {
			return nil
		}
		rolledBack = true
		defer func() { _ = closeGuards() }()
		var result error
		for index := len(retained) - 1; index >= 0; index-- {
			path := retained[index]
			if !path.created {
				continue
			}
			if err := path.dest.RemoveAll(); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
			result = errors.Join(result, path.dest.RemoveMissingParents())
		}
		return result
	}
	fail := func(primary error, cleanup ...func() error) (func() error, func() error, func() error, error) {
		cleanup = append(cleanup, rollback)
		return nil, nil, nil, joinRollbackErrors(primary, cleanup...)
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		source := stack.Sources[name]
		path := p.sourcePath(name, source)
		switch source.Kind {
		case "local":
			destination, err := openAbsolute(path)
			if err != nil {
				return fail(fmt.Errorf("validate local source %q: %w", name, err))
			}
			_, exists, err := destination.Lstat()
			if err == nil && !exists {
				if rel, relErr := filepath.Rel(p.root, path); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					// A local source inside the stack root that does not exist
					// yet is stack-owned machinery — e.g. a workspace slot the
					// declared `src` workspace cuts on `angee dev` — so init
					// staging skips it instead of failing the render.
					_ = destination.Close()
					continue
				}
			}
			if err != nil || !exists {
				if err == nil {
					err = os.ErrNotExist
				}
				return fail(fmt.Errorf("local source %q path %s: %w", name, path, err), destination.Close)
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fail(fmt.Errorf("local source %q path %s: %w", name, path, err), destination.Close)
			}
			trusted, err := copierx.OpenTrustedRoot(resolved)
			if err != nil {
				return fail(fmt.Errorf("retain local source %q: %w", name, err), destination.Close)
			}
			retained = append(retained, stagedSourcePath{path: path, dest: destination, validationRoot: trusted, validationPath: path})
		case "git":
			destination, err := openAbsolute(path)
			if err != nil {
				return fail(fmt.Errorf("stage git source %q: %w", name, err))
			}
			_, exists, err := destination.Lstat()
			if err != nil {
				return fail(fmt.Errorf("stage git source %q: %w", name, err), destination.Close)
			}
			if !exists {
				// The two-command contract: `angee init` renders; `angee dev`
				// materializes. An absent git cache is dev's to clone — init
				// stays fast and render-only.
				_ = destination.Close()
				continue
			}
			repository, err := destination.HasRealDirectory(".git")
			if err != nil {
				return fail(fmt.Errorf("inspect git source %q: %w", name, err), destination.Close)
			}
			if !repository {
				return fail(fmt.Errorf("git source %q destination %s exists but is not a repository", name, path), destination.Close)
			}
			gitRoot, err := destination.RetainRealSubdirectory(".git", filepath.Join(path, ".git"))
			if err != nil {
				return fail(fmt.Errorf("retain git source %q metadata: %w", name, err), destination.Close)
			}
			retained = append(retained, stagedSourcePath{path: path, dest: destination, validationRoot: gitRoot, validationPath: filepath.Join(path, ".git")})
		default:
			return fail(fmt.Errorf("source kind %q is not implemented", source.Kind))
		}
	}
	verify := func() error {
		var result error
		for _, path := range retained {
			result = errors.Join(result, path.dest.VerifyPathEntryIdentity(path.path))
			if path.validationRoot != nil {
				result = errors.Join(result, path.validationRoot.VerifyPath(path.validationPath))
			}
		}
		return result
	}
	return rollback, closeGuards, verify, nil
}

func openAbsoluteGuardedPath(path string) (*copierx.GuardedPath, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	ancestor := filepath.Dir(abs)
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("source destination ancestor %q is not a real directory", ancestor)
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return nil, statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return nil, fmt.Errorf("source destination %q has no existing directory ancestor", abs)
		}
		ancestor = parent
	}
	root, err := copierx.OpenTrustedRoot(ancestor)
	if err != nil {
		return nil, err
	}
	destination, openErr := root.OpenGuardedPath(filepath.Dir(abs), filepath.Base(abs), nil)
	closeErr := root.Close()
	if openErr != nil {
		return nil, errors.Join(openErr, closeErr)
	}
	if closeErr != nil {
		_ = destination.Close()
		return nil, closeErr
	}
	return destination, nil
}

func (p *Platform) SourceList(ctx context.Context, q query.Args) ([]api.SourceState, int, error) {
	if err := query.Validate(q, queryfields.Source); err != nil {
		return nil, 0, invalidQueryError(err)
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, 0, err
	}
	states := make([]api.SourceState, 0, len(stack.Sources))
	for _, name := range sortedKeys(stack.Sources) {
		state, err := p.sourceState(ctx, name, stack.Sources[name])
		if err != nil {
			state = api.SourceState{Name: name, Kind: stack.Sources[name].Kind, Path: p.sourcePath(name, stack.Sources[name]), State: "error", Error: err.Error()}
		}
		states = append(states, state)
	}
	page, total := query.Apply(states, q, queryfields.Source)
	return page, total, nil
}

func (p *Platform) SourceFetch(ctx context.Context, name string) (state api.SourceState, err error) {
	defer sourceOperation("source.fetch", "Fetch", name).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "source")
	if err != nil {
		return api.SourceState{}, err
	}
	defer release()
	stack, err := p.LoadStack()
	if err != nil {
		return api.SourceState{}, err
	}
	source, ok := stack.Sources[name]
	if !ok {
		return api.SourceState{}, &NotFoundError{Kind: "source", Name: name}
	}
	if err := p.materializeSource(ctx, name, source, false); err != nil {
		return api.SourceState{}, err
	}
	return p.sourceState(ctx, name, source)
}

func (p *Platform) SourceStatus(ctx context.Context, name string) (api.SourceState, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return api.SourceState{}, err
	}
	source, ok := stack.Sources[name]
	if !ok {
		return api.SourceState{}, &NotFoundError{Kind: "source", Name: name}
	}
	return p.sourceState(ctx, name, source)
}

func (p *Platform) SourcePull(ctx context.Context, name string) (state api.SourceState, err error) {
	defer sourceOperation("source.pull", "Pull", name).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "source")
	if err != nil {
		return api.SourceState{}, err
	}
	defer release()
	stack, err := p.LoadStack()
	if err != nil {
		return api.SourceState{}, err
	}
	source, ok := stack.Sources[name]
	if !ok {
		return api.SourceState{}, &NotFoundError{Kind: "source", Name: name}
	}
	if source.Kind != "git" {
		return api.SourceState{}, fmt.Errorf("source %q is not a git source", name)
	}
	if err := p.materializeSource(ctx, name, source, false); err != nil {
		return api.SourceState{}, err
	}
	path := p.sourcePath(name, source)
	if err := p.gitClient().Pull(ctx, path); err != nil {
		return api.SourceState{}, gitFailure(err, gitStep{action: "pulling", object: fmt.Sprintf("source %q", name), remote: source.Repo, cause: CauseFetchFailed, source: name})
	}
	return p.sourceState(ctx, name, source)
}

func (p *Platform) SourcePush(ctx context.Context, name, ref string) (state api.SourceState, err error) {
	defer sourceOperation("source.push", "Push", name).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "source")
	if err != nil {
		return api.SourceState{}, err
	}
	defer release()
	stack, err := p.LoadStack()
	if err != nil {
		return api.SourceState{}, err
	}
	source, ok := stack.Sources[name]
	if !ok {
		return api.SourceState{}, &NotFoundError{Kind: "source", Name: name}
	}
	if source.Kind != "git" {
		return api.SourceState{}, fmt.Errorf("source %q is not a git source", name)
	}
	path := p.sourcePath(name, source)
	if _, err := os.Stat(filepath.Join(path, ".git")); os.IsNotExist(err) {
		hint := fmt.Sprintf("Run `angee source fetch %s` to clone it.", name)
		return api.SourceState{}, &OperationError{
			Code:    CodePreconditionFailed,
			Cause:   CauseSourceCacheMissing,
			Source:  name,
			Paths:   []string{path},
			Hint:    hint,
			Summary: fmt.Sprintf("source %q has no cache at %s, so there is nothing to push. %s", name, path, hint),
			Err:     err,
		}
	} else if err != nil {
		return api.SourceState{}, err
	}
	client := p.gitClient()
	if err := requireCleanCheckout(ctx, client, path, fmt.Sprintf("source %q", name), "", name); err != nil {
		return api.SourceState{}, err
	}
	step := gitStep{action: "pushing", object: fmt.Sprintf("source %q", name), remote: pushRemoteURL(ctx, client, path), cause: CausePushFailed, source: name}
	if err := client.Push(ctx, path, ref); err != nil {
		return api.SourceState{}, gitFailure(err, step)
	}
	return p.sourceState(ctx, name, source)
}

// materializeSource ensures a source's cache exists on disk. A missing cache is
// cloned, and a clone failure is always returned. An existing cache has its
// origin reconciled with source.Repo (see syncSourceOrigin) and is refreshed
// with a fetch; bestEffortRefresh decides what a failed refresh means. The
// explicit `angee source fetch` / `source pull` verbs pass false, so origin
// follows the manifest and a stale-or-unreachable remote surfaces to the
// caller. Bring-up and workspace provisioning pass true: the cache already
// holds what worktree materialization and stack up read, so a source the
// operator cannot reach or authenticate — a private or SSH repo with no
// in-container key — must not block them; the refresh is only a freshness
// step, and a failure warns and keeps the existing cache. A cancelled or
// timed-out context is never best-effort.
func (p *Platform) materializeSource(ctx context.Context, name string, source manifest.Source, bestEffortRefresh bool) error {
	path := p.sourcePath(name, source)
	switch source.Kind {
	case "git":
		client := p.gitClient()
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			finish := logctx.Step(ctx, "refreshing source "+name, slog.String("remote", logctx.RedactURL(source.Repo)))
			err := syncSourceOrigin(ctx, client, name, path, source.Repo, !bestEffortRefresh)
			if err == nil {
				step := gitStep{action: "fetching", object: fmt.Sprintf("source %q", name), remote: source.Repo, cause: CauseFetchFailed, source: name}
				err = gitFailure(client.Fetch(ctx, path), step)
			}
			if err != nil {
				if !bestEffortRefresh || ctx.Err() != nil {
					finish(err)
					return err
				}
				// Keep git's multi-line error out of the warning and point at
				// `source pull`, which surfaces it in full on demand.
				// A bounded timeout is still best-effort: the stall is over and the
				// cache is usable, so say why and carry on.
				cause := ""
				if git.IsTimeout(err) {
					cause = " (timed out; raise ANGEE_GIT_TIMEOUT for slow remotes)"
				}
				logctx.From(ctx).Warn(fmt.Sprintf("could not refresh source %q%s; using the existing cache (update it with `angee source pull %s`)", name, cause, name))
				finish(nil)
				return nil
			}
			finish(nil)
			return nil
		}
		finish := logctx.Step(ctx, "cloning source "+name, slog.String("remote", logctx.RedactURL(source.Repo)))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			finish(err)
			return err
		}
		step := gitStep{action: "cloning", object: fmt.Sprintf("source %q", name), remote: source.Repo, cause: CauseCloneFailed, source: name}
		err := gitFailure(client.CloneRef(ctx, source.Repo, path, source.DefaultRef), step)
		finish(err)
		return err
	case "local":
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				if rel, relErr := filepath.Rel(p.root, path); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					// A local source inside the stack root that does not exist
					// yet is stack-owned machinery — e.g. a workspace slot the
					// declared `src` workspace cuts right after source
					// materialization on the same bring-up.
					return nil
				}
			}
			return fmt.Errorf("local source %q path %s: %w", name, path, err)
		}
		return nil
	default:
		return fmt.Errorf("source kind %q is not implemented", source.Kind)
	}
}

// syncSourceOrigin reconciles an existing cache's origin with the manifest's
// repo, which is otherwise used only by the first clone. With adopt (the
// explicit `source fetch` / `source pull` verbs) origin is set to the repo.
// Without it (bring-up, workspace provisioning) a mismatch only warns: a
// cache_path outside the stack root can be a cache shared by several stacks,
// or the user's own clone, and a bring-up must not repoint it at whichever
// stack ran last. Only origin is read or written; other remotes are the
// user's. Worktrees share the cache's config, so worktree slots cut from the
// cache follow it.
func syncSourceOrigin(ctx context.Context, client git.Client, name, path, repo string, adopt bool) error {
	if repo == "" || relativeRepoPath(repo) {
		return nil
	}
	if !adopt {
		current, ok, err := client.RemoteURL(ctx, path, "origin")
		if err != nil {
			return fmt.Errorf("source %q: %w", name, err)
		}
		if ok && current == repo {
			return nil
		}
		has := "has no origin remote"
		if ok {
			has = "origin is " + redactedURLAgainst(current, repo)
		}
		logctx.From(ctx).Warn(fmt.Sprintf("source %q cache %s, but angee.yaml declares %s; run `angee source fetch %s` to switch it", name, has, logctx.RedactURL(repo), name))
		return nil
	}
	previous, changed, err := client.EnsureRemoteURL(ctx, path, "origin", repo)
	if err != nil {
		return fmt.Errorf("source %q: %w", name, err)
	}
	if !changed {
		return nil
	}
	had := "no origin remote"
	if previous != "" {
		had = "origin " + redactedURLAgainst(previous, repo)
	}
	logctx.From(ctx).Warn(fmt.Sprintf("source %q cache had %s; set origin to %s from angee.yaml", name, had, logctx.RedactURL(repo)))
	return nil
}

// redactedURLAgainst redacts rawURL for a warning that also names other, and
// says so when the two differ only in credentials the redaction hides.
func redactedURLAgainst(rawURL, other string) string {
	redacted := logctx.RedactURL(rawURL)
	if redacted == logctx.RedactURL(other) {
		return redacted + " (different credentials)"
	}
	return redacted
}

// relativeRepoPath reports whether repo is a relative filesystem path rather
// than a URL, an scp-like `host:path`, or an absolute path. git records a
// local clone source as an absolute path and would resolve a relative origin
// against the cache, so such a repo is left out of origin reconciliation.
func relativeRepoPath(repo string) bool {
	if strings.Contains(repo, "://") || filepath.IsAbs(repo) {
		return false
	}
	colon := strings.Index(repo, ":")
	slash := strings.Index(repo, "/")
	scpLike := colon > 0 && (slash < 0 || colon < slash)
	return !scpLike
}

func (p *Platform) sourceState(ctx context.Context, name string, source manifest.Source) (api.SourceState, error) {
	path := p.sourcePath(name, source)
	state := api.SourceState{Name: name, Kind: source.Kind, Path: path, State: "missing", Pushed: true}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	state.Exists = true
	state.Pushed = true
	if source.Kind != "git" {
		state.State = "ready"
		return state, nil
	}
	client := p.gitClient()
	ref, err := client.CurrentRef(ctx, path)
	if err != nil {
		return state, err
	}
	dirty, err := client.Dirty(ctx, path)
	if err != nil {
		return state, err
	}
	state.Ref = ref
	state.CurrentRef = ref
	state.Dirty = dirty
	if dirty {
		state.State = "dirty"
		state.Pushed = false
		state.UnpushedReason = "uncommitted changes"
		return state, nil
	}
	base, hasUpstream, err := client.Upstream(ctx, path)
	if err != nil {
		return state, err
	}
	if hasUpstream {
		state.Upstream = base
	}
	if base == "" {
		base = source.DefaultRef
	}
	if base == "" {
		state.State = "clean"
		return state, nil
	}
	ahead, behind, err := client.AheadBehind(ctx, path, base)
	if err != nil {
		return state, err
	}
	state.Ahead = ahead
	state.Behind = behind
	switch {
	case ahead > 0 && behind > 0:
		state.State = "diverged"
		state.Pushed = false
		state.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of %s", ahead, base)
	case ahead > 0:
		state.State = "ahead"
		state.Pushed = false
		if hasUpstream {
			state.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of %s", ahead, base)
		} else {
			state.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of base ref %s with no upstream", ahead, base)
		}
	case behind > 0:
		state.State = "behind"
	default:
		state.State = "clean"
	}
	return state, nil
}

func (p *Platform) sourcePath(name string, source manifest.Source) string {
	if source.Kind == "local" && source.Path != "" {
		return manifest.ResolvePath(p.root, source.Path)
	}
	cachePath := source.CachePath
	if cachePath == "" {
		cachePath = filepath.Join("sources", name)
	}
	return manifest.ResolvePath(p.root, cachePath)
}

// sourceOperation names a verb on a top-level source, such as
// `Pull for source "app"`.
func sourceOperation(name, verb, source string) operation {
	return operation{name: name, title: fmt.Sprintf("%s for source %q", verb, source), source: source}
}
