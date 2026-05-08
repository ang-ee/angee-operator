package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/copierx"
	"github.com/fyltr/angee/internal/git"
	"github.com/fyltr/angee/internal/manifest"
	"github.com/fyltr/angee/internal/ports"
	"github.com/fyltr/angee/internal/substitute"
)

func (p *Platform) WorkspaceCreate(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceRef, error) {
	if req.Template == "" {
		return api.WorkspaceRef{}, fmt.Errorf("workspace template is required")
	}
	stack, err := p.loadOrCreateWorkspaceStack()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	templatePath, templateRef, err := p.resolveTemplate(ctx, req.Template, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	metadata, err := copierx.ValidateMetadata(templatePath, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	inputs := workspaceInputs(metadata, req.Inputs)
	name, err := p.workspaceName(metadata, req.Name, inputs)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if _, exists := stack.Workspaces[name]; exists {
		return api.WorkspaceRef{}, fmt.Errorf("workspace %q already exists", name)
	}
	allocations, err := allocateWorkspacePorts(stack, name)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	workspacePath := filepath.Join(p.root, "workspaces", name)
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		return api.WorkspaceRef{}, err
	}
	workspaceSources, err := p.materializeWorkspaceSources(ctx, stack, name, workspacePath, metadata, inputs, allocations)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	renderInputs := copierx.Inputs(inputs)
	renderInputs["workspace_name"] = name
	if err := (copierx.LocalRenderer{}).Copy(ctx, copierx.CopyRequest{Template: templatePath, Dest: workspacePath, Inputs: renderInputs}); err != nil {
		return api.WorkspaceRef{}, err
	}
	resolvedChain, chainRoot, err := p.renderWorkspaceChain(ctx, workspacePath, metadata, inputs, name, allocations)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	resolvedChain = append([]string{templateRef}, resolvedChain...)
	workspace := manifest.Workspace{
		Template: templateRef,
		Inputs:   map[string]string(inputs),
		Sources:  workspaceSources,
		Resolved: manifest.WorkspaceResolved{
			Chain:        resolvedChain,
			ChainRoot:    chainRoot,
			PersistPaths: metadata.Persist,
		},
		TTL: req.TTL,
	}
	if req.TTL != "" {
		duration, err := time.ParseDuration(req.TTL)
		if err != nil {
			return api.WorkspaceRef{}, err
		}
		expires := time.Now().Add(duration).UTC()
		workspace.TTLExpiresAt = &expires
	}
	if stack.Workspaces == nil {
		stack.Workspaces = map[string]manifest.Workspace{}
	}
	stack.Workspaces[name] = workspace
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return api.WorkspaceRef{}, err
	}
	ref := api.WorkspaceRef{Name: name, Path: workspacePath, Template: templateRef, TTL: workspace.TTL, TTLExpiresAt: workspace.TTLExpiresAt}
	if req.Start {
		if err := p.WorkspaceStart(ctx, name); err != nil {
			return ref, err
		}
	}
	return ref, nil
}

func (p *Platform) loadOrCreateWorkspaceStack() (*manifest.Stack, error) {
	stack, err := p.LoadStack()
	if err == nil {
		return stack, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return p.EmptyStack(defaultWorkspaceStackName(p.root)), nil
}

func defaultWorkspaceStackName(root string) string {
	name := filepath.Base(root)
	if name == ".angee" {
		name = filepath.Base(filepath.Dir(root))
	}
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "workspace"
	}
	return name
}

func (p *Platform) WorkspaceList(ctx context.Context) ([]api.WorkspaceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	refs := make([]api.WorkspaceRef, 0, len(stack.Workspaces))
	for _, name := range sortedKeys(stack.Workspaces) {
		workspace := stack.Workspaces[name]
		refs = append(refs, api.WorkspaceRef{Name: name, Path: filepath.Join(p.root, "workspaces", name), Template: workspace.Template, TTL: workspace.TTL, TTLExpiresAt: workspace.TTLExpiresAt})
	}
	return refs, nil
}

