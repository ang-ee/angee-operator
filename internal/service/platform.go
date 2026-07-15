package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/fslock"
	"github.com/ang-ee/angee-operator/internal/manifest"
	mountx "github.com/ang-ee/angee-operator/internal/mount"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/runtime/compose"
	"github.com/ang-ee/angee-operator/internal/runtime/edge"
	"github.com/ang-ee/angee-operator/internal/runtime/proccompose"
	"github.com/ang-ee/angee-operator/internal/secrets"
	"github.com/ang-ee/angee-operator/internal/substitute"
	"gopkg.in/yaml.v3"
)

type Platform struct {
	root            string
	composeBackend  runtime.Backend
	procBackend     runtime.Backend
	portUnavailable func(int) bool
	jobOutput       *jobOutputSink
}

// Option configures a Platform.
type Option func(*Platform)

// WithJobOutput configures a best-effort sink for live manual-job output.
// Platforms used directly by the CLI intentionally leave this unset and
// continue returning buffered output for the CLI to print once.
func WithJobOutput(writer io.Writer) Option {
	return func(platform *Platform) {
		platform.jobOutput = newJobOutputSink(writer)
	}
}

type CompiledStack struct {
	Compose        compose.File
	ProcessCompose proccompose.File
	SecretEnvVars  map[string]string
}

func New(root string, options ...Option) (*Platform, error) {
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		root = cwd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	platform := &Platform{root: abs, composeBackend: compose.NewBackend(), procBackend: proccompose.NewBackend(), portUnavailable: hostPortUnavailable}
	for _, option := range options {
		option(platform)
	}
	return platform, nil
}

func NewWithBackends(root string, composeBackend, procBackend runtime.Backend) (*Platform, error) {
	p, err := New(root)
	if err != nil {
		return nil, err
	}
	if composeBackend != nil {
		p.composeBackend = composeBackend
	}
	if procBackend != nil {
		p.procBackend = procBackend
	}
	return p, nil
}

func (p *Platform) Root() string {
	return p.root
}

func (p *Platform) LoadStack() (*manifest.Stack, error) {
	return manifest.LoadFile(manifest.Path(p.root))
}

func (p *Platform) StackPrepare(ctx context.Context) (*CompiledStack, error) {
	lock := fslock.RootLock(p.root)
	var compiled *CompiledStack
	err := lock.With(ctx, func() error {
		stack, err := p.LoadStack()
		if err != nil {
			return err
		}
		if err := p.materializeStackResources(ctx, stack); err != nil {
			return err
		}
		compiledStack, resolvedSecrets, err := p.compileStackArtifacts(ctx, stack)
		if err != nil {
			return err
		}
		if err := p.writeRuntimeEnv(stack, resolvedSecrets); err != nil {
			return err
		}
		compiled = compiledStack
		return p.writeCompiled(compiled)
	})
	return compiled, err
}

func (p *Platform) materializeStackResources(ctx context.Context, stack *manifest.Stack) error {
	// Ensure the stack's declared persist dirs exist before anything runs.
	// Subpaths are relative to the stack dir (the parent of ANGEE_ROOT),
	// matching the workspace-create convention.
	persistRoot := filepath.Dir(p.root)
	_, closePersistPaths, _, err := materializePersistPaths(ctx, targetPathOpener(persistRoot, persistRoot, nil), persistRoot, stack.Persist, nil)
	if err != nil {
		return err
	}
	if err := closePersistPaths(); err != nil {
		return err
	}
	return p.materializeReferencedSources(ctx, stack)
}

