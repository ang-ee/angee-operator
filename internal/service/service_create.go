package service

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/fslock"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/ports"
	"github.com/ang-ee/angee-operator/internal/substitute"
	"gopkg.in/yaml.v3"
)

// serviceNamePattern bounds resolved service names: lowercase
// alphanumeric, dashes and underscores, length 1-63 (compatible with
// docker-compose service names and process-compose process names).
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// defaultServiceNamePattern is the fallback when a template doesn't
// declare `_angee.name_pattern`. Matches the brief's design contract.
const defaultServiceNamePattern = "agent-${workspace.name}"

// servicePortOwner produces the owner key used in stack.PortLeases so
// service-owned leases are distinct from workspace-owned leases.
func servicePortOwner(serviceName, pool string) string {
	return "service/" + serviceName + "/" + pool
}

// ServiceCreate renders a Copier template with `_angee.kind: service`
// into the outer stack as a single manifest.Service entry. The
// workflow is fully described in `.agents/notes/service-templates.md`.
//
// Returns the resulting api.ServiceState. On failure, allocated port
// leases are released so the next attempt sees clean state.
func (p *Platform) ServiceCreate(ctx context.Context, req api.ServiceCreateRequest) (api.ServiceState, error) {
	if req.Template == "" {
		return api.ServiceState{}, &InvalidInputError{Field: "template", Reason: "service template is required"}
	}
	if req.Workspace == "" {
		return api.ServiceState{}, &InvalidInputError{Field: "workspace", Reason: "target workspace is required"}
	}
	// Hold the root lock for the load → allocate → persist cycle so
	// concurrent ServiceCreate calls don't race on port allocation or
	// the Services[name] uniqueness check (TOCTOU). The lock is
	// released BEFORE calling StackPrepare/ServiceUp because those
	// take the same lock internally and the stdlib fslock is
	// non-recursive.
	lock := fslock.RootLock(p.root)
	var state api.ServiceState
	if err := lock.With(ctx, func() error {
		s, err := p.serviceCreateLocked(ctx, req)
		if err != nil {
			return err
		}
		state = s
		return nil
	}); err != nil {
		return api.ServiceState{}, err
	}
	// Out of the critical section now: compose re-render and optional
	// boot. If these fail the manifest entry persists; the caller can
	// recover with `angee service destroy <name>`.
	if _, err := p.StackPrepare(ctx); err != nil {
		return api.ServiceState{}, fmt.Errorf("re-render compose after service create: %w", err)
	}
	if req.Start {
		if err := p.ServiceUp(ctx, []string{state.Name}); err != nil {
			return api.ServiceState{}, err
		}
	}
	return state, nil
}

