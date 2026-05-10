package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/git"
	"github.com/fyltr/angee/internal/manifest"
)

func (p *Platform) materializeReferencedSources(ctx context.Context, stack *manifest.Stack) error {
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
	for name := range seen {
		source, ok := stack.Sources[name]
		if !ok {
			return fmt.Errorf("source %q is referenced but not declared", name)
		}
		if err := p.materializeSource(ctx, name, source); err != nil {
			return err
		}
	}
	return nil
}

func (p *Platform) SourceList(ctx context.Context) ([]api.SourceState, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	states := make([]api.SourceState, 0, len(stack.Sources))
	for _, name := range sortedKeys(stack.Sources) {
		state, err := p.sourceState(ctx, name, stack.Sources[name])
		if err != nil {
			state = api.SourceState{Name: name, Kind: stack.Sources[name].Kind, Path: p.sourcePath(name, stack.Sources[name]), State: "error", Error: err.Error()}
		}
		states = append(states, state)
	}
	return states, nil
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
	if err := p.materializeSource(ctx, name, source); err != nil {
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
	if err := p.materializeSource(ctx, name, source); err != nil {
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

func (p *Platform) materializeSource(ctx context.Context, name string, source manifest.Source) error {
	path := p.sourcePath(name, source)
	switch source.Kind {
	case "git":
		client := git.New()
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			return client.Fetch(ctx, path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return client.CloneRef(ctx, source.Repo, path, source.DefaultRef)
	case "local":
		if _, err := os.Stat(path); err != nil {
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
