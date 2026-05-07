package service

import (
	"context"
	"io"
	"path/filepath"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/provision"
	"github.com/fyltr/angee/internal/runtime"
)

// AgentList returns all agents defined in angee.yaml with their runtime status.
func (p *Platform) AgentList(ctx context.Context) ([]api.AgentInfo, error) {
	cfg, err := p.loadConfig()
	if err != nil {
		return nil, err
	}

	statusMap := p.buildStatusMap(ctx)

	var agents []api.AgentInfo
	if cfg.Agents == nil {
		return agents, nil
	}
	for name, agent := range cfg.Agents.Items {
		svcName := agentServiceName(name)
		status := "stopped"
		health := "unknown"
		if st, ok := statusMap[svcName]; ok {
			status = st.Status
			health = st.Health
		}
		if h := p.Health.Status(svcName); h != "" {
			health = h
		}
		agents = append(agents, api.AgentInfo{
			Name:      name,
			Lifecycle: agent.Lifecycle,
			Role:      agent.Role,
			Status:    status,
			Health:    health,
		})
	}
	return agents, nil
}

// AgentStart starts a stopped agent by recompiling and applying.
func (p *Platform) AgentStart(ctx context.Context, name string) (*api.ApplyResult, error) {
	if err := validateResourceName("agent", name); err != nil {
		return nil, err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	cfg, err := p.loadConfig()
	if err != nil {
		return nil, err
	}
	spec, err := agentSpec(cfg, name)
	if err != nil {
		return nil, err
	}
	agentDir := p.agentDir(name)
	agentCfg, _, err := loadAgentConfig(agentDir)
	if err == nil {
		if _, err := provision.MaterializeSources(ctx, filepath.Join(agentDir, "workspace"), agentCfg.Sources, false); err != nil {
			return nil, err
		}
		if err := registerAgent(cfg, name, spec.Template, agentCfg); err != nil {
			return nil, BadRequest(err.Error())
		}
	} else {
		p.Log.Warn("agent manifest not loaded during start", "agent", name, "err", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, BadRequest(err.Error())
	}
	if err := p.resolveStackState(cfg, nil, nil); err != nil {
		return nil, err
	}
	if agentCfg != nil {
		if err := p.writeAgentEnv(name, agentCfg); err != nil {
			return nil, err
		}
	}

	if err := p.prepareAndCompile(cfg); err != nil {
		return nil, err
	}

	result, err := p.Backend.Apply(ctx, p.Root.ComposePath())
	if err != nil {
		return nil, err
	}
	p.RestartHealthProbes(ctx)
	return toAPIResult(result), nil
}

// AgentStop stops a running agent.
func (p *Platform) AgentStop(ctx context.Context, name string) error {
	if err := validateResourceName("agent", name); err != nil {
		return err
	}
	cfg, err := p.loadConfig()
	if err != nil {
		return err
	}
	if _, err := agentSpec(cfg, name); err != nil {
		return err
	}
	return p.Backend.Stop(ctx, agentServiceName(name))
}

// AgentLogs returns log text for an agent.
func (p *Platform) AgentLogs(ctx context.Context, name string, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	return p.readLogs(ctx, agentServiceName(name), runtime.LogOptions{Lines: lines})
}

// readLogs reads all log output into a string. Used by MCP tools that can't stream.
func (p *Platform) readLogs(ctx context.Context, service string, opts runtime.LogOptions) (string, error) {
	rc, err := p.Backend.Logs(ctx, service, opts)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ServiceLogs returns a streaming log reader for HTTP responses.
func (p *Platform) ServiceLogs(ctx context.Context, service string, opts runtime.LogOptions) (io.ReadCloser, error) {
	return p.Backend.Logs(ctx, service, opts)
}

// ServiceLogsText returns log text for MCP tools.
func (p *Platform) ServiceLogsText(ctx context.Context, service string, lines int) (string, error) {
	if lines <= 0 {
		lines = 100
	}
	return p.readLogs(ctx, service, runtime.LogOptions{Lines: lines})
}