func (p *Platform) stageStackResources(ctx context.Context, stack *manifest.Stack, openAbsolute func(string) (*copierx.GuardedPath, error)) (func() error, func() error, func() error, error) {
	persistRoot := filepath.Dir(p.root)
	persistOpener := func(rel string) (*copierx.GuardedPath, error) {
		return openAbsolute(filepath.Join(persistRoot, filepath.FromSlash(rel)))
	}
	rollbackPersist, closePersist, verifyPersist, err := materializePersistPaths(ctx, persistOpener, persistRoot, stack.Persist, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	rollbackSources, closeSources, verifySources, err := p.stageReferencedSources(ctx, stack, openAbsolute)
	if err != nil {
		return nil, nil, nil, errors.Join(err, rollbackPersist())
	}
	rollback := func() error {
		return errors.Join(rollbackSources(), rollbackPersist())
	}
	closeResources := func() error {
		return errors.Join(closeSources(), closePersist())
	}
	verifyResources := func() error {
		return errors.Join(verifySources(), verifyPersist())
	}
	return rollback, closeResources, verifyResources, nil
}

// compileStackArtifacts resolves inputs and compiles runtime documents without
// materializing persist paths or source caches. Secret resolution retains its
// normal backend semantics for imported and generated declarations.
func (p *Platform) compileStackArtifacts(ctx context.Context, stack *manifest.Stack) (*CompiledStack, map[string]string, error) {
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return nil, nil, err
	}
	resolvedSecrets, err := secrets.ResolveDeclarations(ctx, backend, stack.Secrets, os.LookupEnv)
	if err != nil {
		return nil, nil, err
	}
	compiled, err := Compile(stack, p.root, resolvedSecrets)
	if err != nil {
		return nil, nil, err
	}
	return compiled, resolvedSecrets, nil
}

func (p *Platform) runtimeEnvFile(stack *manifest.Stack) string {
	if stack.SecretsBackend.Type == "openbao" {
		return filepath.Join(p.root, "run", "secrets.env")
	}
	return stack.EnvFilePath(p.root)
}

func (p *Platform) writeRuntimeEnv(stack *manifest.Stack, resolved map[string]string) error {
	openBaoPath := filepath.Join(p.root, "run", "secrets.env")
	if stack.SecretsBackend.Type != "openbao" || len(resolved) == 0 {
		return p.removeObsoleteOpenBaoRuntimeEnv(stack, openBaoPath)
	}
	path := openBaoPath
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var out strings.Builder
	for _, key := range sortedKeys(resolved) {
		out.WriteString(substitute.SecretEnvName(key))
		out.WriteByte('=')
		out.WriteString(resolved[key])
		out.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(out.String()), 0o600)
}

func (p *Platform) retainActiveEnvFile(stack *manifest.Stack, openAbsolute func(string) (*copierx.GuardedPath, error)) (func() error, func() error, error) {
	if stack.SecretsBackend.Type == "openbao" {
		noop := func() error { return nil }
		return noop, noop, nil
	}
	configured := stack.EnvFilePath(p.root)
	entry, err := openAbsolute(configured)
	if err != nil {
		return nil, nil, err
	}
	info, exists, err := entry.Lstat()
	if err != nil {
		_ = entry.Close()
		return nil, nil, err
	}
	guards := []*copierx.GuardedPath{entry}
	var target *copierx.GuardedPath
	resolvedTarget := ""
	if exists {
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(configured)
			if err != nil {
				_ = entry.Close()
				return nil, nil, err
			}
			resolvedTarget = resolved
			target, err = openAbsolute(resolved)
			if err != nil {
				_ = entry.Close()
				return nil, nil, err
			}
			guards = append(guards, target)
		}
	}
	verify := func() error {
		var result error
		if exists {
			result = errors.Join(result, entry.VerifyPathEntryIdentity(configured))
		} else {
			result = errors.Join(result, entry.VerifyPathAbsent(configured))
		}
		if target != nil {
			result = errors.Join(result, target.VerifyPathEntryIdentity(resolvedTarget))
		}
		return result
	}
	closeGuards := func() error {
		var result error
		for _, guard := range guards {
			result = errors.Join(result, guard.Close())
		}
		return result
	}
	return verify, closeGuards, nil
}

func (p *Platform) activeEnvFileUsesPath(stack *manifest.Stack, candidate string) bool {
	if stack.SecretsBackend.Type == "openbao" {
		return false
	}
	configured := stack.EnvFilePath(p.root)
	if filepath.Clean(configured) == filepath.Clean(candidate) {
		return true
	}
	configuredInfo, configuredErr := os.Stat(configured)
	candidateInfo, candidateErr := os.Stat(candidate)
	return configuredErr == nil && candidateErr == nil && os.SameFile(configuredInfo, candidateInfo)
}

