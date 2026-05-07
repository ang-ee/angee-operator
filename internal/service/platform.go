package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/compiler"
	"github.com/fyltr/angee/internal/config"
	"github.com/fyltr/angee/internal/git"
	"github.com/fyltr/angee/internal/root"
	"github.com/fyltr/angee/internal/runtime"
	composeruntime "github.com/fyltr/angee/internal/runtime/compose"
)

// Platform is the business logic core. Both HTTP handlers and MCP tools
// call methods on this struct. It owns all dependencies and never knows
// about HTTP or JSON-RPC.
type Platform struct {
	Root     *root.Root
	Cfg      *config.OperatorConfig
	Backend  runtime.RuntimeBackend
	Compiler *compiler.Compiler
	Git      *git.Repo
	Health   *HealthChecker
	Log      *slog.Logger

	// healthMu guards healthCancel against racing RestartHealthProbes calls
	// (e.g. when two HTTP clients trigger Deploy concurrently).
	healthMu     sync.Mutex
	healthCancel context.CancelFunc

	// writeMu serializes mutating operations (Deploy, Rollback, ConfigSet,
	// AgentStart/Stop) so concurrent requests can't race on angee.yaml /
	// docker-compose.yaml on disk. Read-only operations (Status, Plan,
	// History, AgentList) are not serialized.
	writeMu sync.Mutex
}

// NewPlatform creates a Platform for the given ANGEE_ROOT path.
func NewPlatform(angeeRoot string, logger *slog.Logger) (*Platform, error) {
	if err := os.MkdirAll(angeeRoot, 0755); err != nil {
		return nil, fmt.Errorf("creating ANGEE_ROOT: %w", err)
	}
	r, err := root.Open(angeeRoot)
	if err != nil {
		return nil, err
	}

	cfg, err := r.LoadOperatorConfig()
	if err != nil {
		return nil, fmt.Errorf("loading operator config: %w", err)
	}

	if logger == nil {
		logger = slog.Default()
	}

	comp := compiler.New(angeeRoot, cfg.Docker.Network)

	p := &Platform{
		Root:     r,
		Cfg:      cfg,
		Git:      git.New(angeeRoot),
		Log:      logger,
		Compiler: comp,
		Health:   newHealthChecker(logger),
	}

	comp.APIKey = p.resolveAPIKey()

	projectName := filepath.Base(filepath.Dir(angeeRoot))
	if projectName == "." || projectName == string(filepath.Separator) || projectName == "" {
		projectName = "angee"
	}
	angeeCfg, err := r.LoadAngeeConfig()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("loading angee.yaml: %w", err)
		}
	} else if angeeCfg.Name != "" {
		projectName = angeeCfg.Name
	}

	switch cfg.Runtime {
	case "kubernetes":
		return nil, fmt.Errorf("kubernetes backend not yet implemented")
	default:
		bk := composeruntime.New(angeeRoot, projectName)
		bk.Log = logger
		p.Backend = bk
	}

	return p, nil
}

// resolveAPIKey returns the API key from environment or config.
func (p *Platform) resolveAPIKey() string {
	if key := os.Getenv("ANGEE_API_KEY"); key != "" {
		return key
	}
	return p.Cfg.APIKey
}

// APIKey returns the resolved API key.
func (p *Platform) APIKey() string {
	return p.resolveAPIKey()
}

// ── Platform operations ─────────────────────────────────────────────────────

// HealthCheck returns operator liveness.
func (p *Platform) HealthCheck() *api.HealthResponse {
	return &api.HealthResponse{Status: "ok", Root: p.Root.Path, Runtime: p.Cfg.Runtime}
}

// Deploy compiles angee.yaml and applies it to the runtime.
//
// Deploy holds writeMu for the duration so concurrent requests serialize on
// the on-disk angee.yaml/docker-compose.yaml pair. Compose itself serializes
// per-project, but the in-process compile step is racy.
func (p *Platform) Deploy(ctx context.Context, message string) (*api.ApplyResult, error) {
	cfg, err := p.loadConfig()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, BadRequest(err.Error())
	}
	return p.deployConfig(ctx, cfg)
}

func (p *Platform) deployConfig(ctx context.Context, cfg *config.AngeeConfig) (*api.ApplyResult, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	if err := p.prepareAndCompile(cfg); err != nil {
		return nil, fmt.Errorf("preparing: %w", err)
	}

	result, err := p.Backend.Apply(ctx, p.Root.ComposePath())
	if err != nil {
		return nil, fmt.Errorf("applying: %w", err)
	}

	p.Log.Info("deployed",
		"started", len(result.ServicesStarted),
		"updated", len(result.ServicesUpdated),
		"removed", len(result.ServicesRemoved),
	)
	p.RestartHealthProbes(ctx)

	return toAPIResult(result), nil
}

// Plan shows what deploy would change without applying.
func (p *Platform) Plan(ctx context.Context) (*api.ChangeSet, error) {
	cfg, err := p.loadConfig()
	if err != nil {
		return nil, err
	}
	if err := p.compileAndWrite(cfg); err != nil {
		return nil, err
	}
	cs, err := p.Backend.Diff(ctx, p.Root.ComposePath())
	if err != nil {
		return nil, err
	}
	return &api.ChangeSet{Add: cs.Add, Update: cs.Update, Remove: cs.Remove}, nil
}