func (p *Platform) WorkspaceGet(ctx context.Context, name string) (api.WorkspaceRef, error) {
	if err := ctx.Err(); err != nil {
		return api.WorkspaceRef{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return api.WorkspaceRef{}, fmt.Errorf("workspace %q is not declared", name)
	}
	return api.WorkspaceRef{Name: name, Path: filepath.Join(p.root, "workspaces", name), Template: workspace.Template, TTL: workspace.TTL, TTLExpiresAt: workspace.TTLExpiresAt}, nil
}

func (p *Platform) WorkspaceDestroy(ctx context.Context, name string, purge bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	if _, ok := stack.Workspaces[name]; !ok {
		return fmt.Errorf("workspace %q is not declared", name)
	}
	delete(stack.Workspaces, name)
	releaseWorkspacePorts(stack, name)
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return err
	}
	if purge {
		return os.RemoveAll(filepath.Join(p.root, "workspaces", name))
	}
	return nil
}

func (p *Platform) WorkspaceUpdate(ctx context.Context, name string, inputs map[string]string, ttl string) (api.WorkspaceRef, error) {
	if err := ctx.Err(); err != nil {
		return api.WorkspaceRef{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return api.WorkspaceRef{}, fmt.Errorf("workspace %q is not declared", name)
	}
	if inputs != nil {
		if workspace.Inputs == nil {
			workspace.Inputs = map[string]string{}
		}
		for key, value := range inputs {
			workspace.Inputs[key] = value
		}
	}
	if ttl != "" {
		duration, err := time.ParseDuration(ttl)
		if err != nil {
			return api.WorkspaceRef{}, err
		}
		expires := time.Now().Add(duration).UTC()
		workspace.TTL = ttl
		workspace.TTLExpiresAt = &expires
	}
	stack.Workspaces[name] = workspace
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return api.WorkspaceRef{}, err
	}
	return api.WorkspaceRef{Name: name, Path: filepath.Join(p.root, "workspaces", name), Template: workspace.Template, TTL: workspace.TTL, TTLExpiresAt: workspace.TTLExpiresAt}, nil
}

func (p *Platform) WorkspaceLogs(ctx context.Context, name string, follow bool) (<-chan string, error) {
	return p.WorkspaceLogsLimited(ctx, name, follow, 0)
}

func (p *Platform) WorkspaceLogsLimited(ctx context.Context, name string, follow bool, maxBytes int) (<-chan string, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, fmt.Errorf("workspace %q is not declared", name)
	}
	if workspace.Resolved.ChainRoot == "" {
		ch := make(chan string)
		close(ch)
		return ch, nil
	}
	inner, err := New(filepath.Join(p.root, "workspaces", name, workspace.Resolved.ChainRoot))
	if err != nil {
		return nil, err
	}
	return inner.StackLogsLimited(ctx, nil, follow, maxBytes)
}

func releaseWorkspacePorts(stack *manifest.Stack, workspaceName string) {
	for poolName, leases := range stack.PortLeases {
		kept := leases[:0]
		for _, lease := range leases {
			if strings.HasPrefix(lease.Owner, "workspace/"+workspaceName+"/") {
				continue
			}
			kept = append(kept, lease)
		}
		if len(kept) == 0 {
			delete(stack.PortLeases, poolName)
			continue
		}
		stack.PortLeases[poolName] = kept
	}
}

func (p *Platform) WorkspaceStart(ctx context.Context, name string) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return fmt.Errorf("workspace %q is not declared", name)
	}
	if workspace.Resolved.ChainRoot == "" {
		return nil
	}
	innerRoot := filepath.Join(p.root, "workspaces", name, workspace.Resolved.ChainRoot)
	if _, err := os.Stat(manifest.Path(innerRoot)); err != nil {
		return err
	}
	inner, err := New(innerRoot)
	if err != nil {
		return err
	}
	innerStack, err := inner.LoadStack()
	if err != nil {
		return err
	}
	for _, service := range innerStack.Services {
		if service.Runtime == manifest.RuntimeLocal {
			return inner.StackDev(ctx, false)
		}
	}
	return inner.StackUp(ctx, nil, false)
}

