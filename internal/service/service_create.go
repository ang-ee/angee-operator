package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/copierx"
	"github.com/fyltr/angee/internal/manifest"
	"github.com/fyltr/angee/internal/ports"
	"github.com/fyltr/angee/internal/substitute"
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

	allocations, err := allocateServicePorts(stack, serviceName)
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
	defer os.RemoveAll(scratch)
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
	// not copied.
	buildContext := filepath.Join(p.root, ".angee", "services", serviceName)
	if err := os.RemoveAll(buildContext); err != nil {
		return api.ServiceState{}, fmt.Errorf("clear previous build context %s: %w", buildContext, err)
	}
	if err := moveRenderedAssets(scratch, buildContext); err != nil {
		return api.ServiceState{}, fmt.Errorf("install build context: %w", err)
	}
	// On failure after this point, also wipe the build context.
	prevRollback := rollback
	rollback = func() {
		_ = os.RemoveAll(buildContext)
		prevRollback()
	}

	if err := validateService(serviceName, renderedService); err != nil {
		return api.ServiceState{}, err
	}
	if stack.Services == nil {
		stack.Services = map[string]manifest.Service{}
	}
	stack.Services[serviceName] = renderedService
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return api.ServiceState{}, err
	}
	// Past this point a failure leaves the manifest entry in place; the
	// caller can recover with `angee service destroy`. Cancel the
	// rollback since the leases are now committed.
	rollback = nil

	if _, err := p.StackPrepare(ctx); err != nil {
		return api.ServiceState{}, fmt.Errorf("re-render compose after service create: %w", err)
	}
	if req.Start {
		if err := p.ServiceStart(ctx, []string{serviceName}); err != nil {
			return api.ServiceState{}, err
		}
	}
	// Keep workspace reference alive in the returned state via the
	// existing ServiceState shape; full workspace mount details are
	// reachable through StackStatus.
	_ = workspace
	return api.ServiceState{Name: serviceName, Runtime: string(renderedService.Runtime), Status: "declared"}, nil
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
func allocateServicePorts(stack *manifest.Stack, serviceName string) (map[string]int, error) {
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

func parsePartialServiceManifest(data []byte) (partialServiceManifest, error) {
	// First pass: decode into a structured shape.
	var parsed partialServiceManifest
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return partialServiceManifest{}, err
	}
	// Second pass: re-decode into a generic map to enforce the
	// allowlist. We accept only `services:` at the top level. Anything
	// else means the template is emitting outside its blast radius.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return partialServiceManifest{}, err
	}
	for key := range raw {
		if key == "services" {
			continue
		}
		return partialServiceManifest{}, &InvalidInputError{Field: "template", Reason: fmt.Sprintf("rendered service.yaml may only contain `services:`, found %q", key)}
	}
	return parsed, nil
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
