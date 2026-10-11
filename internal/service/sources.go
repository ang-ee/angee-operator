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
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/gitdriver"
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
		return api.SourceState{}, gitFailure(err, gitStep{action: "pulling", object: fmt.Sprintf("source %q", name), remote: source.CloneURL(), cause: CauseFetchFailed, source: name})
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
// cloned from the source's clone remote and given its other declared remotes,
// and a clone failure is always returned. An existing cache has its remotes
// reconciled with the manifest's (see syncSourceRemotes) and is refreshed
// with a fetch; bestEffortRefresh decides what a failed refresh means. The
// explicit `angee source fetch` / `source pull` verbs pass false, so the
// remotes follow the manifest and a stale-or-unreachable remote surfaces to the
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
		cloneRemote, remote, _ := source.CloneRemote()
		holder := cacheRemotes(name)
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			finish := logctx.Step(ctx, "refreshing source "+name, slog.String("remote", logctx.RedactURL(remote.URL)))
			mode := remoteSyncAdopt
			if bestEffortRefresh {
				mode = remoteSyncReport
			}
			err := syncSourceRemotes(ctx, client, holder, path, source, mode)
			if err == nil {
				step := gitStep{action: "fetching", object: fmt.Sprintf("source %q", name), remote: remote.URL, cause: CauseFetchFailed, source: name}
				err = gitFailure(client.Fetch(ctx, path), step)
			}
			if err == nil && mode == remoteSyncAdopt {
				// After the fetch, so the clone remote's branch exists to track.
				err = adoptBaseTracking(ctx, client, holder, path, source)
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
		finish := logctx.Step(ctx, "cloning source "+name, slog.String("remote", logctx.RedactURL(remote.URL)))
		err := p.cloneSourceCache(ctx, client, holder, path, source, cloneRemote, remote.URL)
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

// cloneSourceCache clones a missing cache from its clone remote into a
// sibling staging directory, gives it its declared remotes there, and only
// then renames it to path. Until the reconcile has run, the canonical remote
// it came from is pushable, so nothing may use the clone at path before; and a
// failed attempt removes only its own staging directory, never a path another
// process may have started using. A path that holds something other than a
// git checkout is refused.
//
// A cache_path that is a symlink is cloned into its target, as git clone does.
// A path that is an existing empty directory, which a rename may not replace
// (a mount point, say), is cloned in place, as git clone does, and configured
// right after: for that moment the canonical remote is not yet blocked.
func (p *Platform) cloneSourceCache(ctx context.Context, client git.Client, holder remoteHolder, path string, source manifest.Source, cloneRemote, cloneURL string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("source %q: cache path %s: %w", holder.source, path, err)
		}
		path = resolved
	}
	step := gitStep{action: "cloning", object: fmt.Sprintf("source %q", holder.source), remote: cloneURL, cause: CauseCloneFailed, source: holder.source}
	switch empty, err := emptyDirectory(path); {
	case err == nil && empty:
		// A failed clone leaves the directory as git found it, and what is
		// in it then may be another writer's, so only a clone of ours that
		// could not be configured is cleared, leaving the directory empty.
		if err := gitFailure(client.CloneRef(ctx, cloneURL, path, source.DefaultRef, cloneRemote), step); err != nil {
			return err
		}
		if err := syncSourceRemotes(ctx, client, holder, path, source, remoteSyncFresh); err != nil {
			clearDirectory(path)
			return err
		}
		return nil
	case err == nil:
		return fmt.Errorf("source %q: cache path %s is not empty and holds no git checkout", holder.source, path)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("source %q: cache path %s: %w", holder.source, path, err)
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	removeCloneStages(parent, filepath.Base(path))
	stageRoot, err := os.MkdirTemp(parent, cloneStagePrefix(filepath.Base(path)))
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	// git creates the clone directory itself, with the umask's permissions, as
	// a plain clone has them; the temporary directory around it is 0700.
	stage := filepath.Join(stageRoot, "clone")
	if err := gitFailure(client.CloneRef(ctx, cloneURL, stage, source.DefaultRef, cloneRemote), step); err != nil {
		return err
	}
	if err := syncSourceRemotes(ctx, client, holder, stage, source, remoteSyncFresh); err != nil {
		return err
	}
	if err := os.Rename(stage, path); err != nil {
		return fmt.Errorf("source %q: move the new clone into %s: %w", holder.source, path, err)
	}
	return nil
}

// cloneStagePrefix names the staging directories a cache named base is cloned
// in, beside it.
func cloneStagePrefix(base string) string {
	return "." + base + ".clone-"
}