func (p *Platform) removeObsoleteOpenBaoRuntimeEnv(stack *manifest.Stack, candidate string) error {
	if p.activeEnvFileUsesPath(stack, candidate) {
		return nil
	}
	verifyEnv, closeEnv, err := p.retainActiveEnvFile(stack, openAbsoluteGuardedPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeEnv() }()
	if p.activeEnvFileUsesPath(stack, candidate) {
		return nil
	}
	destination, err := openAbsoluteGuardedPath(candidate)
	if err != nil {
		return err
	}
	defer func() { _ = destination.Close() }()
	data, info, exists, err := destination.ReadRegularFile()
	if err != nil {
		return err
	}
	if !exists {
		return errors.Join(verifyEnv(), destination.VerifyPathAbsent(candidate))
	}
	if err := destination.RemoveAll(); err != nil {
		return err
	}
	postErr := errors.Join(verifyEnv(), destination.VerifyPathAbsent(candidate))
	if postErr != nil {
		return joinRollbackErrors(postErr, func() error { return destination.WriteFile(data, info.Mode().Perm()) })
	}
	return nil
}

func (p *Platform) runtimeArtifactDocuments(renderTarget string, stack *manifest.Stack, compiled *CompiledStack, resolved map[string]string) (map[string][]byte, map[string]bool, map[string]fs.FileMode, error) {
	documents := map[string][]byte{}
	deletions := map[string]bool{}
	modes := map[string]fs.FileMode{}
	relative := func(path string) (string, error) {
		rel, err := filepath.Rel(renderTarget, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("runtime artifact %q escapes render target %q", path, renderTarget)
		}
		return filepath.ToSlash(rel), nil
	}
	add := func(path string, data []byte, mode fs.FileMode) error {
		rel, err := relative(path)
		if err != nil {
			return err
		}
		documents[rel] = data
		modes[rel] = mode
		return nil
	}
	remove := func(path string) error {
		rel, err := relative(path)
		if err != nil {
			return err
		}
		deletions[rel] = true
		return nil
	}
	openBaoPath := filepath.Join(p.root, "run", "secrets.env")
	if stack.SecretsBackend.Type == "openbao" && len(resolved) != 0 {
		var out strings.Builder
		for _, key := range sortedKeys(resolved) {
			out.WriteString(substitute.SecretEnvName(key))
			out.WriteByte('=')
			out.WriteString(resolved[key])
			out.WriteByte('\n')
		}
		if err := add(openBaoPath, []byte(out.String()), 0o600); err != nil {
			return nil, nil, nil, err
		}
	} else if !p.activeEnvFileUsesPath(stack, openBaoPath) {
		if err := remove(openBaoPath); err != nil {
			return nil, nil, nil, err
		}
	}
	composePath := filepath.Join(p.root, "docker-compose.yaml")
	if len(compiled.Compose.Services) > 0 {
		data, err := compose.Marshal(compiled.Compose)
		if err != nil {
			return nil, nil, nil, err
		}
		if err := add(composePath, data, 0o644); err != nil {
			return nil, nil, nil, err
		}
	} else if err := remove(composePath); err != nil {
		return nil, nil, nil, err
	}
	processPath := filepath.Join(p.root, "process-compose.yaml")
	if len(compiled.ProcessCompose.Processes) > 0 {
		data, err := proccompose.Marshal(compiled.ProcessCompose)
		if err != nil {
			return nil, nil, nil, err
		}
		if err := add(processPath, data, 0o644); err != nil {
			return nil, nil, nil, err
		}
	} else if err := remove(processPath); err != nil {
		return nil, nil, nil, err
	}
	return documents, deletions, modes, nil
}

func (p *Platform) StackCompile(ctx context.Context) (*CompiledStack, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return nil, err
	}
	resolvedSecrets, err := secrets.ResolveDeclarations(ctx, backend, stack.Secrets, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	if err := p.materializeReferencedSources(ctx, stack); err != nil {
		return nil, err
	}
	return Compile(stack, p.root, resolvedSecrets)
}

func (p *Platform) StackStatus(ctx context.Context) (api.StackStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return api.StackStatusResponse{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.StackStatusResponse{}, err
	}
	resp := api.StackStatusResponse{
		Root:       p.root,
		Name:       stack.Name,
		Services:   map[string]api.ServiceState{},
		Jobs:       map[string]api.JobState{},
		Workspaces: map[string]api.WorkspaceRef{},
	}
	runtimeStates := p.runtimeServiceStates(ctx, stack)
	for _, name := range sortedKeys(stack.Services) {
		service := stack.Services[name]
		observed := runtimeStates[name]
		status := observed.State
		if status == "" {
			status = "declared"
		}
		resp.Services[name] = api.ServiceState{Name: name, Runtime: string(service.Runtime), Status: status, Health: observed.Health}
	}
	for _, name := range sortedKeys(stack.Jobs) {
		job := stack.Jobs[name]
		resp.Jobs[name] = api.JobState{Name: name, Runtime: string(job.Runtime)}
	}
	for _, name := range sortedKeys(stack.Workspaces) {
		workspace := stack.Workspaces[name]
		resp.Workspaces[name] = api.WorkspaceRef{
			Name:         name,
			Path:         filepath.Join(p.root, "workspaces", name),
			Template:     workspace.Template,
			TTL:          workspace.TTL,
			TTLExpiresAt: workspace.TTLExpiresAt,
		}
	}
	return resp, nil
}