func (p *Platform) WorkspaceStop(ctx context.Context, name string) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return fmt.Errorf("workspace %q is not declared", name)
	}
	if workspace.Resolved.ChainRoot == "" {
		return nil
	}
	inner, err := New(filepath.Join(p.root, "workspaces", name, workspace.Resolved.ChainRoot))
	if err != nil {
		return err
	}
	return inner.StackDown(ctx)
}

func (p *Platform) WorkspaceGitStatus(ctx context.Context, name string) ([]api.SourceState, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, fmt.Errorf("workspace %q is not declared", name)
	}
	states := []api.SourceState{}
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		source, ok := stack.Sources[wsSource.Source]
		if !ok {
			return nil, fmt.Errorf("workspace %q source %q references undeclared source %q", name, slot, wsSource.Source)
		}
		state, err := p.workspaceSourceState(ctx, name, slot, source, wsSource)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func (p *Platform) WorkspacePush(ctx context.Context, name, ref string) ([]api.SourceState, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, fmt.Errorf("workspace %q is not declared", name)
	}
	client := git.New()
	states := []api.SourceState{}
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		if wsSource.Mode != "worktree" && wsSource.Mode != "clone" {
			continue
		}
		source, ok := stack.Sources[wsSource.Source]
		if !ok || source.Kind != "git" {
			continue
		}
		path := filepath.Join(p.root, "workspaces", name, wsSource.Subpath)
		dirty, err := client.Dirty(ctx, path)
		if err != nil {
			return nil, err
		}
		if dirty {
			return nil, fmt.Errorf("workspace %q source %q has uncommitted changes", name, slot)
		}
		pushRef := ref
		if pushRef == "" {
			pushRef = wsSource.Branch
		}
		if err := client.Push(ctx, path, pushRef); err != nil {
			return nil, err
		}
		state, err := p.workspaceSourceState(ctx, name, slot, source, wsSource)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func (p *Platform) workspaceSourceState(ctx context.Context, workspaceName, slot string, source manifest.Source, wsSource manifest.WorkspaceSource) (api.SourceState, error) {
	path := filepath.Join(p.root, "workspaces", workspaceName, wsSource.Subpath)
	state := api.SourceState{Name: wsSource.Source, Slot: slot, Kind: source.Kind, Path: path}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	state.Exists = true
	if source.Kind == "git" {
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
		state.Dirty = dirty
	}
	return state, nil
}

func (p *Platform) materializeWorkspaceSources(ctx context.Context, stack *manifest.Stack, workspaceName, workspacePath string, metadata copierx.Metadata, inputs map[string]string, alloc map[string]int) (map[string]manifest.WorkspaceSource, error) {
	result := map[string]manifest.WorkspaceSource{}
	for slot, spec := range metadata.Sources {
		sourceName := spec.Source
		if sourceName == "" {
			sourceName = slot
		}
		source, ok := stack.Sources[sourceName]
		if !ok {
			var err error
			source, err = resolveWorkspaceTemplateSource(spec, inputs, workspaceName, alloc)
			if err != nil {
				return nil, err
			}
			if source.Kind != "" {
				if stack.Sources == nil {
					stack.Sources = map[string]manifest.Source{}
				}
				stack.Sources[sourceName] = source
				ok = true
			}
		}
		if !ok {
			if spec.Optional {
				continue
			}
			return nil, fmt.Errorf("workspace source %q references undeclared source %q", slot, sourceName)
		}
		resolved, err := p.resolveWorkspaceSource(spec, sourceName, inputs, workspaceName, alloc)
		if err != nil {
			return nil, err
		}
		if resolved.Subpath == "" {
			resolved.Subpath = slot
		}
		dest := filepath.Join(workspacePath, resolved.Subpath)
		if err := p.materializeWorkspaceSource(ctx, sourceName, source, resolved, dest); err != nil {
			if spec.Optional {
				continue
			}
			return nil, err
		}
		result[slot] = resolved
	}
	return result, nil
}

