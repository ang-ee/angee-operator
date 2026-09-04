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

func (p *Platform) SourceFetch(ctx context.Context, name string) (api.SourceState, error) {
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

func (p *Platform) SourcePull(ctx context.Context, name string) (api.SourceState, error) {
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
	if err := git.New().Pull(ctx, p.sourcePath(name, source)); err != nil {
		return api.SourceState{}, err
	}
	return p.sourceState(ctx, name, source)
}

func (p *Platform) SourcePush(ctx context.Context, name, ref string) (api.SourceState, error) {
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
	dirty, err := git.New().Dirty(ctx, path)
	if err != nil {
		return api.SourceState{}, err
	}
	if dirty {
		return api.SourceState{}, fmt.Errorf("source %q has uncommitted changes", name)
	}
	if err := git.New().Push(ctx, path, ref); err != nil {
		return api.SourceState{}, err
	}
	return p.sourceState(ctx, name, source)
}

// materializeSource ensures a source's cache exists on disk. A missing cache is
// cloned, and a clone failure is always returned. When the cache already exists
// it is refreshed with a fetch, and bestEffortRefresh decides what a failed
// refresh means. The explicit `angee source fetch` / `source pull` verbs pass
// false so a stale-or-unreachable remote surfaces to the caller. Bring-up and
// workspace provisioning pass true: the cache already holds what worktree
// materialization and stack up read, so a source the operator cannot reach or
// authenticate — a private or SSH repo with no in-container key — must not block
// them; the refresh is only a freshness step, and a failure warns and keeps the
// existing cache. A cancelled or timed-out context is never best-effort.
func (p *Platform) materializeSource(ctx context.Context, name string, source manifest.Source, bestEffortRefresh bool) error {
	path := p.sourcePath(name, source)
	switch source.Kind {
	case "git":
		client := git.New()
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			finish := logctx.Step(ctx, "refreshing source "+name, slog.String("remote", logctx.RedactURL(source.Repo)))
			if err := client.Fetch(ctx, path); err != nil {
				if !bestEffortRefresh || ctx.Err() != nil {
					finish(err)
					return err
				}
				// The git error can carry the remote URL (a token, for an
				// HTTPS-with-credentials remote), so keep it out of the log and
				// point at `source pull`, which surfaces the full error on demand.
				logctx.From(ctx).Warn(fmt.Sprintf("could not refresh source %q; using the existing cache (update it with `angee source pull %s`)", name, name))
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
		err := client.CloneRef(ctx, source.Repo, path, source.DefaultRef)
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
	client := git.New()
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