// runtimeServiceStates returns the observed runtime state of each
// service keyed by manifest name, merged across the container and
// local backends. Errors are intentionally swallowed: the common
// failure modes (compose file not yet rendered, docker daemon
// unreachable, process-compose supervisor offline) all legitimately
// translate to "not observed running", and StackStatus falls back to
// the "declared" sentinel for services missing from this map.
func (p *Platform) runtimeServiceStates(ctx context.Context, stack *manifest.Stack) map[string]runtime.ServiceStatus {
	states := map[string]runtime.ServiceStatus{}
	if statuses, err := p.composeBackend.Status(ctx, runtime.StatusRequest{Root: p.root}); err == nil {
		for _, s := range statuses {
			states[s.Name] = s
		}
	}
	procReq := runtime.StatusRequest{Root: p.root, ControlPort: processComposeControlPort(stack)}
	if statuses, err := p.procBackend.Status(ctx, procReq); err == nil {
		for _, s := range statuses {
			states[s.Name] = s
		}
	}
	return states
}

func Compile(stack *manifest.Stack, root string, resolvedSecrets map[string]string) (*CompiledStack, error) {
	secretEnvVars := map[string]string{}
	for name := range resolvedSecrets {
		secretEnvVars[name] = substitute.SecretEnvName(name)
	}
	ctx := baseSubstitutionContext(stack, root, resolvedSecrets, secretEnvVars)
	mountResolver := resourceResolver(stack, root)

	compiled := &CompiledStack{
		Compose: compose.File{
			// Project identity is derived from the absolute root, not stack.Name:
			// the Compose project is a daemon-global namespace and stack.Name is a
			// non-unique display label. See composeProjectName.
			Name:     composeProjectName(stack.Name, root),
			Services: map[string]compose.Service{},
			Volumes:  map[string]compose.Volume{},
		},
		ProcessCompose: proccompose.File{
			Version:   "0.5",
			Processes: map[string]proccompose.Process{},
		},
		SecretEnvVars: secretEnvVars,
	}

	for name, volume := range stack.Volumes {
		compiled.Compose.Volumes[name] = compose.Volume{Driver: composeVolumeDriver(volume.Driver)}
	}

	for _, name := range sortedKeys(stack.Services) {
		service := stack.Services[name]
		svcCtx := ctx
		svcCtx.Name = name
		env, err := substitute.ResolveMap(service.Env, svcCtx)
		if err != nil {
			return nil, fmt.Errorf("service %s env: %w", name, err)
		}
		command, err := substitute.ResolveSlice(service.Command, svcCtx)
		if err != nil {
			return nil, fmt.Errorf("service %s command: %w", name, err)
		}
		ports, err := substitute.ResolveSlice([]string(service.Ports), svcCtx)
		if err != nil {
			return nil, fmt.Errorf("service %s ports: %w", name, err)
		}
		mounts, err := substitute.ResolveSlice([]string(service.Mounts), svcCtx)
		if err != nil {
			return nil, fmt.Errorf("service %s mounts: %w", name, err)
		}
		workdir, err := substitute.Resolve(service.Workdir, svcCtx)
		if err != nil {
			return nil, fmt.Errorf("service %s workdir: %w", name, err)
		}
		switch service.Runtime {
		case manifest.RuntimeContainer:
			containerMounts, err := resolveContainerMounts(mounts, mountResolver)
			if err != nil {
				return nil, fmt.Errorf("service %s mounts: %w", name, err)
			}
			compiled.Compose.Services[name] = compose.Service{
				Image:       service.Image,
				Build:       service.Build,
				Command:     command,
				Environment: env,
				Ports:       ports,
				Volumes:     containerMounts,
				WorkingDir:  workdir,
				DependsOn:   composeDependsOn(append(service.After, service.DependsOn...), stack),
			}
		case manifest.RuntimeLocal:
			localEnv, err := localMountEnv(mounts, mountResolver)
			if err != nil {
				return nil, fmt.Errorf("service %s mounts: %w", name, err)
			}
			if len(localEnv) > 0 && env == nil {
				env = map[string]string{}
			}
			for key, value := range localEnv {
				env[key] = value
			}
			workdir, err = mountx.ResolveWorkdir(workdir, mountResolver)
			if err != nil {
				return nil, fmt.Errorf("service %s workdir: %w", name, err)
			}
			if workdir != "" && !filepath.IsAbs(workdir) {
				workdir = filepath.Join(root, workdir)
			}
			compiled.ProcessCompose.Processes[name] = proccompose.Process{
				Command:     shellCommand(command),
				Environment: envList(env),
				WorkingDir:  workdir,
				DependsOn:   processDependsOn(append(service.After, service.DependsOn...), stack),
			}
		}
	}

	for _, name := range sortedKeys(stack.Jobs) {
		job := stack.Jobs[name]
		if job.Runtime != manifest.RuntimeLocal {
			continue
		}
		jobCtx := ctx
		jobCtx.Name = name
		env, err := substitute.ResolveMap(job.Env, jobCtx)
		if err != nil {
			return nil, fmt.Errorf("job %s env: %w", name, err)
		}
		command, err := substitute.ResolveSlice(job.Command, jobCtx)
		if err != nil {
			return nil, fmt.Errorf("job %s command: %w", name, err)
		}
		mounts, err := substitute.ResolveSlice([]string(job.Mounts), jobCtx)
		if err != nil {
			return nil, fmt.Errorf("job %s mounts: %w", name, err)
		}
		workdir, err := substitute.Resolve(job.Workdir, jobCtx)
		if err != nil {
			return nil, fmt.Errorf("job %s workdir: %w", name, err)
		}
		localEnv, err := localMountEnv(mounts, mountResolver)
		if err != nil {
			return nil, fmt.Errorf("job %s mounts: %w", name, err)
		}
		if len(localEnv) > 0 && env == nil {
			env = map[string]string{}
		}
		for key, value := range localEnv {
			env[key] = value
		}
		workdir, err = mountx.ResolveWorkdir(workdir, mountResolver)
		if err != nil {
			return nil, fmt.Errorf("job %s workdir: %w", name, err)
		}
		if workdir != "" && !filepath.IsAbs(workdir) {
			workdir = filepath.Join(root, workdir)
		}
		compiled.ProcessCompose.Processes[name] = proccompose.Process{
			Command:     shellCommand(command),
			Environment: envList(env),
			WorkingDir:  workdir,
			DependsOn:   processDependsOn(job.DependsOn, stack),
		}
	}

	edgeBackend, err := edge.FromManifest(stack.Ingress)
	if err != nil {
		return nil, fmt.Errorf("ingress backend: %w", err)
	}
	if err := edgeBackend.Contribute(stack, &compiled.Compose); err != nil {
		return nil, fmt.Errorf("ingress contribute: %w", err)
	}

	return compiled, nil
}