func resolveWorkspaceTemplateSource(spec copierx.TemplateSource, inputs map[string]string, workspaceName string, alloc map[string]int) (manifest.Source, error) {
	ctx := substitute.Context{Inputs: inputs, Name: workspaceName, Alloc: alloc}
	kind, err := substitute.Resolve(spec.Kind, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	repo, err := substitute.Resolve(spec.Repo, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	url, err := substitute.Resolve(spec.URL, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	path, err := substitute.Resolve(spec.Path, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	defaultRef, err := substitute.Resolve(spec.DefaultRef, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	cachePath, err := substitute.Resolve(spec.CachePath, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	if kind == "" {
		switch {
		case repo != "":
			kind = "git"
		case path != "":
			kind = "local"
		}
	}
	return manifest.Source{Kind: kind, Repo: repo, URL: url, Path: path, DefaultRef: defaultRef, CachePath: cachePath}, nil
}

func (p *Platform) resolveWorkspaceSource(spec copierx.TemplateSource, sourceName string, inputs map[string]string, workspaceName string, alloc map[string]int) (manifest.WorkspaceSource, error) {
	ctx := substitute.Context{Inputs: inputs, Name: workspaceName, Alloc: alloc}
	branch, err := substitute.Resolve(spec.Branch, ctx)
	if err != nil {
		return manifest.WorkspaceSource{}, err
	}
	ref, err := substitute.Resolve(spec.Ref, ctx)
	if err != nil {
		return manifest.WorkspaceSource{}, err
	}
	subpath, err := substitute.Resolve(spec.Subpath, ctx)
	if err != nil {
		return manifest.WorkspaceSource{}, err
	}
	return manifest.WorkspaceSource{Source: sourceName, Mode: spec.Mode, Branch: branch, Ref: ref, Subpath: subpath}, nil
}

func (p *Platform) materializeWorkspaceSource(ctx context.Context, sourceName string, source manifest.Source, ws manifest.WorkspaceSource, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	switch source.Kind {
	case "git":
		if ws.Mode == "worktree" {
			if err := p.materializeSource(ctx, sourceName, source); err != nil {
				return err
			}
			client := git.New()
			ref := ws.Ref
			if ref == "" {
				ref = source.DefaultRef
			}
			return client.WorktreeAddBranch(ctx, p.sourcePath(sourceName, source), dest, ws.Branch, ref)
		}
		ref := ws.Ref
		if ref == "" {
			ref = source.DefaultRef
		}
		return git.New().CloneRef(ctx, source.Repo, dest, ref)
	case "local":
		return os.Symlink(p.sourcePath(sourceName, source), dest)
	default:
		return fmt.Errorf("workspace source kind %q is not implemented", source.Kind)
	}
}

func (p *Platform) renderWorkspaceChain(ctx context.Context, workspacePath string, metadata copierx.Metadata, inputs map[string]string, workspaceName string, alloc map[string]int) ([]string, string, error) {
	chain := []string{}
	chainRoot := ""
	subCtx := substitute.Context{Inputs: inputs, Name: workspaceName, Alloc: alloc}
	if metadata.ChainRoot != "" {
		resolved, err := substitute.Resolve(metadata.ChainRoot, subCtx)
		if err != nil {
			return nil, "", err
		}
		chainRoot = resolved
	}
	for _, entry := range metadata.Chain {
		if entry.Template == "" {
			continue
		}
		templateRef, err := substitute.Resolve(entry.Template, subCtx)
		if err != nil {
			return nil, "", err
		}
		path, ref, err := p.resolveWorkspaceChainTemplate(ctx, workspacePath, templateRef)
		if err != nil {
			return nil, "", err
		}
		renderInputs := copierx.Inputs{}
		for key, value := range inputs {
			renderInputs[key] = value
		}
		for key, value := range entry.Inputs {
			resolved, err := substitute.Resolve(value, subCtx)
			if err != nil {
				return nil, "", err
			}
			renderInputs[key] = resolved
		}
		destRoot := chainRoot
		if entry.Root != "" {
			resolved, err := substitute.Resolve(entry.Root, subCtx)
			if err != nil {
				return nil, "", err
			}
			destRoot = resolved
		}
		if destRoot == "" {
			return nil, "", fmt.Errorf("chain entry %q requires a root", entry.Template)
		}
		if err := (copierx.LocalRenderer{}).Copy(ctx, copierx.CopyRequest{Template: path, Dest: filepath.Join(workspacePath, destRoot), Inputs: renderInputs}); err != nil {
			return nil, "", err
		}
		chain = append(chain, ref)
	}
	return chain, chainRoot, nil
}

func (p *Platform) resolveWorkspaceChainTemplate(ctx context.Context, workspacePath, ref string) (string, string, error) {
	if ref != "" && !filepath.IsAbs(ref) && !isRemoteTemplateRef(ref) {
		candidate := filepath.Join(workspacePath, filepath.FromSlash(ref))
		if _, err := os.Stat(filepath.Join(candidate, "copier.yml")); err == nil {
			return candidate, ref, nil
		}
	}
	return p.resolveTemplate(ctx, ref, "stack")
}

func allocateWorkspacePorts(stack *manifest.Stack, workspaceName string) (map[string]int, error) {
	alloc := map[string]int{}
	if len(stack.Operator.PortPool) == 0 {
		return alloc, nil
	}
	pools, err := ports.FromManifest(stack.Operator.PortPool, stack.PortLeases)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if stack.PortLeases == nil {
		stack.PortLeases = map[string][]manifest.PortLease{}
	}
	for _, name := range sortedKeys(pools) {
		owner := "workspace/" + workspaceName + "/" + name
		port, err := pools[name].Allocate(owner)
		if err != nil {
			return nil, err
		}
		alloc[name] = port
		leases := stack.PortLeases[name]
		found := false
		for i := range leases {
			if leases[i].Owner == owner {
				leases[i].Port = port
				found = true
			}
		}
		if !found {
			leases = append(leases, manifest.PortLease{Port: port, Owner: owner, CreatedAt: now})
		}
		stack.PortLeases[name] = leases
	}
	return alloc, nil
}

func workspaceInputs(metadata copierx.Metadata, provided map[string]string) map[string]string {
	inputs := map[string]string{}
	for key, spec := range metadata.Inputs {
		if spec.Default != nil {
			inputs[key] = fmt.Sprint(spec.Default)
		}
	}
	for key, value := range provided {
		inputs[key] = value
	}
	return inputs
}

func (p *Platform) workspaceName(metadata copierx.Metadata, explicit string, inputs map[string]string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	pattern := metadata.InstanceNaming.Pattern
	if pattern == "" {
		pattern = metadata.InstanceNaming.Fallback
	}
	if pattern == "" {
		pattern = "${inputs.name | slug}"
	}
	name, err := substitute.Resolve(pattern, substitute.Context{Inputs: inputs})
	if err != nil {
		return "", err
	}
	if max := metadata.InstanceNaming.MaxLength; max > 0 && len(name) > max {
		name = name[:max]
		name = strings.Trim(name, "-_")
	}
	if name == "" {
		return "", fmt.Errorf("workspace name resolved empty")
	}
	return name, nil
}

func (p *Platform) resolveTemplate(ctx context.Context, ref, kind string) (string, string, error) {
	if ref == "" {
		return "", "", fmt.Errorf("template reference is empty")
	}
	if isRemoteTemplateRef(ref) {
		return p.resolveRemoteTemplate(ctx, ref, kind)
	}
	if filepath.IsAbs(ref) {
		if _, err := os.Stat(ref); err != nil {
			return "", "", err
		}
		return ref, ref, nil
	}
	family := kind + "s"
	kindRef := ref
	if !strings.Contains(ref, "/") {
		kindRef = family + "/" + ref
	}
	if !strings.HasPrefix(kindRef, family+"/") {
		return "", "", fmt.Errorf("template %q does not match kind %q", ref, kind)
	}
	candidates := []string{
		filepath.Join(p.root, ".templates", kindRef),
		filepath.Join(p.root, "templates", kindRef),
		filepath.Join(p.root, kindRef),
		filepath.Join(p.root, ref),
	}
	if cwd, err := os.Getwd(); err == nil && cwd != p.root {
		candidates = append(candidates,
			filepath.Join(cwd, ".templates", kindRef),
			filepath.Join(cwd, "templates", kindRef),
		)
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(candidate, "copier.yml")); err == nil {
			return candidate, kindRef, nil
		}
	}
	return "", "", fmt.Errorf("template %q was not found", ref)
}