func (p *Platform) serviceCreateLocked(ctx context.Context, req api.ServiceCreateRequest) (api.ServiceState, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return api.ServiceState{}, err
	}
	workspace, ok := stack.Workspaces[req.Workspace]
	if !ok {
		return api.ServiceState{}, &NotFoundError{Kind: "workspace", Name: req.Workspace}
	}
	templatePath, _, err := p.resolveTemplate(ctx, req.Template, "service")
	if err != nil {
		return api.ServiceState{}, err
	}
	metadata, err := copierx.ValidateMetadata(templatePath, "service")
	if err != nil {
		return api.ServiceState{}, err
	}
	if err := manifest.Ensure(stack, metadata.Ensure); err != nil {
		return api.ServiceState{}, err
	}

	inputs := mergeServiceInputs(metadata, req.Inputs)
	workspacePath := filepath.Join(p.root, "workspaces", req.Workspace)

	// Resolve the service name from the template's name_pattern (or the
	// caller's override). The substitute context exposes the workspace
	// name as `${workspace.name}` plus all template inputs.
	resolveCtx := substitute.Context{
		Inputs:        inputs,
		Name:          req.Workspace,
		WorkspacePath: workspacePath,
		Workspaces:    map[string]string{req.Workspace: workspacePath},
	}
	serviceName, err := resolveServiceName(metadata, req.Name, req.Workspace, resolveCtx)
	if err != nil {
		return api.ServiceState{}, err
	}
	if _, exists := stack.Services[serviceName]; exists {
		return api.ServiceState{}, &ConflictError{Kind: "service", Name: serviceName, Reason: "already exists"}
	}

	allocations, err := p.allocateServicePorts(stack, serviceName)
	if err != nil {
		return api.ServiceState{}, err
	}
	// Defer a rollback that releases the just-allocated leases if we
	// return early with an error. Cleared on success.
	rollback := func() {
		releaseServicePortLeases(stack, serviceName)
		// Persist the release on disk so retries see clean state.
		_ = manifest.SaveFile(manifest.Path(p.root), stack)
	}
	defer func() {
		if rollback != nil {
			rollback()
		}
	}()

	renderInputs := copierx.Inputs(inputs)
	renderInputs["service_name"] = serviceName
	renderInputs["workspace_name"] = req.Workspace
	renderInputs["workspace_path"] = workspacePath
	for pool, port := range allocations {
		renderInputs["alloc_"+pool] = strconv.Itoa(port)
	}

	scratch, err := os.MkdirTemp("", "angee-service-render-*")
	if err != nil {
		return api.ServiceState{}, fmt.Errorf("create render scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := (copierx.LocalRenderer{}).Copy(ctx, copierx.CopyRequest{
		Template: templatePath,
		Dest:     scratch,
		Inputs:   renderInputs,
	}); err != nil {
		return api.ServiceState{}, fmt.Errorf("render service template: %w", err)
	}

	servicePath := filepath.Join(scratch, "service.yaml")
	rendered, err := os.ReadFile(servicePath)
	if err != nil {
		return api.ServiceState{}, fmt.Errorf("read rendered service.yaml: %w", err)
	}
	parsed, err := parsePartialServiceManifest(rendered)
	if err != nil {
		return api.ServiceState{}, fmt.Errorf("parse rendered service.yaml: %w", err)
	}
	if len(parsed.Services) != 1 {
		return api.ServiceState{}, &InvalidInputError{Field: "template", Reason: fmt.Sprintf("rendered service.yaml must declare exactly one service, got %d", len(parsed.Services))}
	}
	renderedService, renderedName := singleService(parsed.Services)
	if renderedName != serviceName {
		return api.ServiceState{}, &InvalidInputError{Field: "template", Reason: fmt.Sprintf("rendered service key %q does not match resolved name %q", renderedName, serviceName)}
	}

	// Move the rest of the rendered tree (typically `docker/`) into the
	// stack-owned build-context dir. service.yaml itself is consumed and
	// not copied. p.root is the control root (ANGEE_ROOT, normally `.angee`),
	// so the dir is `<root>/services/<name>` — a sibling of `workspaces/`,
	// `run/`, etc. — not `<root>/.angee/services/<name>`.
	buildContext := filepath.Join(p.root, "services", serviceName)
	if err := os.RemoveAll(buildContext); err != nil {
		return api.ServiceState{}, fmt.Errorf("clear previous build context %s: %w", buildContext, err)
	}
	if err := moveRenderedAssets(scratch, buildContext); err != nil {
		return api.ServiceState{}, fmt.Errorf("install build context: %w", err)
	}
	// Validate the rendered service entry before installing the build
	// context or registering it in the stack. Includes a containment
	// check on `build.context` so a hostile template can't escape into
	// the stack root.
	if err := validateRenderedServiceBuildContext(renderedService, serviceName); err != nil {
		return api.ServiceState{}, err
	}
	if err := validateService(serviceName, renderedService); err != nil {
		return api.ServiceState{}, err
	}

	// A routed service is reached through the edge and publishes no host port,
	// so release any pool lease optimistically taken before the template (which
	// determines routing) was rendered.
	if isRouted(stack, renderedService) {
		releaseServicePortLeases(stack, serviceName)
	}

	// On failure after this point, also drop the newly-declared secrets,
	// wipe the build context, and remove the in-memory service entry so the
	// deferred rollback's SaveFile doesn't persist a half-installed service.
	// Order matters: drop newly-declared secrets → wipe build context →
	// drop services map entry → release leases → persist clean state.
	//
	// Declare any secrets the service references (`${secret.NAME}`) that the stack
	// does not yet declare. A service template is forbidden from declaring secrets
	// itself (blast radius), so the operator declares what it references — as plain
	// external entries whose value comes from the secrets backend (e.g. secretSet).
	// Referencing grants no value: resolution still fails if none was ever set.
	declaredSecrets := ensureServiceSecrets(stack, renderedService)

	prevRollback := rollback
	rollback = func() {
		for _, name := range declaredSecrets {
			delete(stack.Secrets, name)
		}
		_ = os.RemoveAll(buildContext)
		delete(stack.Services, serviceName)
		prevRollback()
	}

	if stack.Services == nil {
		stack.Services = map[string]manifest.Service{}
	}
	stack.Services[serviceName] = renderedService
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return api.ServiceState{}, err
	}
	// Past this point the manifest entry and leases are committed.
	// Cancel the rollback; StackPrepare / ServiceUp run after the
	// caller releases the lock (see ServiceCreate).
	rollback = nil

	// Keep workspace reference alive in the returned state via the
	// existing ServiceState shape; full workspace mount details are
	// reachable through StackStatus.
	_ = workspace
	return api.ServiceState{Name: serviceName, Runtime: string(renderedService.Runtime), Status: "declared"}, nil
}