// Rollback reverts to a previous git commit and redeploys.
func (p *Platform) Rollback(ctx context.Context, sha string) (*api.RollbackResponse, error) {
	if sha == "" {
		return nil, BadRequest("sha is required")
	}
	if err := p.Git.RevertCtx(ctx, sha); err != nil {
		// Revert failed (often because the target SHA is the only ancestor).
		// Fall back to a hard reset; surface BOTH errors so operators can
		// see why each strategy failed.
		if err2 := p.Git.ResetHardCtx(ctx, sha); err2 != nil {
			return nil, fmt.Errorf("rollback failed: %w", errors.Join(err, err2))
		}
	}
	result, err := p.Deploy(ctx, "")
	if err != nil {
		return nil, err
	}
	newSHA, _ := p.Git.CurrentSHA()
	return &api.RollbackResponse{RolledBackTo: newSHA, Deploy: result}, nil
}

// Status returns the current runtime state of all services and agents.
func (p *Platform) Status(ctx context.Context) ([]*runtime.ServiceStatus, error) {
	statuses, err := p.Backend.Status(ctx)
	if err != nil {
		return nil, err
	}
	if statuses == nil {
		statuses = []*runtime.ServiceStatus{}
	}
	// Overlay operator-side health probe results.
	for _, st := range statuses {
		if h := p.Health.Status(st.Name); h != "" {
			st.Health = h
		}
	}
	return statuses, nil
}

// Scale adjusts the replica count for a service.
func (p *Platform) Scale(ctx context.Context, service string, replicas int) (*api.ScaleResponse, error) {
	if service == "" {
		return nil, BadRequest("service is required")
	}
	if err := p.Backend.Scale(ctx, service, replicas); err != nil {
		return nil, err
	}
	return &api.ScaleResponse{Service: service, Replicas: replicas}, nil
}

// Down brings the entire stack down.
func (p *Platform) Down(ctx context.Context) error {
	return p.Backend.Down(ctx)
}

// Pull pulls images through the selected backend.
func (p *Platform) Pull(ctx context.Context) (*api.ProvisionResponse, error) {
	if err := p.Backend.Pull(ctx); err != nil {
		return nil, err
	}
	return &api.ProvisionResponse{Status: "ok", Message: "Pulled images", Root: p.Root.Path}, nil
}

// ── Private helpers ─────────────────────────────────────────────────────────

func (p *Platform) loadConfig() (*config.AngeeConfig, error) {
	return p.Root.LoadAngeeConfig()
}

func (p *Platform) prepareAndCompile(cfg *config.AngeeConfig) error {
	if cfg.Agents != nil {
		for agentName := range cfg.Agents.Items {
			if err := p.Root.EnsureAgentDir(agentName); err != nil {
				return fmt.Errorf("agent dir for %s: %w", agentName, err)
			}
		}
		for agentName, agent := range cfg.Agents.Items {
			if err := compiler.RenderAgentFiles(p.Root.Path, p.Root.AgentDir(agentName), agent, cfg.MCPServers); err != nil {
				return fmt.Errorf("agent files for %s: %w", agentName, err)
			}
		}
	}
	return p.compileAndWrite(cfg)
}

func (p *Platform) compileAndWrite(cfg *config.AngeeConfig) error {
	cf, err := p.Compiler.Compile(cfg)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	return compiler.Write(cf, p.Root.ComposePath())
}

// RestartHealthProbes (re)starts health-check goroutines for the current config.
//
// Synchronised under healthMu — without it, two concurrent Deploys can read
// the same `healthCancel`, both invoke it, and both assign new cancel funcs,
// leaving an orphan goroutine running with a context nobody can cancel.
func (p *Platform) RestartHealthProbes(parentCtx context.Context) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()

	if p.healthCancel != nil {
		p.healthCancel()
		p.healthCancel = nil
	}
	cfg, err := p.Root.LoadAngeeConfig()
	if err != nil {
		p.Log.Warn("skipping health probes: cannot load angee.yaml", "error", err)
		return
	}
	probes := p.Health.Reload(cfg)
	if len(probes) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(parentCtx)
	p.healthCancel = cancel
	p.Health.Run(ctx, probes)
	p.Log.Info("health probes started", "count", len(probes))
}

// agentServiceName returns the docker-compose service name for an agent.
func agentServiceName(name string) string {
	return compiler.AgentServicePrefix + name
}

// buildStatusMap returns a map of service name → runtime status.
// On backend failure we log and return an empty map — callers get a
// partial answer rather than a missing one, which is more useful for
// list-style endpoints.
func (p *Platform) buildStatusMap(ctx context.Context) map[string]*runtime.ServiceStatus {
	statuses, err := p.Backend.Status(ctx)
	if err != nil {
		p.Log.Warn("status lookup failed", "err", err)
	}
	m := make(map[string]*runtime.ServiceStatus, len(statuses))
	for _, st := range statuses {
		m[st.Name] = st
	}
	return m
}

// toAPIResult converts a runtime.ApplyResult to an api.ApplyResult.
func toAPIResult(r *runtime.ApplyResult) *api.ApplyResult {
	if r == nil {
		return nil
	}
	return &api.ApplyResult{
		ServicesStarted: r.ServicesStarted,
		ServicesUpdated: r.ServicesUpdated,
		ServicesRemoved: r.ServicesRemoved,
	}
}