// removeCloneStages removes the staging directories an interrupted clone of
// the cache named base left in parent: cloneStagePrefix followed by the digits
// os.MkdirTemp appends, so a sibling directory that merely starts with the
// prefix is left alone. Until the primary lock (D9), a clone of the same cache
// running at the same time in another process loses its staging directory and
// fails, as one of the two would at the rename.
func removeCloneStages(parent, base string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), cloneStagePrefix(base))
		if !entry.IsDir() || !ok || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, entry.Name()))
	}
}

// clearDirectory removes what dir holds, leaving dir itself.
func clearDirectory(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}

// canonicalPushURL is the push URL a canonical remote gets: a scheme no git
// remote helper serves, so every push to it fails, and `git remote -v` shows
// why. The fetch URL is untouched.
func canonicalPushURL(remote string) string {
	return "angee-fetch-only://" + remote
}

// remoteHolder is a checkout whose remotes syncSourceRemotes reconciles, as
// its warnings and errors name it: a source's cache, or a clone slot.
type remoteHolder struct {
	source string // the source's name, for errors
	label  string // the checkout, as a warning names it
	fix    string // how a report warning says to fix a difference ("run … to")
}

func cacheRemotes(source string) remoteHolder {
	return remoteHolder{
		source: source,
		label:  fmt.Sprintf("source %q cache", source),
		fix:    fmt.Sprintf("run `angee source fetch %s` to", source),
	}
}

// cloneSlotRemotes names a clone slot, a repository of its own that no verb
// reconciles after its creation, so its warnings point at git.
func cloneSlotRemotes(workspace, slot, source, path string) remoteHolder {
	return remoteHolder{
		source: source,
		label:  fmt.Sprintf("workspace %q clone slot %q", workspace, slot),
		fix:    fmt.Sprintf("use git in %s to", path),
	}
}

// reportCloneSlotRemotes warns how a git clone slot (`mode: clone`, or no mode)
// differs from the remotes the manifest declares. The slot is configured only
// when it is created, so a remote marked canonical since then shows here, when
// the slot is read (status, repair). A slot without its own .git is skipped:
// git would read the remotes of whatever repository encloses it.
func (p *Platform) reportCloneSlotRemotes(ctx context.Context, workspace, slot, path string, source manifest.Source, wsSource manifest.WorkspaceSource) {
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		return
	}
	holder := cloneSlotRemotes(workspace, slot, wsSource.Source, path)
	if err := syncSourceRemotes(ctx, p.gitClient(), holder, path, source, remoteSyncReport); err != nil {
		logctx.From(ctx).Warn(fmt.Sprintf("could not check the remotes of %s: %v", holder.label, err))
	}
}

// remoteSync says what syncSourceRemotes may do with a difference it finds.
type remoteSync int

const (
	// remoteSyncReport only warns (bring-up, workspace provisioning).
	remoteSyncReport remoteSync = iota
	// remoteSyncAdopt writes the manifest's remotes and warns what it changed
	// (the explicit `source fetch` / `source pull` verbs).
	remoteSyncAdopt
	// remoteSyncFresh writes them silently, into a checkout just cloned.
	remoteSyncFresh
)