func envList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := sortedKeys(env)
	items := make([]string, 0, len(keys))
	for _, key := range keys {
		items = append(items, key+"="+env[key])
	}
	return items
}

func processDependsOn(names []string, stack *manifest.Stack) map[string]proccompose.ProcessDependency {
	if len(names) == 0 {
		return nil
	}
	deps := map[string]proccompose.ProcessDependency{}
	for _, name := range names {
		condition := "process_started"
		if _, ok := stack.Jobs[name]; ok {
			condition = "process_completed_successfully"
		}
		deps[name] = proccompose.ProcessDependency{Condition: condition}
	}
	return deps
}

func composeDependsOn(names []string, stack *manifest.Stack) map[string]compose.ServiceDependency {
	if len(names) == 0 {
		return nil
	}
	deps := map[string]compose.ServiceDependency{}
	for _, name := range names {
		condition := "service_started"
		if _, ok := stack.Jobs[name]; ok {
			condition = "service_completed_successfully"
		}
		deps[name] = compose.ServiceDependency{Condition: condition}
	}
	return deps
}

func resolveContainerMounts(mounts []string, resolver mountx.Resolver) ([]string, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	resolved := make([]string, 0, len(mounts))
	for _, raw := range mounts {
		if !strings.Contains(raw, "://") {
			resolved = append(resolved, raw)
			continue
		}
		mount, err := mountx.ResolveContainer(raw, resolver)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, mount)
	}
	return resolved, nil
}