// ensureServiceSecrets declares any `${secret.NAME}` the service references that
// the stack does not already declare, returning the names newly added (so a
// rollback can drop them). New entries are plain external secrets — value-less
// declarations resolved from the secrets backend; referencing one grants no value.
func ensureServiceSecrets(stack *manifest.Stack, s manifest.Service) []string {
	var added []string
	for _, name := range substitute.SecretRefs(serviceSecretStrings(s)...) {
		if _, ok := stack.Secrets[name]; ok {
			continue
		}
		if stack.Secrets == nil {
			stack.Secrets = map[string]manifest.Secret{}
		}
		stack.Secrets[name] = manifest.Secret{}
		added = append(added, name)
	}
	return added
}

// serviceSecretStrings returns the service strings that undergo substitution
// (and so may reference `${secret.NAME}`): env values, command, ports, mounts,
// and workdir — the same set Compile resolves.
func serviceSecretStrings(s manifest.Service) []string {
	strs := make([]string, 0, len(s.Env)+len(s.Command)+len(s.Ports)+len(s.Mounts)+1)
	// Iterate env keys in sorted order so the referenced-secret discovery
	// order is deterministic (Go map iteration is randomized).
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		strs = append(strs, s.Env[k])
	}
	strs = append(strs, s.Command...)
	strs = append(strs, s.Ports...)
	strs = append(strs, s.Mounts...)
	strs = append(strs, s.Workdir)
	return strs
}

// resolveServiceName picks the resolved service name from (in order):
// caller override, template's name_pattern, instance_naming.pattern,
// or the defaultServiceNamePattern.
func resolveServiceName(metadata copierx.Metadata, override, workspaceName string, ctx substitute.Context) (string, error) {
	if override != "" {
		if !serviceNamePattern.MatchString(override) {
			return "", &InvalidInputError{Field: "name", Reason: "service name must match " + serviceNamePattern.String()}
		}
		return override, nil
	}
	pattern := metadata.NamePattern
	if pattern == "" {
		pattern = metadata.InstanceNaming.Pattern
	}
	if pattern == "" {
		pattern = defaultServiceNamePattern
	}
	// The substitute resolver understands `${workspace.name}` via the
	// Name field; keep an explicit alias for clarity in templates.
	if !strings.Contains(pattern, "${workspace.name}") {
		pattern = strings.ReplaceAll(pattern, "${name}", "${workspace.name}")
	}
	patternCtx := ctx
	patternCtx.Name = workspaceName
	if patternCtx.Workspaces == nil {
		patternCtx.Workspaces = map[string]string{}
	}
	// The substitute resolver doesn't speak `${workspace.name}` directly;
	// inline-substitute it with the actual workspace name so templates
	// can use the natural-looking form alongside the resolver's
	// `${name}` alias.
	expanded := strings.ReplaceAll(pattern, "${workspace.name}", workspaceName)
	resolved, err := substitute.Resolve(expanded, patternCtx)
	if err != nil {
		return "", fmt.Errorf("resolve service name pattern %q: %w", pattern, err)
	}
	resolved = strings.ToLower(strings.TrimSpace(resolved))
	if !serviceNamePattern.MatchString(resolved) {
		return "", &InvalidInputError{Field: "name", Reason: fmt.Sprintf("resolved service name %q must match %s", resolved, serviceNamePattern.String())}
	}
	return resolved, nil
}