// syncSourceRemotes reconciles a checkout's git config with the remotes the
// manifest declares (`repo:` declares origin), which the first clone used only
// in part:
//   - each declared remote's fetch URL;
//   - under `remotes:`, the canonical remote's push URL, set to
//     canonicalPushURL so the checkout can fetch from it but never push to it;
//     a remote `remotes:` declares without canonical loses that push URL;
//   - remote.pushDefault, when push_default is set;
//   - in report mode, whether the default branch tracks the clone remote
//     (adoptBaseTracking writes it, after a fetch).
//
// With remoteSyncReport a difference only warns: a cache_path outside the stack
// root can be a cache shared by several stacks, or the user's own clone, and a
// bring-up must not repoint it at whichever stack ran last. The `repo:` form
// never reads or writes push URLs or tracking, so a stack declaring it cannot
// undo another stack's canonical block on a shared cache. Remotes the manifest
// does not declare are the user's: they are never read or written, and one the
// manifest stops declaring stays. Worktrees share the cache's config, so
// worktree slots cut from the cache follow it.
func syncSourceRemotes(ctx context.Context, client git.Client, holder remoteHolder, path string, source manifest.Source, mode remoteSync) error {
	remotes := source.DeclaredRemotes()
	for _, remoteName := range sortedKeys(remotes) {
		remote := remotes[remoteName]
		present, err := syncRemoteURL(ctx, client, holder, path, remoteName, remote.URL, mode)
		if err != nil {
			return err
		}
		// A missing remote was just reported; it has no push URL to report too.
		if len(source.Remotes) == 0 || !present {
			continue
		}
		if err := syncCanonicalPushURL(ctx, client, holder, path, remoteName, remote.Canonical, mode); err != nil {
			return err
		}
	}
	if source.PushDefault != "" {
		current, _, err := client.Config(ctx, path, "remote.pushDefault")
		if err != nil {
			return err
		}
		if current != source.PushDefault {
			switch mode {
			case remoteSyncReport:
				logctx.From(ctx).Warn(fmt.Sprintf("%s pushes to %s by default, but angee.yaml declares push_default %s; %s switch it", holder.label, describeRemote(current), source.PushDefault, holder.fix))
			default:
				if err := client.SetPushDefault(ctx, path, source.PushDefault); err != nil {
					return fmt.Errorf("source %q: %w", holder.source, err)
				}
				if mode == remoteSyncAdopt {
					logctx.From(ctx).Warn(fmt.Sprintf("%s pushed to %s by default; set push_default %s from angee.yaml", holder.label, describeRemote(current), source.PushDefault))
				}
			}
		}
	}
	if mode == remoteSyncReport {
		return reportBaseTracking(ctx, client, holder, path, source)
	}
	return nil
}

// baseTracking returns the branch a `remotes:` source's default branch should
// track at path, <clone remote>/<default_ref>, with what it tracks now; ok is
// false when there is nothing to reconcile: a `repo:` source (it clones from
// origin, which git already tracks), no default_ref, no such local branch, or
// the branch already tracks it. A fresh clone tracks it from the start.
func baseTracking(ctx context.Context, client git.Client, path string, source manifest.Source) (want, current string, ok bool, err error) {
	if len(source.Remotes) == 0 || source.DefaultRef == "" {
		return "", "", false, nil
	}
	cloneRemote, _, found := source.CloneRemote()
	if !found || !client.RefExists(ctx, path, "refs/heads/"+source.DefaultRef) {
		return "", "", false, nil
	}
	remote, merge, err := client.BranchUpstream(ctx, path, source.DefaultRef)
	if err != nil {
		return "", "", false, err
	}
	if remote == cloneRemote && merge == "refs/heads/"+source.DefaultRef {
		return "", "", false, nil
	}
	current = "nothing"
	if remote != "" {
		current = remote + "/" + strings.TrimPrefix(merge, "refs/heads/")
	}
	return cloneRemote + "/" + source.DefaultRef, current, true, nil
}

func reportBaseTracking(ctx context.Context, client git.Client, holder remoteHolder, path string, source manifest.Source) error {
	want, current, ok, err := baseTracking(ctx, client, path, source)
	if err != nil {
		return fmt.Errorf("source %q: %w", holder.source, err)
	}
	if ok {
		logctx.From(ctx).Warn(fmt.Sprintf("%s branch %s tracks %s, but angee.yaml takes it from %s; %s switch it", holder.label, source.DefaultRef, current, want, holder.fix))
	}
	return nil
}

// adoptBaseTracking makes a `remotes:` source's default branch track the clone
// remote, as a fresh clone's does, so `source pull` and the base follow the
// canonical remote rather than a cache's old origin (a cache cloned under
// `repo:` tracks origin, which `remotes:` may now name a fork).
func adoptBaseTracking(ctx context.Context, client git.Client, holder remoteHolder, path string, source manifest.Source) error {
	want, current, ok, err := baseTracking(ctx, client, path, source)
	if err != nil {
		return fmt.Errorf("source %q: %w", holder.source, err)
	}
	if !ok {
		return nil
	}
	if !client.RefExists(ctx, path, "refs/remotes/"+want) {
		logctx.From(ctx).Warn(fmt.Sprintf("%s branch %s tracks %s, but angee.yaml takes it from %s, which does not exist", holder.label, source.DefaultRef, current, want))
		return nil
	}
	// The full ref, so a local branch named like the remote branch is not
	// taken for it.
	if err := client.SetUpstream(ctx, path, source.DefaultRef, "refs/remotes/"+want); err != nil {
		return fmt.Errorf("source %q: %w", holder.source, err)
	}
	logctx.From(ctx).Warn(fmt.Sprintf("%s branch %s tracked %s; set it to track %s from angee.yaml", holder.label, source.DefaultRef, current, want))
	return nil
}

