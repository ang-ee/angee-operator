package config

import (
	"fmt"
	"strings"
)

var validLifecycles = map[string]bool{
	"":                true,
	LifecyclePlatform: true,
	LifecycleSidecar:  true,
	LifecycleWorker:   true,
	LifecycleSystem:   true,
	LifecycleAgent:    true,
	LifecycleJob:      true,
}

var validSourceKinds = map[string]bool{
	"git":      true,
	"github":   true,
	"local":    true,
	"archive":  true,
	"template": true,
	"volume":   true,
	"url":      true,
}

var validMCPTransports = map[string]bool{
	"stdio":           true,
	"sse":             true,
	"streamable-http": true,
}

// Validate checks an AngeeConfig for structural errors. It collects all
// problems rather than returning on the first failure.
func (c *AngeeConfig) Validate() error {
	var errs []string

	if c.Name == "" {
		errs = append(errs, "name is required")
	}
	if c.Kind != "" && c.Kind != "stack" && c.Kind != "workspace" && c.Kind != "agent" {
		errs = append(errs, fmt.Sprintf("kind %q is invalid", c.Kind))
	}

	validateSources(c, &errs)
	validatePortLeases(c, &errs)
	validateServices(c, &errs)
	validateJobs(c, &errs)
	validateWorkflows(c, &errs)
	validateAgents(c, &errs)
	validateMCPServers(c, &errs)

	if len(errs) > 0 {
		return fmt.Errorf("validation errors:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func validateSources(c *AngeeConfig, errs *[]string) {
	for name, source := range c.Sources {
		if source.Kind == "" {
			*errs = append(*errs, fmt.Sprintf("source %q: kind is required", name))
		} else if !validSourceKinds[source.Kind] {
			*errs = append(*errs, fmt.Sprintf("source %q: invalid kind %q", name, source.Kind))
		}
		if source.Target == "" {
			*errs = append(*errs, fmt.Sprintf("source %q: target is required", name))
		}
		if (source.Kind == "git" || source.Kind == "github") && source.Repo == "" && source.URL == "" {
			*errs = append(*errs, fmt.Sprintf("source %q: git/github source requires repo or url", name))
		}
	}
}

func validatePortLeases(c *AngeeConfig, errs *[]string) {
	usedDefaults := map[int]string{}
	for name, lease := range c.PortLeases {
		if lease.Default < 0 || lease.Default > 65535 {
			*errs = append(*errs, fmt.Sprintf("port_lease %q: default port must be between 1 and 65535", name))
		}
		if lease.Default == 0 {
			continue
		}
		if other, ok := usedDefaults[lease.Default]; ok {
			*errs = append(*errs, fmt.Sprintf("port_lease conflict: default port %d used by both %q and %q", lease.Default, other, name))
		} else {
			usedDefaults[lease.Default] = name
		}
	}
}

func validateServices(c *AngeeConfig, errs *[]string) {
	domains := map[string]string{}
	for name, svc := range c.Services {
		if !validLifecycles[svc.Lifecycle] {
			*errs = append(*errs, fmt.Sprintf("service %q: invalid lifecycle %q", name, svc.Lifecycle))
		}
		if svc.Source != "" {
			if _, ok := c.Sources[svc.Source]; !ok {
				*errs = append(*errs, fmt.Sprintf("service %q: source %q is not defined in sources", name, svc.Source))
			}
		}
		for _, port := range svc.Ports {
			if port.Name != "" {
				if _, ok := c.PortLeases[port.Name]; !ok {
					*errs = append(*errs, fmt.Sprintf("service %q: port lease %q is not defined in port_leases", name, port.Name))
				}
			}
		}
		for _, after := range svc.After {
			if _, ok := c.Services[after]; ok {
				continue
			}
			if _, ok := c.Jobs[after]; ok {
				continue
			}
			*errs = append(*errs, fmt.Sprintf("service %q: after dependency %q is not defined", name, after))
		}
		for _, d := range svc.Domains {
			port := d.Port
			if port == 0 {
				port = 8000
			}
			key := fmt.Sprintf("%s:%d", d.Host, port)
			if other, exists := domains[key]; exists {
				*errs = append(*errs, fmt.Sprintf("domain conflict: %s claimed by both %q and %q", key, other, name))
			} else {
				domains[key] = name
			}
		}
	}
}

func validateJobs(c *AngeeConfig, errs *[]string) {
	for name, job := range c.Jobs {
		if job.Source != "" {
			if _, ok := c.Sources[job.Source]; !ok {
				*errs = append(*errs, fmt.Sprintf("job %q: source %q is not defined in sources", name, job.Source))
			}
		}
		for _, after := range job.After {
			if _, ok := c.Jobs[after]; ok {
				continue
			}
			if _, ok := c.Services[after]; ok {
				continue
			}
			*errs = append(*errs, fmt.Sprintf("job %q: after dependency %q is not defined", name, after))
		}
	}
}

func validateWorkflows(c *AngeeConfig, errs *[]string) {
	for name, workflow := range c.Workflows {
		for i, activity := range workflow.Activities {
			if activity.Run == "" {
				*errs = append(*errs, fmt.Sprintf("workflow %q: activities[%d].run is required", name, i))
			}
			if activity.Source != "" {
				if _, ok := c.Sources[activity.Source]; !ok {
					*errs = append(*errs, fmt.Sprintf("workflow %q: source %q is not defined in sources", name, activity.Source))
				}
			}
			if activity.Service != "" {
				if _, ok := c.Services[activity.Service]; !ok {
					*errs = append(*errs, fmt.Sprintf("workflow %q: service %q is not defined in services", name, activity.Service))
				}
			}
		}
	}
}

func validateAgents(c *AngeeConfig, errs *[]string) {
	if c.Agents == nil {
		return
	}
	for name, agent := range c.Agents.Items {
		if agent.Workspace != "" && c.Workspaces != nil {
			if _, ok := c.Workspaces.Items[agent.Workspace]; !ok {
				*errs = append(*errs, fmt.Sprintf("agent %q: workspace %q is not defined in workspaces.items", name, agent.Workspace))
			}
		}
		if agent.Service != "" {
			if _, ok := c.Services[agent.Service]; !ok {
				*errs = append(*errs, fmt.Sprintf("agent %q: service %q is not defined in services", name, agent.Service))
			}
		}
		for _, ref := range agent.MCPServers {
			if _, ok := c.MCPServers[ref]; !ok {
				*errs = append(*errs, fmt.Sprintf("agent %q: mcp_server %q is not defined in mcp_servers", name, ref))
			}
		}
		for i, f := range agent.Files {
			hasTemplate := f.Template != ""
			hasSource := f.Source != ""
			if hasTemplate == hasSource {
				*errs = append(*errs, fmt.Sprintf("agent %q: files[%d] must have exactly one of template or source", name, i))
			}
			if f.Mount == "" {
				*errs = append(*errs, fmt.Sprintf("agent %q: files[%d] mount is required", name, i))
			}
		}
	}
}

func validateMCPServers(c *AngeeConfig, errs *[]string) {
	for name, mcp := range c.MCPServers {
		if !validMCPTransports[mcp.Transport] {
			*errs = append(*errs, fmt.Sprintf("mcp_server %q: invalid transport %q", name, mcp.Transport))
		}
		if mcp.Transport == "stdio" && len(mcp.Command) == 0 && mcp.Image == "" {
			*errs = append(*errs, fmt.Sprintf("mcp_server %q: stdio transport requires command or image", name))
		}
		if (mcp.Transport == "sse" || mcp.Transport == "streamable-http") && mcp.URL == "" {
			*errs = append(*errs, fmt.Sprintf("mcp_server %q: %s transport requires url", name, mcp.Transport))
		}
	}
}