// allocateServicePorts allocates one port from every pool declared in
// stack.Operator.PortPool, scoped to the named service. Mirrors
// allocateWorkspacePorts but with a service-prefixed owner so the
// leases don't collide.
func (p *Platform) allocateServicePorts(stack *manifest.Stack, serviceName string) (map[string]int, error) {
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
		owner := servicePortOwner(serviceName, name)
		port, err := pools[name].AllocateAvailable(owner, p.portUnavailable)
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

// releaseServicePortLeases drops every lease whose owner identifies
// the named service. Used by the rollback path and by ServiceDestroy.
func releaseServicePortLeases(stack *manifest.Stack, serviceName string) {
	prefix := "service/" + serviceName + "/"
	for pool, leases := range stack.PortLeases {
		kept := leases[:0]
		for _, lease := range leases {
			if !strings.HasPrefix(lease.Owner, prefix) {
				kept = append(kept, lease)
			}
		}
		stack.PortLeases[pool] = kept
	}
}

// mergeServiceInputs combines template-declared defaults with the
// caller's overrides. Mirrors workspaceInputs.
func mergeServiceInputs(metadata copierx.Metadata, provided map[string]string) map[string]string {
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

// partialServiceManifest is a strict subset of manifest.Stack used to
// parse the rendered service.yaml. Any field outside the allowlist
// returns an error so a template can't sneak jobs / sources / secrets
// / volumes into the outer stack.
type partialServiceManifest struct {
	Services map[string]manifest.Service `yaml:"services"`
}

// serviceManifestHeaderAllow lists the harmless manifest header keys a
// template author might naturally include on the rendered service.yaml.
// They're tolerated but ignored; only `services:` is consumed.
var serviceManifestHeaderAllow = map[string]struct{}{
	"version": {},
	"kind":    {},
	"name":    {},
}

func parsePartialServiceManifest(data []byte) (partialServiceManifest, error) {
	// First pass: decode into a structured shape.
	var parsed partialServiceManifest
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return partialServiceManifest{}, err
	}
	// Second pass: re-decode into a generic map to enforce the
	// allowlist. We accept `services:` plus the standard header keys
	// (version/kind/name). Anything else means the template is
	// emitting outside its blast radius — jobs, sources, secrets,
	// volumes, port_leases are all rejected.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return partialServiceManifest{}, err
	}
	for key := range raw {
		if key == "services" {
			continue
		}
		if _, ok := serviceManifestHeaderAllow[key]; ok {
			continue
		}
		return partialServiceManifest{}, &InvalidInputError{Field: "template", Reason: fmt.Sprintf("rendered service.yaml may only contain `services:` (with optional version/kind/name header keys); found %q", key)}
	}
	return parsed, nil
}

// validateRenderedServiceBuildContext ensures the rendered service's
// `build.context`, if any, resolves inside the stack-owned build dir
// at `services/<service_name>/` (relative to the control root, where the
// compose file is written). A hostile template could otherwise render
// `build.context: ../../../etc` and point compose at arbitrary host paths.
//
// The manifest's Build field is untyped (`any`) — it can be a bare
// string ("./docker") or a struct map ({context: "...", dockerfile:
// "..."}). Handle both shapes.
func validateRenderedServiceBuildContext(service manifest.Service, serviceName string) error {
	if service.Build == nil {
		return nil
	}
	context := ""
	switch typed := service.Build.(type) {
	case string:
		context = typed
	case map[string]any:
		if raw, ok := typed["context"]; ok {
			str, ok := raw.(string)
			if !ok {
				return &InvalidInputError{Field: "template", Reason: "rendered service build.context must be a string"}
			}
			context = str
		}
	case map[any]any:
		// yaml.v3 may emit map[any]any for nested maps depending on
		// decoding mode; cover both.
		if raw, ok := typed["context"]; ok {
			str, ok := raw.(string)
			if !ok {
				return &InvalidInputError{Field: "template", Reason: "rendered service build.context must be a string"}
			}
			context = str
		}
	}
	if context == "" {
		// Build with no context (image-only build, image: from build)
		// is fine; nothing to validate.
		return nil
	}
	// Canonical install location for template-rendered services.
	expectedPrefix := filepath.ToSlash(filepath.Join("services", serviceName)) + "/"
	clean := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(context, "./")))
	if filepath.IsAbs(context) {
		return &InvalidInputError{Field: "template", Reason: fmt.Sprintf("rendered service build.context %q must be relative to the stack root", context)}
	}
	if !strings.HasPrefix(clean, expectedPrefix) && clean != strings.TrimSuffix(expectedPrefix, "/") {
		return &InvalidInputError{Field: "template", Reason: fmt.Sprintf("rendered service build.context %q must live under services/%s/", context, serviceName)}
	}
	return nil
}

func singleService(services map[string]manifest.Service) (manifest.Service, string) {
	for name, svc := range services {
		return svc, name
	}
	return manifest.Service{}, ""
}

// moveRenderedAssets walks src and moves every file/dir other than
// service.yaml into dst. service.yaml is consumed by the parser and
// not copied. Empty source produces an empty dst dir.
func moveRenderedAssets(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	hasAssets := false
	for _, entry := range entries {
		if entry.Name() == "service.yaml" {
			continue
		}
		hasAssets = true
		break
	}
	if !hasAssets {
		return nil
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "service.yaml" {
			continue
		}
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("move %s -> %s: %w", from, to, err)
		}
	}
	return nil
}