func describeRemote(name string) string {
	if name == "" {
		return "no configured remote"
	}
	return "remote " + name
}

// syncRemoteURL reconciles one declared remote's fetch URL and reports whether
// the remote is present afterwards (in report mode: whether it was). A relative
// path, which only `repo:` can declare, is skipped: git records a local clone
// source as an absolute path and would resolve a relative URL against the
// checkout.
func syncRemoteURL(ctx context.Context, client git.Client, holder remoteHolder, path, remote, rawURL string, mode remoteSync) (bool, error) {
	if rawURL == "" || manifest.IsRelativeURL(rawURL) {
		return false, nil
	}
	if mode == remoteSyncReport {
		current, ok, err := client.RemoteURL(ctx, path, remote)
		if err != nil {
			return false, fmt.Errorf("source %q: %w", holder.source, err)
		}
		if ok && current == rawURL {
			return true, nil
		}
		has := "has no " + remote + " remote"
		if ok {
			has = remote + " is " + redactedURLAgainst(current, rawURL)
		}
		logctx.From(ctx).Warn(fmt.Sprintf("%s %s, but angee.yaml declares %s; %s switch it", holder.label, has, logctx.RedactURL(rawURL), holder.fix))
		return ok, nil
	}
	previous, changed, err := client.EnsureRemoteURL(ctx, path, remote, rawURL)
	if err != nil {
		return false, fmt.Errorf("source %q: %w", holder.source, err)
	}
	if !changed || mode == remoteSyncFresh {
		return true, nil
	}
	had := "no " + remote + " remote"
	if previous != "" {
		had = remote + " " + redactedURLAgainst(previous, rawURL)
	}
	logctx.From(ctx).Warn(fmt.Sprintf("%s had %s; set %s to %s from angee.yaml", holder.label, had, remote, logctx.RedactURL(rawURL)))
	return true, nil
}

// syncCanonicalPushURL gives a canonical remote canonicalPushURL as its only
// push URL, and removes that one from a remote declared without canonical. A
// push URL the operator did not set on a non-canonical remote is the user's.
func syncCanonicalPushURL(ctx context.Context, client git.Client, holder remoteHolder, path, remote string, canonical bool, mode remoteSync) error {
	if !canonical && mode == remoteSyncReport {
		return nil
	}
	guard := canonicalPushURL(remote)
	pushURLs, err := client.PushURLs(ctx, path, remote)
	if err != nil {
		return fmt.Errorf("source %q: %w", holder.source, err)
	}
	guarded := len(pushURLs) == 1 && pushURLs[0] == guard
	switch {
	case canonical == guarded:
		return nil
	case mode == remoteSyncReport:
		logctx.From(ctx).Warn(fmt.Sprintf("%s can push to %s, which angee.yaml marks canonical (fetch-only); %s block pushes to it", holder.label, remote, holder.fix))
	case canonical:
		if err := client.SetPushURL(ctx, path, remote, guard); err != nil {
			return fmt.Errorf("source %q: %w", holder.source, err)
		}
		if mode == remoteSyncAdopt {
			logctx.From(ctx).Warn(fmt.Sprintf("%s could push to %s; blocked pushes to it, since angee.yaml marks it canonical (fetch-only)", holder.label, remote))
		}
	default:
		if err := client.UnsetPushURL(ctx, path, remote); err != nil {
			return fmt.Errorf("source %q: %w", holder.source, err)
		}
		if mode == remoteSyncAdopt {
			logctx.From(ctx).Warn(fmt.Sprintf("%s could not push to %s; allowed pushes to it, since angee.yaml no longer marks it canonical", holder.label, remote))
		}
	}
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
	if source.Kind != "git" {
		state.State = "ready"
		return state, nil
	}
	// A cache is not a slot, but the git driver reads any git checkout, so the
	// cache and its slots share one status rule.
	read, err := gitdriver.New(p.gitClient()).Status(ctx, vcs.Slot{Path: path, BaseRef: source.DefaultRef, BaseRemote: sourceBaseRemote(source)})
	if err != nil {
		return state, err
	}
	state.Ref = read.CurrentRef
	state.CurrentRef = read.CurrentRef
	state.Dirty = read.Dirty
	state.Upstream = read.Upstream
	state.Ahead = read.Ahead
	state.Behind = read.Behind
	state.State = workspaceSourceStateOf(read)
	state.Pushed = read.Pushed
	state.UnpushedReason = read.UnpushedReason
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