func localMountEnv(mounts []string, resolver mountx.Resolver) (map[string]string, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, raw := range mounts {
		if !strings.Contains(raw, "://") {
			continue
		}
		key, value, err := mountx.ResolveLocalEnv(raw, resolver)
		if err != nil {
			return nil, err
		}
		env[key] = value
	}
	return env, nil
}

func resourceResolver(stack *manifest.Stack, root string) mountx.Resolver {
	resolver := mountx.Resolver{
		Workspaces: map[string]string{},
		Sources:    map[string]string{},
		Volumes:    map[string]string{},
	}
	for name := range stack.Workspaces {
		resolver.Workspaces[name] = filepath.Join(root, "workspaces", name)
	}
	for name, source := range stack.Sources {
		if source.Kind == "local" && source.Path != "" {
			resolver.Sources[name] = manifest.ResolvePath(root, source.Path)
			continue
		}
		cachePath := source.CachePath
		if cachePath == "" {
			cachePath = filepath.Join("sources", name)
		}
		resolver.Sources[name] = manifest.ResolvePath(root, cachePath)
	}
	for name, volume := range stack.Volumes {
		path := volume.Path
		if path == "" {
			path = filepath.Join("volumes", name)
		}
		resolver.Volumes[name] = manifest.ResolvePath(root, path)
	}
	return resolver
}

func shellCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool { return !isShellSafeRune(r) }) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func isShellSafeRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_+-=./:", r)
}

func (p *Platform) writeCompiled(compiled *CompiledStack) error {
	composePath := filepath.Join(p.root, "docker-compose.yaml")
	if len(compiled.Compose.Services) > 0 {
		data, err := compose.Marshal(compiled.Compose)
		if err != nil {
			return err
		}
		if err := os.WriteFile(composePath, data, 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(composePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	processPath := filepath.Join(p.root, "process-compose.yaml")
	if len(compiled.ProcessCompose.Processes) > 0 {
		data, err := proccompose.Marshal(compiled.ProcessCompose)
		if err != nil {
			return err
		}
		if err := os.WriteFile(processPath, data, 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(processPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (c *CompiledStack) Text() (string, error) {
	var out strings.Builder
	if len(c.Compose.Services) > 0 {
		data, err := compose.Marshal(c.Compose)
		if err != nil {
			return "", err
		}
		out.WriteString("# docker-compose.yaml\n")
		out.Write(data)
		if !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
	}
	if len(c.ProcessCompose.Processes) > 0 {
		data, err := proccompose.Marshal(c.ProcessCompose)
		if err != nil {
			return "", err
		}
		if out.Len() > 0 {
			out.WriteString("---\n")
		}
		out.WriteString("# process-compose.yaml\n")
		out.Write(data)
	}
	if out.Len() == 0 {
		out.WriteString("# no runtime services declared\n")
	}
	return out.String(), nil
}

func baseSubstitutionContext(stack *manifest.Stack, root string, resolvedSecrets, secretEnvVars map[string]string) substitute.Context {
	ports := make(map[string]int, len(stack.Ports))
	for name, port := range stack.Ports {
		ports[name] = port.Value
	}
	workspaces := make(map[string]string, len(stack.Workspaces))
	for name := range stack.Workspaces {
		workspaces[name] = filepath.Join(root, "workspaces", name)
	}
	sources := make(map[string]string, len(stack.Sources))
	for name, source := range stack.Sources {
		cachePath := source.CachePath
		if cachePath == "" {
			cachePath = filepath.Join("sources", name)
		}
		sources[name] = manifest.ResolvePath(root, cachePath)
	}
	return substitute.Context{
		Secrets:       resolvedSecrets,
		SecretEnvVars: secretEnvVars,
		Ports:         ports,
		Workspaces:    workspaces,
		Sources:       sources,
		Operator: substitute.Operator{
			URL:    stack.Operator.URL,
			Domain: stack.Operator.Domain,
		},
	}
}

func composeVolumeDriver(driver string) string {
	if driver == "" || driver == "local-fs" {
		return "local"
	}
	return driver
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func MarshalYAML(v any) ([]byte, error) {
	return yaml.Marshal(v)
}
